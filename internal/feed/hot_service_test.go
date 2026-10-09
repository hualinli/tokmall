package feed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubHotFeed struct {
	entries []HotEntry
	err     error
	calls   int
}

func (h *stubHotFeed) CurrentWindowEnd() time.Time { return time.Now().UTC().Truncate(time.Minute) }
func (h *stubHotFeed) Page(_ context.Context, _ time.Time, _ *HotCursor, limit int) ([]HotEntry, error) {
	h.calls++
	return h.entries[:min(len(h.entries), limit)], h.err
}

func TestHotFallbackContinuesTimelineAfterRecovery(t *testing.T) {
	for _, initialErr := range []error{ErrHotFeedUnavailable, errors.New("redis unavailable")} {
		svc := newTestService(&fakeRepo{videos: fiveVideos()})
		hot := &stubHotFeed{err: initialErr}
		svc.SetHotFeed(hot, nil)
		first, err := svc.ListHot(context.Background(), 1, "")
		require.NoError(t, err)
		require.NotNil(t, first.NextCursor)
		hot.err = nil
		hot.entries = []HotEntry{{VideoID: 5, HotScore: 3}}
		second, err := svc.ListHot(context.Background(), 1, *first.NextCursor)
		require.NoError(t, err)
		require.Equal(t, int64(4), second.Items[0].VideoID)
		require.Equal(t, 1, hot.calls, "fallback cursor pins the timeline even after hot feed recovers")
	}
}

func TestHotFeedIncludesDynamicFields(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: fiveVideos(), likes: map[string]bool{"1-5": true, "2-5": true}})
	svc.SetHotFeed(&stubHotFeed{entries: []HotEntry{{VideoID: 5, HotScore: 6}}}, nil)
	svc.SetProductCardReader(&changingCards{rows: []productCardRow{{VideoID: 5, ProductID: 7, Title: "product", MinPrice: 100, ProductStatus: "ON_SALE"}}})
	page, err := svc.ListHot(context.Background(), 1, "")
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Items[0].LikeCount)
	require.Equal(t, int64(7), page.Items[0].ProductCards[0].ProductID)
}

func TestHotContinuationDoesNotRestartOnFailureOrEmptyPage(t *testing.T) {
	svc := newTestService(&fakeRepo{videos: fiveVideos()})
	hot := &stubHotFeed{entries: []HotEntry{{VideoID: 5, HotScore: 3}}}
	svc.SetHotFeed(hot, nil)
	first, err := svc.ListHot(context.Background(), 1, "")
	require.NoError(t, err)
	hot.entries = nil
	last, err := svc.ListHot(context.Background(), 1, *first.NextCursor)
	require.NoError(t, err)
	require.Empty(t, last.Items)
	require.False(t, last.HasMore)
	hot.err = ErrInvalidCursor
	_, err = svc.ListHot(context.Background(), 1, *first.NextCursor)
	require.ErrorIs(t, err, ErrInvalidCursor)
}
