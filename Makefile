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

# 默认目标 = make help：目标多了以后，"有哪些命令、各自干什么"比默认去编译更有用。
.DEFAULT_GOAL := help

.PHONY: help build run fmt vet test tidy tidy-check verify coverage vulncheck check clean \
	dev dev-backend dev-web stop logs drill drill-failure reconcile outbox-status loadgen

# 目标清单由本文件的行尾注释生成：新增目标只要补上 "## 说明" 就会自动出现在 help 里，
# 不需要维护第二份会腐烂的清单（awk 用行尾 "## " 之前/之后切两段）。
help: ## 显示所有可用目标及一行说明（默认目标）
	@echo "TokMall Makefile 目标："
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) \
		| awk -F':.*## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "提交前门禁：make verify（fmt → vet → test → tidy-check）"

build: ## 编译 api / outboxctl / reconcile / loadgen 到 bin/
	$(GO) build -o $(BIN) ./cmd/api
	$(GO) build -o bin/outboxctl ./cmd/outboxctl
	$(GO) build -o bin/reconcile ./cmd/reconcile
	$(GO) build -o bin/loadgen ./cmd/loadgen

run: ## 前台运行 api（开发模式）
	$(GO) run ./cmd/api

fmt: ## gofmt 原地格式化所有 Go 代码
	$(GO) fmt ./...

vet: ## go vet 静态检查
	$(GO) vet ./...

test: ## 跑全部 Go 测试
	$(GO) test ./...

tidy: ## 整理 go.mod / go.sum（会改文件，改完需提交）
	$(GO) mod tidy

# 只读校验：tidy -diff 只打印差异、不写文件，退出码非 0 即"需要 tidy"。
# 与 CI 里的 `go mod tidy && git diff --exit-code` 语义等价，但不会弄脏本地工作区。
tidy-check: ## 校验 go.mod/go.sum 已是 tidy 状态（只读，不改文件）
	@$(GO) mod tidy -diff || { echo "❌ go.mod/go.sum 需要 tidy：请运行 make tidy 并提交"; exit 1; }
	@echo "✅ go.mod / go.sum 已是 tidy 状态"

# CI 的四道门禁本地一次跑完。顺序有意为之：先 fmt 自动修好格式，再 vet/test 验证，
# 最后确认依赖文件没有漂移——跑完就是"可以直接提交"的状态。
verify: fmt vet test tidy-check ## 本地跑 CI 同款门禁（fmt → vet → test → tidy 漂移）
	@echo "✅ 本地门禁全部通过（fmt / vet / test / tidy-check）"

# -coverpkg=./... 让每个包都按"整体代码"统计（否则未测包会拉低/虚高数字）；
# 排除 web（前端不是 Go 包，防止未来出现 Go 工具时才在这里踩坑）。
coverage: ## 生成 coverage.out 并打印总覆盖率（-coverpkg=./...）
	$(GO) test -covermode=atomic -coverprofile=coverage.out -coverpkg=./... $$($(GO) list ./... | grep -v /web)
	@echo "── 总覆盖率 ──"
	@$(GO) tool cover -func=coverage.out | tail -1

# govulncheck 只对"可达"漏洞报警（比单纯比对版本号少 90% 噪音）。
vulncheck: ## 已知漏洞扫描（govulncheck 缺失时自动 go install）
	@command -v govulncheck >/dev/null 2>&1 || $(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	@PATH="$$PATH:$$($(GO) env GOPATH)/bin" govulncheck ./...

check: fmt vet test ## fmt + vet + test（历史快捷目标）
	@echo "✅ check 通过（fmt / vet / test）"

clean: ## 删除构建产物 bin/
	rm -rf $(BIN)

# 一键开发环境：依赖容器 + 后端 + 前端（全部后台运行）
dev: ## 起依赖容器 + 后端 + 前端（后台，日志 logs/）
	docker compose up -d postgres minio redis
	$(MAKE) dev-backend
	$(MAKE) dev-web
	@echo "✅ 前端 http://localhost:5173  后端 http://localhost:8080"
	@echo "   日志: tail -f $(API_LOG) / $(WEB_LOG) | make stop 停服"

# 仅起后端（编译最新代码 + 后台运行，日志落 logs/api.log）
# 子 shell + </dev/null：后台进程彻底脱离进程树，避免父 shell/管道等待挂起
dev-backend: ## 只起后端（编译后后台运行，日志 logs/api.log）
	$(MAKE) build
	@mkdir -p $(dir $(API_LOG))
	(env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) nohup $(BIN) > $(API_LOG) 2>&1 < /dev/null &)
	@sleep 2 && echo "✅ 后端已起 (PID $$(pgrep -f 'bin/api'))"

# 仅起前端（Vite dev server，日志落 logs/web.log）
dev-web: ## 只起前端（Vite dev server，日志 logs/web.log）
	@mkdir -p $(dir $(WEB_LOG))
	(cd web && nohup pnpm dev > ../$(WEB_LOG) 2>&1 < /dev/null &)
	@sleep 4 && echo "✅ 前端已起 (http://localhost:5173)"

# 停掉前后端（幂等，进程不在时静默跳过）
stop: ## 停掉前后端（幂等）
	-kill -9 $$(pgrep -f "bin/api") 2>/dev/null || true
	-kill -9 $$(pgrep -f "vite") 2>/dev/null || true
	@echo "✅ 前后端已停止"

monitoring: ## 启动本地观测栈（Prometheus + Grafana，含记录/告警规则与预置看板）
	DOCKER_CONFIG=$(CURDIR)/.docker-anon docker compose --profile monitoring up -d prometheus grafana
	@echo "Prometheus: http://localhost:9090  Grafana: http://localhost:3000（匿名可进，看板已 provision）"
	@echo "提示：先 make dev-backend，让应用指标在 127.0.0.1:9091 有数据"

logs: ## 跟踪前后端日志（Ctrl-C 退出）
	tail -f $(API_LOG) $(WEB_LOG)

# 端到端业务闭环演练（内容 → 商品 → 下单 → 支付 → 事件 → 投影 → 退款）
drill: build ## 端到端业务闭环演练（curl + psql 全程可见）
	@bash scripts/drill_commerce_loop.sh

# 故障演练（Redis 停机、中继停摆、事件丢失与对账重放）
drill-failure: build ## 故障演练（Redis 停机 / 事件丢失 / 对账重放）
	@bash scripts/drill_failure_modes.sh

# 对账（有差异时退出码 1）；make reconcile REBUILD=1 可先从事实重建投影
reconcile: build ## 六项对账检查（REPLAY=1 重放 / REBUILD=1 重建读模型）
	@env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) \
		./bin/reconcile $(if $(REBUILD),--rebuild,) $(if $(REPLAY),--replay,)

outbox-status: build ## Outbox 积压与死信一览（outboxctl status）
	@env APP_ENV=development JWT_SECRET=$(JWT_SECRET) FEED_CURSOR_SECRET=$(FEED_CURSOR_SECRET) ./bin/outboxctl status

# 下单链路压测（限流需自行关闭：make dev-backend RATE_LIMIT_ENABLED=false 不支持，请用 env 起服务）
loadgen: build ## 压测（SCENARIO=commerce|feed CONCURRENCY=50 DURATION=20s）
	@./bin/loadgen -scenario $(or $(SCENARIO),commerce) -concurrency $(or $(CONCURRENCY),50) -duration $(or $(DURATION),20s)
