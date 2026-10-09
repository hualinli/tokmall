package feed

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// 热榜（Hot Feed）
//
// 借鉴自同类项目 GCFeed 的"分钟桶 + 滑动窗口快照"结构，但修掉了它的两个缺陷：
//
//	缺陷一（游标漂移）：GCFeed 的缓存路径用 `window_end + offset` 翻页。
//	  窗口一旦被重建（TTL 到期后按分钟桶重新合并、期间新互动改变分数），
//	  offset 就会指向不同的条目 → 翻页重复或漏数。
//	  本实现改为冻结窗口 + 分数 keyset 游标，已有窗口不重写，
//	  快照过期时拒绝旧游标。分数游标本身不能弥补排序键改变。
//
//	缺陷二（重建击穿）：GCFeed 在窗口缺失时每个请求都重做一次 60-key 合并
//	  （窗口内没有正分成员时 ZUNIONSTORE 不创建 key，Exists 永远为 0）。
//	  本实现用三层防护：后台物化（每分钟）+ singleflight 合并回源 + double-check。
//
// 与既有 HotRanker 的关系：`HotRanker` 是进程内实时计算的热度，分数随请求时刻
// 变化，因此 `SupportsCursorPagination()` 一直是 false（AGENTS.md 记为
// "排序键未与游标闭合"）。本实现把分数**物化**到 Redis（写入时累加、读时冻结），
// 排序键因此稳定可游标——这正是那条遗留问题缺失的一半。

const (
	// hotMinuteBucketTTL：分钟桶保留时长。窗口 60 分钟，桶留 2 小时，
	// 跨小时边界重建时旧桶仍在。
	hotMinuteBucketTTL = 2 * time.Hour
	// hotWindowTTL：物化窗口的存活时间。TTL 内窗口内容不变（冻结快照），
	// 这是"翻页稳定"的物理保证。
	hotWindowTTL = 10 * time.Minute
	// hotWindowMinutes：滑动窗口长度（分钟）。
	hotWindowMinutes = 60
	// hotWindowMaxEntries：窗口条目硬上限。
	//
	// 为什么必须有上限：ZUNIONSTORE 的成本与桶内成员数成正比，而热榜只需要头部。
	// "Redis 里的新结构必须有硬上限"是引入任何 ZSET/List 时的纪律——
	// 否则它迟早变成 OOM 的来源。
	hotWindowMaxEntries = 2000
	// hotFeedDefaultLimit / hotFeedMaxLimit：热榜单页大小。
	hotFeedDefaultLimit = 20
	hotFeedMaxLimit     = 50
)

// 热度权重：一次**状态变化**（不是一次请求）贡献的分数。
//
// 两个设计点：
//
//  1. 为什么按状态变化计分：点赞接口是幂等的（ON CONFLICT DO NOTHING），
//     重复点赞不改变事实，也就不应该重复加热度。仓储把"是否真的插入/删除"返回给
//     service，service 只在此刻计分——刷接口刷不出热度（同类项目的评论接口
//     只在客户端主动传幂等键时去重，不传就能无限刷，热度榜可被单人刷爆）。
//
//  2. 为什么权重定义在 feed 而不是 interaction：权重是**排序模型的一部分**
//     （榜单认为一次点赞值多少分），不是互动模块的业务规则。互动模块只上报
//     "发生了什么动作"，由这里决定分值——这样调整权重不需要改互动模块，
//     也不会出现两处各定义一份权重。
const (
	HotActionLike   = "like"
	HotActionUnlike = "unlike"
	HotActionPlay   = "play"
)

var hotScoreWeights = map[string]int{
	HotActionLike:   3,
	HotActionUnlike: -3,
	// 播放是最弱信号：只值 1 分（且必须是有幂等键的新播放事件）。
	HotActionPlay: 1,
}

// HotScoreForAction 返回动作对应的热度增量。未知动作返回 0（不计分）。
func HotScoreForAction(action string) int { return hotScoreWeights[action] }

// HotScoreRecorder 是热榜计分接口（interaction / video 模块作为使用方定义，
// main 装配时注入 Redis 实现）。计分失败**不得影响主流程**：
// 点赞成功但热度没加上，只是榜单少一点分数；点赞失败才是业务问题。
type HotScoreRecorder interface {
	// AddHotScore 按**动作名**计分（权重由 feed 决定，见 hotScoreWeights）。
	// 调用方只上报"发生了什么"，不关心分值。
	AddHotScore(ctx context.Context, videoID int64, action string, at time.Time) error
}

// HotEntry 是热榜窗口里的一条：视频 ID 与窗口内热度分。
type HotEntry struct {
	VideoID  int64
	HotScore int64
}

// HotCursor 是热榜游标载荷：冻结窗口 + 分数位置。
type HotCursor struct {
	// WindowEnd 冻结窗口（精确到分钟）。同一段翻页期间不变。
	WindowEnd time.Time `json:"window_end"`
	// HotScore + VideoID 是 keyset 位置：分数并列时用 video_id 做次级排序，
	// 因此"同分"不会导致重复或漏数。
	HotScore int64 `json:"hot_score"`
	VideoID  int64 `json:"video_id"`
}

// hotCursorRankVersion 是热榜游标版本：与 time-v1 隔离，切换排序规则时递增，
// 旧游标自动失效（沿用既有 Ranker 版本约定）。
const hotCursorRankVersion = "hot-window-v2"

// ErrHotFeedUnavailable 表示热榜数据源不可用（Redis 故障或窗口为空）。
// 调用方据此降级为时间流而不是返回错误——热榜是"锦上添花"的场景，
// 不可用时让用户看到内容比看到错误更重要。
var ErrHotFeedUnavailable = errors.New("hot feed unavailable")

// hotMinuteKey 是分钟桶 key。
func hotMinuteKey(at time.Time) string {
	return "feed:hot:v2:minute:" + at.UTC().Format("200601021504")
}

// hotMinuteKeys 返回参与合并的 60 个分钟桶 key（含 windowEnd 所在分钟）。
func hotMinuteKeys(windowEnd time.Time) []string {
	end := windowEnd.UTC().Truncate(time.Minute)
	keys := make([]string, 0, hotWindowMinutes)
	for i := hotWindowMinutes - 1; i >= 0; i-- {
		keys = append(keys, hotMinuteKey(end.Add(-time.Duration(i)*time.Minute)))
	}
	return keys
}

// hotWindowKey 是物化窗口 key：windowEnd 精确到分钟。
func hotWindowKey(windowEnd time.Time) string {
	return "feed:hot:v2:window:" + strconv.FormatInt(windowEnd.UTC().Truncate(time.Minute).Unix(), 10)
}

func hotRankMember(videoID int64) string { return fmt.Sprintf("%019d", videoID) }

func hotRankVideoID(member string) (int64, bool) {
	id, err := strconv.ParseInt(member, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// HotWindowStats 是热榜的可观测接口（observability 实现）。
type HotWindowStats interface {
	HotScoreUpdate(action string)
	HotWindowRebuild(source string)
	HotWindowSize(size int)
	HotFeedSource(source string)
	HotFeedDegraded(reason string)
}

type nopHotStats struct{}

func (nopHotStats) HotScoreUpdate(string)   {}
func (nopHotStats) HotWindowRebuild(string) {}
func (nopHotStats) HotWindowSize(int)       {}
func (nopHotStats) HotFeedSource(string)    {}
func (nopHotStats) HotFeedDegraded(string)  {}
