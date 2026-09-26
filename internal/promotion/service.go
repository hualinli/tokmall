package promotion

import (
	"context"
	"errors"
)

var ErrInvalidParam = errors.New("invalid promotion parameter")

const MaxActiveProductsPerVideo = 5

type Repository interface {
	Upsert(ctx context.Context, authorID, videoID, productID int64, sortOrder int32) (*VideoProduct, error)
	UpdateSort(ctx context.Context, authorID, videoID, productID int64, sortOrder int32) (*VideoProduct, error)
	Disable(ctx context.Context, authorID, videoID, productID int64) (*VideoProduct, error)
	ListActiveCards(ctx context.Context, videoID int64) ([]ProductCard, error)
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

type LinkParams struct {
	AuthorID, VideoID, ProductID int64
	SortOrder                    int32
}

type LinkResult struct {
	VideoID   int64  `json:"video_id"`
	ProductID int64  `json:"product_id"`
	Status    Status `json:"status"`
	SortOrder int32  `json:"sort_order"`
}

func (s *Service) Link(ctx context.Context, p LinkParams) (*LinkResult, error) {
	if invalid(p) {
		return nil, ErrInvalidParam
	}
	relation, err := s.repo.Upsert(ctx, p.AuthorID, p.VideoID, p.ProductID, p.SortOrder)
	return toResult(relation, err)
}

func (s *Service) UpdateSort(ctx context.Context, p LinkParams) (*LinkResult, error) {
	if invalid(p) {
		return nil, ErrInvalidParam
	}
	relation, err := s.repo.UpdateSort(ctx, p.AuthorID, p.VideoID, p.ProductID, p.SortOrder)
	return toResult(relation, err)
}

func (s *Service) Unlink(ctx context.Context, authorID, videoID, productID int64) (*LinkResult, error) {
	if authorID <= 0 || videoID <= 0 || productID <= 0 {
		return nil, ErrInvalidParam
	}
	relation, err := s.repo.Disable(ctx, authorID, videoID, productID)
	return toResult(relation, err)
}

func (s *Service) ListCards(ctx context.Context, videoID int64) ([]ProductCard, error) {
	if videoID <= 0 {
		return nil, ErrInvalidParam
	}
	cards, err := s.repo.ListActiveCards(ctx, videoID)
	if cards == nil {
		cards = []ProductCard{}
	}
	return cards, err
}

func invalid(p LinkParams) bool {
	return p.AuthorID <= 0 || p.VideoID <= 0 || p.ProductID <= 0 || p.SortOrder < 0
}

func toResult(relation *VideoProduct, err error) (*LinkResult, error) {
	if err != nil {
		return nil, err
	}
	return &LinkResult{VideoID: relation.VideoID, ProductID: relation.ProductID, Status: relation.Status, SortOrder: relation.SortOrder}, nil
}
