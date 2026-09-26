# TokMall

TokMall 是基于 CloudWeGo Hertz 的短视频 Feed + 商城学习项目。后端保持模块化单体，使用 PostgreSQL、Redis 和 MinIO；`web/` 是 React 前端。项目重点是内容可见性、幂等、游标分页、库存与订单状态机，以及用测试和压测验证设计取舍。

## 本地启动

按 `go.mod`、`web/package.json` 准备 Go、pnpm 和 Docker Compose，然后在仓库根目录执行：

```bash
make dev
# 前端 http://localhost:5173，API http://localhost:8080，指标 http://localhost:9091/metrics
```

```bash
go test ./...
cd web && pnpm build && pnpm test:e2e
```

`pnpm test:e2e` 需要本地 API、PostgreSQL、Redis、MinIO 运行。停止本轮环境：`make stop && docker compose stop postgres minio redis`。

## 文档

- [文档导航与当前进度](docs/README.md)
- [后端架构与模块边界](docs/backend-architecture.md)
- [Commerce v1 业务范围](docs/commerce-business.md)与[当前技术实现](docs/commerce-v1-technical-design.md)
- [OpenAPI 契约](internal/api/openapi.yaml)
- [前端说明](web/README.md)

播放事件 Kafka 化、真实支付、真实物流、秒杀与 Agent 客服均不属于目前已交付功能；准确阶段状态见文档导航。
