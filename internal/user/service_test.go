package user

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lihualin/tokmall/internal/token"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeRepo 内存版仓库：模拟 gorm 行为，重点回填主键（对应 Create 后的 RETURNING）。
type fakeRepo struct {
	usersByID       map[int64]*User
	usersByUsername map[string]*User
	usersByEmail    map[string]*User
	follows         map[string]bool // key: "follower-followee"
	nextID          int64
	createErr       error
	findErr         error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		usersByID:       make(map[int64]*User),
		usersByUsername: make(map[string]*User),
		usersByEmail:    make(map[string]*User),
		follows:         make(map[string]bool),
		nextID:          1,
	}
}

func (f *fakeRepo) FollowCounts(_ context.Context, userID int64) (following, followers int64, err error) {
	for k := range f.follows {
		parts := strings.SplitN(k, "-", 2)
		if f.isDeleted(parseTestID(parts[0])) || f.isDeleted(parseTestID(parts[1])) {
			continue
		}
		if parts[0] == strconv.FormatInt(userID, 10) {
			following++
		}
		if parts[1] == strconv.FormatInt(userID, 10) {
			followers++
		}
	}
	return following, followers, nil
}

func (f *fakeRepo) IsFollowing(_ context.Context, followerID, followeeID int64) (bool, error) {
	if f.isDeleted(followeeID) {
		return false, nil
	}
	return f.follows[fmt.Sprintf("%d-%d", followerID, followeeID)], nil
}

func (f *fakeRepo) isDeleted(userID int64) bool {
	u, ok := f.usersByID[userID]
	return ok && u.DeletedAt.Valid
}

func parseTestID(raw string) int64 {
	id, _ := strconv.ParseInt(raw, 10, 64)
	return id
}

func (f *fakeRepo) UpdateProfile(_ context.Context, userID int64, bio *string, avatarKey *string) error {
	u, ok := f.usersByID[userID]
	if !ok {
		return ErrNotFound
	}
	if bio != nil {
		u.Bio = *bio
	}
	if avatarKey != nil {
		u.AvatarKey = avatarKey
	}
	return nil
}

func (f *fakeRepo) Create(ctx context.Context, u *User) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.usersByEmail[u.Email]; ok {
		return ErrEmailTaken
	}
	if _, ok := f.usersByUsername[u.Username]; ok {
		return ErrUsernameTaken
	}
	u.UserID = f.nextID
	f.nextID++
	f.usersByID[u.UserID] = u
	f.usersByEmail[u.Email] = u
	f.usersByUsername[u.Username] = u
	return nil
}

func (f *fakeRepo) FindByID(ctx context.Context, id int64) (*User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.usersByID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) FindByUsername(ctx context.Context, username string) (*User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.usersByUsername[username]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) FindByEmail(ctx context.Context, email string) (*User, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	u, ok := f.usersByEmail[email]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

type fakeTokenManager struct {
	signErr   error
	verifyRes *token.Claims
	verifyErr error
}

func (f *fakeTokenManager) Sign(claims token.Claims) (string, error) {
	if f.signErr != nil {
		return "", f.signErr
	}
	return "token-" + claims.Type, nil
}

func (f *fakeTokenManager) Verify(tokenString string) (*token.Claims, error) {
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	return f.verifyRes, nil
}

// fakeBlacklist 内存版黑名单：记录 jti，两个方法可独立注入故障。
type fakeBlacklist struct {
	blocked   map[string]bool
	blockErr  error
	existsErr error
}

func newFakeBlacklist() *fakeBlacklist {
	return &fakeBlacklist{blocked: make(map[string]bool)}
}

func (f *fakeBlacklist) Block(_ context.Context, jti string, _ time.Duration) error {
	if f.blockErr != nil {
		return f.blockErr
	}
	f.blocked[jti] = true
	return nil
}

func (f *fakeBlacklist) IsBlocked(_ context.Context, jti string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.blocked[jti], nil
}

func newTestService(repo *fakeRepo, tokens *fakeTokenManager) *Service {
	return NewService(repo, tokens, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})
}

// fakeAvatar 预签名 URL 假实现。
type fakeAvatar struct {
	err error
}

func (f *fakeAvatar) PresignAvatarURL(_ context.Context, objectKey string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "http://minio.local/signed/" + objectKey, nil
}

// fakeImageValidator 头像校验假实现。
type fakeImageValidator struct {
	err error
}

func (f *fakeImageValidator) ValidateImage(_ context.Context, _ int64, _ string) error {
	return f.err
}

func refreshClaims(jti string) *token.Claims {
	return &token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   "1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Type: token.TypeRefresh,
		Role: "USER",
	}
}

// seedUser 给 fakeRepo 预置 id=1 的用户（Refresh 会做存在性检查）。
func seedUser(repo *fakeRepo) {
	repo.usersByID[1] = &User{UserID: 1, Email: "a@b.com", Username: "alice12", Bio: ""}
}

// TestRegisterLoginFullFlow 全流程模板：注册 -> 用户名登录 -> 邮箱登录
// -> 错误密码 -> 用户不存在，串联验证数据在两层之间的流动。
func TestRegisterLoginFullFlow(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, &fakeTokenManager{})

	// 1. 注册
	result, err := svc.Register(context.Background(), RegisterParams{
		Email:    "alice@example.com",
		Username: "alice12",
		Password: "password123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.AccessToken)
	require.NotEmpty(t, result.RefreshToken)
	require.Equal(t, int64(1), result.User.UserID)
	require.Equal(t, "alice12", result.User.Username)
	require.Equal(t, "alice@example.com", result.User.Email)

	// 2. 用户名登录
	byUsername, err := svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "alice12",
		Password:        "password123",
	})
	require.NoError(t, err)
	require.Equal(t, result.User.UserID, byUsername.User.UserID)

	// 3. 邮箱登录
	byEmail, err := svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "alice@example.com",
		Password:        "password123",
	})
	require.NoError(t, err)
	require.Equal(t, result.User.UserID, byEmail.User.UserID)

	// 4. 错误密码
	_, err = svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "alice12",
		Password:        "wrong-password",
	})
	require.ErrorIs(t, err, ErrInvalidCredentials)

	// 5. 用户不存在（两条路径都必须是 ErrInvalidCredentials）
	_, err = svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "nobody",
		Password:        "password123",
	})
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "nobody@example.com",
		Password:        "password123",
	})
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

// TestRegisterValidation 表驱动模板：非法入参一律 ErrInvalidParam。
func TestRegisterValidation(t *testing.T) {
	svc := newTestService(newFakeRepo(), &fakeTokenManager{})

	cases := []struct {
		name string
		req  RegisterParams
	}{
		{"bad email", RegisterParams{Email: "not-an-email", Username: "alice12", Password: "password123"}},
		{"username starts with digit", RegisterParams{Email: "a@b.com", Username: "1alice1", Password: "password123"}},
		{"username too short", RegisterParams{Email: "a@b.com", Username: "ab", Password: "password123"}},
		{"username too long", RegisterParams{Email: "a@b.com", Username: "abcdefghijklmnopq", Password: "password123"}},
		{"password too short", RegisterParams{Email: "a@b.com", Username: "alice12", Password: "short"}},
		{"password too long", RegisterParams{Email: "a@b.com", Username: "alice12", Password: string(make([]byte, 73))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Register(context.Background(), tc.req)
			require.ErrorIs(t, err, ErrInvalidParam)
		})
	}
}

// TestRegisterDuplicate 唯一约束分支：两个唯一键独立报错。
func TestRegisterDuplicate(t *testing.T) {
	svc := newTestService(newFakeRepo(), &fakeTokenManager{})

	first := RegisterParams{Email: "a@b.com", Username: "alice12", Password: "password123"}
	_, err := svc.Register(context.Background(), first)
	require.NoError(t, err)

	_, err = svc.Register(context.Background(), RegisterParams{
		Email: "a@b.com", Username: "bob1234", Password: "password123",
	})
	require.ErrorIs(t, err, ErrEmailTaken)

	_, err = svc.Register(context.Background(), RegisterParams{
		Email: "c@d.com", Username: "alice12", Password: "password123",
	})
	require.ErrorIs(t, err, ErrUsernameTaken)
}

// TestRegisterRepoFailure 未知错误必须能穿透：errors.Is 命中原始错误，
// 证明 service 没有把它吞成业务错误。
func TestRegisterRepoFailure(t *testing.T) {
	boom := errors.New("db connection lost")
	repo := newFakeRepo()
	repo.createErr = boom
	svc := newTestService(repo, &fakeTokenManager{})

	_, err := svc.Register(context.Background(), RegisterParams{
		Email: "a@b.com", Username: "alice12", Password: "password123",
	})
	require.ErrorIs(t, err, boom)
}

// TestLoginRepoFailure repo 故障不能变成 ErrInvalidCredentials，必须原样上抛。
func TestLoginRepoFailure(t *testing.T) {
	boom := errors.New("db connection lost")
	repo := newFakeRepo()
	repo.findErr = boom
	svc := newTestService(repo, &fakeTokenManager{})

	_, err := svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "alice12",
		Password:        "password123",
	})
	require.ErrorIs(t, err, boom)
}

func TestRegisterSignTokenFailure(t *testing.T) {
	repo := newFakeRepo()
	svc := newTestService(repo, &fakeTokenManager{signErr: errors.New("jwt failure")})
	_, err := svc.Register(context.Background(), RegisterParams{
		Email: "abas3@123.com", Username: "alice12", Password: "1234123jkaljd",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "sign token")
}

func TestRefreshSuccess(t *testing.T) {
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	repo := newFakeRepo()
	seedUser(repo)
	svc := NewService(repo, tm, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

	result, err := svc.Refresh(context.Background(), "old-refresh-token")
	require.NoError(t, err)
	require.NotEmpty(t, result.AccessToken)
	require.NotEmpty(t, result.RefreshToken)
	require.Equal(t, int64(15*60), result.ExpiresIn)
}

// TestRefreshRotatesOldJti rotation 的核心：刷新后旧 jti 必须被拉黑，
// 旧 token 再用立刻被拒。
func TestRefreshRotatesOldJti(t *testing.T) {
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	blacklist := newFakeBlacklist()
	repo := newFakeRepo()
	seedUser(repo)
	svc := NewService(repo, tm, blacklist, &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "old-refresh-token")
	require.NoError(t, err)
	require.True(t, blacklist.blocked["jti-1"])

	tm.verifyRes = refreshClaims("jti-1")
	_, err = svc.Refresh(context.Background(), "old-refresh-token")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

// TestRefreshDeletedUser 软删用户：旧 refresh token 必须被拒（不能续签）。
func TestRefreshDeletedUser(t *testing.T) {
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	svc := NewService(newFakeRepo(), tm, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "old-refresh-token")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestRefreshRejected(t *testing.T) {
	cases := []struct {
		name string
		mod  func(tm *fakeTokenManager, bl *fakeBlacklist)
	}{
		{
			"expired token",
			func(tm *fakeTokenManager, _ *fakeBlacklist) {
				tm.verifyRes = &token.Claims{
					RegisteredClaims: jwt.RegisteredClaims{
						ID:        "jti-expired",
						Subject:   "1",
						ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
					},
					Type: token.TypeRefresh,
					Role: "USER",
				}
			},
		},
		{
			"access token passed to refresh",
			func(tm *fakeTokenManager, _ *fakeBlacklist) {
				c := refreshClaims("jti-access")
				c.Type = token.TypeAccess
				tm.verifyRes = c
			},
		},
		{
			"signature invalid",
			func(tm *fakeTokenManager, _ *fakeBlacklist) {
				tm.verifyErr = errors.New("bad signature")
			},
		},
		{
			"token already blocked",
			func(_ *fakeTokenManager, bl *fakeBlacklist) {
				bl.blocked["jti-blocked"] = true
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm := &fakeTokenManager{verifyRes: refreshClaims("jti-blocked")}
			bl := newFakeBlacklist()
			tc.mod(tm, bl)
			repo := newFakeRepo()
			seedUser(repo)
			svc := NewService(repo, tm, bl, &fakeAvatar{}, &fakeImageValidator{})

			_, err := svc.Refresh(context.Background(), "refresh-token")
			require.ErrorIs(t, err, ErrInvalidCredentials)
		})
	}
}

func TestLogout(t *testing.T) {
	t.Run("blocks valid token", func(t *testing.T) {
		tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
		bl := newFakeBlacklist()
		svc := NewService(newFakeRepo(), tm, bl, &fakeAvatar{}, &fakeImageValidator{})

		require.NoError(t, svc.Logout(context.Background(), "refresh-token"))
		require.True(t, bl.blocked["jti-1"])
	})

	// 幂等：无效/过期/类型不符的 token，logout 都返回成功。
	t.Run("idempotent on already-invalid token", func(t *testing.T) {
		tm := &fakeTokenManager{verifyErr: errors.New("bad signature")}
		svc := NewService(newFakeRepo(), tm, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})
		require.NoError(t, svc.Logout(context.Background(), "garbage"))
	})

	t.Run("expired token is no-op", func(t *testing.T) {
		tm := &fakeTokenManager{verifyRes: &token.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				ID:        "jti-expired",
				Subject:   "1",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			},
			Type: token.TypeRefresh,
			Role: "USER",
		}}
		bl := newFakeBlacklist()
		svc := NewService(newFakeRepo(), tm, bl, &fakeAvatar{}, &fakeImageValidator{})
		require.NoError(t, svc.Logout(context.Background(), "refresh-token"))
		require.False(t, bl.blocked["jti-expired"])
	})

	t.Run("access token is no-op", func(t *testing.T) {
		c := refreshClaims("jti-access")
		c.Type = token.TypeAccess
		tm := &fakeTokenManager{verifyRes: c}
		bl := newFakeBlacklist()
		svc := NewService(newFakeRepo(), tm, bl, &fakeAvatar{}, &fakeImageValidator{})

		require.NoError(t, svc.Logout(context.Background(), "access-token"))
		require.False(t, bl.blocked["jti-access"])
	})

	t.Run("redis failure surfaces", func(t *testing.T) {
		tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
		bl := newFakeBlacklist()
		bl.blockErr = errors.New("redis down")
		svc := NewService(newFakeRepo(), tm, bl, &fakeAvatar{}, &fakeImageValidator{})
		require.Error(t, svc.Logout(context.Background(), "refresh-token"))
	})
}

func TestRefreshBlacklistFailure(t *testing.T) {
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	bl := newFakeBlacklist()
	bl.existsErr = errors.New("redis down")
	repo := newFakeRepo()
	seedUser(repo)
	svc := NewService(repo, tm, bl, &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "refresh-token")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidCredentials)
}

// TestRefreshBlockFailure 缺口1：IsBlocked 通过但 Block 失败——错误必须
// 上抛（不是 401），且旧 token 不得被标记为已拉黑。
func TestRefreshBlockFailure(t *testing.T) {
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	bl := newFakeBlacklist()
	bl.blockErr = errors.New("redis down")
	repo := newFakeRepo()
	seedUser(repo)
	svc := NewService(repo, tm, bl, &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "refresh-token")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidCredentials)
	require.False(t, bl.blocked["jti-1"])
}

// TestRefreshSignFailureAfterRotation 缺口2：旧 jti 拉黑后签发失败——
// 错误上抛，且旧 token 已被拉黑（安全侧优先：宁可让用户重登）。
func TestRefreshSignFailureAfterRotation(t *testing.T) {
	tm := &fakeTokenManager{
		verifyRes: refreshClaims("jti-1"),
		signErr:   errors.New("jwt failure"),
	}
	bl := newFakeBlacklist()
	repo := newFakeRepo()
	seedUser(repo)
	svc := NewService(repo, tm, bl, &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "refresh-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "sign token")
	require.True(t, bl.blocked["jti-1"])
}

// TestRefreshBadSubject 缺口3：subject 不是数字——必须 401，不能 500。
func TestRefreshBadSubject(t *testing.T) {
	c := refreshClaims("jti-1")
	c.Subject = "not-a-number"
	tm := &fakeTokenManager{verifyRes: c}
	svc := NewService(newFakeRepo(), tm, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "refresh-token")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

// TestRefreshRepoFailure 缺口4：查用户时 DB 故障——必须上抛原始错误，
// 不能误判成 401。
func TestRefreshRepoFailure(t *testing.T) {
	boom := errors.New("db connection lost")
	repo := newFakeRepo()
	seedUser(repo)
	repo.findErr = boom
	tm := &fakeTokenManager{verifyRes: refreshClaims("jti-1")}
	svc := NewService(repo, tm, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Refresh(context.Background(), "refresh-token")
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrInvalidCredentials)
}

// TestGetProfile 缺口5：GetProfile 完全没测。
func TestGetProfile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newFakeRepo()
		repo.usersByID[7] = &User{UserID: 7, Email: "p@b.com", Username: "prof7", Bio: "hi"}
		svc := NewService(repo, &fakeTokenManager{}, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

		p, err := svc.GetProfile(context.Background(), 7)
		require.NoError(t, err)
		require.Equal(t, int64(7), p.UserID)
		require.Equal(t, "prof7", p.Username)
		require.Equal(t, "hi", p.Bio)
	})

	t.Run("not found", func(t *testing.T) {
		svc := NewService(newFakeRepo(), &fakeTokenManager{}, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})
		_, err := svc.GetProfile(context.Background(), 99)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("repo failure", func(t *testing.T) {
		boom := errors.New("db connection lost")
		repo := newFakeRepo()
		repo.findErr = boom
		svc := NewService(repo, &fakeTokenManager{}, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

		_, err := svc.GetProfile(context.Background(), 1)
		require.ErrorIs(t, err, boom)
	})
}

// TestLoginEmailPathRepoFailure 缺口7：email 登录路径的 repo 故障。
func TestLoginEmailPathRepoFailure(t *testing.T) {
	boom := errors.New("db connection lost")
	repo := newFakeRepo()
	repo.findErr = boom
	svc := NewService(repo, &fakeTokenManager{}, newFakeBlacklist(), &fakeAvatar{}, &fakeImageValidator{})

	_, err := svc.Login(context.Background(), LoginParams{
		EmailOrUsername: "alice@example.com",
		Password:        "password123",
	})
	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, ErrInvalidCredentials)
}

// TestGetPublicProfile 公开资料：email 不泄露、计数、is_following、is_me。
func TestGetPublicProfile(t *testing.T) {
	repo := newFakeRepo()
	seedUser(repo)
	svc := newTestService(repo, &fakeTokenManager{})

	t.Run("anonymous viewer", func(t *testing.T) {
		p, err := svc.GetPublicProfile(context.Background(), 0, 1)
		require.NoError(t, err)
		require.Equal(t, "alice12", p.Username)
		// PublicProfile 类型本身不含 Email 字段——结构上杜绝隐私泄露
		require.False(t, p.IsFollowing, "匿名 is_following 恒 false")
		require.False(t, p.IsMe)
	})

	t.Run("viewer has followed", func(t *testing.T) {
		repo.follows["2-1"] = true
		p, err := svc.GetPublicProfile(context.Background(), 2, 1)
		require.NoError(t, err)
		require.True(t, p.IsFollowing)
		require.False(t, p.IsMe)
	})

	t.Run("viewer is the user", func(t *testing.T) {
		p, err := svc.GetPublicProfile(context.Background(), 1, 1)
		require.NoError(t, err)
		require.True(t, p.IsMe)
	})

	t.Run("not found", func(t *testing.T) {
		_, err := svc.GetPublicProfile(context.Background(), 0, 99)
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("avatar presigned", func(t *testing.T) {
		repo.usersByID[1].AvatarKey = ptr("avatars/1/a.jpg")
		p, err := svc.GetPublicProfile(context.Background(), 0, 1)
		require.NoError(t, err)
		require.NotNil(t, p.AvatarURL)
		require.Equal(t, "http://minio.local/signed/avatars/1/a.jpg", *p.AvatarURL)
	})
}

func TestGetPublicProfileFollowCountsExcludeDeletedUsers(t *testing.T) {
	repo := newFakeRepo()
	seedUser(repo)
	repo.usersByID[2] = &User{UserID: 2, Username: "bob123"}
	repo.usersByID[3] = &User{UserID: 3, Username: "carol1"}
	repo.usersByID[3].DeletedAt = gorm.DeletedAt{Time: time.Now(), Valid: true}
	repo.follows["1-2"] = true // active followee，计入 following
	repo.follows["1-3"] = true // deleted followee，不计入 following
	repo.follows["2-1"] = true // active follower，计入 followers
	repo.follows["3-1"] = true // deleted follower，不计入 followers

	svc := newTestService(repo, &fakeTokenManager{})
	p, err := svc.GetPublicProfile(context.Background(), 2, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), p.FollowingCount)
	require.Equal(t, int64(1), p.FollowersCount)
	require.True(t, p.IsFollowing, "active viewer 对 active 目标的关注关系应保留")
}

// TestUpdateMe 更新资料：头像必须通过校验、局部更新语义。
func TestUpdateMe(t *testing.T) {
	repo := newFakeRepo()
	seedUser(repo)
	svc := newTestService(repo, &fakeTokenManager{})

	t.Run("update bio only", func(t *testing.T) {
		bio := "new bio"
		p, err := svc.UpdateMe(context.Background(), 1, UpdateMeParams{Bio: &bio})
		require.NoError(t, err)
		require.Equal(t, "new bio", p.Bio)
	})

	t.Run("avatar key validated then stored", func(t *testing.T) {
		key := "images/1/face.jpg"
		p, err := svc.UpdateMe(context.Background(), 1, UpdateMeParams{AvatarKey: &key})
		require.NoError(t, err)
		require.NotNil(t, p.AvatarKey)
		require.Equal(t, key, *p.AvatarKey)
	})

	t.Run("forged avatar key rejected", func(t *testing.T) {
		svc := NewService(repo, &fakeTokenManager{}, newFakeBlacklist(),
			&fakeAvatar{}, &fakeImageValidator{err: errors.New("ticket invalid")})
		key := "images/forged.jpg"
		_, err := svc.UpdateMe(context.Background(), 1, UpdateMeParams{AvatarKey: &key})
		require.ErrorIs(t, err, ErrInvalidImageKey)
	})
}

func ptr(s string) *string {
	return &s
}
