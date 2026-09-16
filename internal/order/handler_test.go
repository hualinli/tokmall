package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/stretchr/testify/require"
)

type handlerOrderRepository struct {
	command CreateCommand
	err     error
	replay  bool
}

func (r *handlerOrderRepository) Create(_ context.Context, cmd CreateCommand) (*createRecord, error) {
	r.command = cmd
	if r.err != nil {
		return nil, r.err
	}
	return &createRecord{
		Order: Order{ID: 31, OrderNo: "order-31", Status: StatusPendingPayment,
			TotalAmount: 1998, ExpiresAt: time.Date(2026, 9, 16, 12, 30, 0, 0, time.UTC)},
		Item: OrderItem{ProductID: 7, SKUID: cmd.SKUID, MerchantIDSnapshot: 9,
			ProductTitleSnapshot: "keyboard", SKUSpecSnapshot: "black", UnitPrice: 999,
			Quantity: cmd.Quantity, LineAmount: 1998},
		Replayed: r.replay,
	}, nil
}

func newOrderHandlerEngine(repo Repository, userID int64) *route.Engine {
	h := route.NewEngine(config.NewOptions([]config.Option{}))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Set(middleware.CtxUserIDKey, userID)
		c.Next(ctx)
	})
	NewHandler(newTestOrderService(repo)).RegisterProtectedRoutes(h.Group("/api/v1"))
	return h
}

func performCreateOrder(h *route.Engine, body string) *ut.ResponseRecorder {
	return ut.PerformRequest(h, http.MethodPost, "/api/v1/orders",
		&ut.Body{Body: bytes.NewBufferString(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

func TestHandlerCreateUsesAuthenticatedUserAndStableResponse(t *testing.T) {
	repo := &handlerOrderRepository{}
	w := performCreateOrder(newOrderHandlerEngine(repo, 42), `{
		"sku_id":11,"quantity":2,"idempotency_key":"retry-1",
		"source":{"video_id":5,"author_id":6,"product_id":7}
	}`)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, int64(42), repo.command.UserID)
	require.Equal(t, int64(11), repo.command.SKUID)
	require.Equal(t, &Source{VideoID: 5, AuthorID: 6, ProductID: 7}, repo.command.Source)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, float64(31), payload["order_id"])
	require.Equal(t, false, payload["replayed"])
	require.NotContains(t, payload, "UserID")
}

func TestHandlerCreateReplayKeepsStatusAndSchema(t *testing.T) {
	repo := &handlerOrderRepository{replay: true}
	w := performCreateOrder(newOrderHandlerEngine(repo, 42),
		`{"sku_id":11,"quantity":2,"idempotency_key":"retry-1"}`)
	require.Equal(t, http.StatusCreated, w.Code)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, true, payload["replayed"])
	require.Equal(t, "order-31", payload["order_no"])
}

func TestHandlerCreateMapsErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		biz  string
	}{
		{"invalid", ErrInvalidParam, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"not sellable", ErrNotFound, http.StatusNotFound, "PRODUCT_NOT_FOUND"},
		{"stock", ErrInsufficientStock, http.StatusConflict, "INSUFFICIENT_STOCK"},
		{"idempotency", ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{"unknown", errors.New("database unavailable"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := performCreateOrder(newOrderHandlerEngine(&handlerOrderRepository{err: tt.err}, 42),
				`{"sku_id":11,"quantity":2,"idempotency_key":"retry-1"}`)
			require.Equal(t, tt.code, w.Code)
			require.Contains(t, w.Body.String(), tt.biz)
		})
	}
}

func TestHandlerCreateRejectsMalformedRequest(t *testing.T) {
	repo := &handlerOrderRepository{}
	w := performCreateOrder(newOrderHandlerEngine(repo, 42), `{"sku_id":11}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ARGUMENT")
}
