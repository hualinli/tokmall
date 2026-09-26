package promotion

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRepository struct {
	method                       string
	authorID, videoID, productID int64
	sortOrder                    int32
	relation                     *VideoProduct
	cards                        []ProductCard
	err                          error
}

func (r *fakeRepository) record(method string, authorID, videoID, productID int64, sortOrder int32) (*VideoProduct, error) {
	r.method, r.authorID, r.videoID, r.productID, r.sortOrder = method, authorID, videoID, productID, sortOrder
	if r.err != nil {
		return nil, r.err
	}
	if r.relation != nil {
		return r.relation, nil
	}
	return &VideoProduct{VideoID: videoID, AuthorID: authorID, ProductID: productID, Status: StatusActive, SortOrder: sortOrder}, nil
}
func (r *fakeRepository) Upsert(_ context.Context, a, v, p int64, s int32) (*VideoProduct, error) {
	return r.record("upsert", a, v, p, s)
}
func (r *fakeRepository) UpdateSort(_ context.Context, a, v, p int64, s int32) (*VideoProduct, error) {
	return r.record("sort", a, v, p, s)
}
func (r *fakeRepository) Disable(_ context.Context, a, v, p int64) (*VideoProduct, error) {
	x, err := r.record("disable", a, v, p, 0)
	if x != nil {
		x.Status = StatusDisabled
	}
	return x, err
}
func (r *fakeRepository) ListActiveCards(_ context.Context, v int64) ([]ProductCard, error) {
	r.videoID = v
	return r.cards, r.err
}

func TestServiceCommandsUseAuthenticatedAuthor(t *testing.T) {
	repo := &fakeRepository{}
	svc := NewService(repo)
	linked, err := svc.Link(context.Background(), LinkParams{AuthorID: 7, VideoID: 8, ProductID: 9, SortOrder: 2})
	require.NoError(t, err)
	require.Equal(t, "upsert", repo.method)
	require.Equal(t, int64(7), repo.authorID)
	require.Equal(t, int32(2), linked.SortOrder)
	updated, err := svc.UpdateSort(context.Background(), LinkParams{AuthorID: 7, VideoID: 8, ProductID: 9, SortOrder: 4})
	require.NoError(t, err)
	require.Equal(t, "sort", repo.method)
	require.Equal(t, int32(4), updated.SortOrder)
	unlinked, err := svc.Unlink(context.Background(), 7, 8, 9)
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, unlinked.Status)
}

func TestServiceRejectsInvalidIdentifiersAndSort(t *testing.T) {
	svc := NewService(&fakeRepository{})
	for _, p := range []LinkParams{{}, {AuthorID: 1, VideoID: 2, ProductID: 3, SortOrder: -1}} {
		_, err := svc.Link(context.Background(), p)
		require.ErrorIs(t, err, ErrInvalidParam)
	}
	_, err := svc.ListCards(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidParam)
}

func TestServicePreservesBusinessErrorsAndNormalizesEmptyList(t *testing.T) {
	svc := NewService(&fakeRepository{err: ErrActiveLimit})
	_, err := svc.Link(context.Background(), LinkParams{AuthorID: 1, VideoID: 2, ProductID: 3})
	require.ErrorIs(t, err, ErrActiveLimit)
	cards, err := NewService(&fakeRepository{}).ListCards(context.Background(), 2)
	require.NoError(t, err)
	require.NotNil(t, cards)
	require.Empty(t, cards)
	unknown := errors.New("db")
	_, err = NewService(&fakeRepository{err: unknown}).Unlink(context.Background(), 1, 2, 3)
	require.ErrorIs(t, err, unknown)
}
