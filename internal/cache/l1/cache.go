// Package l1 实现进程内一级缓存（L1），配合 Redis 二级缓存（L2）组成两级缓存。
//
// 为什么值得加：TokMall 的 Feed 首页缓存命中时仍要付出"Redis 往返 + 大 payload
// 传输 + JSON 反序列化"的成本（压测基线里 11.3k QPS 的天花板就在这）。
// 进程内缓存把热 key 的读取降到一次内存查表 + 一次反序列化（甚至可以缓存
// 已构造好的结果对象，见 feed 模块的用法）。
//
// 难点从来不在"怎么缓存"，而在**失效**：
//   - 单实例下删除本地 key 就够了；多实例下，A 实例写入后 B 实例的 L1 仍是旧值；
//   - 常见的两种做法是"广播失效"（Pub/Sub）与"版本号 + 短 TTL 探测"。
//     本项目选后者：写路径递增 Redis 里的版本号，读路径用**本地缓存的版本号**
//     （默认 1 秒刷新一次）与 L1 条目记录的版本比对，不一致就当未命中。
//     代价是最长 1 秒的陈旧窗口，收益是不需要维护订阅连接、丢消息也不会永久脏
//     （TTL 兜底），而且**每个实例的失效判断完全一致**。
//
// 分片是为了降低锁竞争：单锁 LRU 在几十万 QPS 下会成为瓶颈，
// 这里按 key 的哈希取模分到固定数量的分片，每个分片一把锁 + 一个 LRU 链表。
package l1

import (
	"container/list"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 是缓存统计接口（由 observability 实现）。
type Metrics interface {
	L1Hit()
	L1Miss()
	L1Eviction()
	L1Stale() // 因为版本不一致被判为未命中
}

type nopMetrics struct{}

func (nopMetrics) L1Hit()      {}
func (nopMetrics) L1Miss()     {}
func (nopMetrics) L1Eviction() {}
func (nopMetrics) L1Stale()    {}

type entry struct {
	key       string
	value     []byte
	version   int64
	expiresAt time.Time
}

type shard struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	order    *list.List // 头部最新，尾部最旧
	capacity int
}

// Cache 是分片 LRU + TTL + 版本校验的进程内缓存。
// 并发安全；零值不可用，必须用 New 构造。
type Cache struct {
	shards  []*shard
	ttl     time.Duration
	metrics Metrics

	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
	stales    atomic.Int64
}

type Option func(*Cache)

// WithTTL 设置条目存活时间（默认 5 秒）。L1 的 TTL 应当**远短于** L2，
// 因为它是"最终一致"的那一层，短 TTL 是失效机制失灵时的兜底。
func WithTTL(ttl time.Duration) Option { return func(c *Cache) { c.ttl = ttl } }

// WithMetrics 注入统计。
func WithMetrics(metrics Metrics) Option { return func(c *Cache) { c.metrics = metrics } }

// New 构造缓存。shards 为分片数，capacity 为**总分片**容量上限（每片取均值）。
func New(shards, capacity int, opts ...Option) *Cache {
	if shards <= 0 {
		shards = 16
	}
	if capacity <= 0 {
		capacity = 1024
	}
	perShard := capacity / shards
	if perShard < 1 {
		perShard = 1
	}
	c := &Cache{ttl: 5 * time.Second, metrics: nopMetrics{}}
	for _, opt := range opts {
		opt(c)
	}
	c.shards = make([]*shard, shards)
	for i := range c.shards {
		c.shards[i] = &shard{items: make(map[string]*list.Element, perShard), order: list.New(), capacity: perShard}
	}
	return c
}

// Get 读取缓存。version 必须与条目写入时的版本一致才命中——
// 这是多实例失效的关键：写入方递增版本号后，其它实例的旧条目自动失效。
func (c *Cache) Get(key string, version int64) ([]byte, bool) {
	s := c.shardFor(key)
	now := time.Now()

	s.mu.Lock()
	element, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		c.misses.Add(1)
		c.metrics.L1Miss()
		return nil, false
	}
	item, ok := element.Value.(*entry)
	if !ok {
		// 链表节点类型由本包独占维护，理论上不可能失败；
		// 但断言失败会 panic，这里按未命中处理（防御性）。
		s.mu.Unlock()
		c.misses.Add(1)
		c.metrics.L1Miss()
		return nil, false
	}
	if now.After(item.expiresAt) {
		s.order.Remove(element)
		delete(s.items, key)
		s.mu.Unlock()
		c.misses.Add(1)
		c.metrics.L1Miss()
		return nil, false
	}
	if item.version != version {
		// 版本不一致：数据已被写入方更新，本地副本作废。
		s.order.Remove(element)
		delete(s.items, key)
		s.mu.Unlock()
		c.stales.Add(1)
		c.metrics.L1Stale()
		return nil, false
	}
	s.order.MoveToFront(element)
	value := item.value
	s.mu.Unlock()

	c.hits.Add(1)
	c.metrics.L1Hit()
	return value, true
}

// Set 写入缓存，记录写入时的数据版本。
func (c *Cache) Set(key string, value []byte, version int64) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.items[key]; ok {
		item, ok := existing.Value.(*entry)
		if !ok {
			// 链表节点由本包独占维护，类型不可能不符；防御性删除后按新增处理。
			s.order.Remove(existing)
			delete(s.items, key)
		} else {
			item.value = value
			item.version = version
			item.expiresAt = time.Now().Add(c.ttl)
			s.order.MoveToFront(existing)
			return
		}
	}
	element := s.order.PushFront(&entry{key: key, value: value, version: version, expiresAt: time.Now().Add(c.ttl)})
	s.items[key] = element
	if s.order.Len() > s.capacity {
		c.evictLocked(s)
	}
}

// Delete 删除单个 key（写路径的本地失效；其它实例靠版本号失效）。
func (c *Cache) Delete(key string) {
	s := c.shardFor(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.items[key]; ok {
		s.order.Remove(element)
		delete(s.items, key)
	}
}

// Purge 清空全部条目（配置变更、人工运维用）。
func (c *Cache) Purge() {
	for _, s := range c.shards {
		s.mu.Lock()
		s.items = make(map[string]*list.Element, s.capacity)
		s.order.Init()
		s.mu.Unlock()
	}
}

// Len 返回当前条目数（测试与排障用）。
func (c *Cache) Len() int {
	total := 0
	for _, s := range c.shards {
		s.mu.Lock()
		total += len(s.items)
		s.mu.Unlock()
	}
	return total
}

// Stats 返回累计计数。
type Stats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	Stales    int64
	Entries   int
}

func (c *Cache) Stats() Stats {
	return Stats{
		Hits: c.hits.Load(), Misses: c.misses.Load(),
		Evictions: c.evictions.Load(), Stales: c.stales.Load(), Entries: c.Len(),
	}
}

func (c *Cache) evictLocked(s *shard) {
	element := s.order.Back()
	if element == nil {
		return
	}
	item, ok := element.Value.(*entry)
	s.order.Remove(element)
	if !ok {
		return
	}
	delete(s.items, item.key)
	c.evictions.Add(1)
	c.metrics.L1Eviction()
}

func (c *Cache) shardFor(key string) *shard {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	return c.shards[hasher.Sum32()%uint32(len(c.shards))]
}
