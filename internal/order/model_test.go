package order

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrderModelTableNames(t *testing.T) {
	require.Equal(t, "orders", (Order{}).TableName())
	require.Equal(t, "order_items", (OrderItem{}).TableName())
	require.Equal(t, "payments", (Payment{}).TableName())
	require.Equal(t, "video_products", (VideoProduct{}).TableName())
}

func TestOrderAndPaymentStatusValuesMatchDatabaseContract(t *testing.T) {
	require.ElementsMatch(t, []Status{
		StatusPendingPayment,
		StatusPaid,
		StatusFulfilling,
		StatusCompleted,
		StatusCanceled,
		StatusExpired,
	}, []Status{"PENDING_PAYMENT", "PAID", "FULFILLING", "COMPLETED", "CANCELED", "EXPIRED"})

	require.ElementsMatch(t, []PaymentStatus{
		PaymentStatusPending,
		PaymentStatusSuccess,
		PaymentStatusFailed,
	}, []PaymentStatus{"PENDING", "SUCCESS", "FAILED"})
}
