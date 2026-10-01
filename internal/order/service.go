package order

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidParam        = errors.New("invalid param")
	ErrNotFound            = errors.New("sellable sku or source not found")
	ErrInsufficientStock   = errors.New("insufficient stock")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
	ErrOrderNotFound       = errors.New("order not found")
	ErrOrderStateConflict  = errors.New("order state conflict")
	// ErrOrderExpired：支付到达时订单已超过 expires_at。支付事务会顺手把订单
	// 关单并回补库存（lazy expiry），因此这是终态而不是可重试的临时冲突。
	ErrOrderExpired          = errors.New("order expired before payment")
	ErrPaymentAmountMismatch = errors.New("payment amount mismatch")
	// 券相关：分别对应"券不可用/已用"、"券不存在或不属于你"、"未达门槛"。
	ErrCouponNotFound        = errors.New("user coupon not found")
	ErrCouponConflict        = errors.New("user coupon is not usable")
	ErrCouponNotUsable       = errors.New("user coupon expired")
	ErrCouponThresholdNotMet = errors.New("order amount does not meet the coupon threshold")
)

const (
	maxIdempotencyKeyLength = 128
	maxOrderQuantity        = 1000
	defaultPaymentTTL       = 30 * time.Minute
)

type Source struct {
	VideoID   int64 `json:"video_id"`
	AuthorID  int64 `json:"author_id"`
	ProductID int64 `json:"product_id"`
}

type CreateParams struct {
	UserID         int64
	SKUID          int64
	Quantity       int32
	IdempotencyKey string
	Source         *Source
	// UserCouponID 是可选的持券 ID。它是**请求指纹的一部分**：
	// 同一个幂等键换一张券重放必须冲突，否则会出现"用 A 券的请求拿到 B 券的订单"。
	UserCouponID *int64
}

type CreateCommand struct {
	CreateParams
	OrderNo            string
	RequestFingerprint string
	ExpiresAt          time.Time
}

type CreateResult struct {
	OrderID        int64
	OrderNo        string
	Status         Status
	TotalAmount    Amount
	OriginalAmount Amount
	DiscountAmount Amount
	UserCouponID   *int64
	ExpiresAt      time.Time
	Item           ItemView
	Replayed       bool
}

type ItemView struct {
	ProductID    int64  `json:"product_id"`
	SKUID        int64  `json:"sku_id"`
	MerchantID   int64  `json:"merchant_id"`
	ProductTitle string `json:"product_title"`
	SKUSpec      string `json:"sku_spec"`
	UnitPrice    Amount `json:"unit_price"`
	Quantity     int32  `json:"quantity"`
	LineAmount   Amount `json:"line_amount"`
}

// createRecord 是 repository 到 service 的内部数据载体，持久化 model
// 在 service 边界转换为只读结果，后续 handler 不得直接序列化 model。
type createRecord struct {
	Order    Order
	Item     OrderItem
	Replayed bool
}

type PayParams struct {
	UserID          int64
	OrderID         int64
	PaymentNo       string
	ProviderTradeNo string
	Amount          Amount
}

type PayCommand struct {
	PayParams
	PaidAt time.Time
}

type PaymentResult struct {
	OrderID         int64
	OrderNo         string
	Status          Status
	PaymentNo       string
	ProviderTradeNo string
	Amount          Amount
	PaidAt          time.Time
	Replayed        bool
}

type CancelParams struct {
	UserID  int64
	OrderID int64
}

type CancelResult struct {
	OrderID    int64
	OrderNo    string
	Status     Status
	CanceledAt time.Time
	Replayed   bool
}

// orderStatusTransitions 是订单状态图的完整定义（含售后边）。
// 退款相关迁移的含义：
//   - PAID/FULFILLING → REFUND_REQUESTED：买家申请退款，订单离开可履约状态，
//     因此"发货/确认收货"不需要额外查询退款表就能被状态机挡住；
//   - REFUND_REQUESTED → REFUNDED：商家同意，退款成功，订单终态；
//   - REFUND_REQUESTED → PAID/FULFILLING：商家拒绝，退回到 refund_from_status
//     （由 refund_repository 从订单行读取，不接受调用方指定）。
//
// 这张图描述"状态之间能不能到达"，不描述"哪条命令能触发"。
// 履约命令必须使用下面的 fulfillmentTransitions：否则 REFUND_REQUESTED → FULFILLING
// 这条"拒绝退款时退回原状态"的边会被商家发货命令误用，绕过退款裁决。
// （真实 PG 集成测试曾捕获该缺陷，数据库 CHECK 约束在应用层放行时兜住了它。）
var orderStatusTransitions = map[Status]map[Status]struct{}{
	StatusPendingPayment: {
		StatusPaid: {}, StatusCanceled: {}, StatusExpired: {},
	},
	StatusPaid: {
		StatusFulfilling: {}, StatusRefundRequested: {},
	},
	StatusFulfilling: {
		StatusCompleted: {}, StatusRefundRequested: {},
	},
	StatusRefundRequested: {
		StatusRefunded: {}, StatusPaid: {}, StatusFulfilling: {},
	},
}

// fulfillmentTransitions 是"商家发货 / 买家确认收货"两条命令允许的迁移。
// 与完整状态图分开，是为了让"状态可达性"和"命令权限"各自显式：
// 退款申请把订单移出 PAID/FULFILLING 后，履约命令在这里没有出口。
var fulfillmentTransitions = map[Status]map[Status]struct{}{
	StatusPaid:       {StatusFulfilling: {}},
	StatusFulfilling: {StatusCompleted: {}},
}

func canOrderTransition(from, to Status) bool {
	_, ok := orderStatusTransitions[from][to]
	return ok
}

func canFulfillTransition(from, to Status) bool {
	_, ok := fulfillmentTransitions[from][to]
	return ok
}

type paymentRecord struct {
	Order    Order
	Payment  Payment
	Replayed bool
}

type cancelRecord struct {
	Order    Order
	Replayed bool
}

// Repository 定义在消费侧；GormRepository 为每个订单命令提供独立事务边界。
type Repository interface {
	Create(ctx context.Context, cmd CreateCommand) (*createRecord, error)
	Pay(ctx context.Context, cmd PayCommand) (*paymentRecord, error)
	Cancel(ctx context.Context, userID, orderID int64, canceledAt time.Time) (*cancelRecord, error)
	Get(ctx context.Context, actorID, orderID int64, merchant bool) (*orderRecord, error)
	List(ctx context.Context, actorID, beforeID int64, limit int, merchant bool) ([]orderRecord, error)
	Transition(ctx context.Context, actorID, orderID int64, merchant bool, to Status, at time.Time) (*transitionRecord, error)
}

func (s *Service) Pay(ctx context.Context, params PayParams) (*PaymentResult, error) {
	params.PaymentNo = strings.TrimSpace(params.PaymentNo)
	params.ProviderTradeNo = strings.TrimSpace(params.ProviderTradeNo)
	if params.UserID <= 0 || params.OrderID <= 0 || params.PaymentNo == "" || params.ProviderTradeNo == "" || params.Amount <= 0 ||
		len(params.PaymentNo) > 64 || len(params.ProviderTradeNo) > 128 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.Pay(ctx, PayCommand{PayParams: params, PaidAt: s.now()})
	if err != nil {
		return nil, err
	}
	if record == nil || record.Payment.PaidAt == nil || record.Payment.ProviderTradeNo == nil {
		return nil, errors.New("order repository returned invalid payment result")
	}
	return &PaymentResult{
		OrderID: record.Order.ID, OrderNo: record.Order.OrderNo, Status: record.Order.Status,
		PaymentNo: record.Payment.PaymentNo, ProviderTradeNo: *record.Payment.ProviderTradeNo,
		Amount: record.Payment.Amount, PaidAt: *record.Payment.PaidAt, Replayed: record.Replayed,
	}, nil
}

func (s *Service) Cancel(ctx context.Context, params CancelParams) (*CancelResult, error) {
	if params.UserID <= 0 || params.OrderID <= 0 {
		return nil, ErrInvalidParam
	}
	record, err := s.repo.Cancel(ctx, params.UserID, params.OrderID, s.now())
	if err != nil {
		return nil, err
	}
	if record == nil || record.Order.CanceledAt == nil {
		return nil, errors.New("order repository returned invalid cancel result")
	}
	return &CancelResult{
		OrderID: record.Order.ID, OrderNo: record.Order.OrderNo, Status: record.Order.Status,
		CanceledAt: *record.Order.CanceledAt, Replayed: record.Replayed,
	}, nil
}

type Service struct {
	repo        Repository
	refunds     RefundRepository
	now         func() time.Time
	newOrderNo  func() (string, error)
	newRefundNo func() (string, error)
	paymentTTL  time.Duration
}

func NewService(repo Repository) *Service {
	return &Service{
		repo: repo,
		now:  time.Now,
		newOrderNo: func() (string, error) {
			id, err := uuid.NewV7()
			return id.String(), err
		},
		newRefundNo: defaultRefundNo,
		paymentTTL:  defaultPaymentTTL,
	}
}

// SetRefundRepository 由装配方注入售后仓储（与 video/feed 的 SetProductCardReader 同一风格）。
// 未注入时售后命令返回明确错误，而不是 nil 解引用 panic。
func (s *Service) SetRefundRepository(repo RefundRepository) { s.refunds = repo }

func (s *Service) Create(ctx context.Context, params CreateParams) (*CreateResult, error) {
	params.IdempotencyKey = strings.TrimSpace(params.IdempotencyKey)
	if err := validateCreateParams(params); err != nil {
		return nil, err
	}
	fingerprint, err := requestFingerprint(params)
	if err != nil {
		return nil, err
	}
	orderNo, err := s.newOrderNo()
	if err != nil {
		return nil, err
	}
	result, err := s.repo.Create(ctx, CreateCommand{
		CreateParams:       params,
		OrderNo:            orderNo,
		RequestFingerprint: fingerprint,
		ExpiresAt:          s.now().Add(s.paymentTTL),
	})
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			slog.Warn("order idempotency conflict", "user_id", params.UserID, "idempotency_key", params.IdempotencyKey)
		}
		return nil, err
	}
	if result == nil {
		return nil, errors.New("order repository returned nil result")
	}
	return mapCreateResult(result), nil
}

func mapCreateResult(record *createRecord) *CreateResult {
	return &CreateResult{
		OrderID: record.Order.ID, OrderNo: record.Order.OrderNo, Status: record.Order.Status,
		TotalAmount: record.Order.TotalAmount, OriginalAmount: record.Order.OriginalAmount,
		DiscountAmount: record.Order.DiscountAmount, UserCouponID: record.Order.UserCouponID,
		ExpiresAt: record.Order.ExpiresAt,
		Item: ItemView{
			ProductID: record.Item.ProductID, SKUID: record.Item.SKUID,
			MerchantID: record.Item.MerchantIDSnapshot, ProductTitle: record.Item.ProductTitleSnapshot,
			SKUSpec: record.Item.SKUSpecSnapshot, UnitPrice: record.Item.UnitPrice,
			Quantity: record.Item.Quantity, LineAmount: record.Item.LineAmount,
		},
		Replayed: record.Replayed,
	}
}

func validateCreateParams(p CreateParams) error {
	if p.UserID <= 0 || p.SKUID <= 0 || p.Quantity <= 0 || p.Quantity > maxOrderQuantity ||
		p.IdempotencyKey == "" || len(p.IdempotencyKey) > maxIdempotencyKeyLength {
		return ErrInvalidParam
	}
	if p.Source != nil && (p.Source.VideoID <= 0 || p.Source.AuthorID <= 0 || p.Source.ProductID <= 0) {
		return ErrInvalidParam
	}
	if p.UserCouponID != nil && *p.UserCouponID <= 0 {
		return ErrInvalidParam
	}
	return nil
}

type fingerprintPayload struct {
	UserID       int64   `json:"user_id"`
	SKUID        int64   `json:"sku_id"`
	Quantity     int32   `json:"quantity"`
	Source       *Source `json:"source"`
	UserCouponID *int64  `json:"user_coupon_id"`
}

func requestFingerprint(p CreateParams) (string, error) {
	payload, err := json.Marshal(fingerprintPayload{
		UserID: p.UserID, SKUID: p.SKUID, Quantity: p.Quantity, Source: p.Source,
		UserCouponID: p.UserCouponID,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
