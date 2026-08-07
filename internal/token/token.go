package token

import (
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	TypeAccess  = "access"
	TypeRefresh = "refresh"
)

type Manager struct {
	secret []byte
}

type Claims struct {
	jwt.RegisteredClaims
	Type string
	Role string
}

func NewManager(secret []byte) *Manager {
	return &Manager{secret: secret}
}

// GenClaims 生成带 jti 的 claims：jti 是 token 唯一 ID，
// refresh token 撤销（黑名单）以它为键。
func GenClaims(userID, expiresIn int64, t, role string) Claims {
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   strconv.Itoa(int(userID)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Second * time.Duration(expiresIn))),
		},
		Type: t,
		Role: role,
	}
}
func (m *Manager) Sign(claims Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(m.secret)
}
func (m *Manager) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, errors.New("unexpected signing method")
			}
			return m.secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	switch claims.Type {
	case TypeAccess, TypeRefresh:
		return claims, nil
	default:
		return nil, errors.New("unexpected token type")
	}
}

func (c *Claims) IsAccess() bool {
	return c.Type == TypeAccess
}

func (c *Claims) IsRefresh() bool {
	return c.Type == TypeRefresh
}
