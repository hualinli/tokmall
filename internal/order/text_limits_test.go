package order

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 合同 maxLength 按字符计：128 个中文字符（384 字节）的幂等键必须合法，129 个必须拒绝。
func TestIdempotencyKeyCountsCharacters(t *testing.T) {
	params := CreateParams{UserID: 1, SKUID: 1, Quantity: 1}

	params.IdempotencyKey = strings.Repeat("中", maxIdempotencyKeyLength)
	require.NoError(t, validateCreateParams(params))

	params.IdempotencyKey = strings.Repeat("中", maxIdempotencyKeyLength+1)
	require.ErrorIs(t, validateCreateParams(params), ErrInvalidParam)
}

// 支付单号 64 字符、渠道单号 128 字符：边界内的值进入仓储，超出一个字符即拒绝。
func TestPaymentIdentifiersCountCharacters(t *testing.T) {
	svc := NewService(&fakeOrderRepository{byKey: map[string]*createRecord{}})
	params := PayParams{UserID: 1, OrderID: 1, PaymentNo: "pay", ProviderTradeNo: "trade", Amount: 100}

	// fakeOrderRepository.Pay 固定返回 "not implemented"：能拿到它就说明校验已通过。
	params.PaymentNo = strings.Repeat("中", 64)
	params.ProviderTradeNo = strings.Repeat("中", 128)
	_, err := svc.Pay(context.Background(), params)
	require.False(t, errors.Is(err, ErrInvalidParam), "in-bounds identifiers must reach the repository, got %v", err)

	params.PaymentNo = strings.Repeat("中", 65)
	_, err = svc.Pay(context.Background(), params)
	require.ErrorIs(t, err, ErrInvalidParam)
}

// 退款理由与商家备注合同 maxLength 200，按字符计。
func TestRefundTextCountsCharacters(t *testing.T) {
	repo := &fakeRefundRepository{}
	service := newRefundService(t, repo)
	ctx := context.Background()

	_, err := service.RequestRefund(ctx, RequestRefundParams{UserID: 9, OrderID: 42, Reason: strings.Repeat("中", maxRefundReasonLength)})
	require.NoError(t, err)
	_, err = service.RequestRefund(ctx, RequestRefundParams{UserID: 9, OrderID: 42, Reason: strings.Repeat("中", maxRefundReasonLength+1)})
	require.ErrorIs(t, err, ErrInvalidParam)

	_, err = service.DecideRefund(ctx, DecideRefundParams{MerchantID: 7, OrderID: 42, Remark: strings.Repeat("中", maxRefundReasonLength)})
	require.NoError(t, err)
	_, err = service.DecideRefund(ctx, DecideRefundParams{MerchantID: 7, OrderID: 42, Remark: strings.Repeat("中", maxRefundReasonLength+1)})
	require.ErrorIs(t, err, ErrInvalidParam)
}
