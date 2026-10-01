// Package existence 提供"这个 ID 到底有没有可能存在"的廉价预判，
// 用来挡住缓存穿透：对不存在的 ID 反复查询会把请求全部打到数据库。
//
// 两层防护，顺序固定：
//
//	布隆过滤器（进程内，零网络往返）
//	  → 判 false：一定不存在，直接返回 404，不查库也不查缓存；
//	负缓存（Redis，带 TTL 抖动）
//	  → 上一次查库确认不存在，短期内不再查库；
//	数据库
//	  → 只有前两层都放行才真正查询，查到不存在时回写负缓存。
//
// 为什么布隆过滤器用进程内而不是 Redis：它只需要回答"可能/一定不"，
// 放在进程内没有网络往返（这正是它相对负缓存的价值）；代价是每个实例
// 各有一份，靠"定期重建 + 轮换"保持与数据库接近，而不是靠分布式一致性。
// 删除/下架不会从过滤器里移除（布隆过滤器不支持删除），残留的假阳性
// 只会让请求多走一次数据库，不会把存在的数据判成不存在——方向是安全的。
package existence

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Filter 是固定容量的布隆过滤器实现（不依赖第三方库）。
//
// 参数按标准公式推导：
//
//	m = -n·ln(p) / (ln2)²   位数组长度
//	k = (m/n)·ln2           哈希函数个数
//
// 自己实现而不是引库的原因：位数组、双哈希（Kirsch-Mitzenmacher 优化）
// 和误判率验证合起来不到 100 行，且可以在测试里直接对照理论误判率；
// 引入依赖反而看不到这层数学。生产上换成熟库同样是合理选择。
type Filter struct {
	bits    []uint64
	m       uint64 // 位数组长度
	k       uint64 // 哈希个数
	n       uint64 // 设计容量
	added   uint64 // 已加入元素数（近似）
	mu      sync.RWMutex
	addedAt atomic.Int64
}

// NewFilter 按期望元素数与目标误判率创建过滤器。
func NewFilter(expected uint64, falsePositiveRate float64) *Filter {
	if expected == 0 {
		expected = 1
	}
	if falsePositiveRate <= 0 || falsePositiveRate >= 1 {
		falsePositiveRate = 0.01
	}
	m := uint64(math.Ceil(-float64(expected) * math.Log(falsePositiveRate) / (math.Ln2 * math.Ln2)))
	if m < 64 {
		m = 64
	}
	// 向上取整到 64 位的整数倍，便于按字寻址。
	m = (m + 63) / 64 * 64
	k := uint64(math.Round(float64(m) / float64(expected) * math.Ln2))
	if k < 1 {
		k = 1
	}
	if k > 32 {
		k = 32
	}
	return &Filter{
		bits: make([]uint64, m/64),
		m:    m,
		k:    k,
		n:    expected,
	}
}

// Add 加入一个元素。布隆过滤器只能加不能删，这是它的定义决定的。
func (f *Filter) Add(value uint64) {
	h1, h2 := splitmix64(value), splitmix64(value^0x9e3779b97f4a7c15)
	f.mu.Lock()
	for i := uint64(0); i < f.k; i++ {
		pos := (h1 + i*h2) % f.m
		f.bits[pos/64] |= 1 << (pos % 64)
	}
	f.added++
	f.mu.Unlock()
	f.addedAt.Store(time.Now().UnixNano())
}

// Test 返回 true 表示"可能存在"，false 表示"一定不存在"。
// 注意方向：假阳性（说存在其实不存在）允许，假阴性（说不存在其实存在）不允许。
func (f *Filter) Test(value uint64) bool {
	h1, h2 := splitmix64(value), splitmix64(value^0x9e3779b97f4a7c15)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for i := uint64(0); i < f.k; i++ {
		pos := (h1 + i*h2) % f.m
		if f.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

// Count 返回已加入的元素数（近似，含重复加入）。
func (f *Filter) Count() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.added
}

// Bits / Hashes 暴露推导出的参数，便于测试对照理论值与文档说明。
func (f *Filter) Bits() uint64   { return f.m }
func (f *Filter) Hashes() uint64 { return f.k }

// AddedAt 返回最后一次写入时间，供"多久没更新过"的告警使用。
func (f *Filter) AddedAt() time.Time { return time.Unix(0, f.addedAt.Load()) }

// splitmix64 是一个廉价、雪崩性好的整数混合函数：把 ID 打散成均匀分布的
// 64 位值。用它同时生成 h1/h2 是布隆过滤器的常见做法（双哈希），
// 等价于 k 个独立哈希函数但只算一次。
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// Rotating 是"双缓冲 + 轮换"的布隆过滤器集合：
//
//	重建时写入 standby，重建完成后与 active 交换；查询同时问 active 与 standby。
//
// 为什么需要它：布隆过滤器不能删除元素，业务数据却在删除；只靠"只加不减"
// 会让误判率随历史数据无限上升。轮换用一次全量重建把过滤器拉回设计容量，
// 同时保证切换瞬间不出现假阴性（旧数据仍在 standby 里）。
type Rotating struct {
	expected uint64
	fpRate   float64

	mu      sync.RWMutex
	active  *Filter
	standby *Filter

	rebuiltAt time.Time
	builds    atomic.Int64
	// ready 表示至少完成过一次全量重建。空过滤器会对所有 ID 返回"不存在"，
	// 如果启动后立刻用它做判断，真实数据会被整片判成 404——这是灾难性假阴性。
	// 因此重建完成前一律放行（warmup 状态），只损失一点防护效果，不影响正确性。
	ready atomic.Bool
}

func NewRotating(expected uint64, fpRate float64) *Rotating {
	return &Rotating{
		expected: expected,
		fpRate:   fpRate,
		active:   NewFilter(expected, fpRate),
		standby:  NewFilter(expected, fpRate),
	}
}

func (r *Rotating) Add(value uint64) {
	r.mu.RLock()
	active := r.active
	r.mu.RUnlock()
	active.Add(value)
}

// Ready 表示过滤器已经装载过一次全量数据，可以参与"一定不存在"的判断。
func (r *Rotating) Ready() bool { return r.ready.Load() }

func (r *Rotating) Test(value uint64) bool {
	r.mu.RLock()
	active, standby := r.active, r.standby
	r.mu.RUnlock()
	// 两个都问：重建期间新数据进 standby，旧数据还在 active。
	return active.Test(value) || standby.Test(value)
}

// BeginRebuild 清空 standby，准备接收全量数据。
func (r *Rotating) BeginRebuild() *Filter {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.standby = NewFilter(r.expected, r.fpRate)
	return r.standby
}

// CommitRebuild 把重建好的过滤器切换为 active，原 active 保留为 standby。
func (r *Rotating) CommitRebuild() {
	r.mu.Lock()
	r.active, r.standby = r.standby, r.active
	r.rebuiltAt = time.Now()
	r.mu.Unlock()
	r.builds.Add(1)
	r.ready.Store(true)
}

func (r *Rotating) RebuiltAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rebuiltAt
}

func (r *Rotating) Builds() int64 { return r.builds.Load() }

// Snapshot 返回 active 过滤器的统计，供指标暴露。
func (r *Rotating) Snapshot() (count uint64, bits uint64, hashes uint64) {
	r.mu.RLock()
	active := r.active
	r.mu.RUnlock()
	return active.Count(), active.Bits(), active.Hashes()
}

// Guard 把"布隆过滤器 + 负缓存"组合成对业务友好的接口。
//
// 负缓存放在 Redis 而不是进程内：它需要跨实例共享（一台实例查过的
// 不存在的 ID，另一台不必再查库），并且带 TTL 自动过期。
type Guard struct {
	filters map[string]*Rotating
	missing MissingMarker
	stats   Stats
	ttl     time.Duration
	jitter  time.Duration
	now     func() time.Time
}

// MissingMarker 是负缓存的存储接口（由 Redis 实现，定义在消费侧）。
type MissingMarker interface {
	// MarkMissing 记录"该 ID 已确认不存在"，ttl 后自动过期。
	MarkMissing(ctx context.Context, namespace string, id int64, ttl time.Duration) error
	// IsMissing 返回该 ID 是否处于"已确认不存在"的窗口内。
	IsMissing(ctx context.Context, namespace string, id int64) (bool, error)
}

// Stats 是缓存穿透防护的指标接口（实现在 observability）。
type Stats interface {
	BloomRejected(namespace string)
	NegativeHit(namespace string)
	MissMarked(namespace string)
	Lookup(namespace string, result string)
}

type NopStats struct{}

func (NopStats) BloomRejected(string)  {}
func (NopStats) NegativeHit(string)    {}
func (NopStats) MissMarked(string)     {}
func (NopStats) Lookup(string, string) {}

type GuardOption func(*Guard)

// WithTTL 设置负缓存有效期；WithJitter 让同一批 key 不同时过期（防雪崩）。
func WithTTL(ttl time.Duration) GuardOption  { return func(g *Guard) { g.ttl = ttl } }
func WithJitter(j time.Duration) GuardOption { return func(g *Guard) { g.jitter = j } }
func WithStats(stats Stats) GuardOption      { return func(g *Guard) { g.stats = stats } }

// NewGuard 创建防护器。namespaces 里每个名字对应一组独立的布隆过滤器
// （视频、商品各自独立，避免一个域的容量影响另一个域的误判率）。
func NewGuard(namespaces map[string]uint64, marker MissingMarker, fpRate float64, opts ...GuardOption) *Guard {
	g := &Guard{
		filters: make(map[string]*Rotating, len(namespaces)),
		missing: marker,
		stats:   NopStats{},
		ttl:     30 * time.Second,
		jitter:  10 * time.Second,
		now:     time.Now,
	}
	for name, expected := range namespaces {
		g.filters[name] = NewRotating(expected, fpRate)
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// ErrNoFilter 表示查询了未注册的命名空间——这是装配错误，必须显式暴露。
var ErrNoFilter = errors.New("existence: namespace has no filter registered")

// MayExist 是热路径上的廉价判断：
//
//	false → 一定不存在（布隆过滤器保证无假阴性），调用方直接返回 404；
//	true  → 可能不存在，继续走负缓存与数据库。
//
// Redis 故障时返回 true（放行到数据库）：宁可多查一次库，也不能因为
// 缓存故障把真实存在的数据判成不存在。
func (g *Guard) MayExist(ctx context.Context, namespace string, id int64) (bool, error) {
	filter, ok := g.filters[namespace]
	if !ok {
		return false, ErrNoFilter
	}
	if !filter.Ready() {
		// 尚未完成首次装载：放行到数据库，绝不因为"过滤器是空的"而误判不存在。
		g.stats.Lookup(namespace, "warmup")
		return true, nil
	}
	if !filter.Test(uint64(id)) {
		g.stats.BloomRejected(namespace)
		g.stats.Lookup(namespace, "bloom_rejected")
		return false, nil
	}
	missing, err := g.missing.IsMissing(ctx, namespace, id)
	if err != nil {
		// 负缓存故障必须 fail-open：返回"可能存在"让请求继续走到数据库。
		// 反过来（当成不存在）会凭空造出假 404，是灾难性错误。
		g.stats.Lookup(namespace, "negative_cache_error")
		return true, nil //nolint:nilerr // 依赖故障时刻意 fail-open，见上方注释
	}
	if missing {
		g.stats.NegativeHit(namespace)
		g.stats.Lookup(namespace, "negative_cache_hit")
		return false, nil
	}
	g.stats.Lookup(namespace, "database")
	return true, nil
}

// MarkMissing 在数据库确认"不存在"之后回写负缓存。
// TTL 带随机抖动：否则同一批被扫描的 ID 会在同一秒集中过期，
// 又一起打到数据库（这就是缓存雪崩的最小形态）。
func (g *Guard) MarkMissing(ctx context.Context, namespace string, id int64) {
	ttl := g.ttl
	if g.jitter > 0 {
		// 抖动来自真随机数：用时间取模会让同一毫秒内的写入拿到同样的 TTL。
		ttl += time.Duration(rand.Int64N(int64(g.jitter)))
	}
	if err := g.missing.MarkMissing(ctx, namespace, id, ttl); err != nil {
		// 负缓存写失败只影响后续请求的成本，不影响本次正确性。
		return
	}
	g.stats.MissMarked(namespace)
}

// Add 在业务侧成功创建实体后把 ID 加入过滤器，避免"刚创建就被判不存在"。
func (g *Guard) Add(namespace string, id int64) {
	if filter, ok := g.filters[namespace]; ok {
		filter.Add(uint64(id))
	}
}

// Rebuild 全量重建指定命名空间的过滤器：loader 负责把当前所有存在的 ID
// 喂给返回的 Filter，然后 Commit。重建期间查询同时看新旧两份，不会出现假阴性。
func (g *Guard) Rebuild(namespace string, loader func(add func(id int64) error) error) error {
	filter, ok := g.filters[namespace]
	if !ok {
		return ErrNoFilter
	}
	filter.BeginRebuild()
	if err := loader(func(id int64) error { filter.Add(uint64(id)); return nil }); err != nil {
		// 重建失败不回滚 standby：它仍是空过滤器，查询结果由 active 决定，
		// 不会产生假阴性；下次重建会重新覆盖。
		return err
	}
	filter.CommitRebuild()
	return nil
}

// Snapshot 返回某命名空间 active 过滤器的统计（元素数、位数组长度、哈希个数）。
func (g *Guard) Snapshot(namespace string) (count uint64, bits uint64, hashes uint64) {
	filter, ok := g.filters[namespace]
	if !ok {
		return 0, 0, 0
	}
	return filter.Snapshot()
}

// Ready 返回某个命名空间是否已完成首次装载。
func (g *Guard) Ready(namespace string) bool {
	filter, ok := g.filters[namespace]
	return ok && filter.Ready()
}

// Namespaces 返回已注册的命名空间，供启动时逐个重建。
func (g *Guard) Namespaces() []string {
	names := make([]string, 0, len(g.filters))
	for name := range g.filters {
		names = append(names, name)
	}
	return names
}
