package feed

import (
	"context"
	"math"
	"sort"
	"time"
)

// Scene 排序场景：不同场景允许不同的排序规则（M8 演进）。
type Scene string

const (
	SceneGlobal     Scene = "global"
	SceneFollowing  Scene = "following"
	SceneAuthorHome Scene = "author_home"
)

// Candidate 排序候选：召回结果 + 排序所需计数。
// 计数在 Rank 前由 service 按 NeedsCounts 填充（TimeRanker 不查计数，省一次聚合）。
type Candidate struct {
	Video     Video
	PlayCount int64
	LikeCount int64
}

// Ranker 排序器：候选召回与排序分离（M8）。
// 演进路径：TimeRanker -> HotRanker -> HybridRanker；
// 每次换排序规则，Version() 必须递增——旧游标因版本不匹配自动失效（service 校验）。
type Ranker interface {
	// Rank 对候选集排序（返回新切片，不修改入参）。
	Rank(ctx context.Context, candidates []Candidate, scene Scene) ([]Candidate, error)
	// NeedsCounts 排序是否需要播放/点赞计数。
	NeedsCounts() bool
	// Version 排序规则版本：写入游标，切换排序规则时递增使旧游标失效。
	Version() string
	// SupportsCursorPagination 表示排序结果是否能与当前游标语义组合。
	// 排序键必须与召回的游标键一致，或自身提供冻结的稳定排序键。
	SupportsCursorPagination() bool
}

// TimeRanker 时间排序：与现有全局 Feed 的 SQL 排序键一致
// (published_at DESC, id DESC)。tie-break 用 id 保证稳定顺序。
type TimeRanker struct{}

func (TimeRanker) Rank(_ context.Context, candidates []Candidate, _ Scene) ([]Candidate, error) {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Video.PublishedAt.Equal(*out[j].Video.PublishedAt) {
			return out[i].Video.PublishedAt.After(*out[j].Video.PublishedAt)
		}
		return out[i].Video.ID > out[j].Video.ID
	})
	return out, nil
}

func (TimeRanker) NeedsCounts() bool { return false }

// Version 与 cursor.go 的 CurrentRankVersion 历史值保持一致
// （time-v1 是全局 Feed 与关注 Feed 共用的时间排序版本）。
func (TimeRanker) Version() string { return "time-v1" }

func (TimeRanker) SupportsCursorPagination() bool { return true }

// HotRanker 热度排序：分数 = (log(plays+1) + 2*log(likes+1)) / (age_hours + 2)^1.5。
// - log 压缩：防头部视频分数无限膨胀（播放百万与千万差距收敛）
// - likes 权重高于 plays：plays 可刷（自播/机器），likes 是更强的质量信号
// - 时间衰减：(age+2)^1.5，两小时窗口内新内容有探索流量，冷启动兜底
// 分数相同时回退时间排序（tie-break 可解释）。
type HotRanker struct{}

func (HotRanker) Rank(_ context.Context, candidates []Candidate, _ Scene) ([]Candidate, error) {
	now := time.Now()
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	sort.SliceStable(out, func(i, j int) bool {
		si := hotScore(&out[i], now)
		sj := hotScore(&out[j], now)
		if si != sj {
			return si > sj
		}
		if !out[i].Video.PublishedAt.Equal(*out[j].Video.PublishedAt) {
			return out[i].Video.PublishedAt.After(*out[j].Video.PublishedAt)
		}
		return out[i].Video.ID > out[j].Video.ID
	})
	return out, nil
}

func (HotRanker) NeedsCounts() bool { return true }

// Version 热度排序第一版：仅用于排序单元实验，不作为在线游标版本。
func (HotRanker) Version() string { return "hot-v1" }

// SupportsCursorPagination 返回 false：当前 HotRanker 的分数依赖请求时刻，
// 而关注 Feed 的召回/游标仍按 (published_at,id) 裁剪。仅让 rank_version
// 变化不能把这两个排序键变成同一个稳定顺序，因此不能安全地产生深分页游标。
func (HotRanker) SupportsCursorPagination() bool { return false }

func hotScore(c *Candidate, now time.Time) float64 {
	age := now.Sub(*c.Video.PublishedAt).Hours()
	plays := math.Log(float64(c.PlayCount) + 1)
	likes := 2 * math.Log(float64(c.LikeCount)+1)
	decay := math.Pow(age+2, 1.5)
	return (plays + likes) / decay
}
