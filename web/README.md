# TokMall 前端

React/TypeScript/Vite/TanStack Router与Query/Tailwind，API类型来自 [OpenAPI](../internal/api/openapi.yaml)。当前页面见 [routes](src/routes/)，不要在页面手写第二套接口形状。

## 已接入范围

全局/关注Feed、登录注册/refresh/登出同步、上传与作者发布、点赞/关注/公开主页、商品/SKU/补库存/挂商品、单SKU购买、买卖双方订单/履约/全额退款、作者毛额/退款/净成交总计。

后端已实现优惠券和热榜，但前端尚无发券/领券/结账选券界面，也没有调用`/api/v1/feed/hot`。媒体worker默认开启；个人页与视频详情已展示PENDING/PASSED/REJECTED/SKIPPED状态及安全拒绝原因，等待时刷新，通过或跳过且待发布时启用发布按钮，服务端执行最终门禁。真实支付与物流均未接入；后续事项见 [roadmap](../docs/roadmap.md)。

## 运行和验证

Node22、pnpm11与CI对齐。先安装依赖，再从根目录`make dev`，前端:5173代理`/api`和`/health`到本地API:8080；配置见 [vite.config.ts](vite.config.ts)。后端环境与迁移见 [运维](../docs/operations.md)。

```bash
pnpm install --frozen-lockfile
pnpm dev
pnpm gen                    # 生成API类型与路由树
pnpm typecheck              # gen + tsc
pnpm lint
pnpm exec vitest run
pnpm build                  # gen + tsc + Vite
pnpm test:e2e               # 后端与PG/Redis/RustFS需已运行
```

生成物`src/api/types.ts`与`src/routeTree.gen.ts`提交到仓库，CI重新生成并检查漂移。根目录Go测试检查路由/schema契约，不验证浏览器交互。

[Playwright](playwright.config.ts)使用单worker，必要时启动Vite，复用已有前端；它不替你启动后端。当前E2E合成媒体要求测试后端显式`MEDIA_PROBE_ENABLED=false`，与真实媒体准入测试分开。用例覆盖认证、Feed播放/分页、社交、上传、交易/退款与[过期订单重新购买](e2e/checkout-retry.spec.ts)；关注/点赞既检查乐观UI也等待服务端事实。单测与E2E通过数是某次运行结果，不能永久写成固定覆盖承诺。

Token目前持久化在localStorage，多标签用BroadcastChannel/storage事件同步；这不防同源XSS，HttpOnly Cookie/CSRF改造仍未实现，见 [JWT 与浏览器凭据](../docs/architecture.md#auth)。
