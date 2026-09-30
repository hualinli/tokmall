package order

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRefundRepository 只实现退款命令，用来验证 service 层的参数校验、
// 状态判定和视图映射；真实并发语义由 postgres_integration_test.go 证明。
type fakeRefundRepository struct {
	requested  []RequestRefundCommand
	decided    []DecideRefundCommand
	requestRec *refundRecord
	decideRec  *refundRecord
	getRec     *refundRecord
	err        error
}

func (f *fakeRefundRepository) RequestRefund(_ context.Context, cmd RequestRefundCommand) (*refundRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.requested = append(f.requested, cmd)
	if f.requestRec != nil {
		return f.requestRec, nil
	}
	from := StatusPaid
	return &refundRecord{
		Order:  Order{ID: cmd.OrderID, OrderNo: "order-1", Status: StatusRefundRequested, RefundFromStatus: &from},
		Refund: Refund{ID: 7, RefundNo: cmd.RefundNo, OrderID: cmd.OrderID, Amount: 1200, Reason: cmd.Reason, Status: RefundStatusRequested, RequestedAt: cmd.RequestedAt},
	}, nil
}

func (f *fakeRefundRepository) DecideRefund(_ context.Context, cmd DecideRefundCommand) (*refundRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.decided = append(f.decided, cmd)
	if f.decideRec != nil {
		return f.decideRec, nil
	}
	at := cmd.DecidedAt
	status := RefundStatusSucceeded
	orderStatus := StatusRefunded
	if !cmd.Approve {
		status = RefundStatusRejected
		orderStatus = StatusPaid
	}
	record := &refundRecord{
		Order:  Order{ID: cmd.OrderID, OrderNo: "order-1", Status: orderStatus},
		Refund: Refund{ID: 7, OrderID: cmd.OrderID, Amount: 1200, Status: status, DecidedAt: &at},
	}
	if cmd.Approve {
		record.Refund.RefundedAt = &at
		record.Order.RefundedAt = &at
	}
	return record, nil
}

func (f *fakeRefundRepository) GetRefund(context.Context, int64, int64, bool) (*refundRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.getRec == nil {
		return nil, ErrRefundNotFound
	}
	return f.getRec, nil
}

func newRefundService(t *testing.T, repo RefundRepository) *Service {
	t.Helper()
	service := NewService(&fakeOrderRepository{byKey: map[string]*createRecord{}})
	service.SetRefundRepository(repo)
	service.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	return service
}

func TestRequestRefundValidatesInput(t *testing.T) {
	repo := &fakeRefundRepository{}
	service := newRefundService(t, repo)

	cases := map[string]RequestRefundParams{
		"missing user":    {OrderID: 1, Reason: "不想要了"},
		"missing order":   {UserID: 1, Reason: "不想要了"},
		"blank reason":    {UserID: 1, OrderID: 1, Reason: "   "},
		"reason too long": {UserID: 1, OrderID: 1, Reason: strings.Repeat("a", maxRefundReasonLength+1)},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := service.RequestRefund(context.Background(), params)
			require.ErrorIs(t, err, ErrInvalidParam)
		})
	}
	require.Empty(t, repo.requested, "非法参数不应触达仓储层")
}

func TestRequestRefundTrimsReasonAndMapsView(t *testing.T) {
	repo := &fakeRefundRepository{}
	service := newRefundService(t, repo)

	view, err := service.RequestRefund(context.Background(), RequestRefundParams{
		UserID: 9, OrderID: 42, Reason: "  尺码不合适  ",
	})
	require.NoError(t, err)
	require.Equal(t, "尺码不合适", repo.requested[0].Reason)
	require.Equal(t, "尺码不合适", view.Reason)
	require.Equal(t, int64(42), view.OrderID)
	require.Equal(t, RefundStatusRequested, view.Status)
	require.Equal(t, StatusRefundRequested, view.OrderStatus)
	require.True(t, view.RestoreStock, "PAID 阶段退款必须标记为回补库存")
	require.False(t, view.Replayed)
	require.NotEmpty(t, repo.requested[0].RefundNo, "退款单号必须由服务端生成")
}

func TestRequestRefundReportsReplay(t *testing.T) {
	from := StatusFulfilling
	repo := &fakeRefundRepository{requestRec: &refundRecord{
		Order:    Order{ID: 5, OrderNo: "order-5", Status: StatusRefundRequested, RefundFromStatus: &from},
		Refund:   Refund{ID: 3, OrderID: 5, Amount: 999, Status: RefundStatusRequested},
		Replayed: true,
	}}
	service := newRefundService(t, repo)

	view, err := service.RequestRefund(context.Background(), RequestRefundParams{UserID: 1, OrderID: 5, Reason: "重复申请"})
	require.NoError(t, err)
	require.True(t, view.Replayed)
	require.False(t, view.RestoreStock, "FULFILLING 阶段退款不回补库存")
}

func TestDecideRefundRequiresMerchantAndMapsApprove(t *testing.T) {
	repo := &fakeRefundRepository{}
	service := newRefundService(t, repo)

	_, err := service.DecideRefund(context.Background(), DecideRefundParams{OrderID: 1, Approve: true})
	require.ErrorIs(t, err, ErrInvalidParam)

	view, err := service.DecideRefund(context.Background(), DecideRefundParams{
		MerchantID: 3, OrderID: 1, Approve: true, Remark: "  同意退款  ",
	})
	require.NoError(t, err)
	require.Equal(t, int64(3), repo.decided[0].DecidedBy, "裁决人必须是认证上下文里的商家 ID")
	require.Equal(t, "同意退款", repo.decided[0].Remark)
	require.Equal(t, RefundStatusSucceeded, view.Status)
	require.Equal(t, StatusRefunded, view.OrderStatus)
	require.NotNil(t, view.RefundedAt)
}

func TestDecideRefundRejectKeepsNoRefundedAt(t *testing.T) {
	repo := &fakeRefundRepository{}
	service := newRefundService(t, repo)

	view, err := service.DecideRefund(context.Background(), DecideRefundParams{MerchantID: 3, OrderID: 1})
	require.NoError(t, err)
	require.Equal(t, RefundStatusRejected, view.Status)
	require.Equal(t, StatusPaid, view.OrderStatus, "拒绝后订单退回申请前状态")
	require.Nil(t, view.RefundedAt)
}

func TestDecideRefundPropagatesRepositorySentinel(t *testing.T) {
	repo := &fakeRefundRepository{err: ErrRefundAlreadyDecided}
	service := newRefundService(t, repo)

	_, err := service.DecideRefund(context.Background(), DecideRefundParams{MerchantID: 3, OrderID: 1, Approve: true})
	require.ErrorIs(t, err, ErrRefundAlreadyDecided)
}

func TestGetRefundRequiresPositiveIDs(t *testing.T) {
	service := newRefundService(t, &fakeRefundRepository{})
	_, err := service.GetRefund(context.Background(), 0, 0, false)
	require.ErrorIs(t, err, ErrInvalidParam)
}

func TestRefundCommandsFailFastWithoutRepository(t *testing.T) {
	// 未注入售后仓储时必须返回明确错误，而不是 nil 解引用 panic。
	service := NewService(&fakeOrderRepository{byKey: map[string]*createRecord{}})
	_, err := service.RequestRefund(context.Background(), RequestRefundParams{UserID: 1, OrderID: 1, Reason: "x"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidParam)
}

func TestRefundableStatesMatchStateMachine(t *testing.T) {
	require.True(t, refundableFrom(StatusPaid))
	require.True(t, refundableFrom(StatusFulfilling))
	for _, status := range []Status{StatusPendingPayment, StatusCompleted, StatusCanceled, StatusExpired, StatusRefundRequested, StatusRefunded} {
		require.False(t, refundableFrom(status), "%s 不应允许发起退款", status)
	}
}

func TestRefundTransitionsAreDeclared(t *testing.T) {
	require.True(t, canOrderTransition(StatusPaid, StatusRefundRequested))
	require.True(t, canOrderTransition(StatusFulfilling, StatusRefundRequested))
	require.True(t, canOrderTransition(StatusRefundRequested, StatusRefunded))
	require.True(t, canOrderTransition(StatusRefundRequested, StatusPaid), "拒绝退款退回 PAID")
	require.True(t, canOrderTransition(StatusRefundRequested, StatusFulfilling), "拒绝退款退回 FULFILLING")
	require.False(t, canOrderTransition(StatusRefundRequested, StatusCompleted), "退款中不能确认收货")
	require.False(t, canOrderTransition(StatusRefunded, StatusFulfilling), "已退款不能继续发货")
	require.False(t, canOrderTransition(StatusPaid, StatusCanceled), "已支付不能直接取消，必须先走退款")
}

// TestFulfillmentTransitionsAreNarrowerThanStateGraph 锁住这次真实 PG 测试抓到的缺陷：
// 状态图里的"拒绝退款退回 FULFILLING"不能被履约命令当成发货出口。
func TestFulfillmentTransitionsAreNarrowerThanStateGraph(t *testing.T) {
	require.False(t, canFulfillTransition(StatusRefundRequested, StatusFulfilling))
	require.False(t, canFulfillTransition(StatusRefundRequested, StatusCompleted))
	require.False(t, canFulfillTransition(StatusRefunded, StatusFulfilling))
	require.True(t, canFulfillTransition(StatusPaid, StatusFulfilling))
	require.True(t, canFulfillTransition(StatusFulfilling, StatusCompleted))

	// 履约命令允许的每条边都必须存在于完整状态图，避免两张表各自漂移。
	for from, targets := range fulfillmentTransitions {
		for to := range targets {
			require.True(t, canOrderTransition(from, to), "履约边 %s→%s 必须在完整状态图中存在", from, to)
		}
	}
}

func TestRefundServiceNilRepositoryGuard(t *testing.T) {
	service := NewService(&fakeOrderRepository{byKey: map[string]*createRecord{}})
	_, err := service.GetRefund(context.Background(), 1, 1, false)
	require.Error(t, err)
	require.True(t, errors.Is(err, errRefundRepositoryMissing))
}
