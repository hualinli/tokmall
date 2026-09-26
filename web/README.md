# TokMall 前端

React + TypeScript + Vite + TanStack Router/Query + Tailwind。API 类型从仓库的 [`internal/api/openapi.yaml`](../internal/api/openapi.yaml) 生成；接口形状不要在页面里手写另一套。前端当前覆盖 Feed、登录注册、上传发布、互动、商品管理/购买及订单查询与模拟履约。

## 本地运行

在仓库根目录启动完整开发环境：

```bash
make dev
# 前端 http://localhost:5173；后端 http://localhost:8080
```

Vite 将 `/api` 请求代理到本地后端。只开发前端时先确保后端及 PostgreSQL、Redis、MinIO 已运行，再执行：

```bash
cd web
pnpm install
pnpm dev
```

项目用模拟支付与模拟发货，不接收真实款项，也没有真实物流。订单页是 `/orders`，支持买家/商家视角和订单详情。

## 验证与生成

```bash
cd web
pnpm gen       # OpenAPI 类型 + TanStack 路由树
pnpm typecheck
pnpm lint
pnpm build
pnpm test      # Vitest 已配置，当前尚无前端单测文件
pnpm test:e2e  # Playwright；需后端与依赖容器在运行
```

根目录的 `go test ./...` 负责后端与 OpenAPI 路由双向同步测试。Playwright 的核心交易用例覆盖视频挂商品、下单模拟支付、商家模拟发货、买家确认完成和作者统计。测试完成后可执行 `make stop && docker compose stop postgres minio redis` 停止本轮启动的服务。
