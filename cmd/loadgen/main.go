// Command loadgen 是下单/支付链路的压测客户端。
//
// 为什么不用 wrk：这条链路不是"打一个 URL"，而是"注册 → 建商品 → 补库存 →
// 并发下单 → 支付"的有状态流程，且要区分业务失败（库存不足）与系统失败
// （5xx/超时）。这些语义只有自己写客户端才能表达清楚。
//
// 用法（需要后端在跑）：
//
//	loadgen -scenario commerce -concurrency 50 -duration 20s -stock 100000
//	loadgen -scenario feed     -concurrency 100 -duration 10s
//
// 输出：成功率、业务失败分类、QPS、延迟分位（p50/p95/p99/max）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	base        string
	scenario    string
	concurrency int
	duration    time.Duration
	stock       int64
	quantity    int
	timeout     time.Duration
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.base, "base", "http://localhost:8080", "后端地址")
	flag.StringVar(&cfg.scenario, "scenario", "commerce", "场景：commerce（下单+支付）或 feed（读）")
	flag.IntVar(&cfg.concurrency, "concurrency", 50, "并发 worker 数")
	flag.DurationVar(&cfg.duration, "duration", 20*time.Second, "持续时长")
	flag.Int64Var(&cfg.stock, "stock", 1000000, "预置库存")
	flag.IntVar(&cfg.quantity, "quantity", 1, "每单件数")
	flag.DurationVar(&cfg.timeout, "timeout", 10*time.Second, "单请求超时")
	flag.Parse()

	client := &http.Client{Timeout: cfg.timeout}
	runner := &runner{cfg: cfg, client: client, statuses: map[string]*atomic.Int64{}}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration+2*time.Minute)
	defer cancel()

	var err error
	switch cfg.scenario {
	case "commerce":
		err = runner.runCommerce(ctx)
	case "feed":
		err = runner.runFeed(ctx)
	case "penetration":
		err = runner.runPenetration(ctx)
	default:
		err = fmt.Errorf("unknown scenario %q", cfg.scenario)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
	runner.report()
}

type runner struct {
	cfg      config
	client   *http.Client
	statuses map[string]*atomic.Int64
	mu       sync.Mutex
	latency  []time.Duration
	success  atomic.Int64
	business atomic.Int64
	system   atomic.Int64
}

func (r *runner) record(status string, latency time.Duration, kind resultKind) {
	r.mu.Lock()
	if counter, ok := r.statuses[status]; ok {
		counter.Add(1)
	} else {
		counter := &atomic.Int64{}
		counter.Add(1)
		r.statuses[status] = counter
	}
	r.latency = append(r.latency, latency)
	r.mu.Unlock()

	switch kind {
	case kindSuccess:
		r.success.Add(1)
	case kindBusiness:
		r.business.Add(1)
	case kindSystem:
		r.system.Add(1)
	}
}

type resultKind int

const (
	kindSuccess resultKind = iota
	// kindBusiness：预期内的业务失败（库存不足、幂等冲突），不算系统故障。
	kindBusiness
	kindSystem
)

// runCommerce 准备数据后并发下单 + 支付。
func (r *runner) runCommerce(ctx context.Context) error {
	fmt.Println("准备数据：注册商家、建商品、补库存…")
	merchant, err := r.register(ctx, "merchant")
	if err != nil {
		return fmt.Errorf("register merchant: %w", err)
	}
	productID, skuID, err := r.createSellableSKU(ctx, merchant.AccessToken)
	if err != nil {
		return fmt.Errorf("create product: %w", err)
	}
	if err := r.adjustStock(ctx, merchant.AccessToken, productID, skuID, r.cfg.stock); err != nil {
		return fmt.Errorf("adjust stock: %w", err)
	}
	fmt.Printf("准备完成：sku=%d stock=%d\n", skuID, r.cfg.stock)

	buyers := make([]session, r.cfg.concurrency)
	for i := range buyers {
		session, err := r.register(ctx, fmt.Sprintf("buyer%d", i))
		if err != nil {
			return fmt.Errorf("register buyer %d: %w", i, err)
		}
		buyers[i] = session
	}

	fmt.Printf("开始压测：并发=%d 时长=%s\n", r.cfg.concurrency, r.cfg.duration)
	deadline := time.Now().Add(r.cfg.duration)
	var wg sync.WaitGroup
	for i := range buyers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			buyer := buyers[index]
			for sequence := 0; time.Now().Before(deadline); sequence++ {
				key := fmt.Sprintf("load-%d-%d-%d", index, sequence, time.Now().UnixNano())
				orderID, amount, kind, err := r.createOrder(ctx, buyer.AccessToken, skuID, key)
				if err != nil || kind != kindSuccess {
					continue
				}
				r.payOrder(ctx, buyer.AccessToken, orderID, amount, key)
			}
		}(i)
	}
	wg.Wait()
	return nil
}

// runPenetration 压"缓存穿透"路径：反复请求**不存在**的详情 ID。
//
// 这是缓存穿透防护的验收场景：没有防护时每个请求都会打到数据库
// （B+ 树查空），有防护时应该被布隆过滤器/负缓存挡在应用层。
// 通过 /metrics 里的 existence_lookup_total 可以看到请求最终走到了哪一层。
func (r *runner) runPenetration(ctx context.Context) error {
	fmt.Printf("开始压测穿透路径（不存在的视频 ID）：并发=%d 时长=%s\n", r.cfg.concurrency, r.cfg.duration)
	deadline := time.Now().Add(r.cfg.duration)
	var wg sync.WaitGroup
	for worker := range r.cfg.concurrency {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			sequence := 0
			for time.Now().Before(deadline) {
				// 高段位 ID 保证不存在，且每个请求都换一个（避免负缓存把同一 ID 的
				// 重复请求挡掉后测不出布隆过滤器的效果）。
				id := 900_000_000 + worker*1_000_000 + sequence
				sequence++
				started := time.Now()
				response, err := r.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/videos/%d", id), "", nil)
				latency := time.Since(started)
				if err != nil {
					r.record("network_error", latency, kindSystem)
					continue
				}
				switch response.status {
				case http.StatusNotFound:
					r.record("not_found", latency, kindSuccess)
				case http.StatusTooManyRequests:
					r.record("rate_limited", latency, kindBusiness)
				default:
					r.record(fmt.Sprintf("penetration_%d", response.status), latency, kindSystem)
				}
			}
		}(worker)
	}
	wg.Wait()
	return nil
}

// runFeed 压读路径（首页 Feed），用于和 performance-baseline.md 的历史数据对照。
func (r *runner) runFeed(ctx context.Context) error {
	fmt.Printf("开始压测读路径：并发=%d 时长=%s\n", r.cfg.concurrency, r.cfg.duration)
	deadline := time.Now().Add(r.cfg.duration)
	var wg sync.WaitGroup
	for range r.cfg.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				started := time.Now()
				response, err := r.do(ctx, http.MethodGet, "/api/v1/feed?limit=20", "", nil)
				latency := time.Since(started)
				if err != nil {
					r.record("network_error", latency, kindSystem)
					continue
				}
				kind := kindSuccess
				if response.status >= 500 {
					kind = kindSystem
				} else if response.status >= 400 {
					kind = kindBusiness
				}
				r.record(fmt.Sprintf("%d", response.status), latency, kind)
			}
		}()
	}
	wg.Wait()
	return nil
}

type session struct {
	AccessToken string
	UserID      int64
}

func (r *runner) register(ctx context.Context, role string) (session, error) {
	suffix := fmt.Sprintf("%s-%d-%d", role, time.Now().UnixNano(), rand.Intn(1_000_000))
	body := map[string]string{
		"email":    suffix + "@load.test",
		"username": uniqueUsername(),
		"password": "password123",
	}
	response, err := r.do(ctx, http.MethodPost, "/api/v1/auth/register", "", body)
	if err != nil {
		return session{}, err
	}
	if response.status != http.StatusCreated {
		return session{}, fmt.Errorf("register %s: status=%d body=%s", role, response.status, response.body)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		User        struct {
			UserID int64 `json:"user_id"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(response.body), &payload); err != nil {
		return session{}, err
	}
	return session{AccessToken: payload.AccessToken, UserID: payload.User.UserID}, nil
}

// uniqueUsername 生成 6..15 位的合法用户名（首字符字母、其余字母数字）。
// 压测要能反复运行，因此用时间戳 + 随机数保证唯一；截断到 15 位是
// 为了满足业务校验规则（^[a-zA-Z][a-zA-Z0-9]{5,15}$）。
func uniqueUsername() string {
	raw := fmt.Sprintf("u%d%06d", time.Now().UnixNano()%1_000_000_000, rand.Intn(1_000_000))
	if len(raw) > 15 {
		raw = raw[:15]
	}
	for len(raw) < 6 {
		raw += "0"
	}
	return raw
}

func (r *runner) createSellableSKU(ctx context.Context, token string) (int64, int64, error) {
	response, err := r.do(ctx, http.MethodPost, "/api/v1/products", token, map[string]string{
		"title": "loadgen product", "description": "loadgen",
	})
	if err != nil {
		return 0, 0, err
	}
	if response.status != http.StatusOK {
		return 0, 0, fmt.Errorf("create product: status=%d body=%s", response.status, response.body)
	}
	var product struct {
		ProductID int64 `json:"product_id"`
	}
	if err := json.Unmarshal([]byte(response.body), &product); err != nil {
		return 0, 0, err
	}

	response, err = r.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/products/%d/skus", product.ProductID), token,
		map[string]any{"spec": "default", "price": 100})
	if err != nil {
		return 0, 0, err
	}
	if response.status != http.StatusCreated {
		return 0, 0, fmt.Errorf("create sku: status=%d body=%s", response.status, response.body)
	}
	var sku struct {
		SKUID int64 `json:"sku_id"`
	}
	if err := json.Unmarshal([]byte(response.body), &sku); err != nil {
		return 0, 0, err
	}
	return product.ProductID, sku.SKUID, nil
}

func (r *runner) adjustStock(ctx context.Context, token string, productID, skuID, stock int64) error {
	path := fmt.Sprintf("/api/v1/products/%d/skus/%d/stock", productID, skuID)
	response, err := r.do(ctx, http.MethodPost, path, token, map[string]any{
		"change": stock, "idempotency_key": fmt.Sprintf("loadgen-stock-%d", time.Now().UnixNano()),
	})
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return fmt.Errorf("adjust stock: status=%d body=%s", response.status, response.body)
	}
	return nil
}

func (r *runner) createOrder(ctx context.Context, token string, skuID int64, key string) (int64, int64, resultKind, error) {
	started := time.Now()
	response, err := r.do(ctx, http.MethodPost, "/api/v1/orders", token, map[string]any{
		"sku_id": skuID, "quantity": r.cfg.quantity, "idempotency_key": key,
	})
	latency := time.Since(started)
	if err != nil {
		r.record("network_error", latency, kindSystem)
		return 0, 0, kindSystem, err
	}
	switch response.status {
	case http.StatusCreated:
		var payload struct {
			OrderID     int64 `json:"order_id"`
			TotalAmount int64 `json:"total_amount"`
		}
		if err := json.Unmarshal([]byte(response.body), &payload); err != nil {
			r.record("decode_error", latency, kindSystem)
			return 0, 0, kindSystem, err
		}
		r.record("order_created", latency, kindSuccess)
		return payload.OrderID, payload.TotalAmount, kindSuccess, nil
	case http.StatusConflict:
		// 库存不足是压测的**预期终点**：它证明"不超卖"这条不变量在起作用。
		r.record("order_conflict", latency, kindBusiness)
		return 0, 0, kindBusiness, nil
	case http.StatusTooManyRequests:
		// 限流生效同样是"预期内的保护行为"：它说明按用户的写配额在起作用。
		// 压测链路本身要么提高并发用户数，要么显式关闭限流（RATE_LIMIT_ENABLED=false）。
		r.record("order_429", latency, kindBusiness)
		return 0, 0, kindBusiness, nil
	default:
		r.record(fmt.Sprintf("order_%d", response.status), latency, kindSystem)
		return 0, 0, kindSystem, fmt.Errorf("create order: status=%d body=%s", response.status, response.body)
	}
}

func (r *runner) payOrder(ctx context.Context, token string, orderID, amount int64, key string) {
	started := time.Now()
	response, err := r.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/orders/%d/pay", orderID), token, map[string]any{
		"payment_no": "pay-" + key, "provider_trade_no": "trade-" + key, "amount": amount,
	})
	latency := time.Since(started)
	if err != nil {
		r.record("network_error", latency, kindSystem)
		return
	}
	switch response.status {
	case http.StatusOK:
		r.record("order_paid", latency, kindSuccess)
	case http.StatusConflict:
		r.record("pay_conflict", latency, kindBusiness)
	default:
		r.record(fmt.Sprintf("pay_%d", response.status), latency, kindSystem)
	}
}

type httpResult struct {
	status int
	body   string
}

func (r *runner) do(ctx context.Context, method, path, token string, body any) (*httpResult, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.cfg.base+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return &httpResult{status: response.StatusCode, body: string(payload)}, nil
}

func (r *runner) report() {
	r.mu.Lock()
	latencies := append([]time.Duration(nil), r.latency...)
	statuses := map[string]int64{}
	for status, counter := range r.statuses {
		statuses[status] = counter.Load()
	}
	r.mu.Unlock()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	total := len(latencies)
	fmt.Printf("\n=== 压测结果 ===\n")
	fmt.Printf("总请求数 %d（成功 %d / 业务失败 %d / 系统失败 %d）\n",
		total, r.success.Load(), r.business.Load(), r.system.Load())
	if total > 0 {
		elapsed := r.cfg.duration.Seconds()
		fmt.Printf("吞吐 %.1f req/s（按 %.0fs 计）\n", float64(total)/elapsed, elapsed)
		fmt.Printf("延迟 p50=%s p95=%s p99=%s max=%s\n",
			percentile(latencies, 0.50), percentile(latencies, 0.95),
			percentile(latencies, 0.99), latencies[total-1])
	}
	keys := make([]string, 0, len(statuses))
	for key := range statuses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Println("分类统计：")
	for _, key := range keys {
		fmt.Printf("  %-18s %d\n", key, statuses[key])
	}
}

func percentile(sorted []time.Duration, ratio float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * ratio)
	return sorted[index]
}
