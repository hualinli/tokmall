GO ?= go
BIN := bin/api

# 开发环境默认值（可用 make dev JWT_SECRET=xxx 覆盖；生产环境不应使用 Makefile 启动）
JWT_SECRET ?= test
API_LOG := logs/api.log
WEB_LOG := logs/web.log

# macOS 无 setsid，后台起服务依赖 bash 的 disown
SHELL := /bin/bash

.PHONY: build run fmt vet test tidy check clean dev dev-backend dev-web stop logs

build:
	$(GO) build -o $(BIN) ./cmd/api

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
	(env APP_ENV=development JWT_SECRET=$(JWT_SECRET) nohup $(BIN) > $(API_LOG) 2>&1 < /dev/null &)
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
