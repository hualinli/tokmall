package attribution

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/stretchr/testify/require"
)

func TestSalesOnlyExposesAuthenticatedAuthor(t *testing.T) {
	repo := &fakeRepository{}
	h := route.NewEngine(config.NewOptions([]config.Option{}))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Set(middleware.CtxUserIDKey, int64(42))
		c.Next(ctx)
	})
	NewHandler(NewService(repo)).RegisterProtectedRoutes(h.Group("/api/v1"))

	denied := ut.PerformRequest(h, http.MethodGet, "/api/v1/authors/43/sales", nil)
	require.Equal(t, http.StatusNotFound, denied.Code)
	require.Zero(t, repo.called)

	allowed := ut.PerformRequest(h, http.MethodGet, "/api/v1/authors/42/sales", nil)
	require.Equal(t, http.StatusOK, allowed.Code)
	require.Equal(t, int64(42), repo.called)
	require.Contains(t, allowed.Body.String(), `"author_id":42`)
}
