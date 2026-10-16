package video

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/lihualin/tokmall/internal/breaker"
	"github.com/lihualin/tokmall/internal/storage"
)

var (
	ErrInvalidParam     = errors.New("invalid param")
	ErrForbidden        = errors.New("forbidden")
	ErrUploadURLExpired = errors.New("upload url expired or never issued")
	// ErrStorageUnavailable：对象存储熔断打开（下游暂时不可用）。
	// 与"上传凭证过期"区分开：前者是依赖故障（503，可重试），
	// 后者是客户端问题（400，需要重新申请凭证）。
	ErrStorageUnavailable = errors.New("object storage temporarily unavailable")
	// ErrIdempotencyConflict 表示 event_id 已经绑定了另一组业务事实。
	// 它与“同一请求重试成功”不同，handler 应明确返回 409。
	ErrIdempotencyConflict = errors.New("idempotency key reused with different payload")
)

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// Repository 由 service 定义、消费，GormRepository 隐式满足。
type Repository interface {
	CreateVideo(ctx context.Context, v *Video) error
	FindVideoByID(ctx context.Context, id int64) (*Video, error)
	UpdateVideoStatus(ctx context.Context, id, authorID int64, from, to Status, publishedAt *time.Time) error
	CreateTicket(ctx context.Context, t *UploadTicket) error
	ConsumeTicket(ctx context.Context, userID int64, objectKey string, finalize func() error) error
	// ConsumeTicketAndCreateVideo 让"凭证核销 + 视频落库"成为一个事务，
	// 避免核销成功但建行失败把用户已上传的对象变成孤儿。
	ConsumeTicketAndCreateVideo(ctx context.Context, userID int64, objectKey string, v *Video, finalize func() error) error
	ListByAuthor(ctx context.Context, authorID int64, before *Cursor, limit int) ([]Video, error)
	LikeCounts(ctx context.Context, videoIDs []int64) (map[int64]int64, error)
	IsLiked(ctx context.Context, userID, videoID int64) (bool, error)
	CreatePlayEvent(ctx context.Context, e *PlayEvent) (*PlayEventCreateResult, error)
}

// ExistenceGuard 是缓存穿透防护的消费侧接口（实现在 internal/existence）。
// 定义在这里而不是实现里：service 只依赖"能不能问一句是否存在"这个能力。
type ExistenceGuard interface {
	MayExist(ctx context.Context, namespace string, id int64) (bool, error)
	MarkMissing(ctx context.Context, namespace string, id int64)
	Add(namespace string, id int64)
}

// existenceNamespace 必须与装配方注册的布隆过滤器命名空间一致。
const existenceNamespace = "video"

// Storage 同样由 service 定义：最小化依赖面，方便单测注入 fake。
type Storage interface {
	PresignPutURL(ctx context.Context, objectKey string, fileSize int64, contentType string) (string, error)
	PresignGetURL(ctx context.Context, objectKey string) (string, error)
	PresignAvatarURL(ctx context.Context, objectKey string) (string, error)
	StatObject(ctx context.Context, objectKey string) (int64, error)
	DeleteObject(ctx context.Context, objectKey string) error
	FinalizeUpload(ctx context.Context, source, destination string, maxSize int64) error
}

// FeedCacheInvalidator 消费侧接口：发布/删除成功后失效 feed 首页缓存。
// 实现是 feed.RedisFeedCache（main 装配），video 不知道 feed 存在。
type FeedCacheInvalidator interface {
	InvalidateFeedCache(ctx context.Context) error
}

type Service struct {
	repo              Repository
	productCards      ProductCardReader
	storage           Storage
	expiry            time.Duration
	feedCache         FeedCacheInvalidator
	cursor            *codec
	existence         ExistenceGuard
	hotScore          HotScoreRecorder
	mediaProbeEnabled bool
}

// SetMediaProbeEnabled controls whether new uploads require async validation.
func (s *Service) SetMediaProbeEnabled(enabled bool) { s.mediaProbeEnabled = enabled }

func (s *Service) SetProductCardReader(reader ProductCardReader) { s.productCards = reader }

// SetExistenceGuard 由装配方注入缓存穿透防护（未注入时行为与现状一致）。
func (s *Service) SetExistenceGuard(guard ExistenceGuard) { s.existence = guard }

// HotScoreRecorder 是热榜计分接口（使用方定义，main 注入 feed 的实现）。
// 参数是动作名而不是分值：权重属于排序模型（feed 决定一次播放值多少分）。
type HotScoreRecorder interface {
	AddHotScore(ctx context.Context, videoID int64, action string, at time.Time) error
}

// SetHotScoreRecorder 注入热榜计分器（main 装配）。未注入时计分是空操作。
func (s *Service) SetHotScoreRecorder(recorder HotScoreRecorder) { s.hotScore = recorder }

// recordHotScore 记一次热度。失败只记日志：热榜是派生数据，
// 不能因为它写失败而让播放接口报错。
func (s *Service) recordHotScore(ctx context.Context, videoID int64, action string) {
	if s.hotScore == nil {
		return
	}
	if err := s.hotScore.AddHotScore(ctx, videoID, action, time.Now()); err != nil {
		slog.Warn("record hot score failed", "video_id", videoID, "action", action, "err", err)
	}
}

func NewService(repo Repository, storage Storage, uploadExpiry time.Duration, cursorSecret string, feedCache FeedCacheInvalidator) *Service {
	return &Service{
		mediaProbeEnabled: true,
		repo:              repo,
		storage:           storage,
		expiry:            uploadExpiry,
		feedCache:         feedCache,
		cursor:            NewCodec(cursorSecret),
	}
}

// invalidateFeed 先写库、后删缓存（Cache-Aside 标准动作）。
// 失效失败只记 Warn 不回滚请求：DB 已写入，脏窗口由 TTL 兜底
// （最坏 60s 后自动恢复）。缓存失效绝不能进入请求的成功路径。
// mapStorageError 把"熔断打开"翻译成领域哨兵，让 handler 能返回 503 而不是 500。
// 这是熔断的最后一环：快速失败之后，对外语义也要正确——依赖暂时不可用
// 与"服务端 bug"对客户端和监控的含义完全不同。
func mapStorageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, breaker.ErrOpen) {
		return ErrStorageUnavailable
	}
	return err
}

func (s *Service) invalidateFeed(ctx context.Context) {
	if s.feedCache == nil {
		return
	}
	if err := s.feedCache.InvalidateFeedCache(ctx); err != nil {
		slog.Warn("invalidate feed cache failed", "err", err)
	}
}

// statusTransitions 状态机：key 是当前状态，value 是允许迁移到的状态集合。
var statusTransitions = map[Status][]Status{
	StatusUploading:     {StatusProcessing, StatusFailed},
	StatusProcessing:    {StatusPendingReview, StatusFailed},
	StatusPendingReview: {StatusPublished, StatusBlocked, StatusFailed, StatusDeleted},
	StatusPublished:     {StatusBlocked, StatusDeleted},
	StatusBlocked:       {StatusDeleted},
	StatusFailed:        {StatusDeleted},
}

func canTransition(from, to Status) bool {
	for _, s := range statusTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

type UploadResult struct {
	ObjectKey string    `json:"object_key"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CreateParams struct {
	Title           string
	ObjectKey       string
	CoverKey        *string
	DurationSeconds int
}

type CreateResult struct {
	VideoID int64  `json:"video_id"`
	Status  Status `json:"status"`
}

type PublishResult struct {
	VideoID int64  `json:"video_id"`
	Status  Status `json:"status"`
}

// MediaValidationView 仅向作者返回安全的校验结果，不暴露探测工具原始输出。
type MediaValidationView struct {
	Status     MediaProbeStatus `json:"status"`
	Reason     *string          `json:"reason"`
	CanPublish bool             `json:"can_publish"`
}

type VideoDetail struct {
	MediaValidation *MediaValidationView `json:"media_validation,omitempty"`
	VideoID         int64                `json:"video_id"`
	AuthorID        int64                `json:"author_id"`
	Title           string               `json:"title"`
	VideoURL        string               `json:"video_url"`
	DurationSeconds int                  `json:"duration_seconds"`
	Status          Status               `json:"status"`
	PublishedAt     *time.Time           `json:"published_at"`
	CreatedAt       time.Time            `json:"created_at"`
	Author          AuthorRef            `json:"author"`
	LikeCount       int64                `json:"like_count"`
	IsLiked         bool                 `json:"is_liked"`
	ProductCards    []ProductCard        `json:"product_cards"`
}

// AuthorRef 详情页作者信息（与 feed 同形状，模块各自定义类型）。
type AuthorRef struct {
	UserID    int64   `json:"user_id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatar_url"`
}

type VideoListItem struct {
	MediaValidation *MediaValidationView `json:"media_validation,omitempty"`
	VideoID         int64                `json:"video_id"`
	Title           string               `json:"title"`
	VideoURL        string               `json:"video_url"`
	CoverURL        *string              `json:"cover_url"`
	DurationSeconds int                  `json:"duration_seconds"`
	Status          Status               `json:"status"`
	PublishedAt     *time.Time           `json:"published_at"`
	CreatedAt       time.Time            `json:"created_at"`
}

type VideoListResult struct {
	Items      []VideoListItem `json:"items"`
	NextCursor *string         `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// GetUploadURL 发放预签名上传 URL 并记录 ticket。
// 支持视频与图片（封面/头像共用），文件大小上限因类型而异。
func (s *Service) GetUploadURL(ctx context.Context, userID int64, filename string, fileSize int64, contentType string) (*UploadResult, error) {
	var objectKey string
	switch contentType {
	case "video/mp4", "video/webm", "video/quicktime":
		if fileSize < 1<<20 || fileSize > 512<<20 {
			return nil, ErrInvalidParam
		}
		objectKey = fmt.Sprintf("videos/%d/%s.mp4", userID, uuid.NewString())
	case "image/jpeg", "image/png", "image/webp":
		if fileSize < 1<<10 || fileSize > 5<<20 {
			return nil, ErrInvalidParam
		}
		ext := "jpg"
		switch contentType {
		case "image/png":
			ext = "png"
		case "image/webp":
			ext = "webp"
		}
		objectKey = fmt.Sprintf("covers/%d/%s.%s", userID, uuid.NewString(), ext)
	default:
		return nil, ErrInvalidParam
	}
	uploadURL, err := s.storage.PresignPutURL(ctx, "staging/"+objectKey, fileSize, contentType)
	if err != nil {
		if mapped := mapStorageError(err); errors.Is(mapped, ErrStorageUnavailable) {
			return nil, mapped
		}
		slog.Error("presign upload url failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("presign upload url: %w", err)
	}
	err = s.repo.CreateTicket(ctx, &UploadTicket{
		UserID:    userID,
		ObjectKey: objectKey,
		ExpiresAt: time.Now().Add(s.expiry),
	})
	if err != nil {
		slog.Error("create ticket failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("create ticket: %w", err)
	}
	return &UploadResult{
		ObjectKey: objectKey,
		UploadURL: uploadURL,
		ExpiresAt: time.Now().Add(s.expiry),
	}, nil
}

// Create 校验 ticket 并确认文件真实上传后创建视频记录（PENDING_REVIEW）。
func (s *Service) Create(ctx context.Context, userID int64, p CreateParams) (*CreateResult, error) {
	title := p.Title
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return nil, ErrInvalidParam
	}
	if p.DurationSeconds < 1 || p.DurationSeconds > 3600 {
		return nil, ErrInvalidParam
	}
	if !strings.HasPrefix(p.ObjectKey, fmt.Sprintf("videos/%d/", userID)) {
		return nil, ErrUploadURLExpired
	}
	if p.CoverKey != nil && !strings.HasPrefix(*p.CoverKey, fmt.Sprintf("covers/%d/", userID)) {
		return nil, ErrUploadURLExpired
	}
	v := &Video{
		AuthorID:         userID,
		Title:            title,
		ObjectKey:        p.ObjectKey,
		CoverKey:         p.CoverKey,
		DurationSeconds:  p.DurationSeconds,
		Status:           StatusPendingReview,
		MediaProbeStatus: ProbePending,
	}
	if !s.mediaProbeEnabled {
		v.MediaProbeStatus = ProbeSkipped
	}
	if err := s.repo.ConsumeTicketAndCreateVideo(ctx, userID, p.ObjectKey, v, func() error {
		if err := s.finalizeUpload(ctx, p.ObjectKey, 512<<20); err != nil {
			return err
		}
		if p.CoverKey != nil {
			return s.finalizeUpload(ctx, *p.CoverKey, 5<<20)
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrTicketInvalid) {
			return nil, ErrUploadURLExpired
		}
		if errors.Is(err, ErrInvalidParam) || errors.Is(err, ErrUploadURLExpired) || errors.Is(err, ErrStorageUnavailable) {
			return nil, err
		}
		slog.Error("create video failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("create video: %w", err)
	}
	// 新视频立即更新本地过滤器；其它实例尚未同步时也会放行到数据库。
	if s.existence != nil {
		s.existence.Add(existenceNamespace, v.ID)
	}
	return &CreateResult{VideoID: v.ID, Status: v.Status}, nil
}

// Publish 作者发布：PENDING_REVIEW -> PUBLISHED。
func (s *Service) Publish(ctx context.Context, userID, videoID int64) (*PublishResult, error) {
	// 先读是为了把对外错误区分为 404/409；它本身不能承担并发
	// 授权，真正的写入仍必须带 author_id + from_status 条件。
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		slog.Error("find video for publish failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("find video for publish: %w", err)
	}
	// DELETED 对所有人不可见，沿用详情/播放的 404 语义。
	if v.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	if v.AuthorID != userID {
		// 未发布视频只对作者可见；发布接口也不能通过错误响应泄露
		// 他人视频的存在性。
		return nil, ErrNotFound
	}
	if !canTransition(v.Status, StatusPublished) || !v.MediaProbeStatus.AllowsPublish() {
		return nil, ErrStatusConflict
	}

	now := time.Now()
	err = s.repo.UpdateVideoStatus(ctx, videoID, userID, StatusPendingReview, StatusPublished, &now)
	if err != nil {
		if errors.Is(err, ErrStatusConflict) {
			return nil, ErrStatusConflict
		}
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		slog.Error("publish video failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("publish video: %w", err)
	}
	s.invalidateFeed(ctx)
	return &PublishResult{VideoID: videoID, Status: StatusPublished}, nil
}

// Delete 作者删除：任意非终态 -> DELETED。
func (s *Service) Delete(ctx context.Context, userID, videoID int64) error {
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		return err
	}
	// 非作者与不存在的视频返回同一错误：视频 ID 是自增序列，403 会变成
	// "这个 ID 存在但不是你的"的存在性预言机，与本项目"不可见一律 404"的
	// 约定冲突（发布、详情、点赞都已经是 404）。
	if v.AuthorID != userID {
		return ErrNotFound
	}
	if v.Status == StatusDeleted {
		return nil
	}
	if !canTransition(v.Status, StatusDeleted) {
		return ErrStatusConflict
	}
	err = s.repo.UpdateVideoStatus(ctx, videoID, v.AuthorID, v.Status, StatusDeleted, nil)
	if err != nil {
		if errors.Is(err, ErrStatusConflict) {
			return ErrStatusConflict
		}
		slog.Error("delete video failed", "video_id", videoID, "err", err)
		return fmt.Errorf("delete video: %w", err)
	}
	s.invalidateFeed(ctx)
	// 软删提交后尝试立即删对象；失败由持久化任务重试。
	// 删除完成前，已签发的 GET URL 仍可能有效——status 已 DELETED，
	// feed/详情已不可见，同事务写入的清理任务会继续重试。
	s.removeObjects(ctx, v)
	return nil
}

// removeObjects 删除视频与封面对象（幂等：RemoveObject 对不存在对象不报错）。
func (s *Service) removeObjects(ctx context.Context, v *Video) {
	keys := []string{v.ObjectKey}
	if v.CoverKey != nil {
		keys = append(keys, *v.CoverKey)
	}
	for _, k := range keys {
		if err := s.storage.DeleteObject(ctx, k); err != nil {
			slog.Error("delete object failed", "object_key", k, "err", err)
		}
	}
}

// ListMine 作者的管理视图：自己的全部视频（不含 DELETED），按创建时间倒序。
// 不缓存：QPS 低（只有作者自己看）+ 新鲜性要求高（管理操作后必须立即可见）。
func (s *Service) ListMine(ctx context.Context, userID int64, limit int, cursorRaw string) (*VideoListResult, error) {
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return nil, ErrInvalidParam
	}
	var before *Cursor
	if cursorRaw != "" {
		cur, err := s.cursor.Decode(cursorRaw)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		before = cur
	}
	videos, err := s.repo.ListByAuthor(ctx, userID, before, limit)
	if err != nil {
		slog.Error("list videos by author failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("list by author: %w", err)
	}
	result := &VideoListResult{Items: make([]VideoListItem, 0, limit)}
	hasMore := len(videos) > limit
	if hasMore {
		videos = videos[:limit]
	}
	for _, v := range videos {
		item := VideoListItem{
			MediaValidation: mediaValidationView(&v),
			VideoID:         v.ID,
			Title:           v.Title,
			DurationSeconds: v.DurationSeconds,
			Status:          v.Status,
			PublishedAt:     v.PublishedAt,
			CreatedAt:       v.CreatedAt,
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
		result.Items = append(result.Items, item)
	}
	if hasMore {
		last := videos[len(videos)-1]
		next, err := s.cursor.Encode(Cursor{
			CreatedAt:   last.CreatedAt,
			VideoID:     last.ID,
			RankVersion: CurrentRankVersion,
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

// Play 记录一次播放事件。幂等语义：event_id（客户端标准 UUID，v4/v7 均接受）
// 重复提交时必须匹配同一组业务字段；网络重试不重复计数，不同载荷复用 event_id
// 返回 ErrIdempotencyConflict。不同 event_id 的多次播放各自计数（有效播放由产品层定义）。
// 只接受 PUBLISHED 视频的播放事件，未发布/已删除一律 ErrNotFound（不泄露存在性）。
// playedAt 为客户端事件时间（与网络延迟无关），缺省服务端 now()；
// 未来时间（容忍 5 分钟时钟偏差）拒绝，防止脏数据污染后续排序分数。
func (s *Service) Play(ctx context.Context, userID, videoID int64, eventID string, sessionID *string, playedAt *time.Time, traceID *string) error {
	canonicalEventID, err := parsePlayEventID(eventID)
	if err != nil {
		return err
	}
	now := time.Now()
	if playedAt != nil {
		if playedAt.After(now.Add(5 * time.Minute)) {
			return ErrInvalidParam
		}
	}
	if s.existence != nil {
		if ok, guardErr := s.existence.MayExist(ctx, existenceNamespace, videoID); guardErr == nil && !ok {
			return ErrNotFound
		}
	}
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		if s.existence != nil && errors.Is(err, ErrNotFound) {
			s.existence.MarkMissing(ctx, existenceNamespace, videoID)
		}
		return err
	}
	if v.Status != StatusPublished {
		return ErrNotFound
	}
	// PostgreSQL TIMESTAMPTZ 精度为微秒。入口统一转 UTC 并截断到微秒，
	// 防止 Go 的纳秒值在首次 INSERT 后被数据库截断，导致同一事件重放
	// 因时间精度差异被误判为幂等冲突。
	played := normalizePlayEventTime(now)
	playedAtProvided := playedAt != nil
	if playedAt != nil {
		played = normalizePlayEventTime(*playedAt)
	}
	event := &PlayEvent{
		EventID:          canonicalEventID,
		UserID:           userID,
		VideoID:          videoID,
		SessionID:        normalizePlayEventSessionID(sessionID),
		PlayedAt:         played,
		PlayedAtProvided: playedAtProvided,
		TraceID:          normalizeTraceID(traceID),
	}
	result, err := s.repo.CreatePlayEvent(ctx, event)
	if err != nil {
		slog.Error("create play event failed", "user_id", userID, "video_id", videoID, "err", err)
		return fmt.Errorf("create play event: %w", err)
	}
	if result == nil {
		// repository 违反了自己的写入契约；不要把 nil 解引用变成 panic。
		slog.Error("create play event returned nil result", "user_id", userID, "video_id", videoID)
		return errors.New("create play event: repository returned nil result")
	}
	if result.Inserted {
		// 只有真的插入了新播放事件才加热度：重放同一个 event_id 是幂等命中，
		// 重复计分会让"刷播放接口"直接刷出热榜排名。
		s.recordHotScore(ctx, videoID, "play")
		return nil
	}
	if result.Existing == nil {
		// 冲突后读不到已有事实通常意味着数据库事务/数据被异常删除，
		// 这是服务端故障而不是客户端可重试的业务冲突。
		slog.Error("play event conflict missing existing fact", "event_id", canonicalEventID)
		return errors.New("create play event: existing fact missing after conflict")
	}
	if !samePlayEventBusinessFact(event, result.Existing) {
		slog.Warn("play event idempotency conflict", "event_id", canonicalEventID,
			"user_id", userID, "video_id", videoID)
		return ErrIdempotencyConflict
	}
	return nil
}

// parsePlayEventID 接受标准 36 字符 UUID 文本并统一为小写 canonical 形式。
// 数据库的 UUID 类型本身还接受无短横线、URN 和大括号等表示；API 不接受
// 这些变体，避免同一事实在边界层出现多种文本表示。这里不限制 UUID 版本：
// 当前 Web 客户端 crypto.randomUUID() 生成 v4，未来的客户端也可以使用 v7。
func parsePlayEventID(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", ErrInvalidParam
	}
	u, err := uuid.Parse(raw)
	if err != nil || u == uuid.Nil || u.Variant() != uuid.RFC4122 {
		return "", ErrInvalidParam
	}
	canonical := u.String()
	if !strings.EqualFold(raw, canonical) {
		return "", ErrInvalidParam
	}
	return canonical, nil
}

// normalizePlayEventTime 对齐 PostgreSQL TIMESTAMPTZ 的微秒精度。
func normalizePlayEventTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// maxSessionIDLength / maxTraceIDLength 与 play_events 的列宽一致。
// 超长值必须在入口挡住：否则 PostgreSQL 会报 22001（value too long），
// 被兜底成 500——任意登录用户都能用一次超长 session_id 触发服务端错误。
const (
	maxSessionIDLength = 128
	maxTraceIDLength   = 64
)

func normalizePlayEventSessionID(sessionID *string) *string {
	if sessionID == nil || *sessionID == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*sessionID)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maxSessionIDLength {
		return nil
	}
	return &trimmed
}

// normalizeTraceID 只接受服务端 requestid 中间件生成的值（UUID v7，36 字符）。
// 客户端自带的 X-Request-ID 会被 requestid 原样回显，如果直接写库，
// 既能让观测字段被伪造，也能用超长值把请求打成 500。
func normalizeTraceID(traceID *string) *string {
	if traceID == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*traceID)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > maxTraceIDLength {
		return nil
	}
	if _, err := uuid.Parse(trimmed); err != nil {
		return nil
	}
	return &trimmed
}

// samePlayEventBusinessFact 比较会影响播放统计语义的字段。trace_id 是每次
// HTTP 请求生成的观测元数据，重试时通常不同，因此不能纳入幂等指纹；created_at
// 同理是数据库事实时间。played_at_provided 让“缺省时间由服务端生成”与“显式
// 提供客户端时间”保持可区分，否则同一 event_id 的两种请求会被错误地合并。
func samePlayEventBusinessFact(a, b *PlayEvent) bool {
	if a == nil || b == nil {
		return false
	}
	if a.UserID != b.UserID || a.VideoID != b.VideoID ||
		a.PlayedAtProvided != b.PlayedAtProvided {
		return false
	}
	if !samePlayEventOptionalString(a.SessionID, b.SessionID) {
		return false
	}
	if a.PlayedAtProvided {
		return normalizePlayEventTime(a.PlayedAt).Equal(normalizePlayEventTime(b.PlayedAt))
	}
	return true
}

func samePlayEventOptionalString(a, b *string) bool {
	if a == nil || *a == "" {
		return b == nil || *b == ""
	}
	return b != nil && *b == *a
}

// ValidateImage 校验图片上传凭证 + 文件真实存在，核销票证。
// 供 user 模块 PATCH /me 的头像校验（消费侧接口由 user 定义，main 装配）。
func (s *Service) ValidateImage(ctx context.Context, userID int64, objectKey string) error {
	if !strings.HasPrefix(objectKey, fmt.Sprintf("covers/%d/", userID)) {
		return ErrUploadURLExpired
	}
	err := s.repo.ConsumeTicket(ctx, userID, objectKey, func() error {
		return s.finalizeUpload(ctx, objectKey, 5<<20)
	})
	if errors.Is(err, ErrTicketInvalid) {
		return ErrUploadURLExpired
	}
	return err
}

func (s *Service) finalizeUpload(ctx context.Context, objectKey string, maxSize int64) error {
	sealCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.storage.FinalizeUpload(sealCtx, "staging/"+objectKey, objectKey, maxSize)
	switch {
	case errors.Is(err, storage.ErrObjectNotFound):
		return ErrUploadURLExpired
	case errors.Is(err, storage.ErrInvalidObjectSize):
		return ErrInvalidParam
	default:
		return mapStorageError(err)
	}
}

// Get 按可见性策略返回详情：已删除对所有人不可见；
// 未发布仅作者可见；已发布对所有人可见。
// 不可见一律返回 ErrNotFound（不泄露存在性）。
// viewerID=0（匿名）时 is_liked 恒 false。
func (s *Service) Get(ctx context.Context, viewerID, videoID int64) (*VideoDetail, error) {
	// 仅共享负缓存中的真实 DB miss 可以短路；本地 Bloom 阴性仍放行。
	if s.existence != nil {
		if ok, guardErr := s.existence.MayExist(ctx, existenceNamespace, videoID); guardErr == nil && !ok {
			return nil, ErrNotFound
		}
	}
	v, err := s.repo.FindVideoByID(ctx, videoID)
	if err != nil {
		// 只有"记录确实不存在"才写负缓存。可见性拒绝（未发布、非作者）
		// 也会走到这里，但它们对作者本人是可见的——把可见性拒绝写进
		// 负缓存会让作者在 TTL 窗口内看不到自己的视频。
		if s.existence != nil && errors.Is(err, ErrNotFound) {
			s.existence.MarkMissing(ctx, existenceNamespace, videoID)
		}
		return nil, err
	}
	if v.Status == StatusDeleted {
		return nil, ErrNotFound
	}
	if v.AuthorID != viewerID && v.Status != StatusPublished {
		return nil, ErrNotFound
	}
	videoURL, err := s.storage.PresignGetURL(ctx, v.ObjectKey)
	if err != nil {
		slog.Error("presign video url failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("presign video url: %w", err)
	}
	detail := &VideoDetail{
		VideoID:         v.ID,
		AuthorID:        v.AuthorID,
		Title:           v.Title,
		VideoURL:        videoURL,
		DurationSeconds: v.DurationSeconds,
		Status:          v.Status,
		PublishedAt:     v.PublishedAt,
		CreatedAt:       v.CreatedAt,
		Author: AuthorRef{
			UserID:   v.AuthorID,
			Username: v.Username,
		},
	}
	if v.AuthorID == viewerID {
		detail.MediaValidation = mediaValidationView(v)
	}
	if v.AvatarKey != nil {
		// 头像签名失败降级为 null（装饰性字段）
		avatar, err := s.storage.PresignAvatarURL(ctx, *v.AvatarKey)
		if err != nil {
			slog.Warn("presign avatar url failed", "video_id", videoID, "err", err)
		} else {
			detail.Author.AvatarURL = &avatar
		}
	}
	counts, err := s.repo.LikeCounts(ctx, []int64{videoID})
	if err != nil {
		slog.Error("like counts failed", "video_id", videoID, "err", err)
		return nil, fmt.Errorf("like counts: %w", err)
	}
	detail.LikeCount = counts[videoID]
	if viewerID > 0 {
		liked, err := s.repo.IsLiked(ctx, viewerID, videoID)
		if err != nil {
			slog.Error("is liked failed", "viewer_id", viewerID, "video_id", videoID, "err", err)
			return nil, fmt.Errorf("is liked: %w", err)
		}
		detail.IsLiked = liked
	}
	detail.ProductCards = []ProductCard{}
	if v.Status == StatusPublished && s.productCards != nil {
		rows, err := s.productCards.ListProductCards(ctx, videoID)
		if err != nil {
			return nil, fmt.Errorf("list product cards: %w", err)
		}
		for _, row := range rows {
			card := ProductCard{ProductID: row.ProductID, Title: row.Title, MinPrice: row.MinPrice, ProductStatus: row.ProductStatus, SortOrder: row.SortOrder}
			if row.CoverKey != "" {
				url, err := s.storage.PresignGetURL(ctx, row.CoverKey)
				if err != nil {
					return nil, fmt.Errorf("presign product cover: %w", err)
				}
				card.CoverURL = &url
			}
			detail.ProductCards = append(detail.ProductCards, card)
		}
	}
	return detail, nil
}

func mediaValidationView(v *Video) *MediaValidationView {
	view := &MediaValidationView{Status: v.MediaProbeStatus, CanPublish: v.Status == StatusPendingReview && v.MediaProbeStatus.AllowsPublish()}
	if v.MediaProbeStatus != ProbeRejected {
		return view
	}
	// 数据库存工具诊断用于排障；对外只映射固定文案，不能透传路径、URL或 stderr。
	reason := "视频未通过校验，请重新上传符合要求的视频。"
	raw := ""
	if v.MediaProbeError != nil {
		raw = *v.MediaProbeError
	}
	switch {
	case raw == "video exceeds the object size limit":
		reason = "视频文件过大，请压缩后重新上传。"
	case strings.HasPrefix(raw, "not a playable video:"):
		reason = "无法读取视频，请检查文件是否损坏并重新导出。"
	case strings.HasPrefix(raw, "duration ") && strings.Contains(raw, "shorter than the minimum"):
		reason = "视频时长过短，请重新上传。"
	case strings.HasPrefix(raw, "duration ") && strings.Contains(raw, "exceeds the maximum"):
		reason = "视频时长超过上限，请缩短后重新上传。"
	case strings.HasPrefix(raw, "width ") || strings.HasPrefix(raw, "height "):
		reason = "视频分辨率超过上限，请降低分辨率后重新上传。"
	case strings.HasPrefix(raw, "video codec "):
		reason = "视频编码不受支持，请转换编码后重新上传。"
	case raw == "audio stream is required":
		reason = "视频缺少音轨，请补充音轨后重新上传。"
	}
	view.Reason = &reason
	return view
}
