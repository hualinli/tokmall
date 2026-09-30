package order

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/lihualin/tokmall/internal/middleware"
	"github.com/lihualin/tokmall/internal/response"
)

// RequestRefundRequest 是买家退款申请 DTO。user_id/merchant_id/amount 都不接受
// 客户端提交：金额由订单事实决定，身份由认证上下文提供，避免越权与金额篡改。
type RequestRefundRequest struct {
	Reason string `json:"reason" binding:"required"`
}

type DecideRefundRequest struct {
	Remark string `json:"remark"`
}

func (h *Handler) requestRefund(ctx context.Context, c *app.RequestContext) {
	orderID, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req RequestRefundRequest
	if err := c.BindAndValidate(&req); err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.RequestRefund(ctx, RequestRefundParams{
		UserID: c.GetInt64(middleware.CtxUserIDKey), OrderID: orderID, Reason: req.Reason,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.Created(c, view)
}

func (h *Handler) getBuyerRefund(ctx context.Context, c *app.RequestContext) {
	h.getRefund(ctx, c, false)
}

func (h *Handler) getMerchantRefund(ctx context.Context, c *app.RequestContext) {
	h.getRefund(ctx, c, true)
}

func (h *Handler) getRefund(ctx context.Context, c *app.RequestContext, merchant bool) {
	orderID, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	view, err := h.service.GetRefund(ctx, c.GetInt64(middleware.CtxUserIDKey), orderID, merchant)
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}

func (h *Handler) approveRefund(ctx context.Context, c *app.RequestContext) {
	h.decideRefund(ctx, c, true)
}

func (h *Handler) rejectRefund(ctx context.Context, c *app.RequestContext) {
	h.decideRefund(ctx, c, false)
}

func (h *Handler) decideRefund(ctx context.Context, c *app.RequestContext, approve bool) {
	orderID, err := parseOrderID(c.Param("id"))
	if err != nil {
		response.Error(c, response.ErrInvalidArgument)
		return
	}
	var req DecideRefundRequest
	// 拒绝可以不带备注，因此空 body 不算错误；带 body 时按结构体解析。
	if len(c.Request.Body()) > 0 {
		if err := c.BindAndValidate(&req); err != nil {
			response.Error(c, response.ErrInvalidArgument)
			return
		}
	}
	view, err := h.service.DecideRefund(ctx, DecideRefundParams{
		MerchantID: c.GetInt64(middleware.CtxUserIDKey), OrderID: orderID,
		Approve: approve, Remark: req.Remark,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}
	response.OK(c, view)
}
