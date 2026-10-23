import type { StudyPoint } from './types'

export const authPoints: StudyPoint[] = [
  {
    id: 'optional',
    title: '匿名和坏票据不是一回事',
    foundation:
      '可选登录的接口既给游客看，也给登录用户看个性化字段。若把过期票据当成游客，客户端会以为自己还登录着，看到的却是别人的公开数据。',
    apply:
      '没有 Authorization 时，user_id 写成 0，请求继续。头存在但格式、签名、过期不对，或者拿 refresh 当 access，中间件直接 401。handler 里的用户 ID 只从上下文取，请求体没有 user_id。',
    code: {
      file: 'internal/middleware/auth.go',
      lines: '28–37',
      body: `// 无 Authorization header → 匿名继续；
// 有 header 但无效（格式/签名/过期）→ 401。
if string(ctx.GetHeader("Authorization")) == "" {
    ctx.Set(CtxUserIDKey, int64(0))
    ctx.Next(c)
    return
}`,
    },
  },
  {
    id: 'access',
    title: 'access 只验签',
    foundation:
      '每个请求都查库确认账号仍可用，会把认证做成数据库热点，也会把一次库故障变成全站 401。短有效期把这个窗口收小，续期时再查账户。',
    apply:
      'access 约 15 分钟，HS256，必须带过期时间。停用和软删在刷新时检查。旧 access 在过期前仍可能通过。登录时用户不存在也会比一次假的密码哈希，避免用响应时间判断邮箱是否注册。不可用和密码错误是同一个错误。',
    code: {
      file: 'internal/user/service.go',
      lines: '签发',
      body: `// issueToken：access 15min、refresh 7d，claims 带 jti。
// Login：邮箱规范化；不存在仍比 dummy hash。
// !Usable() 与口令错误都返回 ErrInvalidCredentials。`,
    },
  },
  {
    id: 'claim',
    title: '刷新是抢占，不是先查后写',
    foundation:
      '先读「没用过」再签发，两个并发请求都会读到没用过，于是发出两对都能用的新票。原子的 SET NX 让同一把 jti 只有一个成功者。',
    apply:
      'Refresh 先验 refresh、查账户仍可用，再 Claim。抢到的人作废旧票并签发新的一对。抢占成功之后如果签发失败，旧票也回不去，需要重新登录。Logout 把 jti 拉黑；无效票据的登出仍算成功。',
    code: {
      file: 'internal/user/blacklist.go',
      lines: '43–50',
      body: `// Claim 用 SETNX 抢占 refresh token 的一次性使用权。
func (b *RedisRefreshTokenBlacklist) Claim(...) (bool, error) {
    ok, err := b.client.SetNX(ctx, refreshBlacklistKey(jti), "1", ttl).Result()
    return ok, err
}`,
    },
  },
  {
    id: 'owner',
    title: '归属失败和不存在是同一个 404',
    foundation:
      '「这是别人的」和「没有这条」如果返回不同状态码，调用方就能探测资源是否存在。授权按资源上的 owner ID，不靠客户端声明的角色。',
    apply:
      '作者、商家、买家是同一张 users 表。商品的 merchant_id、视频的 author_id 就是用户 ID。没有独立的 MERCHANT 角色门禁，也没有用户自助注销接口。表上有 DISABLED 和软删，公开资料只给出 ACTIVE 且未软删的用户。',
    code: {
      file: 'internal/video/service.go',
      lines: '可见性',
      body: `// Get：DELETED → NotFound
// 非作者且非 PUBLISHED → NotFound
// Delete：非作者 → NotFound；已 DELETED 再删仍成功`,
    },
  },
  {
    id: 'secrets',
    title: '两把密钥',
    foundation:
      '签名密钥泄漏之后，攻击者可以伪造票据或翻页位置。不同用途共用一把密钥时，泄漏一把就同时伪造两种凭证。',
    apply:
      'JWT_SECRET 和 FEED_CURSOR_SECRET 在所有环境必填。生产还要求至少 32 字符、彼此不同、不是占位符，并且数据库、Redis、对象存储走 TLS。游标密钥同时给视频、Feed、互动的游标用。',
    code: {
      file: 'internal/config/config.go',
      lines: 'Load / validate',
      body: `// requiredEnv：JWT_SECRET、FEED_CURSOR_SECRET，所有环境。
// 生产 validate：长度、两密钥不同、禁止占位口令、依赖必须 TLS。
// TRUSTED_PROXIES 拒绝 0.0.0.0/0。`,
    },
  },
]

export const authQuiz = [
  '带着过期 access 刷公开 Feed，会变成匿名还是 401？',
  '两个标签同时用同一张 refresh 换票，为什么只有一个成功？',
  '账号被停用后，已经发出的 access 什么时候失效？',
]
