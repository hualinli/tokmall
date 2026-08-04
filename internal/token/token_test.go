package token

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

var testSecret = []byte("test-secret-not-for-production")

func accessClaims(userID string) Claims {
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
		Type: TypeAccess,
		Role: "USER",
	}
}

func TestSignAndVerify(t *testing.T) {
	manager := NewManager(testSecret)
	claims := accessClaims("test_user")
	tokenStr, err := manager.Sign(claims)
	require.NoError(t, err)
	require.NotEmpty(t, tokenStr)
	parsedClaim, err := manager.Verify(tokenStr)
	require.NoError(t, err)
	require.NotNil(t, parsedClaim)

	require.Equal(t, "test_user", parsedClaim.Subject)
	require.Equal(t, "USER", parsedClaim.Role)
	require.True(t, parsedClaim.IsAccess())
	require.Equal(t, TypeAccess, parsedClaim.Type)
}

func TestVerifyTamperedToken(t *testing.T) {
	manager := NewManager(testSecret)
	claims := accessClaims("test_user")
	tokenStr, err := manager.Sign(claims)
	require.NoError(t, err)
	require.NotEmpty(t, tokenStr)

	tokenByte := []byte(tokenStr)
	tokenByte[10] = 'n'
	tokenStr = string(tokenByte)
	parsedClaim, err := manager.Verify(tokenStr)
	require.Error(t, err)
	require.Nil(t, parsedClaim)
}

func TestVerifyInvalidType(t *testing.T) {
	manager := NewManager(testSecret)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "tt",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
		Type: "bogus",
		Role: "USER",
	}
	tokenStr, err := manager.Sign(claims)
	require.NoError(t, err)
	require.NotEmpty(t, tokenStr)

	parsedClaim, err := manager.Verify(tokenStr)
	require.Error(t, err)
	require.Nil(t, parsedClaim)
}

func TestRefreshTokenIsRefresh(t *testing.T) {
	manager := NewManager(testSecret)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "tt",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
		Type: TypeRefresh,
		Role: "USER",
	}
	tokenStr, err := manager.Sign(claims)
	require.NoError(t, err)
	require.NotEmpty(t, tokenStr)

	parsedClaim, err := manager.Verify(tokenStr)
	require.NoError(t, err)
	require.NotNil(t, parsedClaim)

	require.Equal(t, "tt", parsedClaim.Subject)
	require.Equal(t, "USER", parsedClaim.Role)
	require.True(t, parsedClaim.IsRefresh())
	require.Equal(t, TypeRefresh, parsedClaim.Type)
}
