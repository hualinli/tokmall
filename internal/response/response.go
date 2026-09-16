package response

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

type BizError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

var (
	ErrInvalidArgument       = &BizError{Code: "INVALID_ARGUMENT", Message: "invalid argument"}
	ErrInvalidCursor         = &BizError{Code: "INVALID_CURSOR", Message: "cursor has expired or is malformed"}
	ErrVideoNotFound         = &BizError{Code: "VIDEO_NOT_FOUND", Message: "video not found"}
	ErrVideoNotPublished     = &BizError{Code: "VIDEO_NOT_PUBLISHED", Message: "video not published"}
	ErrUserNotFound          = &BizError{Code: "USER_NOT_FOUND", Message: "user not found"}
	ErrProductNotFound       = &BizError{Code: "PRODUCT_NOT_FOUND", Message: "product not found"}
	ErrInsufficientStock     = &BizError{Code: "INSUFFICIENT_STOCK", Message: "insufficient stock"}
	ErrOrderNotFound         = &BizError{Code: "ORDER_NOT_FOUND", Message: "order not found"}
	ErrOrderStateConflict    = &BizError{Code: "ORDER_STATE_CONFLICT", Message: "order state conflict"}
	ErrPaymentAmountMismatch = &BizError{Code: "PAYMENT_AMOUNT_MISMATCH", Message: "payment amount mismatch"}
	ErrUnauthenticated       = &BizError{Code: "UNAUTHENTICATED", Message: "invalid credentials"}
	ErrForbidden             = &BizError{Code: "FORBIDDEN", Message: "forbidden"}
	ErrRateLimited           = &BizError{Code: "RATE_LIMITED", Message: "too many requests"}
	ErrDuplicateAction       = &BizError{Code: "DUPLICATE_ACTION", Message: "duplicate action"}
	ErrIdempotencyConflict   = &BizError{Code: "IDEMPOTENCY_CONFLICT", Message: "idempotency key conflicts with an existing request"}
	ErrUploadURLExpired      = &BizError{Code: "UPLOAD_URL_EXPIRED", Message: "upload url expired"}
	ErrInternal              = &BizError{Code: "INTERNAL", Message: "internal error"}
	ErrDependencyUnavailable = &BizError{Code: "DEPENDENCY_UNAVAILABLE", Message: "dependency unavailable"}
	ErrMissingAuthHeader     = &BizError{Code: "MISSING_AUTH_HEADER", Message: "missing auth header"}
	ErrInvalidAuthType       = &BizError{Code: "INVALID_AUTH_TYPE", Message: "invalid auth type"}
	ErrInvalidToken          = &BizError{Code: "INVALID_TOKEN", Message: "invalid token"}
)

var statusByCode = map[string]int{
	"INVALID_ARGUMENT":        consts.StatusBadRequest,
	"INVALID_CURSOR":          consts.StatusBadRequest,
	"VIDEO_NOT_FOUND":         consts.StatusNotFound,
	"VIDEO_NOT_PUBLISHED":     consts.StatusNotFound,
	"USER_NOT_FOUND":          consts.StatusNotFound,
	"PRODUCT_NOT_FOUND":       consts.StatusNotFound,
	"INSUFFICIENT_STOCK":      consts.StatusConflict,
	"ORDER_NOT_FOUND":         consts.StatusNotFound,
	"ORDER_STATE_CONFLICT":    consts.StatusConflict,
	"PAYMENT_AMOUNT_MISMATCH": consts.StatusConflict,
	"UNAUTHENTICATED":         consts.StatusUnauthorized,
	"MISSING_AUTH_HEADER":     consts.StatusUnauthorized,
	"INVALID_AUTH_TYPE":       consts.StatusUnauthorized,
	"INVALID_TOKEN":           consts.StatusUnauthorized,
	"FORBIDDEN":               consts.StatusForbidden,
	"RATE_LIMITED":            consts.StatusTooManyRequests,
	"DUPLICATE_ACTION":        consts.StatusConflict,
	"IDEMPOTENCY_CONFLICT":    consts.StatusConflict,
	"UPLOAD_URL_EXPIRED":      consts.StatusBadRequest,
	"INTERNAL":                consts.StatusInternalServerError,
	"DEPENDENCY_UNAVAILABLE":  consts.StatusServiceUnavailable,
}

func OK(ctx *app.RequestContext, data any) {
	ctx.JSON(consts.StatusOK, data)
}

func Created(ctx *app.RequestContext, data any) {
	ctx.JSON(consts.StatusCreated, data)
}

func Accepted(ctx *app.RequestContext, data any) {
	ctx.JSON(consts.StatusAccepted, data)
}

func NoContent(ctx *app.RequestContext) {
	ctx.SetStatusCode(consts.StatusNoContent)
}

func Error(ctx *app.RequestContext, bizErr *BizError) {
	status := statusByCode[bizErr.Code]
	if status == 0 {
		status = consts.StatusInternalServerError
	}
	ctx.JSON(status, map[string]any{
		"error":      bizErr,
		"request_id": string(ctx.Response.Header.Get("X-Request-ID")),
	})
}

func Abort(ctx *app.RequestContext, bizErr *BizError) {
	status := statusByCode[bizErr.Code]
	if status == 0 {
		// Abort 是中间件拒绝路径（认证/限流等），未映射时默认 401 而非 500
		status = consts.StatusUnauthorized
	}
	ctx.AbortWithStatusJSON(status, map[string]any{
		"error":      bizErr,
		"request_id": string(ctx.Response.Header.Get("X-Request-ID")),
	})
}
