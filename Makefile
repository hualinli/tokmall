GO ?= go
BIN := bin/api

# 开发环境默认值（可用 make dev JWT_SECRET=xxx FEED_CURSOR_SECRET=yyy 覆盖）。
# 这两个密钥在所有环境都必须显式提供：内置默认值一旦随 APP_ENV 误配带到
# 公网实例，任何人都能伪造 token 或翻页游标。这里只提供**本地开发**的取值。
JWT_SECRET ?= dev-only-jwt-secret-for-local-run
FEED_CURSOR_SECRET ?= dev-only-cursor-secret-for-local-run
API_LOG := logs/api.log
WEB_LOG := logs/web.log

# macOS 无 setsid，后台起服务依赖 bash 的 disown
SHELL := /bin/bash

.PHONY: build run fmt vet test tidy check clean dev dev-backend dev-web stop logs \
	drill drill-failure reconcile outbox-status loadgen

build:
	$(GO) build -o $(BIN) ./cmd/api
	$(GO) build -o bin/outboxctl ./cmd/outboxctl
	$(GO) build -o bin/reconcile ./cmd/reconcile
	$(GO) build -o bin/loadgen ./cmd/loadgen

run:
	$(GO) run ./cmd/api

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

check: fmt vet test

clean:
	rm -rf $(BIN)

# 一键开发环境：依赖容器 + 后端 + 前端（全部后台运行）
dev:
	docker compose up -d postgres minio redis
	$(MAKE) dev-backend
	$(MAKE) dev-web
	@echo "✅ 前端 http://localhost:5173  后端 http://localhost:8080"
	@echo "   日志: tail -f $(API_LOG) / $(WEB_LOG) | make stop 停服"

# 仅起后端（编译最新代码 + 后台运行，日志落 logs/api.log）
# 子 shell + </dev/null：后台进程彻底脱离进程树，避免父 shell/管道等待挂起
dev-backend:
	$(MAKE) build
	@mkdir -p $(dir $(API_LOG))
	(env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) nohup $(BIN) > $(API_LOG) 2>&1 < /dev/null &)
	@sleep 2 && echo "✅ 后端已起 (PID $$(pgrep -f 'bin/api'))"

# 仅起前端（Vite dev server，日志落 logs/web.log）
dev-web:
	@mkdir -p $(dir $(WEB_LOG))
	(cd web && nohup pnpm dev > ../$(WEB_LOG) 2>&1 < /dev/null &)
	@sleep 4 && echo "✅ 前端已起 (http://localhost:5173)"

# 停掉前后端（幂等，进程不在时静默跳过）
stop:
	-kill -9 $$(pgrep -f "bin/api") 2>/dev/null || true
	-kill -9 $$(pgrep -f "vite") 2>/dev/null || true
	@echo "✅ 前后端已停止"

logs:
	tail -f $(API_LOG) $(WEB_LOG)

# 端到端业务闭环演练（内容 → 商品 → 下单 → 支付 → 事件 → 投影 → 退款）
drill: build
	@bash scripts/drill_commerce_loop.sh

# 故障演练（Redis 停机、中继停摆、事件丢失与对账重放）
drill-failure: build
	@bash scripts/drill_failure_modes.sh

# 对账（有差异时退出码 1）；make reconcile REBUILD=1 可先从事实重建投影
reconcile: build
	@env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) \
		./bin/reconcile $(if $(REBUILD),--rebuild,) $(if $(REPLAY),--replay,)

outbox-status: build
	@env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) ./bin/outboxctl status

# 下单链路压测（限流需自行关闭：make dev-backend RATE_LIMIT_ENABLED=false 不支持，请用 env 起服务）
loadgen: build
	@./bin/loadgen -scenario $(or $(SCENARIO),commerce) -concurrency $(or $(CONCURRENCY),50) -duration $(or $(DURATION),20s)
