package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

var ErrInvalidLimit = errors.New("limit out of range")

// Repository 消费侧接口：目前只有一个查询方法。
type Repository interface {
	ListPublished(ctx context.Context, before *Cursor, limit int) ([]Video, error)
}

// PublicURLProvider 是 storage.Client 的最小投影：只声明 feed 需要的
// 一个方法，避免 feed 与整个 MinIO 客户端耦合。
type PublicURLProvider interface {
	PublicURL(objectKey string) string
}

type Service struct {
	repo    Repository
	cursor  *codec
	storage PublicURLProvider
	cache   FirstPageCache
	stats   CacheStats
	group   singleflight.Group
}

func NewService(repo Repository, codec *codec, storage PublicURLProvider, cache FirstPageCache, stats CacheStats) *Service {
	if stats == nil {
		stats = noopStats{}
	}
	return &Service{
		repo:    repo,
		cursor:  codec,
		storage: storage,
		cache:   cache,
		stats:   stats,
	}
}

type FeedItem struct {
	VideoID         int64      `json:"video_id"`
	AuthorID        int64      `json:"author_id"`
	Title           string     `json:"title"`
	CoverURL        *string    `json:"cover_url"`
	VideoURL        string     `json:"video_url"`
	DurationSeconds int        `json:"duration_seconds"`
	PublishedAt     *time.Time `json:"published_at"`
}

type FeedResult struct {
	Items      []FeedItem `json:"items"`
	NextCursor string     `json:"next_cursor"`
	HasMore    bool       `json:"has_more"`
}

// List 取一页已发布视频。limit 默认 20、上限 100；cursor 为空取第一页。
// 只有"默认 limit 的无游标请求"走缓存：自定义 limit 与缓存内容绑定，
// 混用会串数据；深层翻页命中率低不值得缓存。
func (s *Service) List(ctx context.Context, limit int, cursorRaw string) (*FeedResult, error) {
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return nil, ErrInvalidLimit
	}
	var before *Cursor
	if cursorRaw != "" {
		cur, err := s.cursor.Decode(cursorRaw)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		before = cur
	}
	if before == nil && limit == defaultLimit && s.cache != nil {
		return s.firstPage(ctx)
	}
	return s.loadPage(ctx, before, limit)
}

// firstPage Cache-Aside + singleflight 防击穿：
// 缓存未命中时并发请求合并成一次 DB 查询，避免缓存刚过期瞬间打爆 DB。
// 缓存故障（Redis 挂了）必须降级直连 DB——缓存不能成为单点故障。
func (s *Service) firstPage(ctx context.Context) (*FeedResult, error) {
	cached, err := s.cache.Get(ctx)
	if err != nil {
		slog.Warn("feed cache get failed", "err", err)
	} else if cached != nil {
		s.stats.Hit()
		return cached, nil
	}
	s.stats.Miss()
	v, err, _ := s.group.Do(firstPageKey, func() (any, error) {
		// 二次检查（double-checked locking）：singleflight 只合并时间上
		// 重叠的调用——等待期间若已有执行者回填缓存，直接命中，不再查库。
		cached, err := s.cache.Get(ctx)
		if err == nil && cached != nil {
			return cached, nil
		}
		result, err := s.loadPage(ctx, nil, defaultLimit)
		if err != nil {
			return nil, err
		}
		// Set 失败只影响后续命中率，不能因此让本次请求失败。
		if err := s.cache.Set(ctx, result); err != nil {
			slog.Warn("feed cache set failed", "err", err)
		}
		return result, nil
	})
	if err != nil {
		slog.Error("first page load failed", "err", err)
		return nil, err
	}
	return v.(*FeedResult), nil
}

// loadPage 查库 + 拼 URL + 生成游标，是 firstPage 与翻页共用的核心。
func (s *Service) loadPage(ctx context.Context, before *Cursor, limit int) (*FeedResult, error) {
	videos, err := s.repo.ListPublished(ctx, before, limit)
	if err != nil {
		slog.Error("list published videos failed", "err", err)
		return nil, fmt.Errorf("list published: %w", err)
	}
	result := &FeedResult{Items: make([]FeedItem, 0, limit)}
	hasMore := len(videos) > limit
	if hasMore {
		videos = videos[:limit]
	}
	for _, v := range videos {
		item := FeedItem{
			VideoID:         v.ID,
			AuthorID:        v.AuthorID,
			Title:           v.Title,
			DurationSeconds: v.DurationSeconds,
			PublishedAt:     v.PublishedAt,
		}
		if v.CoverKey != nil {
			cover := s.storage.PublicURL(*v.CoverKey)
			item.CoverURL = &cover
		}
		item.VideoURL = s.storage.PublicURL(v.ObjectKey)
		result.Items = append(result.Items, item)
	}
	if hasMore {
		last := videos[len(videos)-1]
		next, err := s.cursor.Encode(Cursor{
			PublishedAt: *last.PublishedAt,
			VideoID:     last.ID,
			RankVersion: CurrentRankVersion,
		})
		if err != nil {
			slog.Error("encode next cursor failed", "err", err)
			return nil, fmt.Errorf("encode next cursor: %w", err)
		}
		result.NextCursor = next
		result.HasMore = true
	}
	return result, nil
}
