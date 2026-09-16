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
}

type CreateCommand struct {
	CreateParams
	OrderNo            string
	RequestFingerprint string
	ExpiresAt          time.Time
}

type CreateResult struct {
	OrderID     int64
	OrderNo     string
	Status      Status
	TotalAmount Amount
	ExpiresAt   time.Time
	Item        ItemView
	Replayed    bool
}

type ItemView struct {
	ProductID    int64
	SKUID        int64
	MerchantID   int64
	ProductTitle string
	SKUSpec      string
	UnitPrice    Amount
	Quantity     int32
	LineAmount   Amount
}

// createRecord 是 repository 到 service 的内部数据载体，持久化 model
// 在 service 边界转换为只读结果，后续 handler 不得直接序列化 model。
type createRecord struct {
	Order    Order
	Item     OrderItem
	Replayed bool
}

// Repository 定义在消费侧；GormRepository 将整个创建流程包在一个事务中。
type Repository interface {
	Create(ctx context.Context, cmd CreateCommand) (*createRecord, error)
}

type Service struct {
	repo       Repository
	now        func() time.Time
	newOrderNo func() (string, error)
	paymentTTL time.Duration
}

func NewService(repo Repository) *Service {
	return &Service{
		repo: repo,
		now:  time.Now,
		newOrderNo: func() (string, error) {
			id, err := uuid.NewV7()
			return id.String(), err
		},
		paymentTTL: defaultPaymentTTL,
	}
}

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
		TotalAmount: record.Order.TotalAmount, ExpiresAt: record.Order.ExpiresAt,
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
	return nil
}

type fingerprintPayload struct {
	UserID   int64   `json:"user_id"`
	SKUID    int64   `json:"sku_id"`
	Quantity int32   `json:"quantity"`
	Source   *Source `json:"source"`
}

func requestFingerprint(p CreateParams) (string, error) {
	payload, err := json.Marshal(fingerprintPayload{
		UserID: p.UserID, SKUID: p.SKUID, Quantity: p.Quantity, Source: p.Source,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
