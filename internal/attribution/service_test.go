package attribution

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRepository struct{ called int64 }

func (f *fakeRepository) Sales(_ context.Context, authorID int64) (*Sales, error) {
	f.called = authorID
	return &Sales{AuthorID: authorID, ByVideo: []Slice{}, ByProduct: []Slice{}}, nil
}

func TestSalesRejectsInvalidAuthor(t *testing.T) {
	f := &fakeRepository{}
	_, err := NewService(f).GetSales(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidAuthor)
	require.Zero(t, f.called)
	result, err := NewService(f).GetSales(context.Background(), 3)
	require.NoError(t, err)
	require.Equal(t, int64(3), result.AuthorID)
}
