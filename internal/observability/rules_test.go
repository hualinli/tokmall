package observability_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/lihualin/tokmall/internal/observability"
)

// 这条测试解决一个很常见但没人测的问题：**告警规则引用了一个不存在的指标**。
//
// 记录/告警规则是 YAML，写错指标名不会编译报错，Prometheus 只会让那条规则
// 永远不触发（no data）——于是"我们配了告警"变成一句空话。
//
// 实现上刻意严格：把表达式里的每个小写标识符都当成"疑似指标名"，
// 只有命中已注册指标集合、PromQL 语法关键字、或文件里出现过的标签名才放行。
// 这样 `outbox_pending_evnts` 这种拼写错误会被抓住——用"后缀白名单"过滤则抓不到
// （写错的后缀不在白名单里，会被静默跳过）。
func TestPrometheusRulesReferenceExistingMetrics(t *testing.T) {
	registered := registeredMetricNames(t)
	require.NotEmpty(t, registered)

	ruleFiles, err := filepath.Glob(filepath.Join("..", "..", "observability", "prometheus", "rules", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, ruleFiles, "规则文件必须存在于 observability/prometheus/rules/")

	identifier := regexp.MustCompile(`\b[a-z][a-z0-9_]{2,}\b`)
	// PromQL 语法与函数（不是指标）。
	promqlSyntax := map[string]bool{
		"sum": true, "avg": true, "min": true, "max": true, "count": true, "rate": true,
		"irate": true, "increase": true, "by": true, "without": true, "on": true,
		"ignoring": true, "group_left": true, "group_right": true, "offset": true,
		"bool": true, "and": true, "or": true, "unless": true, "clamp_min": true,
		"clamp_max": true, "histogram_quantile": true, "le": true, "quantile": true,
		// 时间函数：返回当前时间戳（不是指标）。
		"time": true, "timestamp": true, "absent": true, "vector": true, "scalar": true,
		"job": true, "instance": true, "env": true, "service": true,
		// 本项目指标用到的标签名。
		"route": true, "status": true, "method": true, "name": true, "result": true,
		"db": true, "event_type": true, "namespace": true,
	}
	// 标签匹配器 {result="bloom_rejected"} 与记录规则名 job:xxx:rate5m 不是指标。
	labelMatcher := regexp.MustCompile(`\{[^}]*\}`)
	recordName := regexp.MustCompile(`\b(?:job|instance):[a-z0-9_:]+`)
	groupName := regexp.MustCompile(`\btokmall\.[a-z_]+`)
	// YAML 结构键。
	yamlKeys := map[string]bool{
		"record": true, "expr": true, "alert": true, "for": true, "labels": true,
		"annotations": true, "summary": true, "description": true, "groups": true,
		"rules": true, "interval": true, "severity": true, "warning": true,
		"critical": true, "true": true, "false": true,
	}

	checked := 0
	for _, file := range ruleFiles {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			// 只扫描表达式：`expr: ...` 与 `- record: ...`。
			if !strings.HasPrefix(trimmed, "expr:") && !strings.HasPrefix(trimmed, "- record:") {
				continue
			}
			cleaned := labelMatcher.ReplaceAllString(trimmed, " ")
			cleaned = recordName.ReplaceAllString(cleaned, " ")
			cleaned = groupName.ReplaceAllString(cleaned, " ")
			for _, token := range identifier.FindAllString(cleaned, -1) {
				if promqlSyntax[token] || yamlKeys[token] || registered[token] {
					checked++
					continue
				}
				t.Fatalf("规则文件 %s 引用了未注册的指标或未知标识符 %q（这条规则永远不会触发）：%s",
					file, token, trimmed)
			}
		}
	}
	require.Positive(t, checked, "至少要校验到一些标识符，否则说明扫描逻辑失效了")
}

// TestPrometheusRulesCoverKeySubsystems：规则文件必须覆盖"出问题会静默"的子系统。
// 缺一条不是 bug，但覆盖度是评审时要看的东西，所以用测试固定下来。
func TestPrometheusRulesCoverKeySubsystems(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "observability", "prometheus", "rules", "tokmall.yml"))
	require.NoError(t, err)
	content := string(raw)

	for _, expected := range []string{
		"outbox_pending_events", "outbox_dead_events", "reconcile_last_run_ok",
		"http_server_duration_seconds_bucket", "db_pool_in_use_connections",
		"feed_cache_hit_total", "breaker_state", "existence_lookup_total",
	} {
		require.Containsf(t, content, expected, "规则文件缺少对 %s 的覆盖", expected)
	}
	// 记录规则命名必须遵循 level:metric:operations 约定。
	require.Regexp(t, `record: (job|instance):[a-z0-9_]+(:[a-z0-9_]+)?`, content,
		"记录规则要用 level:metric:operations 命名")
}

// registeredMetricNames 读出与 main 相同的 collector 会暴露的指标名。
//
// 用 Describe 而不是 Gather：带标签的向量在没有子指标时不会出现在 Gather 结果里，
// 而规则文件引用的恰恰是"还没产生数据"的那些指标名——用 Gather 会把它们误判成不存在。
func registeredMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://user:pass@127.0.0.1:1/none?sslmode=disable")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	registry := prometheus.NewRegistry()
	collectors := []prometheus.Collector{observability.NewDBPoolCollector(db, "tokmall")}
	collectors = append(collectors,
		observability.NewFeedCacheStats(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewOutboxRelayMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewOutboxConsumerMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewReconcileMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewExistenceMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewHTTPMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewBreakerMetrics(registry).Collectors()...)
	collectors = append(collectors,
		observability.NewOrderTimeoutMetrics(registry).Collectors()...)

	names := map[string]bool{}
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	for _, collector := range collectors {
		descriptions := make(chan *prometheus.Desc, 256)
		go func(c prometheus.Collector) {
			c.Describe(descriptions)
			close(descriptions)
		}(collector)
		for desc := range descriptions {
			if match := fqName.FindStringSubmatch(desc.String()); match != nil {
				names[match[1]] = true
			}
		}
	}

	// 直方图的派生序列（_bucket/_count/_sum）要显式补上：Describe 只给出基础名。
	// 本项目只有两个直方图，这里显式列出而不是靠命名猜测。
	for _, histogram := range []string{"http_server_duration_seconds", "db_pool_wait_duration_seconds"} {
		require.Truef(t, names[histogram], "直方图 %s 必须已注册（否则测试假设失效）", histogram)
		for _, suffix := range []string{"_bucket", "_count", "_sum"} {
			names[histogram+suffix] = true
		}
	}
	require.NotEmpty(t, names)
	return names
}
