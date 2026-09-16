package interaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var ErrInvalidParam = errors.New("invalid param")
var ErrVideoNotFound = errors.New("video not found")

// Repository 由 service 定义：GormRepository 隐式满足。
type Repository interface {
	// VideoIsInteractable 只返回对互动可见的视频：存在且处于 PUBLISHED。
	// 这是 interaction 对 videos 表的最小 read model，不依赖 video 模块类型。
	VideoIsInteractable(ctx context.Context, videoID int64) (bool, error)
	AddLike(ctx context.Context, userID, videoID int64) error
	RemoveLike(ctx context.Context, userID, videoID int64) error
	AddFollow(ctx context.Context, followerID, followeeID int64) error
	RemoveFollow(ctx context.Context, followerID, followeeID int64) error
	UserExists(ctx context.Context, userID int64) (bool, error)
	ListFollowing(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error)
	ListFollowers(ctx context.Context, userID int64, before *Cursor, limit int) ([]FollowEdge, error)
	IsFollowingMany(ctx context.Context, viewerID int64, userIDs []int64) (map[int64]bool, error)
}

// AvatarURLProvider 消费侧接口：列表项的头像 key 转预签名 URL（长有效期）。
type AvatarURLProvider interface {
	PresignAvatarURL(ctx context.Context, objectKey string) (string, error)
}

// FollowingFeedInvalidator 是 interaction 的消费侧接口：关注关系成功变更后，
// 只需要通知 follower 自己的关注 Feed 第一页失效。interaction 不依赖 feed
// 模块的缓存实现或排序类型，具体装配由 main 完成。
type FollowingFeedInvalidator interface {
	InvalidateFollowingFeed(ctx context.Context, followerID int64) error
}

type Service struct {
	repo                     Repository
	avatar                   AvatarURLProvider
	cursor                   *codec
	followingFeedInvalidator FollowingFeedInvalidator
}

// NewService 构造 interaction 服务。invalidator 可选：缓存关闭时传 nil，
// 关注写入仍保持正确，只是不做缓存加速。
func NewService(repo Repository, avatar AvatarURLProvider, cursorSecret string, invalidators ...FollowingFeedInvalidator) *Service {
	var invalidator FollowingFeedInvalidator
	if len(invalidators) > 0 {
		invalidator = invalidators[0]
	}
	return &Service{
		repo:                     repo,
		avatar:                   avatar,
		cursor:                   NewCodec(cursorSecret),
		followingFeedInvalidator: invalidator,
	}
}

type LikeResult struct {
	VideoID int64 `json:"video_id"`
	Liked   bool  `json:"liked"`
}

type FollowResult struct {
	FolloweeID int64 `json:"followee_id"`
	Following  bool  `json:"following"`
}

// Like 幂等点赞：结果语义固定为 liked=true。
func (s *Service) Like(ctx context.Context, userID, videoID int64) (*LikeResult, error) {
	if err := s.ensureInteractableVideo(ctx, videoID); err != nil {
		return nil, err
	}
	if err := s.repo.AddLike(ctx, userID, videoID); err != nil {
		if errors.Is(err, ErrVideoNotFound) {
			return nil, ErrVideoNotFound
		}
		slog.Error("add like failed", "user_id", userID, "video_id", videoID, "err", err)
		return nil, fmt.Errorf("add like: %w", err)
	}
	return &LikeResult{VideoID: videoID, Liked: true}, nil
}

// Unlike 幂等取消点赞：结果语义固定为 liked=false。
func (s *Service) Unlike(ctx context.Context, userID, videoID int64) (*LikeResult, error) {
	if err := s.ensureInteractableVideo(ctx, videoID); err != nil {
		return nil, err
	}
	if err := s.repo.RemoveLike(ctx, userID, videoID); err != nil {
		if errors.Is(err, ErrVideoNotFound) {
			return nil, ErrVideoNotFound
		}
		slog.Error("remove like failed", "user_id", userID, "video_id", videoID, "err", err)
		return nil, fmt.Errorf("remove like: %w", err)
	}
	return &LikeResult{VideoID: videoID, Liked: false}, nil
}

func (s *Service) ensureInteractableVideo(ctx context.Context, videoID int64) error {
	if videoID <= 0 {
		return ErrInvalidParam
	}
	visible, err := s.repo.VideoIsInteractable(ctx, videoID)
	if err != nil {
		slog.Error("check video visibility failed", "video_id", videoID, "err", err)
		return fmt.Errorf("check video visibility: %w", err)
	}
	if !visible {
		// 不区分不存在、未发布和已删除，避免通过互动接口探测视频状态。
		return ErrVideoNotFound
	}
	return nil
}

// Follow 关注：禁止自关注，目标用户必须处于可交互的 ACTIVE 状态。
func (s *Service) Follow(ctx context.Context, followerID, followeeID int64) (*FollowResult, error) {
	if followerID == followeeID {
		return nil, ErrInvalidParam
	}
	if err := s.ensureActiveUser(ctx, followeeID); err != nil {
		return nil, err
	}
	if err := s.repo.AddFollow(ctx, followerID, followeeID); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrUserNotFound
		}
		slog.Error("add follow failed", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, fmt.Errorf("add follow: %w", err)
	}
	s.invalidateFollowingFeed(ctx, followerID)
	return &FollowResult{FolloweeID: followeeID, Following: true}, nil
}

// Unfollow 幂等取消关注。已删除/禁用的目标用户按不存在处理，避免借此接口探测状态。
func (s *Service) Unfollow(ctx context.Context, followerID, followeeID int64) (*FollowResult, error) {
	if err := s.ensureActiveUser(ctx, followeeID); err != nil {
		return nil, err
	}
	if err := s.repo.RemoveFollow(ctx, followerID, followeeID); err != nil {
		slog.Error("remove follow failed", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, fmt.Errorf("remove follow: %w", err)
	}
	s.invalidateFollowingFeed(ctx, followerID)
	return &FollowResult{FolloweeID: followeeID, Following: false}, nil
}

// invalidateFollowingFeed 只在关注写入成功后调用，遵循“先写库、后删缓存”。
// 缓存是加速层：删除失败只记录 Warn，让 TTL 最终收敛，不能把已经成功的
// 关注/取关改写成失败；invalidator 为 nil 时表示缓存关闭，安全降级直查。
func (s *Service) invalidateFollowingFeed(ctx context.Context, followerID int64) {
	if s.followingFeedInvalidator == nil {
		return
	}
	if err := s.followingFeedInvalidator.InvalidateFollowingFeed(ctx, followerID); err != nil {
		slog.Warn("invalidate following feed cache failed", "user_id", followerID, "err", err)
	}
}

// ensureActiveUser 是 interaction 对 users 表的最小 read model：只有未软删且
// ACTIVE 的用户才是可关注资源。不存在、软删和禁用统一按不存在处理。
func (s *Service) ensureActiveUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return ErrInvalidParam
	}
	exists, err := s.repo.UserExists(ctx, userID)
	if err != nil {
		slog.Error("check user visibility failed", "user_id", userID, "err", err)
		return fmt.Errorf("check user visibility: %w", err)
	}
	if !exists {
		return ErrUserNotFound
	}
	return nil
}

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

// ProfileItem 关注/粉丝列表项：公开资料子集 + 个性化字段。
// 注意：不含计数（每项两次 COUNT 的 N+1 不值得，需要时再加）。
type ProfileItem struct {
	UserID      int64   `json:"user_id"`
	Username    string  `json:"username"`
	Bio         string  `json:"bio"`
	AvatarURL   *string `json:"avatar_url"`
	IsFollowing bool    `json:"is_following"`
	IsMe        bool    `json:"is_me"`
}

type ProfileListResult struct {
	Items      []ProfileItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

// ListFollowing 某人关注了谁（按关系时间倒序）。
func (s *Service) ListFollowing(ctx context.Context, viewerID, userID int64, limit int, cursorRaw string) (*ProfileListResult, error) {
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return nil, ErrInvalidParam
	}
	before, err := s.decodeCursor(cursorRaw)
	if err != nil {
		return nil, err
	}
	if err := s.ensureActiveUser(ctx, userID); err != nil {
		return nil, err
	}
	edges, err := s.repo.ListFollowing(ctx, userID, before, limit)
	if err != nil {
		slog.Error("list following failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("list following: %w", err)
	}
	return s.assemble(ctx, viewerID, edges, limit)
}

// ListFollowers 谁关注了某人。
func (s *Service) ListFollowers(ctx context.Context, viewerID, userID int64, limit int, cursorRaw string) (*ProfileListResult, error) {
	if limit == 0 {
		limit = defaultListLimit
	}
	if limit < 1 || limit > maxListLimit {
		return nil, ErrInvalidParam
	}
	before, err := s.decodeCursor(cursorRaw)
	if err != nil {
		return nil, err
	}
	if err := s.ensureActiveUser(ctx, userID); err != nil {
		return nil, err
	}
	edges, err := s.repo.ListFollowers(ctx, userID, before, limit)
	if err != nil {
		slog.Error("list followers failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("list followers: %w", err)
	}
	return s.assemble(ctx, viewerID, edges, limit)
}

func (s *Service) decodeCursor(cursorRaw string) (*Cursor, error) {
	if cursorRaw == "" {
		return nil, nil
	}
	cur, err := s.cursor.Decode(cursorRaw)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	return cur, nil
}

func (s *Service) assemble(ctx context.Context, viewerID int64, edges []FollowEdge, limit int) (*ProfileListResult, error) {
	result := &ProfileListResult{Items: make([]ProfileItem, 0, limit)}
	hasMore := len(edges) > limit
	if hasMore {
		edges = edges[:limit]
	}
	ids := make([]int64, 0, len(edges))
	for _, e := range edges {
		ids = append(ids, e.UserID)
	}
	following, err := s.repo.IsFollowingMany(ctx, viewerID, ids)
	if err != nil {
		slog.Error("is following many failed", "err", err)
		return nil, fmt.Errorf("is following many: %w", err)
	}
	for _, e := range edges {
		item := ProfileItem{
			UserID:      e.UserID,
			Username:    e.Username,
			Bio:         e.Bio,
			IsFollowing: following[e.UserID],
			IsMe:        viewerID == e.UserID,
		}
		if e.AvatarKey != nil {
			avatar, err := s.avatar.PresignAvatarURL(ctx, *e.AvatarKey)
			if err != nil {
				slog.Warn("presign avatar url failed", "user_id", e.UserID, "err", err)
			} else {
				item.AvatarURL = &avatar
			}
		}
		result.Items = append(result.Items, item)
	}
	if hasMore {
		last := edges[len(edges)-1]
		next, err := s.cursor.Encode(Cursor{
			CreatedAt:   last.CreatedAt,
			UserID:      last.UserID,
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
