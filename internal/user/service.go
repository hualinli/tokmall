package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"

	"github.com/lihualin/tokmall/internal/token"
	"golang.org/x/crypto/bcrypt"
)

const (
	refreshTokenTTL = 7 * 24 * 60 * 60
	accessTokenTTL  = 15 * 60
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{5,15}$`)

func IsValidUsername(name string) bool {
	return usernameRe.MatchString(name)
}

var ErrInvalidParam = errors.New("invalid param")
var ErrInvalidCredentials = errors.New("invalid credentials")
var dummyHash = []byte("$2a$10$bSPKSKJUdh257leaWepIZeXFZXWgmWxvz6UaPXUFqWCzc28m6boDm")

// 业务代码不 import hertz
type Repository interface {
	Create(ctx context.Context, u *User) error
	FindByUsername(ctx context.Context, username string) (*User, error)
	FindByEmail(ctx context.Context, email string) (*User, error)
}

type TokenManager interface {
	Sign(claims token.Claims) (string, error)
}

type Service struct {
	repo   Repository
	tokens TokenManager
}

func NewService(repo Repository, tokens TokenManager) *Service {
	return &Service{
		repo:   repo,
		tokens: tokens,
	}
}

type RegisterParams struct {
	Email    string
	Username string
	Password string
}

type LoginParams struct {
	EmailOrUsername string
	Password        string
}

type AuthResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	User         PublicUser
}

type PublicUser struct {
	UserID    int64
	Email     string
	Username  string
	Bio       string
	AvatarKey *string
}

func (s *Service) Register(ctx context.Context, p RegisterParams) (*AuthResult, error) {
	_, err := mail.ParseAddress(p.Email)
	if err != nil {
		return nil, ErrInvalidParam
	}
	username := strings.TrimSpace(p.Username)
	if !IsValidUsername(username) {
		return nil, ErrInvalidParam
	}
	// password 先不做trimspace了，先交给客户端完成非法字符校验，后续完成复杂度校验再考虑这个
	// TODO:做密码复杂度校验
	if len(p.Password) < 8 || len(p.Password) > 72 {
		// 没有用特殊的Err类型，一方面是偷懒，另一方面是希望客户端提交前就完成长度校验
		return nil, ErrInvalidParam
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	user := User{
		Email:        p.Email,
		Username:     username,
		PasswordHash: string(hash),
		Bio:          "",
	}
	err = s.repo.Create(ctx, &user)
	switch {
	case errors.Is(err, ErrEmailTaken), errors.Is(err, ErrUsernameTaken):
		return nil, err
	case err != nil:
		slog.Error("create user failed", "err", err)
		return nil, fmt.Errorf("create user: %w", err)
	}
	accessToken, refreshToken, err := s.issueToken(user.UserID)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    accessTokenTTL,
		User: PublicUser{
			UserID:    user.UserID,
			Username:  username,
			Email:     user.Email,
			Bio:       user.Bio,
			AvatarKey: nil,
		},
	}, nil
}

func (s *Service) Login(ctx context.Context, p LoginParams) (*AuthResult, error) {
	_, err := mail.ParseAddress(p.EmailOrUsername)
	var u *User
	if err != nil {
		u, err = s.repo.FindByUsername(ctx, strings.TrimSpace(p.EmailOrUsername))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))
				return nil, ErrInvalidCredentials
			}
			slog.Error("find user failed", "err", err)
			return nil, err
		}
	} else {
		u, err = s.repo.FindByEmail(ctx, p.EmailOrUsername)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))
				return nil, ErrInvalidCredentials
			}
			slog.Error("find user failed", "err", err)
			return nil, err
		}
	}
	err = bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(p.Password))
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	accessToken, refreshToken, err := s.issueToken(u.UserID)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    accessTokenTTL,
		User: PublicUser{
			UserID:    u.UserID,
			Username:  u.Username,
			Email:     u.Email,
			Bio:       u.Bio,
			AvatarKey: u.AvatarKey,
		},
	}, nil
}

func (s *Service) issueToken(userID int64) (string, string, error) {

	accessClaims := token.GenClaims(userID, accessTokenTTL, token.TypeAccess, "USER")
	accessToken, err := s.tokens.Sign(accessClaims)
	if err != nil {
		slog.Error("sign token failed", "err", err)
		return "", "", fmt.Errorf("sign token: %w", err)
	}
	refreshClaims := token.GenClaims(userID, refreshTokenTTL, token.TypeRefresh, "USER")
	refreshToken, err := s.tokens.Sign(refreshClaims)
	if err != nil {
		slog.Error("sign token failed", "err", err)
		return "", "", fmt.Errorf("sign token: %w", err)
	}
	return accessToken, refreshToken, nil
}
