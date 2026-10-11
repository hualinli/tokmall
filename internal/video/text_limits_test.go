package video

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 合同 maxLength 200 按字符计：200 个中文字符（600 字节）必须合法，201 个必须拒绝。
func TestVideoTitleCountsCharacters(t *testing.T) {
	repo, store := newFakeRepo(), newFakeStorage()
	svc := newTestService(repo, store)
	key := "videos/42/cjk-title.mp4"
	registerTicket(t, repo, 42, key)
	store.objectSizes[key] = 1 << 20

	_, err := svc.Create(context.Background(), 42, CreateParams{Title: strings.Repeat("中", 201), ObjectKey: key, DurationSeconds: 10})
	require.ErrorIs(t, err, ErrInvalidParam)
	require.False(t, repo.tickets[key].Used, "rejected title must not consume the upload ticket")

	_, err = svc.Create(context.Background(), 42, CreateParams{Title: strings.Repeat("中", 200), ObjectKey: key, DurationSeconds: 10})
	require.NoError(t, err)
}
