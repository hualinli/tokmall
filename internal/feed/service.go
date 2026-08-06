package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
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
}

func NewService(repo Repository, codec *codec, storage PublicURLProvider) *Service {
	return &Service{
		repo:    repo,
		cursor:  codec,
		storage: storage,
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
