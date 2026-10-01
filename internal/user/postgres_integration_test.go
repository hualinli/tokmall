package user_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/testsupport"
	"github.com/lihualin/tokmall/internal/token"
	"github.com/lihualin/tokmall/internal/user"
)

// TestPostgresRegisterPersistsActiveStatus 是一个由真实故障倒逼出来的测试。
//
// 故障现象：端到端演练里"注册成功、紧接着登录 401"。
// 根因：给 model 映射 status 字段之后，GORM 的 Create 会把零值（空串）
// 写进 status 列，而不是省略该列去用 DEFAULT 'ACTIVE'；于是新用户既不是
// ACTIVE 也不是 DISABLED，登录被判为"账号不可用"。
//
// 这个用例的价值在于：它断言的是"数据库里真实存了什么"，而不是 service
// 返回了什么——fake repository 永远发现不了这类 ORM 与 DDL 默认值打架的问题。
func TestPostgresRegisterPersistsActiveStatus(t *testing.T) {
	db := testsupport.IsolatedPostgres(t, testsupport.RequireDSN(t))

	service := user.NewService(user.NewRepository(db), noopTokenManager{}, noopBlacklist{}, noopAvatar{}, noopImageValidator{})
	_, err := service.Register(context.Background(), user.RegisterParams{
		Email: "Status@Example.COM", Username: "statususer1", Password: "password123",
	})
	require.NoError(t, err)

	var row struct {
		Email  string
		Status string
	}
	require.NoError(t, db.Raw(`SELECT email, status FROM users WHERE username = ?`, "statususer1").Scan(&row).Error)
	require.Equal(t, string(user.UserStatusActive), row.Status, "新用户必须是 ACTIVE，空状态会让登录永久失败")
	require.Equal(t, "status@example.com", row.Email, "邮箱必须按规范化结果落库")

	// 并且真的能登录：这条断言覆盖"注册 → 登录"的完整路径，
	// 而不只是数据库里的一个字段值。
	// 用户名区分大小写（唯一索引按原值），邮箱不区分——两条路径都要能登录。
	_, err = service.Login(context.Background(), user.LoginParams{
		EmailOrUsername: "statususer1", Password: "password123",
	})
	require.NoError(t, err, "用户名登录必须可用")
	_, err = service.Login(context.Background(), user.LoginParams{
		EmailOrUsername: "  STATUS@example.COM ", Password: "password123",
	})
	require.NoError(t, err, "邮箱登录必须大小写与空白不敏感")
}

// 下面是最小化的假实现：这个用例只关心持久化结果，不需要真实 token/存储。
type noopTokenManager struct{}

func (noopTokenManager) Sign(token.Claims) (string, error) { return "signed-token", nil }
func (noopTokenManager) Verify(string) (*token.Claims, error) {
	return &token.Claims{}, nil
}

type noopBlacklist struct{}

func (noopBlacklist) Block(context.Context, string, time.Duration) error { return nil }
func (noopBlacklist) Claim(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (noopBlacklist) IsBlocked(context.Context, string) (bool, error) { return false, nil }

type noopAvatar struct{}

func (noopAvatar) PresignAvatarURL(context.Context, string) (string, error) {
	return "signed://avatar", nil
}

type noopImageValidator struct{}

func (noopImageValidator) ValidateImage(context.Context, int64, string) error { return nil }
