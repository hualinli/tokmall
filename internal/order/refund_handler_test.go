package order

import (
	"context"
	"encoding/json"
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

// handlerRefundRepository 只实现售后命令，用来验证 HTTP 边界：
// 身份来自认证上下文、金额不由客户端提交、错误码映射稳定。
type handlerRefundRepository struct {
	request   RequestRefundCommand
	decide    DecideRefundCommand
	getActor  int64
	getMerc   bool
	err       error
	view      *refundRecord
	callCount int
}

func (r *handlerRefundRepository) RequestRefund(_ context.Context, cmd RequestRefundCommand) (*refundRecord, error) {
	r.callCount++
	r.request = cmd
	if r.err != nil {
		return nil, r.err
	}
	if r.view != nil {
		return r.view, nil
	}
	from := StatusPaid
	return &refundRecord{
		Order:  Order{ID: cmd.OrderID, OrderNo: "order-31", Status: StatusRefundRequested, RefundFromStatus: &from},
		Refund: Refund{ID: 88, RefundNo: cmd.RefundNo, OrderID: cmd.OrderID, Amount: 1998, Reason: cmd.Reason, Status: RefundStatusRequested, RequestedAt: cmd.RequestedAt},
	}, nil
}

func (r *handlerRefundRepository) DecideRefund(_ context.Context, cmd DecideRefundCommand) (*refundRecord, error) {
	r.callCount++
	r.decide = cmd
	if r.err != nil {
		return nil, r.err
	}
	at := cmd.DecidedAt
	status := RefundStatusSucceeded
	orderStatus := StatusRefunded
	if !cmd.Approve {
		status = RefundStatusRejected
		orderStatus = StatusPaid
	}
	record := &refundRecord{
		Order:  Order{ID: cmd.OrderID, OrderNo: "order-31", Status: orderStatus},
		Refund: Refund{ID: 88, OrderID: cmd.OrderID, Amount: 1998, Status: status, DecidedAt: &at},
	}
	return record, nil
}

func (r *handlerRefundRepository) GetRefund(_ context.Context, actorID, _ int64, merchant bool) (*refundRecord, error) {
	r.callCount++
	r.getActor, r.getMerc = actorID, merchant
	if r.err != nil {
		return nil, r.err
	}
	if r.view != nil {
		return r.view, nil
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &refundRecord{
		Order:  Order{ID: 31, OrderNo: "order-31", Status: StatusRefunded},
		Refund: Refund{ID: 88, OrderID: 31, Amount: 1998, Status: RefundStatusSucceeded, DecidedAt: &at, RefundedAt: &at},
	}, nil
}

func newRefundHandlerEngine(repo RefundRepository, userID int64) *route.Engine {
	h := route.NewEngine(config.NewOptions([]config.Option{}))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Set(middleware.CtxUserIDKey, userID)
		c.Next(ctx)
	})
	service := NewService(&fakeOrderRepository{byKey: map[string]*createRecord{}})
	service.SetRefundRepository(repo)
	NewHandler(service).RegisterProtectedRoutes(h.Group("/api/v1"))
	return h
}

func TestHandlerRequestRefundUsesAuthenticatedUserAndOrderAmount(t *testing.T) {
	repo := &handlerRefundRepository{}
	w := performOrderCommand(newRefundHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/31/refund", `{"reason":"尺码不合适"}`)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, int64(42), repo.request.UserID, "买家身份必须来自认证上下文")
	require.Equal(t, int64(31), repo.request.OrderID)
	require.Equal(t, "尺码不合适", repo.request.Reason)
	require.NotEmpty(t, repo.request.RefundNo)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, float64(1998), payload["amount"], "退款金额来自订单事实，不接受客户端提交")
	require.Equal(t, "REQUESTED", payload["status"])
	require.Equal(t, true, payload["restore_stock"])
	require.Equal(t, "REFUND_REQUESTED", payload["order_status"])
	require.NotContains(t, payload, "user_id")
}

func TestHandlerRequestRefundRejectsMissingReason(t *testing.T) {
	repo := &handlerRefundRepository{}
	w := performOrderCommand(newRefundHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/31/refund", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Zero(t, repo.callCount, "非法请求不应触达 service/repository")
}

func TestHandlerRequestRefundRejectsInvalidOrderID(t *testing.T) {
	repo := &handlerRefundRepository{}
	w := performOrderCommand(newRefundHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/abc/refund", `{"reason":"x"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Zero(t, repo.callCount)
}

func TestHandlerRefundErrorMapping(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"order not found":    {ErrOrderNotFound, http.StatusNotFound, "ORDER_NOT_FOUND"},
		"refund not found":   {ErrRefundNotFound, http.StatusNotFound, "REFUND_NOT_FOUND"},
		"refund not allowed": {ErrRefundNotAllowed, http.StatusConflict, "REFUND_NOT_ALLOWED"},
		"already decided":    {ErrRefundAlreadyDecided, http.StatusConflict, "REFUND_ALREADY_DECIDED"},
		"state conflict":     {ErrOrderStateConflict, http.StatusConflict, "ORDER_STATE_CONFLICT"},
		"invalid param":      {ErrInvalidParam, http.StatusBadRequest, "INVALID_ARGUMENT"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &handlerRefundRepository{err: tc.err}
			w := performOrderCommand(newRefundHandlerEngine(repo, 42), http.MethodPost, "/api/v1/orders/31/refund", `{"reason":"x"}`)
			require.Equal(t, tc.status, w.Code)
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			require.Equal(t, tc.code, payload.Error.Code)
		})
	}
}

func TestHandlerApproveRefundAcceptsEmptyBody(t *testing.T) {
	repo := &handlerRefundRepository{}
	engine := newRefundHandlerEngine(repo, 7)
	w := ut.PerformRequest(engine, http.MethodPost, "/api/v1/merchant/orders/31/refund/approve", nil)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(7), repo.decide.MerchantID)
	require.True(t, repo.decide.Approve)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, "SUCCEEDED", payload["status"])
	require.Equal(t, "REFUNDED", payload["order_status"])
}

func TestHandlerRejectRefundKeepsRemark(t *testing.T) {
	repo := &handlerRefundRepository{}
	w := performOrderCommand(newRefundHandlerEngine(repo, 7), http.MethodPost, "/api/v1/merchant/orders/31/refund/reject", `{"remark":"已发货"}`)

	require.Equal(t, http.StatusOK, w.Code)
	require.False(t, repo.decide.Approve)
	require.Equal(t, "已发货", repo.decide.Remark)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, "REJECTED", payload["status"])
	require.Equal(t, "PAID", payload["order_status"])
}

func TestHandlerRefundGetUsesMerchantScope(t *testing.T) {
	repo := &handlerRefundRepository{}
	w := performOrderCommand(newRefundHandlerEngine(repo, 9), http.MethodGet, "/api/v1/merchant/orders/31/refund", "")

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(9), repo.getActor)
	require.True(t, repo.getMerc, "商家端点必须按 merchant_id 归属查询")
}
