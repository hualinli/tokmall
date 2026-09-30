/**
 * 生成 UUID v4。
 *
 * 为什么不直接用 crypto.randomUUID()：该 API 只在「安全上下文」（https 或 localhost）
 * 才存在，用局域网 IP 走 http 访问开发环境时它是 undefined，渲染期调用会抛
 * TypeError 让整页白屏。这里回退到 getRandomValues（不受安全上下文限制）手工拼 v4。
 */
export function randomUUID(): string {
  const webCrypto = globalThis.crypto
  if (typeof webCrypto?.randomUUID === 'function') {
    return webCrypto.randomUUID()
  }
  const bytes = new Uint8Array(16)
  if (typeof webCrypto?.getRandomValues === 'function') {
    webCrypto.getRandomValues(bytes)
  } else {
    // 极端降级（无 WebCrypto 的老浏览器）：教学项目可接受，真实场景应直接拒绝启动
    for (let i = 0; i < 16; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40 // version 4
  bytes[8] = (bytes[8] & 0x3f) | 0x80 // variant 10xx
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
