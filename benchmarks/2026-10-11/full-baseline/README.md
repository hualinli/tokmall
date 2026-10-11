# 当前完整优化版本的 k6 本机基线

结论和限制统一见 [性能基线](../../../docs/performance-baseline.md)。服务版本33c29ea，Feed L1+L2开启，关闭隔离实例限流；权限、事务、幂等、日志、后台worker与对账保留。测试不修改业务库，Redis DB15经空库检查后专用于实验。

## 证据

- `environment.json`：机器、版本、数据集、隔离资源名及服务/工具二进制SHA256。
- `aggregate.json`：逐档结果与三轮复测中位数/范围；分位数是各轮分位数的中位数，没有合并样本。
- 每档 `k6-summary.json` / `summary.json`：请求、完整交易、分位数、HTTP与业务检查，以及交易数据库事实和对账退出码。
- `metrics-before/after.prom`、`samples.jsonl`、`database-before/after.json`：负载前后快照、每5秒资源采样及PG统计。after指标在负载结束时采集；交易facts和reconcile在随后等待消费收敛后采集，两者时间点不同。
- `reconcile.json`：九项对账详细结果；失败档保留投影和回执差异，不把HTTP200当作闭环成功。
- `source.tar.gz`：服务、迁移、依赖清单和最终测试工具源码；`service-working-tree.patch`记录服务范围差异。本轮未改变服务实现。
- 完整逐事件原始k6输出保留在本机 `.gocache/benchmarks/2026-10-11-full-baseline/*/samples-k6.json.gz`；未放入Git归档。`raw-sha256.json`记录原始文件校验和。清理Go缓存前应另行备份此原始目录，紧凑归档不是逐请求原始数据的替代品。

Feed每轮检查每个响应含20个视频。交易每个VU使用独立买家，创建订单并模拟支付，携带实际建立的作者/视频/商品关系；来源视频为模拟已发布媒体。三轮有效交易同时核对k6成功数、支付事实、库存/流水、投影和回执。测试没有用券、退款、真实视频传输或真实资金渠道。

首个10并发探索档曾提示顶层gracefulStop字段不识别；随后改为scenario级配置，三轮Feed确认及交易确认均使用修正配置。交易准备后的清理漏掉归因视频而被外键约束阻止，修正后按相同服务/工具二进制继续；已完成轮次未覆盖或混入失败准备时间。

## 复现

准备标准端口PG/Redis/RustFS，按开发运维编译bin/api、bin/reconcile，安装本轮使用的k6 v2.3.0到bin/k6。确保Redis DB15专用且为空，18080/19091空闲。

```bash
python3 scripts/benchmark_k6.py --output .gocache/benchmarks/new-run --confirm-commerce
python3 scripts/summarize_k6.py .gocache/benchmarks/new-run benchmarks/new-run
```

需要继续中断实验时加`--resume`，须保留原隔离资源、Git HEAD和服务/工具二进制。协调器只跳过已保存summary的轮次；各交易档清理专用库交易数据和测试归因视频，禁止并行使用DB15。脚本在发生HTTP错误或对账差异后停止该SKU分布的更高并发探索；确认轮次失败则停止其后续确认。

本轮测试进程已结束，专用PG库保留最后一轮热点SKU确认的数据，失败档的数据事实与对账结果保存在各档归档中。Redis DB15及测试用桶也保留；后续只能按environment.json中的明确资源名清理，不能清空业务库或Redis DB0。
