package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	FindByID(ctx context.Context, id int64) (*User, error)
	FindByUsername(ctx context.Context, username string) (*User, error)
	FindByEmail(ctx context.Context, email string) (*User, error)
}

type TokenManager interface {
	Sign(claims token.Claims) (string, error)
	Verify(tokenString string) (*token.Claims, error)
}

type Service struct {
	repo      Repository
	tokens    TokenManager
	blacklist RefreshTokenBlacklist
}

func NewService(repo Repository, tokens TokenManager, blacklist RefreshTokenBlacklist) *Service {
	return &Service{
		repo:      repo,
		tokens:    tokens,
		blacklist: blacklist,
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
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	ExpiresIn    int64      `json:"expires_in"`
	User         PublicUser `json:"user"`
}

type PublicUser struct {
	UserID    int64   `json:"user_id"`
	Email     string  `json:"email"`
	Username  string  `json:"username"`
	Bio       string  `json:"bio"`
	AvatarKey *string `json:"avatar_key"`
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

func (s *Service) GetProfile(ctx context.Context, userID int64) (*PublicUser, error) {
	u, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		slog.Error("get profile failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return &PublicUser{
		UserID:    u.UserID,
		Username:  u.Username,
		Email:     u.Email,
		Bio:       u.Bio,
		AvatarKey: u.AvatarKey,
	}, nil
}

// Refresh 用 refresh token 换取新 token 对（rotation）：
// 新 token 签发前先把旧 jti 拉黑——两步骤之间崩溃的后果是
// 旧 token 作废、用户需要重新登录，而不会出现新旧两个 token 同时有效
// （不产生 token 泛滥）。这是无状态 JWT 下无法用事务时对顺序的取舍。
// 已知限制：两个并发请求同时用同一 refresh token 刷新时，可能都通过
// 黑名单检查而各自签发新 token（短暂双活）。工业级解法是 per-user
// 当前 jti 白名单（只认最新一个），列为 ADR 演进项。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*AuthResult, error) {
	claims, err := s.tokens.Verify(refreshToken)
	if err != nil || !claims.IsRefresh() {
		return nil, ErrInvalidCredentials
	}
	remaining := claims.ExpiresAt.Time.Sub(time.Now())
	if remaining <= 0 {
		return nil, ErrInvalidCredentials
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	// 用户已删除（软删）时 token 必须作废，否则删号后旧 refresh token
	// 还能无限续签新 access token。
	if _, err := s.repo.FindByID(ctx, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		slog.Error("find user on refresh failed", "err", err)
		return nil, fmt.Errorf("find user on refresh: %w", err)
	}
	blocked, err := s.blacklist.IsBlocked(ctx, claims.ID)
	if err != nil {
		slog.Error("check refresh blacklist failed", "err", err)
		return nil, fmt.Errorf("check refresh blacklist: %w", err)
	}
	if blocked {
		return nil, ErrInvalidCredentials
	}
	if err := s.blacklist.Block(ctx, claims.ID, remaining); err != nil {
		slog.Error("block refresh token failed", "err", err)
		return nil, fmt.Errorf("block refresh token: %w", err)
	}
	accessToken, refreshTokenNew, err := s.issueToken(userID)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		AccessToken:  accessToken,
		RefreshToken: refreshTokenNew,
		ExpiresIn:    accessTokenTTL,
	}, nil
}

// Logout 拉黑 refresh token。幂等：token 已失效/已过期/类型不符
// 一律视为"已经登出"，返回成功；只有 Redis 故障才算错误。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	claims, err := s.tokens.Verify(refreshToken)
	if err != nil || !claims.IsRefresh() {
		return nil
	}
	remaining := claims.ExpiresAt.Time.Sub(time.Now())
	if remaining <= 0 {
		return nil
	}
	if err := s.blacklist.Block(ctx, claims.ID, remaining); err != nil {
		slog.Error("block refresh token failed", "err", err)
		return fmt.Errorf("block refresh token: %w", err)
	}
	return nil
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
