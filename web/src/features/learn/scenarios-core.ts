import type { Scenario } from './scenario'

export const assemblyScenarios: Scenario[] = [
  {
    id: 'layers',
    title: '模块只在 main 里见面',
    foundation: '分层把 HTTP 字段、业务规则和 SQL 分开。模块化单体里，跨模块的实现由组合根注入，调用方只声明自己需要的能力。',
    apply: '商品要核销封面票时，接口写在 product 包里，main 把视频服务传进去。商品包不引用视频表的 model。',
    code: {
      file: 'cmd/api/main.go',
      lines: '271–276',
      body: `productService := product.NewService(
    productRepository,
    storageClient,
    videoService, // ImageTicketValidator
)`,
    },
  },
  {
    id: 'nil',
    title: '关掉缓存时必须是真的空接口',
    foundation: '接口值里如果装着一个 nil 指针，接口本身不等于 nil。后面用 != nil 判断会放行，一调用方法就 panic。',
    apply: 'Feed 缓存关闭时，main 使用真正的 nil 接口变量，不把 nil 的 *RedisFeedCache 赋给接口。',
    code: {
      file: 'cmd/api/main.go',
      lines: '193–197',
      body: `// 必须用 nil 接口变量。直接声明 *feed.RedisFeedCache 再赋给接口，
// 关闭时会得到 typed nil：接口判 nil 为真但方法调用即 panic。
var feedCache feed.FirstPageCache
var feedInvalidator video.FeedCacheInvalidator`,
    },
  },
  {
    id: 'middleware',
    title: '每个请求先经过的五段',
    foundation: '恢复、请求号、追踪、指标、访问日志是横切能力，不写进每个 handler。指标标签用路由模板，避免把订单号这种高基数放进标签。',
    apply: '顺序是 Recovery、request id、traceparent、HTTP 指标、访问日志。业务 4xx 记警告，未知错误才是 500。',
    code: {
      file: 'cmd/api/main.go',
      lines: '160–171',
      body: `h.Use(
    middleware.Recovery(),
    requestid.New(...),
    middleware.Trace(),
    middleware.HTTPMetrics(httpMetrics),
    middleware.AccessLog(),
)`,
    },
  },
  {
    id: 'secrets',
    title: '签名密钥在所有环境都必填',
    foundation: '令牌和游标一旦能被伪造，身份和翻页位置就不可信。生产还要足够长，并且两把密钥不能相同。',
    apply: 'JWT_SECRET 和 FEED_CURSOR_SECRET 缺少时进程起不来。生产要求至少 32 字符，且两者不同，数据库、Redis、对象存储走 TLS。',
    code: {
      file: 'internal/config/config.go',
      lines: '470–476',
      body: `jwtSecret, err := requiredEnv("JWT_SECRET")
cursorSecret, err := requiredEnv("FEED_CURSOR_SECRET")`,
    },
  },
  {
    id: 'proxies',
    title: '限流故障放行，代理地址默认不信',
    foundation: '限流是保护，不是业务本身。依赖挂了时放行，主路径还在；暴力破解场景要另配拒绝。客户端 IP 只相信明确列出的代理。',
    apply: 'Redis 限流失败时打警告并放行。TRUSTED_PROXIES 默认空，空表示不采用 X-Forwarded-For。配成信任所有地址时启动失败。',
    code: {
      file: 'internal/middleware/ratelimit.go',
      lines: '136–141',
      body: `if err != nil {
    slog.Warn("rate limiter unavailable, fail-open", ...)
    ctx.Next(c)
    return
}`,
    },
  },
  {
    id: 'breaker',
    title: '对象不存在不算熔断故障',
    foundation: '熔断统计的是依赖坏了。正常的「没有这个对象」或「大小不合法」如果也计成故障，几次 404 就能把存储打成打开。打开之后对外应是 503，而不是假装成功。',
    apply: '存储客户端包在 breaker.Do 里。打开时不会执行存储调用，直接返回 ErrOpen，视频接口再映射成 503。对象不存在和无效大小不计入故障。存储暂时连不上，不阻止进程启动。',
    code: {
      file: 'internal/breaker/breaker.go',
      lines: '182–185',
      body: `case StateOpen:
    b.stats.Rejected(b.cfg.Name)
    return ErrOpen`,
    },
  },
  {
    id: 'workers',
    title: '后台任务先停消费者',
    foundation: '和 API 住在同一个进程里的循环任务，停机要有顺序。先停还会写读模型的一方，再停还在投递的一方。',
    apply: '对象清理没有开关。中继先于消费者启动，所以 stopAll 按启动的反序停：先消费者，再中继。HTTP 先停，最多等 30 秒。',
    code: {
      file: 'cmd/api/main.go',
      lines: '381–389',
      body: `// stopAll 逆序停机：先停消费者，再停中继。
for i := len(w.stops) - 1; i >= 0; i-- {
    if err := w.stops[i](timeout); err != nil && firstErr == nil {
        firstErr = err
    }
}`,
    },
  },
  {
    id: 'observe',
    title: '用户编号进日志，不进指标',
    foundation: '观测要能把一次请求串起来，又不能让标签基数爆炸。敏感字段名要脱敏。',
    apply: 'slog 上下文带上请求号、trace、路由。user_id 只出现在日志里。Prometheus 记录规则的名字按 level:metric:operations 检查。',
    code: {
      file: 'internal/observability/logcontext.go',
      lines: '45–47',
      body: `// WithUserID 放入已认证用户（只进日志，绝不进指标标签）。
func WithUserID(ctx context.Context, userID int64) context.Context {
    return context.WithValue(ctx, ctxKeyUserID, userID)
}`,
    },
  },
]

export const authScenarios: Scenario[] = [
  {
    id: 'optional',
    title: '没带票据和票据无效',
    foundation: '可选登录要区分匿名和「客户端以为自己还登录着」。坏票据如果被当成匿名，用户会看到另一套数据。',
    apply: '没有 Authorization 时 user_id 为 0。带了头但签名错、过期，或拿 refresh 当 access，中间件直接 401。',
    code: {
      file: 'internal/middleware/auth.go',
      lines: '28–37',
      body: `if string(ctx.GetHeader("Authorization")) == "" {
    ctx.Set(CtxUserIDKey, int64(0))
    ctx.Next(c)
    return
}`,
    },
  },
  {
    id: 'register',
    title: '注册写成可用账号',
    foundation: '邮箱要规范化，密码要哈希。状态如果留空，可能绕过数据库默认值，写出一个不可用或不受约束的用户。',
    apply: '邮箱走 NormalizeEmail，密码 bcrypt，状态显式写成 ACTIVE。邮箱或用户名占用返回对应冲突。身份不从请求体里的 user_id 读取。',
    code: {
      file: 'internal/user/service.go',
      lines: '239–246',
      body: `user := User{
    Email: email, Username: username, PasswordHash: string(hash),
    Status: UserStatusActive,
}`,
    },
  },
  {
    id: 'login',
    title: '登录不暴露账号是否存在',
    foundation: '用户不存在如果立刻返回，响应时间会和密码错误不同，调用方能枚举邮箱。不可用账号也不该单独报「已停用」。',
    apply: '找不到用户仍比一次假的密码哈希。账号不可用和密码错误是同一个错误。access 大约 15 分钟，refresh 大约 7 天。',
    code: {
      file: 'internal/user/service.go',
      lines: '283',
      body: `_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(p.Password))`,
    },
  },
  {
    id: 'claim',
    title: '刷新票据只能用一次',
    foundation: '两个人同时拿同一张刷新票据来换，如果先查后写，会发出两套都能用的新票。抢占必须是原子的。',
    apply: 'Redis SET NX 抢 jti。抢到的人作废旧票并签发新的一对。抢到之后如果签发失败，旧票也回不去。access 每个请求只验签，不查这张票有没有被拉黑。',
    code: {
      file: 'internal/user/blacklist.go',
      lines: '43–50',
      body: `func (b *RedisRefreshTokenBlacklist) Claim(...) (bool, error) {
    ok, err := b.client.SetNX(ctx, refreshBlacklistKey(jti), "1", ttl).Result()
    return ok, err
}`,
    },
  },
  {
    id: 'owner',
    title: '资源是不是你的，在服务端再对一次',
    foundation: '令牌只说明是谁。能不能改这条视频、商品或订单，要拿令牌里的用户和资源上的作者或商家比较。对不上时，和不存在用同一种响应，避免探出记录在不在。',
    apply: '作者、商家、买家是同一张 users 表，没有单独的商家角色。停用在下次刷新时生效。当前这张 access 只做 HS256 验签，并要求带过期时间。',
    code: {
      file: 'internal/token/token.go',
      lines: '48–58',
      body: `token, err := jwt.ParseWithClaims(tokenString, claims, keyfunc,
    jwt.WithValidMethods([]string{"HS256"}),
    jwt.WithExpirationRequired(),
)`,
    },
  },
]

export const uploadScenarios: Scenario[] = [
  {
    id: 'ticket',
    title: '上传票只允许写入暂存键',
    foundation: '大文件不要经应用进程中转。预签名把一次 PUT 收成限定的键、大小、类型和过期时间。',
    apply: '视频 1MiB 到 512MiB，图片 1KiB 到 5MiB。客户端拿到的 object_key 不含前缀，签名路径是 staging/ 加上这个键。票在签名成功之后才入库。',
    code: {
      file: 'internal/video/service.go',
      lines: '270',
      body: `uploadURL, err := s.storage.PresignPutURL(ctx, "staging/"+objectKey, fileSize, contentType)`,
    },
  },
  {
    id: 'seal',
    title: '封存核对真实大小和 ETag',
    foundation: '签名里的长度可以被绕过。封存时看对象的实际大小，并用当时的 ETag 保证复制的是刚刚检查过的那一版。',
    apply: 'StatObject 之后按 MatchETag 复制到正式键。事务先把票证行锁住并标成已用，视频行在同一事务插入。',
    code: {
      file: 'internal/storage/storage.go',
      lines: '84–100',
      body: `info, err := c.mc.StatObject(...)
if info.Size <= 0 || info.Size > maxSize {
    return ErrInvalidObjectSize
}
c.mc.CopyObject(..., CopySrcOptions{MatchETag: info.ETag})`,
    },
  },
  {
    id: 'orphan',
    title: '对象存储不跟着数据库回滚',
    foundation: '两边没有分布式事务。数据库能保证票证和视频行一起出现；已经复制成功的正式对象，不会因为后面的 SQL 失败而消失。',
    apply: '复制成功后事务若回滚，正式键上留下没有视频引用的对象。这是已知边界，目前没有孤儿回收。staging 的删除排到原 PUT 过期之后，避免旧签名还在写。',
    code: {
      file: 'internal/video/cleanup_worker.go',
      lines: '80–83',
      body: `DELETE FROM upload_tickets
 WHERE expires_at < now() - interval '1 minute'
-- 再插入 staging/{key}，available_at = expires_at + 1 minute`,
    },
  },
  {
    id: 'publish',
    title: '发布靠条件更新，不靠先读',
    foundation: '先读再改挡不住两个请求同时通过预检。真正互斥的是更新语句带上期望的原状态，影响行数为 0 就是冲突。',
    apply: '创建直接落在待发布。只有 PASSED 或 SKIPPED 能发布。更新还要求作者匹配。SKIPPED 表示这次没验，探测关掉时新建视频就是这个状态。',
    code: {
      file: 'internal/video/repository.go',
      lines: '69–83',
      body: `Where("id = ? AND author_id = ? AND status = ?", id, authorID, from)
if to == StatusPublished {
    query = query.Where("media_probe_status IN ?", []string{Passed, Skipped})
}
if res.RowsAffected == 0 { return ErrStatusConflict }`,
    },
  },
  {
    id: 'probe',
    title: '探测结果只接受一次',
    foundation: '多个 worker 可以重复下载同一个文件。终态只能写一次，已删除的视频不能被探测改回可见。工具缺失和文件损坏是两种结果。',
    apply: '取件要求媒体仍是 PENDING、视频未删除、重试时间已到。写结果时对行加锁，条件更新影响一行才算接受。坏文件是 REJECTED；工具缺失是 SKIPPED，并且不写 probed_at。',
    code: {
      file: 'internal/video/probe_repository.go',
      lines: '59–61',
      body: `Where("id = ? AND media_probe_status = ? AND status <> ?",
    videoID, ProbePending, StatusDeleted)`,
    },
  },
  {
    id: 'visible',
    title: '未发布对访客是 404，但记录还在',
    foundation: '看不见和不存在要能区分开，至少在写缓存的时候。把「作者才能看」缓存成「世界上没有这条」，作者自己会在一段时间内打不开。',
    apply: '已删除对所有人不可见。未发布只给作者。负缓存只在数据库确认没有这条记录时写入；本地布隆过滤器已删除，不参与判定。',
    code: {
      file: 'internal/video/service.go',
      lines: '700–712',
      body: `if ok, err := s.existence.MayExist(...); err == nil && !ok {
    return nil, ErrNotFound
}
// 可见性拒绝不 MarkMissing`,
    },
  },
  {
    id: 'bloom',
    title: '布隆过滤器已删除',
    foundation: '每个进程一份过滤器时，这一份说没有，别的进程可能刚刚插入过，所以阴性从不参与判定，只剩内存和每 10 分钟全表重建的成本。',
    apply: 'MayExist 只看 Redis 负缓存：命中才短路 404，读失败放行到数据库。挡住穿透的是负缓存，不是布隆。',
    code: {
      file: 'internal/existence/guard.go',
      lines: '74–88',
      body: `// MayExist 只在负缓存确认 miss 时返回 false。缓存故障放行到数据库。
if missing {
    return false, nil
}
return true, nil`,
    },
  },
  {
    id: 'tool',
    title: '工具缺失和文件损坏是两回事',
    foundation: '解析失败说明文件不像一条可播放的视频。机器上没有探测工具，说明这次没验成，不能把视频当成坏的。',
    apply: 'mediaprobe 把 ErrToolMissing 和 ErrProbeFailed 分开。worker 见到工具缺失写成 SKIPPED，文件坏了写成 REJECTED。校验关闭时新建视频直接是 SKIPPED，作者仍可以发布。',
    code: {
      file: 'internal/mediaprobe/probe.go',
      lines: '27–32',
      body: `// ErrProbeFailed：文件损坏或不是视频。
// ErrToolMissing：没有 ffprobe/ffmpeg，调用方应跳过校验。
ErrToolMissing = errors.New("ffprobe/ffmpeg not installed")`,
    },
  },
]
