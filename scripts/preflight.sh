#!/usr/bin/env bash
#
# scripts/preflight.sh —— 提交/推送前的本地自查：把 CI 会拦下来的事在本地先跑一遍，
# 省掉"推上去 → 等 5 分钟 → 红 → 再推"的往返。
#
#   ./scripts/preflight.sh          # 在仓库任意位置执行都行（脚本内部切到仓库根）
#
# 步骤（第一个失败就以非 0 退出，不会继续跑后面的）：
#   1. gofmt   —— 只检查本仓库的 Go 包目录，不动文件
#   2. go vet ./...
#   3. go test ./... -count=1        （-count=1 不吃测试缓存）
#   4. go mod tidy 漂移检查          （只读：tidy -diff，不写 go.mod/go.sum）
#   5. 前端 tsc -b --noEmit          （仅在 web/node_modules 已安装时执行）
#
# 受限沙箱（AI 代理 / HOME 不可写）里 Go 需要把缓存放进仓库内，自行导出即可——
# 脚本刻意不写死这些变量，否则会污染正常开发环境：
#   export GOPATH="$PWD/.gopath" GOMODCACHE="$PWD/.gopath/pkg/mod" \
#          GOCACHE="$PWD/.gocache" GOSUMDB=off

set -euo pipefail

# 无论从哪里调用，都切到脚本所在的仓库根（go.mod / web/ 都在这一层）
cd "$(dirname "${BASH_SOURCE[0]}")/.."

step_no=0
step() { step_no=$((step_no + 1)); printf '\n▶ [%d] %s\n' "$step_no" "$1"; }
ok() { printf '✅ %s\n' "$1"; }
die() { printf '❌ %s\n' "$1" >&2; exit 1; }

# ---- 1. gofmt -------------------------------------------------------------
step "gofmt 格式检查"
# 只检查本仓库的包目录：gofmt -l . 会连 .gopath/pkg/mod 这类本地缓存里的
# 第三方代码一起列出来，在沙箱里必然误报，所以用 go list 限定第一方包。
if ! pkg_dirs="$(go list -f '{{.Dir}}' ./...)"; then
	die "go list ./... 失败：先确认 Go 环境和依赖可用（go mod download）"
fi
if ! unformatted="$(printf '%s\n' "$pkg_dirs" | xargs gofmt -l | sed "s|^$(pwd)/||")"; then
	die "gofmt 执行失败（原因见上面的报错）：先修掉语法错误再重试"
fi
if [ -n "$unformatted" ]; then
	printf '%s\n' "$unformatted" | sed 's/^/   /' >&2
	die "以上文件未通过 gofmt：请运行 make fmt 后重试"
fi
ok "gofmt 通过（无未格式化文件）"

# ---- 2. go vet ------------------------------------------------------------
step "go vet ./..."
go vet ./... || die "go vet 报错：请修掉上面列出的问题"
ok "go vet 通过"

# ---- 3. go test -----------------------------------------------------------
step "go test ./... -count=1"
go test ./... -count=1 || die "测试失败：先修掉上面失败的用例"
ok "全部测试通过"

# ---- 4. go mod tidy 漂移 --------------------------------------------------
step "go mod tidy 漂移检查"
# tidy -diff（Go ≥1.23）：只打印"tidy 会怎么改"，不落盘，所以不会弄脏你的工作区。
# 退出码非 0 = 有漂移（或 tidy 本身报错，具体原因就在上面的输出里）。
if ! tidy_diff="$(go mod tidy -diff)"; then
	[ -n "$tidy_diff" ] && printf '%s\n' "$tidy_diff" >&2
	die "go.mod/go.sum 不是 tidy 后的状态：请运行 make tidy 并提交（工作区未被脚本改动）"
fi
ok "go.mod / go.sum 已是 tidy 状态"

# ---- 5. 前端类型检查（可选） ---------------------------------------------
if [ -d web ] && [ -d web/node_modules ]; then
	if [ ! -x web/node_modules/.bin/tsc ]; then
		printf '\n⚠️  跳过前端类型检查：web/node_modules 存在但没有 tsc（依赖装了一半？cd web && pnpm install）\n'
	else
		step "前端类型检查（tsc -b --noEmit）"
		(cd web && ./node_modules/.bin/tsc -b --noEmit) || die "前端类型检查失败：请看上面的 tsc 报错"
		ok "前端类型检查通过"
	fi
else
	printf '\n⏭  跳过前端类型检查（web/node_modules 不存在；需要时 cd web && pnpm install）\n'
fi

printf '\n🎉 preflight 全部通过，可以放心推送。\n'
