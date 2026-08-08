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
	ListPublishedByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error)
	LikeCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error)
}

// PlayURLProvider 是 storage.Client 的最小投影：feed 只消费"签发播放 URL"
// 这一个能力。签名即授权：URL 短时效，删除后旧 URL 指向空对象（404）。
type PlayURLProvider interface {
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
}

type Service struct {
	repo    Repository
	cursor  *codec
	storage PlayURLProvider
	cache   FirstPageCache
	stats   CacheStats
	group   singleflight.Group
}

func NewService(repo Repository, codec *codec, storage PlayURLProvider, cache FirstPageCache, stats CacheStats) *Service {
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

// AuthorRef 卡片上的创作者信息（公开，不含 email）。
type AuthorRef struct {
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatar_url"`
}

type FeedItem struct {
	VideoID         int64      `json:"video_id"`
	AuthorID        int64      `json:"author_id"`
	Title           string     `json:"title"`
	CoverURL        *string    `json:"cover_url"`
	VideoURL        string     `json:"video_url"`
	DurationSeconds int        `json:"duration_seconds"`
	PublishedAt     *time.Time `json:"published_at"`
	Author          AuthorRef  `json:"author"`
	LikeCount       int64      `json:"like_count"`
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
	var (
		result *FeedResult
		err    error
	)
	if before == nil && limit == defaultLimit && s.cache != nil {
		result, err = s.firstPage(ctx)
	} else {
		result, err = s.loadPage(ctx, before, limit)
	}
	if err != nil {
		return nil, err
	}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListByAuthor 公开作者主页：已发布作品，FeedItem 结构复用。
// 不缓存（作者页 QPS 低 + 个性化字段）。
func (s *Service) ListByAuthor(ctx context.Context, authorID int64, limit int, cursorRaw string) (*FeedResult, error) {
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
	result, err := s.loadAuthorPage(ctx, authorID, before, limit)
	if err != nil {
		return nil, err
	}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// fillLikeCounts 批量查点赞数并写入 items。关键取舍：
// like_count 不写进缓存、点赞不触发缓存失效（高频写会导致失效风暴，
// 毁掉 first-page 缓存收益）——代价是每次请求一次 IN 查询（毫秒级）。
// 注意浅拷贝 items：singleflight 的共享对象不能被并发改写。
func (s *Service) fillLikeCounts(ctx context.Context, result *FeedResult) error {
	ids := make([]int64, 0, len(result.Items))
	for _, it := range result.Items {
		ids = append(ids, it.VideoID)
	}
	counts, err := s.repo.LikeCounts(ctx, ids)
	if err != nil {
		slog.Error("like counts failed", "err", err)
		return fmt.Errorf("like counts: %w", err)
	}
	// make+copy 而非 append(nil, ...)：空页时保持非 nil（JSON 输出 [] 而非 null）
	items := make([]FeedItem, len(result.Items))
	copy(items, result.Items)
	for i := range items {
		items[i].LikeCount = counts[items[i].VideoID]
	}
	result.Items = items
	return nil
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
	return s.assemble(ctx, videos, limit)
}

// loadAuthorPage 作者主页页：与 loadPage 唯一的区别是查询带 author 过滤。
func (s *Service) loadAuthorPage(ctx context.Context, authorID int64, before *Cursor, limit int) (*FeedResult, error) {
	videos, err := s.repo.ListPublishedByAuthor(ctx, authorID, before, limit)
	if err != nil {
		slog.Error("list videos by author failed", "author_id", authorID, "err", err)
		return nil, fmt.Errorf("list by author: %w", err)
	}
	return s.assemble(ctx, videos, limit)
}

// assemble 视频行 -> FeedItem（拼预签名 URL + 作者信息）+ 游标。
func (s *Service) assemble(ctx context.Context, videos []Video, limit int) (*FeedResult, error) {
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
			Author: AuthorRef{
				UserID:   v.AuthorID,
				Username: v.Username,
			},
		}
		if v.CoverKey != nil {
			cover, err := s.storage.PresignGetURL(ctx, *v.CoverKey)
			if err != nil {
				slog.Error("presign cover url failed", "err", err)
				return nil, fmt.Errorf("presign cover url: %w", err)
			}
			item.CoverURL = &cover
		}
		videoURL, err := s.storage.PresignGetURL(ctx, v.ObjectKey)
		if err != nil {
			slog.Error("presign video url failed", "err", err)
			return nil, fmt.Errorf("presign video url: %w", err)
		}
		item.VideoURL = videoURL
		if v.AvatarKey != nil {
			// 头像签名失败降级为 null（装饰性字段）
			avatar, err := s.storage.PresignGetURL(ctx, *v.AvatarKey)
			if err != nil {
				slog.Warn("presign avatar url failed", "err", err)
			} else {
				item.Author.AvatarURL = &avatar
			}
		}
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
