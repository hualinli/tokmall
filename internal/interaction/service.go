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
}

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
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
