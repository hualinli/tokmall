package order

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 售后（退款）闭环的领域模型与命令。
//
// 业务范围（与 docs/commerce-business.md 保持一致）：
//   - 只支持全额退款：订单是单 SKU 订单，退款金额恒等于订单实付金额；
//   - 可申请状态：PAID（已付款未发货）、FULFILLING（已发货待收货）；
//   - COMPLETED 之后不接受退款申请（本项目不建模 7 天无理由与退货物流）；
//   - 商家同意即视为渠道退款成功（模拟渠道，与模拟支付对称）；
//   - 未发货退款回补库存；已发货退款不回补（货已出库，退货入库不在范围）。
var (
	ErrRefundNotFound       = errors.New("refund not found")
	ErrRefundNotAllowed     = errors.New("order status does not allow refund")
	ErrRefundAlreadyDecided = errors.New("refund already decided")
	// errRefundRepositoryMissing 只在装配错误时出现：宁可在请求上返回明确错误，
	// 也不要 nil 接口解引用 panic 后靠 recovery 兜成 500。
	errRefundRepositoryMissing = errors.New("refund repository is not configured")
)

const maxRefundReasonLength = 200

type RequestRefundParams struct {
	UserID  int64
	OrderID int64
	Reason  string
}

type RequestRefundCommand struct {
	RequestRefundParams
	RefundNo    string
	RequestedAt time.Time
}

// RefundView 是 service 边界的只读结果，model 不出 service。
type RefundView struct {
	RefundID       int64        `json:"refund_id"`
	RefundNo       string       `json:"refund_no"`
	OrderID        int64        `json:"order_id"`
	OrderNo        string       `json:"order_no"`
	Amount         Amount       `json:"amount"`
	Reason         string       `json:"reason"`
	Status         RefundStatus `json:"status"`
	RestoreStock   bool         `json:"restore_stock"`
	RequestedAt    time.Time    `json:"requested_at"`
	DecidedAt      *time.Time   `json:"decided_at,omitempty"`
	DecisionRemark *string      `json:"decision_remark,omitempty"`
	RefundedAt     *time.Time   `json:"refunded_at,omitempty"`
	OrderStatus    Status       `json:"order_status"`
	Replayed       bool         `json:"replayed"`
}

type DecideRefundParams struct {
	MerchantID int64
	OrderID    int64
	Approve    bool
	Remark     string
}

type DecideRefundCommand struct {
	DecideRefundParams
	DecidedBy int64
	DecidedAt time.Time
}

// refundRecord 是 repository → service 的内部载体。
type refundRecord struct {
	Refund   Refund
	Order    Order
	Replayed bool
}

// RefundRepository 定义在消费侧，与 Repository 分开，便于 fake 只实现需要的部分。
type RefundRepository interface {
	RequestRefund(ctx context.Context, cmd RequestRefundCommand) (*refundRecord, error)
	DecideRefund(ctx context.Context, cmd DecideRefundCommand) (*refundRecord, error)
	GetRefund(ctx context.Context, actorID, orderID int64, merchant bool) (*refundRecord, error)
}

func (s *Service) RequestRefund(ctx context.Context, params RequestRefundParams) (*RefundView, error) {
	params.Reason = strings.TrimSpace(params.Reason)
	if params.UserID <= 0 || params.OrderID <= 0 || params.Reason == "" || len(params.Reason) > maxRefundReasonLength {
		return nil, ErrInvalidParam
	}
	if s.refunds == nil {
		return nil, errRefundRepositoryMissing
	}
	refundNo, err := s.newRefundNo()
	if err != nil {
		return nil, err
	}
	record, err := s.refunds.RequestRefund(ctx, RequestRefundCommand{
		RequestRefundParams: params,
		RefundNo:            refundNo,
		RequestedAt:         s.now(),
	})
	if err != nil {
		return nil, err
	}
	return mapRefundView(record), nil
}

func (s *Service) DecideRefund(ctx context.Context, params DecideRefundParams) (*RefundView, error) {
	params.Remark = strings.TrimSpace(params.Remark)
	if params.MerchantID <= 0 || params.OrderID <= 0 || len(params.Remark) > maxRefundReasonLength {
		return nil, ErrInvalidParam
	}
	if s.refunds == nil {
		return nil, errRefundRepositoryMissing
	}
	// 同意退款必须有明确裁决人；拒绝允许不写备注，但备注长度受限。
	record, err := s.refunds.DecideRefund(ctx, DecideRefundCommand{
		DecideRefundParams: params,
		DecidedBy:          params.MerchantID,
		DecidedAt:          s.now(),
	})
	if err != nil {
		return nil, err
	}
	return mapRefundView(record), nil
}

func (s *Service) GetRefund(ctx context.Context, actorID, orderID int64, merchant bool) (*RefundView, error) {
	if actorID <= 0 || orderID <= 0 {
		return nil, ErrInvalidParam
	}
	if s.refunds == nil {
		return nil, errRefundRepositoryMissing
	}
	record, err := s.refunds.GetRefund(ctx, actorID, orderID, merchant)
	if err != nil {
		return nil, err
	}
	return mapRefundView(record), nil
}

func mapRefundView(record *refundRecord) *RefundView {
	r := record.Refund
	return &RefundView{
		RefundID: r.ID, RefundNo: r.RefundNo, OrderID: r.OrderID, OrderNo: record.Order.OrderNo,
		Amount: r.Amount, Reason: r.Reason, Status: r.Status,
		RestoreStock: record.Order.RefundFromStatus != nil && *record.Order.RefundFromStatus == StatusPaid,
		RequestedAt:  r.RequestedAt, DecidedAt: r.DecidedAt, DecisionRemark: r.DecisionRemark,
		RefundedAt: r.RefundedAt, OrderStatus: record.Order.Status, Replayed: record.Replayed,
	}
}

func defaultRefundNo() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return "RF" + id.String(), nil
}

// refundableFrom 描述哪些订单状态允许发起退款申请。
// 这里是唯一一份"可申请状态"定义，映射到订单状态机守卫的由它派生。
func refundableFrom(status Status) bool {
	return status == StatusPaid || status == StatusFulfilling
}
