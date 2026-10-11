# 验证范围与宿主执行限制

本步代码构建前，以下命令已正常完成（显式开启 TOKMALL_RUN_PG_INTEGRATION=1 与 TOKMALL_RUN_REDIS_INTEGRATION=1，真实PG隔离schema、真实Redis）：

```sh
go test -p 1 ./internal/order ./internal/outbox ./internal/attribution ./internal/reconcile ./internal/feed ./internal/database ./cmd/api -count=1 -timeout=120s
make lint
```

七个包通过；工具输出分别为order 3.176s、outbox 1.666s、attribution 0.441s、reconcile 1.450s、feed 0.729s、database 0.371s、cmd/api 0.373s，lint为0 issues。随后构建API和reconcile，实际二进制SHA记录于environment.json。没有在这些检查后修改Go源代码。

六轮k6均正常退出，九项对账均通过。压测数据和业务事实在aggregate.json及每轮summary/reconcile.json中交叉验证；没有人工重放。

压测结束后的可选全套 `go test -p 1 ./...` 补跑未完成。首轮完成到promotion包，reconcile临时目录测试二进制在启动前卡住（RSS约16KiB、CPU 0，尚未执行断言）。转为复制相同测试二进制到项目bin目录的 `-exec` 补跑时，Go协调进程本身又卡在启动前，约3分50秒无进度；同版本的另一本地签名Go副本连 `go version` 也约1分43秒未启动。停止了这些仅属于本任务的阻塞进程，保留partial-full-suite.log作为部分执行记录，不记为全套通过。

macOS程序启动卡顿此前也出现于工具、临时目录测试程序；本地副本/签名仅用于项目忽略目录，不改变工具版本、测试断言或系统安全策略。启动问题没有被当作代码测试失败或成功。真实对象存储开关未开启，未重跑浏览器E2E和完整迁移回滚。本步必须门禁与改动相关回归已通过，但不能宣称最终全套Go复测完成。
