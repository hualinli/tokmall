package promotion

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/middleware"
)

func handlerEngine(repo Repository, userID int64) *route.Engine {
	h := route.NewEngine(config.NewOptions([]config.Option{}))
	h.Use(func(ctx context.Context, c *app.RequestContext) { c.Set(middleware.CtxUserIDKey, userID); c.Next(ctx) })
	handler := NewHandler(NewService(repo))
	g := h.Group("/api/v1")
	handler.RegisterProtectedRoutes(g)
	handler.RegisterOptionalRoutes(g)
	return h
}
func request(h *route.Engine, method, path, body string) *ut.ResponseRecorder {
	return ut.PerformRequest(h, method, path, &ut.Body{Body: bytes.NewBufferString(body), Len: len(body)}, ut.Header{Key: "Content-Type", Value: "application/json"})
}

func TestHandlerLinkUsesAuthenticatedIdentity(t *testing.T) {
	repo := &fakeRepository{}
	w := request(handlerEngine(repo, 42), http.MethodPost, "/api/v1/videos/7/products", `{"product_id":9,"sort_order":2}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(42), repo.authorID)
	require.Contains(t, w.Body.String(), `"status":"ACTIVE"`)
}
func TestHandlerAdjustDisableAndList(t *testing.T) {
	repo := &fakeRepository{cards: []ProductCard{{ProductID: 9, Title: "Keyboard", MinPrice: 1999}}}
	h := handlerEngine(repo, 42)
	w := request(h, http.MethodPatch, "/api/v1/videos/7/products/9", `{"sort_order":3}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "sort", repo.method)
	w = request(h, http.MethodDelete, "/api/v1/videos/7/products/9", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"status":"DISABLED"`)
	w = request(h, http.MethodGet, "/api/v1/videos/7/products", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "Keyboard")
}
func TestHandlerMapsHiddenAndLimitErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		biz  string
	}{{ErrNotFound, 404, "VIDEO_PRODUCT_NOT_FOUND"}, {ErrActiveLimit, 409, "VIDEO_PRODUCT_LIMIT"}} {
		w := request(handlerEngine(&fakeRepository{err: tc.err}, 42), http.MethodPost, "/api/v1/videos/7/products", `{"product_id":9}`)
		require.Equal(t, tc.code, w.Code)
		require.Contains(t, w.Body.String(), tc.biz)
	}
}
func TestHandlerRejectsMalformedInput(t *testing.T) {
	h := handlerEngine(&fakeRepository{}, 42)
	for _, tc := range []struct{ method, path, body string }{{http.MethodPost, "/api/v1/videos/no/products", `{"product_id":9}`}, {http.MethodPost, "/api/v1/videos/7/products", `{}`}, {http.MethodPatch, "/api/v1/videos/7/products/9", `{"sort_order":-1}`}, {http.MethodPatch, "/api/v1/videos/7/products/9", `{}`}} {
		w := request(h, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}
