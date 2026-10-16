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

	"golang.org/x/crypto/bcrypt"

	"github.com/lihualin/tokmall/internal/token"
)

const (
	refreshTokenTTL = 7 * 24 * 60 * 60
	accessTokenTTL  = 15 * 60
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{5,15}$`)

func IsValidUsername(name string) bool {
	return usernameRe.MatchString(name)
}

// maxEmailLength 是 RFC 5321 对邮件地址的长度上限，也与 users.email 的
// VARCHAR(255) 列宽对齐：校验层放行的值必须一定能写进数据库，
// 否则"参数非法"会以 500 的形式暴露给调用者。
const maxEmailLength = 254

// NormalizeEmail 校验并规范化邮箱：解析出纯地址（丢弃显示名与尖括号）、
// 去空白、统一小写。注册与登录必须共用它，否则 Alice@x.com 与 alice@x.com
// 会变成两个账号（绕过唯一约束），或注册后无法登录。
func NormalizeEmail(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", ErrInvalidParam
	}
	email := strings.ToLower(strings.TrimSpace(addr.Address))
	if email == "" || len(email) > maxEmailLength || strings.Count(email, "@") != 1 {
		return "", ErrInvalidParam
	}
	return email, nil
}

var ErrInvalidParam = errors.New("invalid param")
var ErrInvalidCredentials = errors.New("invalid credentials")
var ErrInvalidImageKey = errors.New("image key invalid")
var dummyHash = []byte("$2a$10$bSPKSKJUdh257leaWepIZeXFZXWgmWxvz6UaPXUFqWCzc28m6boDm")

// 业务代码不 import hertz
type Repository interface {
	Create(ctx context.Context, u *User) error
	FindByID(ctx context.Context, id int64) (*User, error)
	FindByUsername(ctx context.Context, username string) (*User, error)
	FindByEmail(ctx context.Context, email string) (*User, error)
	FollowCounts(ctx context.Context, userID int64) (following, followers int64, err error)
	IsFollowing(ctx context.Context, followerID, followeeID int64) (bool, error)
	UpdateProfile(ctx context.Context, userID int64, bio *string, avatarKey *string) error
}

type TokenManager interface {
	Sign(claims token.Claims) (string, error)
	Verify(tokenString string) (*token.Claims, error)
}

type Service struct {
	repo           Repository
	tokens         TokenManager
	blacklist      RefreshTokenBlacklist
	avatar         AvatarURLProvider
	imageValidator ImageUploadValidator
}

func NewService(repo Repository, tokens TokenManager, blacklist RefreshTokenBlacklist, avatar AvatarURLProvider, imageValidator ImageUploadValidator) *Service {
	return &Service{
		repo:           repo,
		tokens:         tokens,
		blacklist:      blacklist,
		avatar:         avatar,
		imageValidator: imageValidator,
	}
}

// AvatarURLProvider 消费侧接口：把 avatar_key 转成可显示的预签名 URL
// （长有效期，见 storage.PresignAvatarURL）。
type AvatarURLProvider interface {
	PresignAvatarURL(ctx context.Context, objectKey string) (string, error)
}

// ImageUploadValidator 消费侧接口：PATCH /me 的 avatar_key 必须校验
// "文件真实上传 + 票证未被使用"，由 video 模块实现（main 装配）。
type ImageUploadValidator interface {
	ValidateImage(ctx context.Context, userID int64, objectKey string) error
}

type UpdateMeParams struct {
	Bio       *string
	AvatarKey *string
}

// UpdateMe 更新自己的资料。avatar_key 必须通过图片上传校验，
// 防伪造引用他人文件（与视频 object_key 同一防伪哲学）。
// 校验失败统一映射为 ErrInvalidImageKey，不依赖 video 模块的哨兵类型。
func (s *Service) UpdateMe(ctx context.Context, userID int64, p UpdateMeParams) (*PublicUser, error) {
	if p.AvatarKey != nil {
		if err := s.imageValidator.ValidateImage(ctx, userID, *p.AvatarKey); err != nil {
			slog.Warn("validate image failed", "user_id", userID, "err", err)
			return nil, ErrInvalidImageKey
		}
	}
	if err := s.repo.UpdateProfile(ctx, userID, p.Bio, p.AvatarKey); err != nil {
		slog.Error("update profile failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("update profile: %w", err)
	}
	return s.GetProfile(ctx, userID)
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
	// AvatarURL 可显示的预签名 URL（长有效期）；avatar_key 仅用于 PATCH 输入。
	AvatarURL *string `json:"avatar_url"`
}

// fillAvatar 给 PublicUser 附加头像显示 URL（key 为 nil 或签名失败则保持 null）。
func (s *Service) fillAvatar(ctx context.Context, u *User, pu *PublicUser) {
	if u.AvatarKey == nil {
		return
	}
	url, err := s.avatar.PresignAvatarURL(ctx, *u.AvatarKey)
	if err != nil {
		slog.Warn("presign avatar url failed", "user_id", u.UserID, "err", err)
		return
	}
	pu.AvatarURL = &url
}

// PublicProfile 公开资料：不含 email（隐私），带社交计数与个性化字段。
type PublicProfile struct {
	UserID         int64   `json:"user_id"`
	Username       string  `json:"username"`
	Bio            string  `json:"bio"`
	AvatarURL      *string `json:"avatar_url"`
	FollowingCount int64   `json:"following_count"`
	FollowersCount int64   `json:"followers_count"`
	IsFollowing    bool    `json:"is_following"`
	IsMe           bool    `json:"is_me"`
}

// GetPublicProfile 公开资料：viewerID=0（匿名）时 is_following 恒 false。
func (s *Service) GetPublicProfile(ctx context.Context, viewerID, userID int64) (*PublicProfile, error) {
	u, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		slog.Error("find user for profile failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("find user for profile: %w", err)
	}
	following, followers, err := s.repo.FollowCounts(ctx, userID)
	if err != nil {
		slog.Error("follow counts failed", "user_id", userID, "err", err)
		return nil, fmt.Errorf("follow counts: %w", err)
	}
	p := &PublicProfile{
		UserID:         u.UserID,
		Username:       u.Username,
		Bio:            u.Bio,
		FollowingCount: following,
		FollowersCount: followers,
		IsMe:           viewerID == userID,
	}
	if viewerID > 0 && viewerID != userID {
		following, err := s.repo.IsFollowing(ctx, viewerID, userID)
		if err != nil {
			slog.Error("is following failed", "viewer_id", viewerID, "user_id", userID, "err", err)
			return nil, fmt.Errorf("is following: %w", err)
		}
		p.IsFollowing = following
	}
	if u.AvatarKey != nil {
		// 头像签名失败只降级为 null（装饰性字段，不值得让整个资料 500）
		url, err := s.avatar.PresignAvatarURL(ctx, *u.AvatarKey)
		if err != nil {
			slog.Warn("presign avatar url failed", "user_id", userID, "err", err)
		} else {
			p.AvatarURL = &url
		}
	}
	return p, nil
}

func (s *Service) Register(ctx context.Context, p RegisterParams) (*AuthResult, error) {
	email, err := NormalizeEmail(p.Email)
	if err != nil {
		return nil, ErrInvalidParam
	}
	p.Email = email
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
		Email:        email,
		Username:     username,
		PasswordHash: string(hash),
		Bio:          "",
		// 显式声明新用户可用：不依赖数据库默认值，也不给"空 status"任何机会。
		Status: UserStatusActive,
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
	identifier := strings.TrimSpace(p.EmailOrUsername)
	// 邮箱大小写不敏感：注册时已规范化存储，登录时用同一规则查询，
	// 否则 Alice@x.com 能注册却登录不上（或反过来绕过唯一约束）。
	if email, err := NormalizeEmail(identifier); err == nil {
		u, findErr := s.repo.FindByEmail(ctx, email)
		if findErr != nil {
			if errors.Is(findErr, ErrNotFound) {
				// 用户不存在时也做一次 bcrypt 比对：抹平"存在/不存在"的响应时间差，
				// 避免账号枚举。返回值刻意丢弃。
				_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))
				return nil, ErrInvalidCredentials
			}
			slog.Error("find user failed", "err", findErr)
			return nil, findErr
		}
		return s.authenticate(ctx, u, p.Password)
	}
	u, err := s.repo.FindByUsername(ctx, identifier)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))
			return nil, ErrInvalidCredentials
		}
		slog.Error("find user failed", "err", err)
		return nil, err
	}
	return s.authenticate(ctx, u, p.Password)
}

// authenticate 校验口令与账号可用性。停用账号走与"口令错误"完全相同的
// 返回路径（含 dummy hash 的时间对齐），避免通过响应差异探测账号状态。
func (s *Service) authenticate(ctx context.Context, u *User, password string) (*AuthResult, error) {
	err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password))
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if !u.Usable() {
		slog.Warn("login rejected for unusable account", "user_id", u.UserID, "status", u.Status)
		return nil, ErrInvalidCredentials
	}
	accessToken, refreshToken, err := s.issueToken(u.UserID)
	if err != nil {
		return nil, err
	}
	pu := &PublicUser{
		UserID:    u.UserID,
		Username:  u.Username,
		Email:     u.Email,
		Bio:       u.Bio,
		AvatarKey: u.AvatarKey,
	}
	s.fillAvatar(ctx, u, pu)
	return &AuthResult{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    accessTokenTTL,
		User:         *pu,
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
	// 停用账号对公开资料按"不存在"处理：interaction 早已按 ACTIVE 过滤，
	// 两处可见性规则必须一致，否则同一账号在关注列表里消失、在主页却还在。
	if !u.Usable() {
		return nil, ErrNotFound
	}
	pu := &PublicUser{
		UserID:    u.UserID,
		Username:  u.Username,
		Email:     u.Email,
		Bio:       u.Bio,
		AvatarKey: u.AvatarKey,
	}
	s.fillAvatar(ctx, u, pu)
	return pu, nil
}

// Refresh 用 refresh token 换取新 token 对（rotation）：
// 新 token 签发前先把旧 jti 拉黑——两步骤之间崩溃的后果是
// 旧 token 作废、用户需要重新登录，而不会出现新旧两个 token 同时有效
// （不产生 token 泛滥）。这是无状态 JWT 下无法用事务时对顺序的取舍。
// Redis SETNX 原子 Claim 旧 jti，并发刷新只有一次可以成功签发。
// Claim 成功后签发失败也需重新登录；access token 不逐请求检查撤销状态。
// 当前权衡见 docs/architecture-decisions.md#auth。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*AuthResult, error) {
	claims, err := s.tokens.Verify(refreshToken)
	if err != nil || !claims.IsRefresh() {
		return nil, ErrInvalidCredentials
	}
	remaining := time.Until(claims.ExpiresAt.Time)
	if remaining <= 0 {
		return nil, ErrInvalidCredentials
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	// 用户被软删或运维停用时 token 必须作废，否则停用后旧 refresh token
	// 还能无限续签新 access token，安全护栏形同虚设。
	current, err := s.repo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		slog.Error("find user on refresh failed", "err", err)
		return nil, fmt.Errorf("find user on refresh: %w", err)
	}
	if !current.Usable() {
		slog.Warn("refresh rejected for unusable account", "user_id", userID, "status", current.Status)
		return nil, ErrInvalidCredentials
	}
	// 旋转是"抢占"而不是"先查后写"：SETNX 的返回值就是仲裁结果。
	// 并发的两次刷新只有一个能抢到旧 jti，另一个直接失效，
	// 被窃取的 refresh token 无法与真实用户并行换取自己的 token 对。
	claimed, err := s.blacklist.Claim(ctx, claims.ID, remaining)
	if err != nil {
		slog.Error("claim refresh token failed", "err", err)
		return nil, fmt.Errorf("claim refresh token: %w", err)
	}
	if !claimed {
		return nil, ErrInvalidCredentials
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
		// 幂等语义：token 无效/过期/类型不符都视为"已经登出"，返回成功。
		// 这不是吞错误——登出的目标状态已经达成，报错只会让客户端做无意义重试。
		return nil //nolint:nilerr // 见上方注释：登出是幂等操作
	}
	remaining := time.Until(claims.ExpiresAt.Time)
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
