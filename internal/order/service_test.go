package order

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeOrderRepository struct {
	mu       sync.Mutex
	byKey    map[string]*createRecord
	commands []CreateCommand
	created  int
	err      error
}

func (f *fakeOrderRepository) Create(_ context.Context, cmd CreateCommand) (*createRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.commands = append(f.commands, cmd)
	key := strconv.FormatInt(cmd.UserID, 10) + ":" + cmd.IdempotencyKey
	if existing := f.byKey[key]; existing != nil {
		if existing.Order.RequestFingerprint != cmd.RequestFingerprint {
			return nil, ErrIdempotencyConflict
		}
		copy := *existing
		copy.Replayed = true
		return &copy, nil
	}
	result := &createRecord{Order: Order{
		ID: int64(f.created + 1), OrderNo: cmd.OrderNo, UserID: cmd.UserID,
		IdempotencyKey: cmd.IdempotencyKey, RequestFingerprint: cmd.RequestFingerprint,
		ExpiresAt: cmd.ExpiresAt,
	}}
	f.created++
	f.byKey[key] = result
	return result, nil
}

func (f *fakeOrderRepository) Pay(context.Context, PayCommand) (*paymentRecord, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeOrderRepository) Cancel(context.Context, int64, int64, time.Time) (*cancelRecord, error) {
	return nil, errors.New("not implemented")
}

type fakeTransitionRepository struct {
	payRecord     *paymentRecord
	cancelRecord  *cancelRecord
	err           error
	payCommand    PayCommand
	cancelUserID  int64
	cancelOrderID int64
	canceledAt    time.Time
}

func (f *fakeTransitionRepository) Create(context.Context, CreateCommand) (*createRecord, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeTransitionRepository) Pay(_ context.Context, cmd PayCommand) (*paymentRecord, error) {
	f.payCommand = cmd
	return f.payRecord, f.err
}

func (f *fakeTransitionRepository) Cancel(_ context.Context, userID, orderID int64, at time.Time) (*cancelRecord, error) {
	f.cancelUserID, f.cancelOrderID, f.canceledAt = userID, orderID, at
	return f.cancelRecord, f.err
}

func newTestOrderService(repo Repository) *Service {
	s := NewService(repo)
	s.now = func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }
	s.newOrderNo = func() (string, error) { return "0199-order-v7", nil }
	return s
}

func TestServiceCreateBuildsCanonicalCommand(t *testing.T) {
	repo := &fakeOrderRepository{byKey: make(map[string]*createRecord)}
	svc := newTestOrderService(repo)
	source := &Source{VideoID: 7, AuthorID: 8, ProductID: 9}

	result, err := svc.Create(context.Background(), CreateParams{
		UserID: 3, SKUID: 11, Quantity: 2, IdempotencyKey: "  retry-key  ", Source: source,
	})
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Len(t, repo.commands, 1)
	cmd := repo.commands[0]
	require.Equal(t, "retry-key", cmd.IdempotencyKey)
	require.Equal(t, "0199-order-v7", cmd.OrderNo)
	require.Len(t, cmd.RequestFingerprint, 64)
	require.Equal(t, time.Date(2026, 9, 16, 12, 30, 0, 0, time.UTC), cmd.ExpiresAt)
}

func TestServiceCreateSameKeySameRequestReplays(t *testing.T) {
	repo := &fakeOrderRepository{byKey: make(map[string]*createRecord)}
	svc := newTestOrderService(repo)
	p := CreateParams{UserID: 3, SKUID: 11, Quantity: 2, IdempotencyKey: "same"}

	first, err := svc.Create(context.Background(), p)
	require.NoError(t, err)
	second, err := svc.Create(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, first.OrderID, second.OrderID)
	require.True(t, second.Replayed)
}

func TestServiceCreateSameKeyDifferentPayloadConflicts(t *testing.T) {
	repo := &fakeOrderRepository{byKey: make(map[string]*createRecord)}
	svc := newTestOrderService(repo)

	_, err := svc.Create(context.Background(), CreateParams{UserID: 3, SKUID: 11, Quantity: 1, IdempotencyKey: "same"})
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), CreateParams{UserID: 3, SKUID: 11, Quantity: 2, IdempotencyKey: "same"})
	require.ErrorIs(t, err, ErrIdempotencyConflict)
}

func TestServiceCreateConcurrentSameRequestCreatesOnce(t *testing.T) {
	repo := &fakeOrderRepository{byKey: make(map[string]*createRecord)}
	svc := newTestOrderService(repo)
	p := CreateParams{UserID: 3, SKUID: 11, Quantity: 2, IdempotencyKey: "same"}

	const workers = 50
	results := make(chan *CreateResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Create(context.Background(), p)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	for result := range results {
		require.NotNil(t, result)
		require.Equal(t, int64(1), result.OrderID)
	}
	require.Equal(t, 1, repo.created)
}

func TestServiceCreateScopesSameKeyByUser(t *testing.T) {
	repo := &fakeOrderRepository{byKey: make(map[string]*createRecord)}
	svc := newTestOrderService(repo)
	first, err := svc.Create(context.Background(), CreateParams{UserID: 3, SKUID: 11, Quantity: 1, IdempotencyKey: "same"})
	require.NoError(t, err)
	second, err := svc.Create(context.Background(), CreateParams{UserID: 4, SKUID: 11, Quantity: 1, IdempotencyKey: "same"})
	require.NoError(t, err)
	require.NotEqual(t, first.OrderID, second.OrderID)
	require.Equal(t, 2, repo.created)
}

func TestServiceCreateValidatesBoundary(t *testing.T) {
	tests := []CreateParams{
		{},
		{UserID: 1, SKUID: 2, Quantity: 0, IdempotencyKey: "k"},
		{UserID: 1, SKUID: 2, Quantity: maxOrderQuantity + 1, IdempotencyKey: "k"},
		{UserID: 1, SKUID: 2, Quantity: 1, IdempotencyKey: string(make([]byte, maxIdempotencyKeyLength+1))},
		{UserID: 1, SKUID: 2, Quantity: 1, IdempotencyKey: "k", Source: &Source{VideoID: 1, AuthorID: 0, ProductID: 3}},
	}
	for _, params := range tests {
		svc := newTestOrderService(&fakeOrderRepository{byKey: make(map[string]*createRecord)})
		_, err := svc.Create(context.Background(), params)
		require.ErrorIs(t, err, ErrInvalidParam)
	}
}

func TestServiceCreatePropagatesBusinessErrors(t *testing.T) {
	svc := newTestOrderService(&fakeOrderRepository{err: ErrInsufficientStock})
	_, err := svc.Create(context.Background(), CreateParams{UserID: 1, SKUID: 2, Quantity: 1, IdempotencyKey: "k"})
	require.True(t, errors.Is(err, ErrInsufficientStock))
}

func TestRequestFingerprintIncludesAllBusinessFieldsButNotKey(t *testing.T) {
	base := CreateParams{UserID: 1, SKUID: 2, Quantity: 3, IdempotencyKey: "a"}
	a, err := requestFingerprint(base)
	require.NoError(t, err)
	base.IdempotencyKey = "b"
	b, err := requestFingerprint(base)
	require.NoError(t, err)
	require.Equal(t, a, b)

	base.Quantity++
	c, err := requestFingerprint(base)
	require.NoError(t, err)
	require.NotEqual(t, a, c)
	base.Quantity--
	base.Source = &Source{VideoID: 4, AuthorID: 5, ProductID: 6}
	d, err := requestFingerprint(base)
	require.NoError(t, err)
	require.NotEqual(t, a, d)
}

func TestMultiplyAmountRejectsOverflow(t *testing.T) {
	amount, ok := multiplyAmount(500, 3)
	require.True(t, ok)
	require.Equal(t, Amount(1500), amount)
	_, ok = multiplyAmount(Amount(math.MaxInt64), 2)
	require.False(t, ok)
}

func TestServicePayCanonicalizesAndMapsResult(t *testing.T) {
	at := time.Date(2026, 9, 16, 12, 1, 0, 0, time.UTC)
	provider := "provider-1"
	repo := &fakeTransitionRepository{payRecord: &paymentRecord{
		Order: Order{ID: 8, OrderNo: "order-8", Status: StatusPaid},
		Payment: Payment{PaymentNo: "payment-1", OrderID: 8, Amount: 1999,
			Status: PaymentStatusSuccess, ProviderTradeNo: &provider, PaidAt: &at},
	}}
	svc := newTestOrderService(repo)
	result, err := svc.Pay(context.Background(), PayParams{
		UserID: 7, OrderID: 8, PaymentNo: " payment-1 ", ProviderTradeNo: " provider-1 ", Amount: 1999,
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), repo.payCommand.UserID)
	require.Equal(t, int64(8), repo.payCommand.OrderID)
	require.Equal(t, "payment-1", repo.payCommand.PaymentNo)
	require.Equal(t, "provider-1", repo.payCommand.ProviderTradeNo)
	require.Equal(t, time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC), repo.payCommand.PaidAt)
	require.Equal(t, StatusPaid, result.Status)
	require.Equal(t, provider, result.ProviderTradeNo)
}

func TestServicePayValidatesAndPropagatesBusinessErrors(t *testing.T) {
	svc := newTestOrderService(&fakeTransitionRepository{})
	for _, params := range []PayParams{{}, {UserID: 1, OrderID: 2, PaymentNo: "p", ProviderTradeNo: "t", Amount: 0}} {
		_, err := svc.Pay(context.Background(), params)
		require.ErrorIs(t, err, ErrInvalidParam)
	}
	svc = newTestOrderService(&fakeTransitionRepository{err: ErrPaymentAmountMismatch})
	_, err := svc.Pay(context.Background(), PayParams{UserID: 1, OrderID: 2, PaymentNo: "p", ProviderTradeNo: "t", Amount: 1})
	require.ErrorIs(t, err, ErrPaymentAmountMismatch)
}

func TestOrderStatusTransitions(t *testing.T) {
	require.True(t, canOrderTransition(StatusPendingPayment, StatusPaid))
	require.True(t, canOrderTransition(StatusPendingPayment, StatusCanceled))
	require.True(t, canOrderTransition(StatusPendingPayment, StatusExpired))
	require.False(t, canOrderTransition(StatusCanceled, StatusPaid))
	require.False(t, canOrderTransition(StatusPaid, StatusCanceled))
}

func TestServiceCancelPassesAuthenticatedOwnerAndMapsReplay(t *testing.T) {
	at := time.Date(2026, 9, 16, 12, 2, 0, 0, time.UTC)
	repo := &fakeTransitionRepository{cancelRecord: &cancelRecord{
		Order: Order{ID: 9, OrderNo: "order-9", Status: StatusCanceled, CanceledAt: &at}, Replayed: true,
	}}
	svc := newTestOrderService(repo)
	result, err := svc.Cancel(context.Background(), CancelParams{UserID: 7, OrderID: 9})
	require.NoError(t, err)
	require.Equal(t, int64(7), repo.cancelUserID)
	require.Equal(t, int64(9), repo.cancelOrderID)
	require.True(t, result.Replayed)
	require.Equal(t, StatusCanceled, result.Status)
}
