package user

import (
	"context"
	"errors"
	"testing"

	"github.com/lihualin/tokmall/internal/token"
	"github.com/stretchr/testify/require"
)

// fakeRepo 内存版仓库：模拟 gorm 行为，重点回填主键（对应 Create 后的 RETURNING）。
type fakeRepo struct {
	usersByID       map[int64]*User
	usersByUsername map[string]*User
	usersByEmail    map[string]*User
	nextID          int64
	createErr       error
	findErr         error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		usersByID:       make(map[int64]*User),
		usersByUsername: make(map[string]*User),
		usersByEmail:    make(map[string]*User),
		nextID:          1,
	}
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
	signErr error
}

func (f *fakeTokenManager) Sign(claims token.Claims) (string, error) {
	if f.signErr != nil {
		return "", f.signErr
	}
	return "token-" + claims.Type, nil
}

func newTestService(repo *fakeRepo, tokens *fakeTokenManager) *Service {
	return NewService(repo, tokens)
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
