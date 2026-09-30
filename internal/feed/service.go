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
	// singleflightTimeout 是 singleflight 执行体的独立超时：它不继承首个
	// 请求的取消信号（避免 leader 断连把等待者一起拖垮），但也不能无限期
	// 占用——Redis/PG 挂住时必须有上界。
	singleflightTimeout = 3 * time.Second
)

var (
	ErrInvalidLimit              = errors.New("limit out of range")
	ErrRankPaginationUnsupported = errors.New("ranker does not support cursor pagination")
)

// Repository 消费侧接口：feed 只消费这几个查询，不拥有视频事实数据。
type Repository interface {
	ListPublished(ctx context.Context, before *Cursor, limit int) ([]Video, error)
	ListPublishedByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error)
	ListFollowingPublished(ctx context.Context, userID int64, before *Cursor, limit int) ([]Video, error)
	LikeCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error)
	PlayCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error)
}

// PlayURLProvider 是 storage.Client 的最小投影：feed 只消费
// "签发播放 URL"和"签发头像 URL"两个能力。
type PlayURLProvider interface {
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
	PresignAvatarURL(ctx context.Context, objectKey string) (string, error)
}

type Service struct {
	repo           Repository
	productCards   ProductCardReader
	cursor         *codec
	storage        PlayURLProvider
	cache          FirstPageCache
	followingCache FollowingFeedCache
	stats          CacheStats
	group          singleflight.Group
	ranker         Ranker
}

func (s *Service) SetProductCardReader(reader ProductCardReader) { s.productCards = reader }

// NewService 构造 feed 服务。cache/followingCache 可为 nil（缓存禁用时降级直查）；
// ranker 可选（variadic）：缺省 TimeRanker（当前生产语义）。
// HotRanker 只保留在 Ranker 单元实验中；不支持通过 Service 暴露在线分页。
func NewService(repo Repository, codec *codec, storage PlayURLProvider, cache FirstPageCache, followingCache FollowingFeedCache, stats CacheStats, rankers ...Ranker) *Service {
	if stats == nil {
		stats = noopStats{}
	}
	ranker := Ranker(TimeRanker{})
	if len(rankers) > 0 {
		ranker = rankers[0]
	}
	return &Service{
		repo:           repo,
		cursor:         codec,
		storage:        storage,
		cache:          cache,
		followingCache: followingCache,
		stats:          stats,
		ranker:         ranker,
	}
}

// AuthorRef 卡片上的创作者信息（公开，不含 email）。
type AuthorRef struct {
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatar_url"`
}

type FeedItem struct {
	VideoID         int64         `json:"video_id"`
	AuthorID        int64         `json:"author_id"`
	Title           string        `json:"title"`
	CoverURL        *string       `json:"cover_url"`
	VideoURL        string        `json:"video_url"`
	DurationSeconds int           `json:"duration_seconds"`
	PublishedAt     *time.Time    `json:"published_at"`
	Author          AuthorRef     `json:"author"`
	LikeCount       int64         `json:"like_count"`
	ProductCards    []ProductCard `json:"product_cards"`
}

type FeedResult struct {
	Items      []FeedItem `json:"items"`
	NextCursor *string    `json:"next_cursor"`
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
	before, err := s.decodeCursor(cursorRaw, CurrentRankVersion)
	if err != nil {
		return nil, err
	}
	var result *FeedResult
	shared := before == nil && limit == defaultLimit && s.cache != nil
	if shared {
		result, err = s.firstPage(ctx)
	} else {
		result, err = s.loadPage(ctx, before, limit)
	}
	if err != nil {
		return nil, err
	}
	// firstPage may return a cache/singleflight-shared value. Establish request
	// ownership before filling the request-time like_count field. Fresh DB pages
	// already belong to this request and avoid the extra copy.
	if shared {
		result = cloneFeedResult(result)
	}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	if err := s.fillProductCards(ctx, result); err != nil {
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
	before, err := s.decodeCursor(cursorRaw, CurrentRankVersion)
	if err != nil {
		return nil, err
	}
	result, err := s.loadAuthorPage(ctx, authorID, before, limit)
	if err != nil {
		return nil, err
	}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	if err := s.fillProductCards(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListFollowing 关注 Feed（拉模式，生产仅使用 TimeRanker）：
// 查询全部关注作者中游标之后的 limit+1 条 -> 填充排序计数（ranker 需要时）->
// Ranker 排序 -> 分页截断。排序与召回分离（M8）：
// 当前只有 TimeRanker 的排序键与游标一致；HotRanker 在语义闭合前由 Service 拒绝。
// 仅默认 limit 的无游标第一页使用按用户隔离的 Redis 缓存；深层翻页实时查询。
func (s *Service) ListFollowing(ctx context.Context, viewerID int64, limit int, cursorRaw string) (*FeedResult, error) {
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return nil, ErrInvalidLimit
	}
	if !s.ranker.SupportsCursorPagination() {
		return nil, ErrRankPaginationUnsupported
	}
	before, err := s.decodeCursor(cursorRaw, s.ranker.Version())
	if err != nil {
		return nil, err
	}
	// 只缓存"无游标 + 默认 limit"的第一页：自定义 limit 与缓存内容绑定，
	// 混用会串数据；深层翻页命中率低（与全局首页缓存 ADR-012 同取舍）。
	var result *FeedResult
	shared := before == nil && limit == defaultLimit && s.followingCache != nil
	if shared {
		result, err = s.followingFirstPage(ctx, viewerID)
	} else {
		result, err = s.loadFollowingPage(ctx, viewerID, before, limit)
	}
	if err != nil {
		return nil, err
	}
	// like_count 不进缓存（点赞高频，避免失效风暴），每次请求实时批量查。
	// followingFirstPage may return a cache/singleflight-shared value, so copy
	// both the result and its Items backing array before this request mutates it.
	// Fresh DB pages already belong to this request and avoid the extra copy.
	if shared {
		result = cloneFeedResult(result)
	}
	if err := s.fillLikeCounts(ctx, result); err != nil {
		return nil, err
	}
	if err := s.fillProductCards(ctx, result); err != nil {
		return nil, err
	}
	return result, nil
}

// Product relations and cover URLs are request-time data. They must not be
// stored in the shared first-page cache: unlinking or taking a product off
// sale must be visible on the next request without invalidating every feed.
func (s *Service) fillProductCards(ctx context.Context, result *FeedResult) error {
	ids := make([]int64, 0, len(result.Items))
	for _, item := range result.Items {
		ids = append(ids, item.VideoID)
	}
	var rows []productCardRow
	if s.productCards != nil && len(ids) > 0 {
		var err error
		rows, err = s.productCards.ListProductCards(ctx, ids)
		if err != nil {
			return fmt.Errorf("list product cards: %w", err)
		}
	}
	byVideo := make(map[int64][]ProductCard, len(ids))
	for _, row := range rows {
		card := ProductCard{ProductID: row.ProductID, Title: row.Title, MinPrice: row.MinPrice, ProductStatus: row.ProductStatus, SortOrder: row.SortOrder}
		if row.CoverKey != "" {
			url, err := s.storage.PresignGetURL(ctx, row.CoverKey)
			if err != nil {
				return fmt.Errorf("presign product cover: %w", err)
			}
			card.CoverURL = &url
		}
		byVideo[row.VideoID] = append(byVideo[row.VideoID], card)
	}
	for i := range result.Items {
		cards := byVideo[result.Items[i].VideoID]
		if cards == nil {
			cards = []ProductCard{}
		}
		result.Items[i].ProductCards = cards
	}
	return nil
}

// followingFirstPage 关注 Feed 第一页缓存：cache-aside + singleflight + double-check，
// 与 firstPage 同模式（防击穿；缓存故障降级直查，Set 失败不影响本次响应）。
// key 含 rank_version：未来切排序规则后旧缓存自然隔离。空关注用户的空结果也缓存
// （避免 0 关注用户每次都查 DB）。返回值可能是缓存/singleflight 共享快照，
// 由 ListFollowing 在写入请求级字段前完成 copy-on-read。
func (s *Service) followingFirstPage(ctx context.Context, viewerID int64) (*FeedResult, error) {
	version := s.ranker.Version()
	cached, err := s.followingCache.Get(ctx, viewerID, version)
	if err != nil {
		slog.Warn("following feed cache get failed", "user_id", viewerID, "err", err)
	} else if cached != nil {
		return cached, nil
	}
	// 执行体绑定到独立上下文：singleflight 会把同一个结果（含错误）广播给
	// 所有等待者，如果闭包直接用"首个请求"的 ctx，leader 断连/超时就会把
	// 失败扩散成整批请求 500——缓存击穿保护反而变成故障放大器。
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), singleflightTimeout)
	defer cancel()
	v, err, _ := s.group.Do(followingCacheKey(viewerID, version), func() (any, error) {
		// double-check：singleflight 只合并时间上重叠的调用，等待期间可能已回填。
		cached, err := s.followingCache.Get(loadCtx, viewerID, version)
		if err == nil && cached != nil {
			return cached, nil
		}
		result, err := s.loadFollowingPage(loadCtx, viewerID, nil, defaultLimit)
		if err != nil {
			return nil, err
		}
		if err := s.followingCache.Set(loadCtx, viewerID, version, result); err != nil {
			slog.Warn("following feed cache set failed", "user_id", viewerID, "err", err)
		}
		return result, nil
	})
	if err != nil {
		slog.Error("following first page load failed", "user_id", viewerID, "err", err)
		return nil, err
	}
	return v.(*FeedResult), nil
}

// loadFollowingPage 关注 Feed 的查询 + 排序 + 拼 URL（无缓存路径，时间游标翻页共用）。
func (s *Service) loadFollowingPage(ctx context.Context, viewerID int64, before *Cursor, limit int) (*FeedResult, error) {
	videos, err := s.repo.ListFollowingPublished(ctx, viewerID, before, limit)
	if err != nil {
		slog.Error("list following published failed", "user_id", viewerID, "err", err)
		return nil, fmt.Errorf("list following published: %w", err)
	}
	candidates := make([]Candidate, len(videos))
	for i, v := range videos {
		candidates[i] = Candidate{Video: v}
	}
	if s.ranker.NeedsCounts() {
		ids := make([]int64, len(videos))
		for i, v := range videos {
			ids[i] = v.ID
		}
		playCounts, err := s.repo.PlayCounts(ctx, ids)
		if err != nil {
			slog.Error("play counts failed", "err", err)
			return nil, fmt.Errorf("play counts: %w", err)
		}
		likeCounts, err := s.repo.LikeCounts(ctx, ids)
		if err != nil {
			slog.Error("like counts failed", "err", err)
			return nil, fmt.Errorf("like counts: %w", err)
		}
		for i := range candidates {
			candidates[i].PlayCount = playCounts[candidates[i].Video.ID]
			candidates[i].LikeCount = likeCounts[candidates[i].Video.ID]
		}
	}
	ranked, err := s.ranker.Rank(ctx, candidates, SceneFollowing)
	if err != nil {
		slog.Error("rank following candidates failed", "err", err)
		return nil, fmt.Errorf("rank following: %w", err)
	}
	sorted := make([]Video, len(ranked))
	for i, c := range ranked {
		sorted[i] = c.Video
	}
	return s.assemble(ctx, sorted, limit, s.ranker.Version())
}

// decodeCursor 解码游标并校验排序版本与调用方的排序语义一致。
// 每个端点传自己的期望版本：全局/作者 Feed 固定 time-v1（SQL 排序，不随 ranker 变）；
// 关注 Feed 仍使用 time-v1，候选范围扩大但时间排序键不变。
func (s *Service) decodeCursor(cursorRaw, expectedVersion string) (*Cursor, error) {
	if cursorRaw == "" {
		return nil, nil
	}
	cur, err := s.cursor.Decode(cursorRaw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	if cur.RankVersion != expectedVersion {
		return nil, ErrInvalidCursor
	}
	return cur, nil
}

// fillLikeCounts 批量查点赞数并写入 items。调用方必须拥有 result 及其
// Items backing array；缓存/ singleflight 返回的共享结果要先经过
// cloneFeedResult。like_count 不写进缓存、点赞不触发缓存失效（高频写会导致
// 失效风暴，毁掉 first-page 缓存收益）——代价是每次请求一次 IN 查询（毫秒级）。
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
	for i := range result.Items {
		result.Items[i].LikeCount = counts[result.Items[i].VideoID]
	}
	return nil
}

// cloneFeedResult 建立请求侧所有权：缓存和 singleflight 都可能把同一个
// *FeedResult 返回给多个请求，外层结果和 Items backing array 必须一起脱离，
// 才能安全填充请求级动态字段。FeedItem 内的 URL 指针在 service 中只读，
// 不需要为本次 LikeCount 更新递归复制。
func cloneFeedResult(result *FeedResult) *FeedResult {
	if result == nil {
		return nil
	}
	cloned := *result
	if result.Items != nil {
		cloned.Items = make([]FeedItem, len(result.Items))
		copy(cloned.Items, result.Items)
	}
	if result.NextCursor != nil {
		nextCursor := *result.NextCursor
		cloned.NextCursor = &nextCursor
	}
	return &cloned
}

// firstPage Cache-Aside + singleflight 防击穿：
// 缓存未命中时并发请求合并成一次 DB 查询，避免缓存刚过期瞬间打爆 DB。
// 缓存故障（Redis 挂了）必须降级直连 DB——缓存不能成为单点故障。
// 返回值可能是缓存/singleflight 共享快照，由 List 在写入请求级字段前
// 完成 copy-on-read；该函数自身不修改返回的 FeedResult。
func (s *Service) firstPage(ctx context.Context) (*FeedResult, error) {
	cached, err := s.cache.Get(ctx)
	if err != nil {
		slog.Warn("feed cache get failed", "err", err)
	} else if cached != nil {
		s.stats.Hit()
		return cached, nil
	}
	s.stats.Miss()
	// 同 followingFirstPage：执行体不继承首个请求的取消信号，
	// 否则 leader 断连会把同批等待者一起拖成 500。
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), singleflightTimeout)
	defer cancel()
	v, err, _ := s.group.Do(firstPageKey, func() (any, error) {
		// 二次检查（double-checked locking）：singleflight 只合并时间上
		// 重叠的调用——等待期间若已有执行者回填缓存，直接命中，不再查库。
		cached, err := s.cache.Get(loadCtx)
		if err == nil && cached != nil {
			return cached, nil
		}
		result, err := s.loadPage(loadCtx, nil, defaultLimit)
		if err != nil {
			return nil, err
		}
		// Set 失败只影响后续命中率，不能因此让本次请求失败。
		if err := s.cache.Set(loadCtx, result); err != nil {
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
	return s.assemble(ctx, videos, limit, CurrentRankVersion)
}

// loadAuthorPage 作者主页页：与 loadPage 唯一的区别是查询带 author 过滤。
func (s *Service) loadAuthorPage(ctx context.Context, authorID int64, before *Cursor, limit int) (*FeedResult, error) {
	videos, err := s.repo.ListPublishedByAuthor(ctx, authorID, before, limit)
	if err != nil {
		slog.Error("list videos by author failed", "author_id", authorID, "err", err)
		return nil, fmt.Errorf("list by author: %w", err)
	}
	return s.assemble(ctx, videos, limit, CurrentRankVersion)
}

// assemble 视频行 -> FeedItem（拼预签名 URL + 作者信息）+ 游标。
// rankVersion 由调用方传入：当前各在线端点均使用 time-v1。
func (s *Service) assemble(ctx context.Context, videos []Video, limit int, rankVersion string) (*FeedResult, error) {
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
			avatar, err := s.storage.PresignAvatarURL(ctx, *v.AvatarKey)
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
		// 已发布视频必须有 published_at（000016 的 CHECK 约束兜底）。
		// 这里仍然显式判断：约束只保护未来的写入，历史脏数据不应该
		// 变成翻页时的空指针 panic（recovery 会把它变成 500）。
		if last.PublishedAt == nil {
			slog.Error("published video missing published_at", "video_id", last.ID)
			return nil, fmt.Errorf("feed invariant violated: video %d is published without published_at", last.ID)
		}
		next, err := s.cursor.Encode(Cursor{
			PublishedAt: *last.PublishedAt,
			VideoID:     last.ID,
			RankVersion: rankVersion,
		})
		if err != nil {
			slog.Error("encode next cursor failed", "err", err)
			return nil, fmt.Errorf("encode next cursor: %w", err)
		}
		result.NextCursor = &next
		result.HasMore = true
	}
	return result, nil
}
