package feed

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// HotFeed 是热榜读取的消费侧接口（service 定义，Redis 实现注入）。
type HotFeed interface {
	// Page 读取一页热榜。cursor 为 nil 表示第一页。
	Page(ctx context.Context, windowEnd time.Time, cursor *HotCursor, limit int) ([]HotEntry, error)
	// CurrentWindowEnd 返回当前窗口（精确到分钟）。
	CurrentWindowEnd() time.Time
}

// ListHot 读取热榜。
//
// 三条设计取舍：
//
//  1. **降级而不是报错**：Redis 不可用或窗口为空（近期没有任何互动）时，
//     回落到时间流并打点标记。热榜是"锦上添花"的场景，让用户看到内容
//     比看到 500 更重要——同类项目在这里直接 500，是可用性上的退步。
//
//  2. **游标冻结快照**：window_end 进签名游标，已有窗口不重写；窗口过期
//     拒绝旧游标。只冻结窗口名而允许重建改变分数仍会重复/漏数。
//
//  3. **只缓存第一页以外不做页缓存**：热榜本身就是 Redis 里的一份物化快照，
//     再套一层页缓存是重复的（也避免了同类项目"每个游标一个 key"的基数问题）。
func (s *Service) ListHot(ctx context.Context, limit int, cursorRaw string) (*FeedResult, error) {
	if limit == 0 {
		limit = hotFeedDefaultLimit
	}
	if limit < 1 || limit > hotFeedMaxLimit {
		return nil, ErrInvalidLimit
	}
	var cursor *HotCursor
	var windowEnd time.Time
	if cursorRaw != "" {
		parsed, err := s.cursor.Decode(cursorRaw)
		if err != nil {
			return nil, err
		}
		// Once a session falls back, keep using its signed timeline cursor even after Redis recovers.
		if parsed.RankVersion == CurrentRankVersion {
			return s.List(ctx, limit, cursorRaw)
		}
		parsed, err = s.decodeHotCursor(cursorRaw)
		if err != nil {
			return nil, err
		}
		windowEnd = parsed.WindowEnd
		cursor = &HotCursor{WindowEnd: windowEnd, HotScore: parsed.HotScore, VideoID: parsed.VideoID}
	}
	if s.hot == nil {
		if cursor != nil {
			return nil, ErrInvalidCursor
		}
		return s.hotFallback(ctx, limit, "", "disabled")
	}
	if cursor == nil {
		windowEnd = s.hot.CurrentWindowEnd()
	}

	// 多取一条判断 HasMore（与时间流 assemble 的做法一致）。
	entries, err := s.hot.Page(ctx, windowEnd, cursor, limit+1)
	if err != nil {
		if errors.Is(err, ErrInvalidCursor) || cursor != nil {
			return nil, err
		}
		if errors.Is(err, ErrHotFeedUnavailable) {
			// 窗口为空（近期没有互动）不是故障，降级为时间流。
			return s.hotFallback(ctx, limit, "", "empty_window")
		}
		slog.WarnContext(ctx, "hot feed unavailable, degrading to timeline", "err", err)
		return s.hotFallback(ctx, limit, "", "redis_error")
	}
	if len(entries) == 0 && cursor == nil {
		return s.hotFallback(ctx, limit, "", "empty_page")
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	items, err := s.loadHotItems(ctx, entries)
	if err != nil {
		return nil, err
	}
	result := &FeedResult{Items: items}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	if err := s.fillProductCards(ctx, result); err != nil {
		return nil, err
	}
	if next := s.encodeHotCursor(windowEnd, entries); next != "" {
		result.NextCursor = &next
	}
	result.HasMore = hasMore
	s.hotStats.HotFeedSource("window")
	return result, nil
}

// hotFallback 降级为时间流。degraded 原因进指标，便于区分"没热度"与"Redis 挂了"。
func (s *Service) hotFallback(ctx context.Context, limit int, cursorRaw, reason string) (*FeedResult, error) {
	s.hotStats.HotFeedDegraded(reason)
	return s.List(ctx, limit, cursorRaw)
}

// loadHotItems 把热榜条目补全成 FeedItem（复用既有查询与拼装逻辑）。
func (s *Service) loadHotItems(ctx context.Context, entries []HotEntry) ([]FeedItem, error) {
	ids := make([]int64, 0, len(entries))
	scores := make(map[int64]int64, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.VideoID)
		scores[entry.VideoID] = entry.HotScore
	}
	videos, err := s.repo.ListByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Video, len(videos))
	for _, video := range videos {
		byID[video.ID] = video
	}

	items := make([]FeedItem, 0, len(entries))
	for _, entry := range entries {
		video, ok := byID[entry.VideoID]
		if !ok {
			// 视频被删除或转为不可见：跳过。热榜是物化快照，
			// 快照里的条目可能在窗口存续期间失效。
			continue
		}
		item, err := s.buildFeedItem(ctx, video)
		if err != nil {
			return nil, err
		}
		item.HotScore = scores[entry.VideoID]
		items = append(items, item)
	}
	return items, nil
}

// decodeHotCursor 解析热榜游标：签名 + 版本 + 字段齐全。
func (s *Service) decodeHotCursor(cursorRaw string) (*Cursor, error) {
	cursor, err := s.cursor.Decode(cursorRaw)
	if err != nil {
		return nil, err
	}
	if cursor.RankVersion != hotCursorRankVersion {
		// 排序规则变了（或客户端拿的是时间流游标）：让客户端回到第一页，
		// 而不是拿两套排序键拼出一个错误的分页。
		return nil, ErrInvalidCursor
	}
	if cursor.WindowEnd.IsZero() || cursor.HotScore <= 0 {
		return nil, ErrInvalidCursor
	}
	return cursor, nil
}

func (s *Service) encodeHotCursor(windowEnd time.Time, entries []HotEntry) string {
	if len(entries) == 0 {
		return ""
	}
	last := entries[len(entries)-1]
	encoded, err := s.cursor.Encode(Cursor{
		VideoID:     last.VideoID,
		WindowEnd:   windowEnd,
		HotScore:    last.HotScore,
		RankVersion: hotCursorRankVersion,
	})
	if err != nil {
		// 编码失败只影响翻页，不影响本页内容：返回空游标让客户端停在第一页。
		slog.Error("encode hot cursor failed", "err", err)
		return ""
	}
	return encoded
}
