/**
 * 登录/注册回跳地址的白名单校验（防开放重定向）。
 *
 * 为什么不能只判断 startsWith('/')：`//evil.com` 是协议相对 URL，浏览器会跳到外站；
 * `/\evil.com`、`/\/evil.com` 也会被 URL 解析器归一化成 `//evil.com`；
 * `https://evil.com` 之类更直接。所以必须做「解析后同源」判定，而不是字符串前缀。
 *
 * @param raw 来自 URL query 的原始回跳地址
 * @param base 当前站点 origin（默认取 window.location.origin，便于单测注入）
 * @returns 安全的站内相对路径（pathname + search + hash）；不合法返回 null
 */
export function safeRedirectPath(raw: string | null | undefined, base?: string): string | null {
  if (!raw) return null
  const origin = base ?? (typeof window === 'undefined' ? '' : window.location.origin)
  if (!origin) return null
  // 必须以单个 '/' 开头：排除绝对 URL、协议相对 URL（//host）与反斜杠变体
  if (!raw.startsWith('/') || raw.startsWith('//') || raw.startsWith('/\\')) return null
  if (raw.includes('\\')) return null
  try {
    const url = new URL(raw, origin)
    // 解析后仍必须同源：兜住编码/归一化带来的各种绕过
    if (url.origin !== origin) return null
    return `${url.pathname}${url.search}${url.hash}`
  } catch {
    return null
  }
}
