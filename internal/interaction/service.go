package interaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var ErrInvalidParam = errors.New("invalid param")

// Repository 由 service 定义：GormRepository 隐式满足。
type Repository interface {
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

type Service struct {
	repo   Repository
	avatar AvatarURLProvider
	cursor *codec
}

func NewService(repo Repository, avatar AvatarURLProvider, cursorSecret string) *Service {
	return &Service{
		repo:   repo,
		avatar: avatar,
		cursor: NewCodec(cursorSecret),
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
	if err := s.repo.AddLike(ctx, userID, videoID); err != nil {
		slog.Error("add like failed", "user_id", userID, "video_id", videoID, "err", err)
		return nil, fmt.Errorf("add like: %w", err)
	}
	return &LikeResult{VideoID: videoID, Liked: true}, nil
}

// Unlike 幂等取消点赞：结果语义固定为 liked=false。
func (s *Service) Unlike(ctx context.Context, userID, videoID int64) (*LikeResult, error) {
	if err := s.repo.RemoveLike(ctx, userID, videoID); err != nil {
		slog.Error("remove like failed", "user_id", userID, "video_id", videoID, "err", err)
		return nil, fmt.Errorf("remove like: %w", err)
	}
	return &LikeResult{VideoID: videoID, Liked: false}, nil
}

// Follow 关注：禁止自关注，目标用户必须存在。
func (s *Service) Follow(ctx context.Context, followerID, followeeID int64) (*FollowResult, error) {
	if followerID == followeeID {
		return nil, ErrInvalidParam
	}
	exists, err := s.repo.UserExists(ctx, followeeID)
	if err != nil {
		slog.Error("check followee failed", "followee_id", followeeID, "err", err)
		return nil, fmt.Errorf("check followee: %w", err)
	}
	if !exists {
		return nil, ErrUserNotFound
	}
	if err := s.repo.AddFollow(ctx, followerID, followeeID); err != nil {
		slog.Error("add follow failed", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, fmt.Errorf("add follow: %w", err)
	}
	return &FollowResult{FolloweeID: followeeID, Following: true}, nil
}

// Unfollow 幂等取消关注。
func (s *Service) Unfollow(ctx context.Context, followerID, followeeID int64) (*FollowResult, error) {
	if err := s.repo.RemoveFollow(ctx, followerID, followeeID); err != nil {
		slog.Error("remove follow failed", "follower_id", followerID, "followee_id", followeeID, "err", err)
		return nil, fmt.Errorf("remove follow: %w", err)
	}
	return &FollowResult{FolloweeID: followeeID, Following: false}, nil
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
	NextCursor string        `json:"next_cursor"`
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
		result.NextCursor = next
		result.HasMore = true
	}
	return result, nil
}
