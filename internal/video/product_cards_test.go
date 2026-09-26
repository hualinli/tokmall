package video

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeCardReader struct {
	rows  []productCardRow
	calls int
}

func (f *fakeCardReader) ListProductCards(_ context.Context, _ int64) ([]productCardRow, error) {
	f.calls++
	return f.rows, nil
}

func TestDetailCardsOnlyForPublishedVideo(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeStorage()
	svc := newTestService(repo, store)
	cards := &fakeCardReader{rows: []productCardRow{{ProductID: 8, Title: "商品", MinPrice: 500, CoverKey: "products/8.jpg"}}}
	svc.SetProductCardReader(cards)
	registerTicket(t, repo, 42, "videos/42/cards.mp4")
	store.objectSizes["videos/42/cards.mp4"] = 1024
	created, err := svc.Create(context.Background(), 42, CreateParams{Title: "带货视频", ObjectKey: "videos/42/cards.mp4", DurationSeconds: 10})
	require.NoError(t, err)
	draft, err := svc.Get(context.Background(), 42, created.VideoID)
	require.NoError(t, err)
	require.Empty(t, draft.ProductCards)
	require.Zero(t, cards.calls)
	_, err = svc.Publish(context.Background(), 42, created.VideoID)
	require.NoError(t, err)
	published, err := svc.Get(context.Background(), 0, created.VideoID)
	require.NoError(t, err)
	require.Equal(t, int64(8), published.ProductCards[0].ProductID)
	require.Equal(t, 1, cards.calls)
}
