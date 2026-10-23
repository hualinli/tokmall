import type { StudyPoint } from './types'

export const assemblyPoints: StudyPoint[] = [
  {
    id: 'layers',
    title: '三层各自守什么',
    foundation:
      '接口、规则和存储的变化速度不同。混在一个函数里时，改一条 SQL 会碰到 HTTP 状态码，改一个字段名会碰到事务。分层是让每一层只翻译一次。',
    apply:
      'handler 读 JSON，从中间件拿 user_id，用 errors.Is 把已知领域错误映射成 4xx。对不上的错误是 500，并记 Error。业务拒绝记警告。repository 把 GORM 的 not found 译成领域哨兵，列名和状态在边界上转成 string。model 不出 service。',
    code: {
      file: 'cmd/api/main.go',
      lines: '271–276',
      body: `productService := product.NewService(
    productRepository,
    storageClient,
    videoService, // ImageTicketValidator：封面防伪造复用视频模块的凭证核销
)`,
    },
  },
  {
    id: 'consumer-interface',
    title: '接口写在调用方',
    foundation:
      '如果商品包 import 视频包的表结构，视频改一列，商品就要跟着编译。调用方只声明自己需要的方法，实现由组合根注入，依赖就停在方法签名上。',
    apply:
      '封面防伪、关注流失效、热榜计分都是这样接的。订单的退款仓库也是同一份 GormRepository，再按消费侧接口注入一次。Feed 的商品卡是调用方自己的 SQL，不借用商品 model。跨模块窄函数如 coupon.DiscountFor 由订单直接调用，这是刻意留下的少数例外。',
    code: {
      file: 'cmd/api/main.go',
      lines: '299–305',
      body: `orderRepository := order.NewRepository(db)
orderService := order.NewService(orderRepository)
// 同一 GormRepository 同时实现订单命令与退款命令；注入分开是为了让
// service 只依赖它真正使用的消费侧接口。
orderService.SetRefundRepository(orderRepository)`,
    },
  },
  {
    id: 'middleware',
    title: '每个请求先经过的五段',
    foundation:
      '恢复、追踪和访问日志要在业务之前装上，后面的 handler 才能把同一次请求的日志对上。指标标签用路由模板，避免把订单号、用户 ID 这种高基数写进时序库。',
    apply:
      '顺序是 Recovery、请求号（UUIDv7）、traceparent、HTTP 指标、访问日志。4xx 记警告，5xx 记错误。路由组上再分：/api/v1 按 IP 限读；登录注册再按账号限流，请求体 16KiB；写接口在登录之后按用户限流。公开 Feed 不强制登录。',
    code: {
      file: 'cmd/api/main.go',
      lines: '160–171',
      body: `h.Use(
    middleware.Recovery(),
    requestid.New(...),
    // 顺序：requestid → trace（需要 requestid 写入 context）→ 指标/访问日志
    middleware.Trace(),
    middleware.HTTPMetrics(httpMetrics),
    middleware.AccessLog(),
)`,
    },
  },
  {
    id: 'fail-open',
    title: '限流故障与代理信任',
    foundation:
      '限流是保护，不是业务正确性的来源。依赖挂了就拒绝所有流量，等于把限流器变成了更大的故障。信任反向代理则相反：配错会让别人伪造客户端 IP，从而绕过按 IP 的限额。',
    apply:
      'Redis 限流失败时放行并记警告。TRUSTED_PROXIES 默认空，空表示不信 X-Forwarded-For，IP 用 TCP 对端。配成 0.0.0.0/0 时进程拒绝启动。对象不存在、大小不合法不计熔断故障；熔断打开后对外是 503。存储连不上不阻止启动。',
    code: {
      file: 'internal/middleware/ratelimit.go',
      lines: '136–141',
      body: `if err != nil {
    // fail-open：不阻塞请求，但必须留痕
    slog.Warn("rate limiter unavailable, fail-open", ...)
    ctx.Next(c)
    return
}`,
    },
  },
  {
    id: 'typed-nil',
    title: '关掉缓存时的空接口',
    foundation:
      'Go 的接口里如果装着一个 nil 指针，接口本身不等于 nil。调用方用 if cache != nil 会放行，接着就在 nil 指针上调方法。',
    apply:
      'Feed 缓存和关注流缓存关闭时，注入的必须是真正的 nil 接口，不能先声明具体指针类型再赋给接口。存在性防护、热榜也是可选装配：配置关闭就不 Set。',
    code: {
      file: 'cmd/api/main.go',
      lines: '193–198',
      body: `// 必须用 nil 接口变量。直接声明 *feed.RedisFeedCache 再赋给接口，
// 关闭时会得到 "typed nil"——接口判 nil 为真但方法调用即 panic。
var feedCache feed.FirstPageCache
var feedInvalidator video.FeedCacheInvalidator
if cfg.FeedCacheEnabled {`,
    },
  },
  {
    id: 'workers',
    title: '后台任务与逆序停机',
    foundation:
      '和 API 同进程的任务，停机顺序要和数据流方向相反。先停还会写入下游的那一个，再停上游，避免停的过程中继续灌入。',
    apply:
      '始终启动对象清理。按配置再启动媒体探测、热榜窗口、支付超时、退款自动同意、自动确认、Outbox 中继、对账、消费者（OUTBOX_TRANSPORT=postgres 时改为直接消费，不启动中继）。中继先于消费者启动，所以停机先停消费者再停中继。HTTP 先停，最多等 30 秒。/metrics 探活失败只记日志，API 仍会起来。对象清理没有开关。',
    code: {
      file: 'cmd/api/main.go',
      lines: '381–390',
      body: `// stopAll 逆序停机：先停消费者（停止写入读模型），再停中继（停止投递）。
func (w *backgroundWorkers) stopAll(timeout time.Duration) error {
    for i := len(w.stops) - 1; i >= 0; i-- {
        if err := w.stops[i](timeout); err != nil && firstErr == nil {
            firstErr = err
        }
    }
    return firstErr
}`,
    },
  },
]

export const assemblyQuiz = [
  '商品包为什么不 import 视频的 model，封面票却还能核销？',
  '限流的 Redis 挂了，请求会怎样？TRUSTED_PROXIES 为空时，客户端 IP 从哪来？',
  '为什么关缓存时不能把一个 nil 指针装进接口？',
]
