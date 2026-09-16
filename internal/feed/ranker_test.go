package feed

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func cand(videoID int64, authorID int64, offset time.Duration, plays, likes int64) Candidate {
	return Candidate{
		Video:     Video{ID: videoID, AuthorID: authorID, PublishedAt: ts(offset)},
		PlayCount: plays,
		LikeCount: likes,
	}
}

func rankTitles(t *testing.T, r Ranker, candidates []Candidate) []string {
	t.Helper()
	ranked, err := r.Rank(context.Background(), candidates, SceneFollowing)
	require.NoError(t, err)
	titles := make([]string, len(ranked))
	for i, c := range ranked {
		titles[i] = c.Video.Title
	}
	return titles
}

func TestTimeRanker_OrdersByPublishedAtDesc(t *testing.T) {
	candidates := []Candidate{
		cand(3, 1, -3*time.Minute, 0, 0), // 最旧
		cand(1, 1, -1*time.Minute, 0, 0), // 最新
		cand(2, 2, -2*time.Minute, 0, 0),
	}
	candidates[0].Video.Title = "oldest"
	candidates[1].Video.Title = "newest"
	candidates[2].Video.Title = "middle"

	titles := rankTitles(t, TimeRanker{}, candidates)
	require.Equal(t, []string{"newest", "middle", "oldest"}, titles)
}

func TestTimeRanker_TieBreakByIDDesc(t *testing.T) {
	candidates := []Candidate{
		cand(5, 1, 0, 0, 0), // 同时间：id 大在前
		cand(3, 1, 0, 0, 0),
	}
	require.Equal(t, []int64{5, 3}, rankIDs(t, TimeRanker{}, candidates))
}

func TestTimeRanker_DoesNotMutateInput(t *testing.T) {
	in := []Candidate{cand(2, 1, 0, 0, 0), cand(1, 1, -time.Minute, 0, 0)}
	before := in[0].Video.ID
	_, err := TimeRanker{}.Rank(context.Background(), in, SceneFollowing)
	require.NoError(t, err)
	require.Equal(t, before, in[0].Video.ID, "排序不得修改入参切片元素")
}

func TestTimeRanker_NeedsNoCounts(t *testing.T) {
	require.False(t, TimeRanker{}.NeedsCounts(), "时间排序不查计数，省一次聚合")
}

func TestTimeRanker_SupportsCursorPagination(t *testing.T) {
	require.True(t, TimeRanker{}.SupportsCursorPagination(), "时间排序键与 SQL 游标一致")
}

func TestHotRanker_PlayCountDominates(t *testing.T) {
	// 发布时间接近（1 分钟差），播放数差距大：播放多的排前
	candidates := []Candidate{
		cand(1, 1, -2*time.Minute, 10, 0),
		cand(2, 1, -1*time.Minute, 1000, 0),
	}
	require.Equal(t, []int64{2, 1}, rankIDs(t, HotRanker{}, candidates),
		"播放数 1000 的应压过 10 的（时间差 1 分钟不足以翻盘）")
}

func TestHotRanker_LikesWeightedHigher(t *testing.T) {
	// 同时间：A 播放 100 无赞，B 播放 50 有 3 赞。
	// 2*log(4) ≈ 2.77 > log(101) ≈ 4.62 - log(51) ≈ 3.93 -> B 应在前
	candidates := []Candidate{
		cand(1, 1, 0, 100, 0),
		cand(2, 1, 0, 50, 3),
	}
	require.Equal(t, []int64{2, 1}, rankIDs(t, HotRanker{}, candidates),
		"likes 权重 2x：少播放多点赞的应排前")
}

func TestHotRanker_DecayOverTime(t *testing.T) {
	// 播放数相同，新内容因时间衰减排前
	candidates := []Candidate{
		cand(1, 1, -10*time.Hour, 100, 0),
		cand(2, 1, -1*time.Hour, 100, 0),
	}
	require.Equal(t, []int64{2, 1}, rankIDs(t, HotRanker{}, candidates),
		"同样播放数，新内容衰减小应排前")
}

func TestHotRanker_FallbackToTimeOnEqualScore(t *testing.T) {
	// 分数相同（无播放无点赞，纯衰减）：回退时间排序
	candidates := []Candidate{
		cand(5, 1, 0, 0, 0),
		cand(3, 1, 0, 0, 0),
		cand(4, 2, -time.Minute, 0, 0),
	}
	require.Equal(t, []int64{5, 3, 4}, rankIDs(t, HotRanker{}, candidates))
}

func TestHotRanker_NeedsCounts(t *testing.T) {
	require.True(t, HotRanker{}.NeedsCounts(), "热度排序需要播放/点赞计数")
}

func TestHotRanker_DoesNotSupportCursorPagination(t *testing.T) {
	require.False(t, HotRanker{}.SupportsCursorPagination(), "热度排序当前仅作为无分页排序实验")
}

// HotRanker 的排序行为保留在上面的单元实验中，但 Service 不得把它暴露为
// 当前时间游标语义下的在线关注 Feed。
func TestListFollowing_RejectsHotRanker(t *testing.T) {
	repo := &fakeRepo{
		videos:  mixedAuthorVideos(),
		follows: map[int64][]int64{7: {1, 2}},
		plays:   map[int64]int64{11: 500, 9: 400, 12: 300, 10: 200, 8: 100},
	}
	svc := NewService(repo, NewCodec("test-cursor-secret"), fakeURLProvider{}, &fakeCache{}, nil, nil, HotRanker{})

	result, err := svc.ListFollowing(context.Background(), 7, 5, "")
	require.ErrorIs(t, err, ErrRankPaginationUnsupported)
	require.Nil(t, result)
}

func rankIDs(t *testing.T, r Ranker, candidates []Candidate) []int64 {
	t.Helper()
	ranked, err := r.Rank(context.Background(), candidates, SceneFollowing)
	require.NoError(t, err)
	ids := make([]int64, len(ranked))
	for i, c := range ranked {
		ids[i] = c.Video.ID
	}
	return ids
}
