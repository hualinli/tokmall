package feed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type changingCards struct {
	rows  []productCardRow
	calls int
}

func (f *changingCards) ListProductCards(_ context.Context, _ []int64) ([]productCardRow, error) {
	f.calls++
	return f.rows, nil
}

func TestProductCardsRefreshAcrossFeedCacheHits(t *testing.T) {
	repo := &fakeRepo{videos: fiveVideos()}
	cache := &fakeCache{}
	cards := &changingCards{rows: []productCardRow{{VideoID: 5, ProductID: 7, Title: "商品", MinPrice: 1200, ProductStatus: "ON_SALE", CoverKey: "products/7.jpg"}}}
	svc := newTestServiceWithCache(repo, cache)
	svc.SetProductCardReader(cards)

	first, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Equal(t, int64(7), first.Items[0].ProductCards[0].ProductID)
	require.Equal(t, "ON_SALE", first.Items[0].ProductCards[0].ProductStatus)
	require.Equal(t, "http://minio.local/signed/products/7.jpg", *first.Items[0].ProductCards[0].CoverURL)
	require.Empty(t, cache.store.Items[0].ProductCards, "dynamic cards must not enter the shared cache")

	cards.rows = nil // unlink or off-sale after the first request
	second, err := svc.List(context.Background(), 0, "")
	require.NoError(t, err)
	require.Empty(t, second.Items[0].ProductCards)
	require.Equal(t, 2, cards.calls)
	require.Equal(t, 1, repo.calls, "the video page still comes from cache")
}
