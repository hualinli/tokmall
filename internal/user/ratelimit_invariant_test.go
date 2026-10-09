package user

// 账号维度限流的安全不变量：
//
//	若两个请求在**业务层**解析出同一个账号，则它们必须落在同一个限流桶。
//
// 这条不变量曾经被打破过：限流侧只做 TrimSpace + ToLower，业务侧先走
// mail.ParseAddress（丢弃显示名与尖括号）。于是 `rotator <victim@e.com>` 与
// `victim@e.com` 在业务层是同一个账号，限流侧却是两个桶——攻击者轮换显示名即可
// 绕过 10/min/账号 的防爆破配额。
//
// 用例走真实 Hertz 路由 + 真实限流中间件 + 真实 Service.Login，不 mock 业务层。
//
// 注意：JSON 字符串里不能出现未转义的双引号，所以显示名一律用不带引号的形式，
// 比如 `rotator-0 <victim@example.com>`——mail.ParseAddress 对它同样会剥掉显示名。

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	hzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/middleware"
)

const (
	victimEmail      = "victim@example.com"
	authAccountLimit = 10  // internal/config/config.go: defaultAuthAccountLimit
	authIPLimit      = 100 // internal/config/config.go: defaultAuthIPLimit
	accountKeyPrefix = "ratelimit:auth:account:"
)

// countingLimiter 按 key 计数并返回**调用方传入的**限额——必须用传入的 limit，
// 否则账号规则(10) 与 IP 规则(100) 会串味，测出来的结论是假的。
type countingLimiter struct {
	mu      sync.Mutex
	buckets map[string]int
}

func (l *countingLimiter) Allow(_ context.Context, key string, limit int, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]int{}
	}
	l.buckets[key]++
	return l.buckets[key] <= limit, nil
}

func (l *countingLimiter) accountKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k := range l.buckets {
		if strings.HasPrefix(k, accountKeyPrefix) {
			out = append(out, k)
		}
	}
	return out
}

func (l *countingLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buckets[key]
}

// lookupRecorder 记录业务层实际拿去查库的账号。
type lookupRecorder struct {
	Repository
	mu     sync.Mutex
	looked []string
}

func (r *lookupRecorder) FindByEmail(_ context.Context, email string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.looked = append(r.looked, email)
	return nil, ErrNotFound // 用户不存在即可；这里只关心"查了哪个账号"
}

func (r *lookupRecorder) FindByUsername(_ context.Context, name string) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.looked = append(r.looked, name)
	return nil, ErrNotFound
}

func (r *lookupRecorder) lookups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.looked...)
}

// newLoginEngine 与 cmd/api/main.go 的 auth 组同构：账号维度 + IP 维度两道限流。
func newLoginEngine(t *testing.T, repo Repository, limiter middleware.Limiter) *route.Engine {
	t.Helper()
	h := NewHandler(NewService(repo, nil, nil, nil, nil))
	r := route.NewEngine(hzconfig.NewOptions([]hzconfig.Option{}))
	h.RegisterRoutes(r.Group("/api/v1"),
		middleware.RateLimit(limiter, middleware.RateLimitRule{
			Scope: "auth", Limit: authAccountLimit, Window: time.Minute,
		}, middleware.AccountKeyExtractor),
		middleware.RateLimit(limiter, middleware.RateLimitRule{
			Scope: "auth", Limit: authIPLimit, Window: time.Minute,
		}),
	)
	return r
}

// bruteForceBodies 造 n 次登录尝试：账号恒为受害者邮箱，只有显示名在变。
func bruteForceBodies(n int) []string {
	bodies := make([]string, 0, n)
	for i := range n {
		identifier := "rotator-" + itoa(i) + " <" + victimEmail + ">"
		bodies = append(bodies, `{"identifier":"`+identifier+`","password":"guess`+itoa(i)+`"}`)
	}
	return bodies
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func driveLogin(t *testing.T, engine *route.Engine, bodies []string) (accepted, rejected int) {
	t.Helper()
	for _, body := range bodies {
		w := ut.PerformRequest(engine, http.MethodPost, "/api/v1/auth/login",
			&ut.Body{Body: bytes.NewBufferString(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if w.Code == http.StatusTooManyRequests {
			rejected++
			continue
		}
		accepted++
	}
	return accepted, rejected
}

// TestAccountRateLimit_DisplayNameMustNotCreateNewBucket 是本条不变量的核心一格：
// 登录只接受 identifier 一个字段，没法靠"多字段挑一个"绕过，只能靠显示名。
func TestAccountRateLimit_DisplayNameMustNotCreateNewBucket(t *testing.T) {
	repo := &lookupRecorder{}
	limiter := &countingLimiter{}
	engine := newLoginEngine(t, repo, limiter)

	const attempts = 15
	accepted, rejected := driveLogin(t, engine, bruteForceBodies(attempts))

	// 断言 1：无论轮换多少个显示名，账号维度只有一个桶。
	keys := limiter.accountKeys()
	require.Len(t, keys, 1,
		"同一账号的显示名变体必须共用一个限流桶，实际得到 %d 个：%v", len(keys), keys)
	require.Equal(t, accountKeyPrefix+victimEmail, keys[0],
		"账号桶必须由业务层归一后的邮箱构成")
	t.Logf("账号桶 = %s，计数 = %d", keys[0], limiter.count(keys[0]))

	// 断言 2：共用一个桶之后，10/min 的配额真的生效。
	require.Equal(t, authAccountLimit, accepted,
		"账号维度 10/min 应当只放行前 %d 次", authAccountLimit)
	require.Equal(t, attempts-authAccountLimit, rejected, "第 %d 次起必须 429", authAccountLimit+1)

	// 断言 3：到达业务层的请求，查的都是同一个受害者账号。
	looked := repo.lookups()
	require.Len(t, looked, accepted, "到达业务层的次数应等于放行次数")
	for _, got := range looked {
		require.Equal(t, victimEmail, got, "业务层应把每个载荷解析成同一个受害者账号")
	}
	t.Logf("放行 = %d，429 = %d，业务层查询账号 = %v", accepted, rejected, looked)
}

// TestAccountRateLimit_ExceedingLimitIsRejected 用纯邮箱（无显示名）再确认一次
// 常规路径没有被这次改动影响。
func TestAccountRateLimit_ExceedingLimitIsRejected(t *testing.T) {
	repo := &lookupRecorder{}
	limiter := &countingLimiter{}
	engine := newLoginEngine(t, repo, limiter)

	const attempts = authAccountLimit + 5
	bodies := make([]string, 0, attempts)
	for i := range attempts {
		bodies = append(bodies, `{"identifier":"`+victimEmail+`","password":"guess`+itoa(i)+`"}`)
	}
	accepted, rejected := driveLogin(t, engine, bodies)
	t.Logf("纯邮箱：放行 = %d，429 = %d", accepted, rejected)

	require.Equal(t, authAccountLimit, accepted)
	require.Equal(t, 5, rejected)
	require.Len(t, limiter.accountKeys(), 1)
	require.Equal(t, accountKeyPrefix+victimEmail, limiter.accountKeys()[0])
}

// TestAccountRateLimit_KeyMatchesBusinessLookup 是最小判据：
// 凡是业务层归一到同一邮箱的输入，限流键必须逐字相同。
func TestAccountRateLimit_KeyMatchesBusinessLookup(t *testing.T) {
	for _, raw := range []string{
		victimEmail,
		"  " + victimEmail + "  ",
		"VICTIM@Example.COM",
		"rotator-a <" + victimEmail + ">",
		"display name <" + victimEmail + ">",
		"another <" + victimEmail + ">",
	} {
		business, err := NormalizeEmail(raw)
		require.NoError(t, err, "业务层应接受 %q", raw)
		limitKey := middleware.NormalizeAccountKey(raw)
		require.Equal(t, business, limitKey,
			"限流键与业务键分叉了：raw=%q business=%q limit=%q", raw, business, limitKey)
	}

	// 登录名（不是合法地址形式）退回大小写不敏感。
	require.Equal(t, "bob123", middleware.NormalizeAccountKey("  Bob123  "))
	require.Empty(t, middleware.NormalizeAccountKey("   "))
}
