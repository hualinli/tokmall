# 后端压测原始证据

结论与适用范围集中在 [性能基线](../docs/performance-baseline.md)，这里解释证据结构与复现方法。

`2026-10-09/smoke` 是5秒工具验收，不用作简历性能指标。`2026-10-09/formal` 是正式实验：Feed A/B/C各180秒×3轮，50并发；交易单热点/100分散SKU，各10/50/100并发、120秒×3轮。API、压测端与Docker同机，属于固定条件下的吞吐对照，不证明独立部署的容量上限。限流关闭，权限、幂等、事务、账本与默认INFO日志保留；媒体使用模拟元数据，支付是项目模拟支付。

## 文件

- `environment.json`：机器、运行时、数据库、资源上限、初始数据、Git提交与三份二进制SHA256。
- 提交归档前已脱敏本机用户名、主机名与异常栈中的本机路径；性能数字、环境规格和源码包保持原值。
- `source.tar.gz`、`working-tree.patch`：测量版本源码与未提交差异；源码包包括当时未跟踪的压测工具。
- 每组目录的 `configuration.json`：实际关键开关；`summary.json`：该轮汇总。
- `loadgen.json`（归档后为 `.json.gz`）：逐请求/逐成功交易原始耗时（微秒），以及创建、支付、完整交易的各自分位数。吞吐分母为实际测量时长，包含最后在途请求收尾；买家/SKU准备不计入。
- `metrics-before.prom`、`metrics-after.prom`、`samples.jsonl`：计数增量与每5秒采样；`database-before/after.json`：PG统计；`containers-*.txt`：容器资源采样。CPU/RSS是采样观察，后台worker也包含在API进程中。
- 交易组的 `reconcile.json`、`summary.json.database_facts`：九项对账与订单、支付、库存、流水、到期/未来Outbox事实。未构造券、退款、归因订单，相关检查无差异不代表这些场景受过压力。
- `aggregate.json/csv`：已完成轮次的中位数与范围。P95/P99取每轮分位数的中位数，不是合并样本的分位数；最终结论要求每组完整三轮。

## 复现

依照 [开发运维](../docs/development-and-operations.md) 编译API、loadgen、reconcile并准备标准端口容器。确保Redis DB15专用且为空，18080/19091可用。每次新实验使用新目录，避免把不同构建混为同一实验。

```bash
python3 scripts/benchmark_backend.py --output benchmarks/new-run \
  --suite all --rounds 3 --feed-seconds 180 --commerce-seconds 120 --clients 10,50,100
python3 scripts/summarize_benchmark.py benchmarks/new-run
```

协调器在已有PG实例中新建 `tokmall_bench_*` 数据库，自动迁移并造100万视频/3万用户；会清空其交易表与Redis DB15，每轮API使用临时目录记录并轮转INFO日志。结束时停止自己创建的API/客户端，保留专用数据库和桶供核验。核验后只按 `environment.json` 的明确资源名清理，不操作业务数据库。失败后可以同目录继续，但要保留相同构建与实验资源；完整实验结束并清理资源后不能同目录重启。

数据库池等待计数已修正为PG连接池真实累计值，反复抓取不会重复累加；旧实现产生的历史等待次数不能直接与新值对比。Feed仍按请求装配点赞/商品卡，命中缓存不意味着完全免查询。
