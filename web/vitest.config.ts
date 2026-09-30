import { defineConfig } from 'vitest/config'

// vitest 只管 src 下的单测；Playwright e2e（e2e/*.spec.ts）由 pnpm test:e2e 运行。
// 不排除的话 vitest 会把 @playwright/test 的 describe 当单测执行而报错。
export default defineConfig({
  test: {
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    // 纯函数单测（formatCount / 缓存更新器 / safe-redirect / 重试策略），无文件时也保持绿
    passWithNoTests: true,
  },
})
