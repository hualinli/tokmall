import { defineConfig, devices } from '@playwright/test'

/**
 * E2E：覆盖核心产品链路（认证/Feed/互动/上传）。
 * 前置条件：后端已在 8080 运行（Vite 代理转发），Redis/RustFS 就绪。
 *
 * 串行执行：上传测试会发布视频进公共 Feed，共享时间线状态。
 */

/**
 * 移动端视口（Feed 是整屏短视频 UI，用例里的坐标/滚动都按 390 宽写）。
 * 必须放在 project 级 use：顶层 use.viewport 会被 project 的
 * devices['Desktop Chrome']（viewport 1280x720）整体覆盖，等于没生效。
 */
const MOBILE_VIEWPORT = { width: 390, height: 844 }

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: [['list']],
  use: {
    baseURL: 'http://localhost:5173',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: MOBILE_VIEWPORT } },
  ],
  webServer: {
    command: 'pnpm dev',
    url: 'http://localhost:5173',
    reuseExistingServer: true,
  },
})
