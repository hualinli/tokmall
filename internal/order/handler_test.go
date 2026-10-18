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
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/middleware"
)

type handlerOrderRepository struct {
	command       CreateCommand
	payCommand    PayCommand
	cancelUserID  int64
	cancelOrderID int64
	err           error
	replay        bool
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

func (r *handlerOrderRepository) Pay(_ context.Context, cmd PayCommand) (*paymentRecord, error) {
	r.payCommand = cmd
	if r.err != nil {
		return nil, r.err
	}
	at := time.Date(2026, 9, 16, 12, 5, 0, 0, time.UTC)
	provider := cmd.ProviderTradeNo
	return &paymentRecord{
		Order: Order{ID: cmd.OrderID, OrderNo: "order-31", Status: StatusPaid},
		Payment: Payment{PaymentNo: cmd.PaymentNo, OrderID: cmd.OrderID, Amount: cmd.Amount,
			Status: PaymentStatusSuccess, ProviderTradeNo: &provider, PaidAt: &at},
		Replayed: r.replay,
	}, nil
}

func (r *handlerOrderRepository) Cancel(_ context.Context, userID, orderID int64, at time.Time) (*cancelRecord, error) {
	r.cancelUserID, r.cancelOrderID = userID, orderID
	if r.err != nil {
		return nil, r.err
	}
	at = at.UTC()
	return &cancelRecord{Order: Order{
		ID: orderID, OrderNo: "order-31", Status: StatusCanceled, CanceledAt: &at,
	}, Replayed: r.replay}, nil
}

func (r *handlerOrderRepository) Get(context.Context, int64, int64, bool) (*orderRecord, error) {
	return nil, r.err
}
func (r *handlerOrderRepository) List(context.Context, int64, int64, int, bool) ([]orderRecord, error) {
	return nil, r.err
}
func (r *handlerOrderRepository) Transition(context.Context, TransitionCommand) (*transitionRecord, error) {
	return nil, r.err
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

func performOrderCommand(h *route.Engine, method, path, body string) *ut.ResponseRecorder {
	return ut.PerformRequest(h, method, path,
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

func TestHandlerPayUsesAuthenticatedOwnerAndPathOrder(t *testing.T) {
	repo := &handlerOrderRepository{}
	w := performOrderCommand(newOrderHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/31/pay",
		`{"payment_no":" payment-31 ","provider_trade_no":" provider-31 ","amount":1998}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(42), repo.payCommand.UserID)
	require.Equal(t, int64(31), repo.payCommand.OrderID)
	require.Equal(t, "payment-31", repo.payCommand.PaymentNo)
	require.Contains(t, w.Body.String(), `"status":"PAID"`)
}

func TestHandlerCancelUsesAuthenticatedOwnerAndPathOrder(t *testing.T) {
	repo := &handlerOrderRepository{replay: true}
	w := performOrderCommand(newOrderHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/31/cancel", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(42), repo.cancelUserID)
	require.Equal(t, int64(31), repo.cancelOrderID)
	require.Contains(t, w.Body.String(), `"replayed":true`)
}

func TestHandlerPaymentAndCancelMapBusinessErrors(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		err  error
		code int
		biz  string
	}{
		{"not owner is hidden", "/api/v1/orders/31/pay", `{"payment_no":"p","provider_trade_no":"t","amount":1}`, ErrOrderNotFound, http.StatusNotFound, "ORDER_NOT_FOUND"},
		{"amount mismatch", "/api/v1/orders/31/pay", `{"payment_no":"p","provider_trade_no":"t","amount":1}`, ErrPaymentAmountMismatch, http.StatusConflict, "PAYMENT_AMOUNT_MISMATCH"},
		{"payment state", "/api/v1/orders/31/pay", `{"payment_no":"p","provider_trade_no":"t","amount":1}`, ErrOrderStateConflict, http.StatusConflict, "ORDER_STATE_CONFLICT"},
		{"payment identity", "/api/v1/orders/31/pay", `{"payment_no":"p","provider_trade_no":"t","amount":1}`, ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{"cancel state", "/api/v1/orders/31/cancel", "", ErrOrderStateConflict, http.StatusConflict, "ORDER_STATE_CONFLICT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := performOrderCommand(newOrderHandlerEngine(&handlerOrderRepository{err: tt.err}, 42), http.MethodPost, tt.path, tt.body)
			require.Equal(t, tt.code, w.Code)
			require.Contains(t, w.Body.String(), tt.biz)
		})
	}
}

func TestHandlerPaymentAndCancelRejectInvalidInput(t *testing.T) {
	h := newOrderHandlerEngine(&handlerOrderRepository{}, 42)
	for _, tc := range []struct{ path, body string }{
		{"/api/v1/orders/nope/pay", `{"payment_no":"p","provider_trade_no":"t","amount":1}`},
		{"/api/v1/orders/31/pay", `{}`},
		{"/api/v1/orders/0/cancel", ""},
	} {
		w := performOrderCommand(h, http.MethodPost, tc.path, tc.body)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "INVALID_ARGUMENT")
	}
}
