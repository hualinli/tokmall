// Package existence 用共享负缓存挡住已经确认不存在的 ID。
//
// 本地布隆过滤器曾经放在这里，但阴性结果不能证明其他实例没有刚创建这条记录，
// 所以它从不参与判定，只占内存并每 10 分钟全表重建。判定只保留负缓存。
package existence

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Guard 只查 Redis 负缓存。未命中或缓存故障都放行到数据库。
type Guard struct {
	namespaces map[string]struct{}
	missing    MissingMarker
	stats      Stats
	ttl        time.Duration
	jitter     time.Duration
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
	NegativeHit(namespace string)
	MissMarked(namespace string)
	Lookup(namespace string, result string)
}

type NopStats struct{}

func (NopStats) NegativeHit(string)    {}
func (NopStats) MissMarked(string)     {}
func (NopStats) Lookup(string, string) {}

type GuardOption func(*Guard)

// WithTTL 设置负缓存有效期；WithJitter 让同一批 key 不同时过期（防雪崩）。
func WithTTL(ttl time.Duration) GuardOption  { return func(g *Guard) { g.ttl = ttl } }
func WithJitter(j time.Duration) GuardOption { return func(g *Guard) { g.jitter = j } }
func WithStats(stats Stats) GuardOption      { return func(g *Guard) { g.stats = stats } }

// NewGuard 创建防护器。namespaces 是允许查询的名字（视频、商品分开），
// 避免一个域的负缓存键和另一个域撞在一起。
func NewGuard(namespaces []string, marker MissingMarker, opts ...GuardOption) *Guard {
	names := make(map[string]struct{}, len(namespaces))
	for _, name := range namespaces {
		names[name] = struct{}{}
	}
	g := &Guard{
		namespaces: names,
		missing:    marker,
		stats:      NopStats{},
		ttl:        30 * time.Second,
		jitter:     10 * time.Second,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// ErrNoFilter 表示查询了未注册的命名空间。名字沿用旧装配错误，避免调用方改判断。
var ErrNoFilter = errors.New("existence: namespace has no filter registered")

// MayExist 只在负缓存确认 miss 时返回 false。缓存故障放行到数据库。
func (g *Guard) MayExist(ctx context.Context, namespace string, id int64) (bool, error) {
	if _, ok := g.namespaces[namespace]; !ok {
		return false, ErrNoFilter
	}
	missing, err := g.missing.IsMissing(ctx, namespace, id)
	if err != nil {
		// 负缓存故障必须 fail-open：返回"可能存在"让请求继续走到数据库。
		// 反过来（当成不存在）会凭空造出假 404。
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
// TTL 带随机抖动：否则同一批被扫描的 ID 会在同一秒集中过期。
func (g *Guard) MarkMissing(ctx context.Context, namespace string, id int64) {
	ttl := g.ttl
	if g.jitter > 0 {
		ttl += time.Duration(rand.Int64N(int64(g.jitter)))
	}
	if err := g.missing.MarkMissing(ctx, namespace, id, ttl); err != nil {
		return
	}
	g.stats.MissMarked(namespace)
}
