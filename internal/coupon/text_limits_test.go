package coupon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 合同 maxLength 100 按字符计：100 个中文字符（300 字节）必须合法，101 个必须拒绝。
func TestCouponTitleCountsCharacters(t *testing.T) {
	svc := newTestService(newFakeRepository())
	params := CreateCouponParams{
		MerchantID: 1, Title: strings.Repeat("中", maxTitleLength), ThresholdAmount: 10000, DiscountAmount: 2000,
		TotalQuantity: 100, PerUserLimit: 1, ValidFrom: time.Now(), ValidTo: time.Now().Add(time.Hour),
	}
	_, err := svc.CreateCoupon(context.Background(), params)
	require.NoError(t, err)

	params.Title = strings.Repeat("中", maxTitleLength+1)
	_, err = svc.CreateCoupon(context.Background(), params)
	require.ErrorIs(t, err, ErrInvalidParam)
}
