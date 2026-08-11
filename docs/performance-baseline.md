# 性能基线（M3 末）

> 记录时间：2026-08-06。M4（Redis + singleflight）之后的任何压测，用同样的方法跑，数字才有可比性。

## 方法

| 项 | 值 |
|---|---|
| 工具 | wrk 4.2.0（kqueue） |
| 参数 | `-t4 -c50 -d10s` |
| 机器 | 本机 macOS（Apple Silicon） |
| 服务 | 单实例 `go run ./cmd/api`（debug 构建，非 -ldflags 优化） |
| 数据库 | PostgreSQL 16 Docker 容器（e2e 实例，5433），连接池 max_open=25 |
| 数据量 | videos 表 7 条 PUBLISHED 记录（**注意：不是百万级数据**） |

> ⚠️ 数据量警告：7 条数据压的是"框架开销 + 连接池吞吐"，不是真实查询性能。
> M4 压测前必须按 mentor 手册 §6 验证节造 100 万条数据再比。

## 结果

| 接口 | QPS | 平均延迟 | 备注 |
|---|---|---|---|
| `GET /api/v1/feed`（未缓存） | **12,219** | 4.9ms | 压测期间 db_pool_wait_count 累计 ~12 万，25 连接接近饱和 |
| `POST /api/v1/auth/login` | **84.7** | 545ms | bcrypt cost 10 为瓶颈，~70ms/次哈希 |

## 可复现命令

```bash
# feed
wrk -t4 -c50 -d10s http://localhost:8080/api/v1/feed

# login（POST + JSON body）
cat > /tmp/login.lua <<'EOF'
wrk.method = "POST"
wrk.path = "/api/v1/auth/login"
wrk.headers["Content-Type"] = "application/json"
wrk.body = '{"identifier":"alice12","password":"password123"}'
EOF
wrk -t4 -c50 -d10s -s /tmp/login.lua http://localhost:8080
```

## 观察与结论

1. **读路径**：12k QPS 无缓存，池接近饱和但延迟稳定——M4 引入 Redis 后对比目标：QPS 提升 + db_pool_wait_count 下降。
2. **登录**：~85 QPS 是 bcrypt 的设计代价（防暴力破解优先）。优化候选：cost 降到 8、换 Argon2id、或接受现状（冷路径）。**不要**在没有业务压力的情况下为压测数字动认证成本。
3. **指标链路已验证**：hertz throughput 计数器与 wrk 数字吻合（122,523 ≈ 12,219×10），metrics 可信。

---

# M4 缓存对比（2026-08-07）

> 同一台机器、同一个 e2e 容器、同样的 100 万条数据，唯一变量是 FEED_CACHE_ENABLED。

## 方法

| 项 | 值 |
|---|---|
| 工具 | vegeta（`-duration=10s -rate=0 -max-workers=50`，无限速） |
| 数据量 | videos 表 **100 万条 PUBLISHED**（`scripts/seed_perf.sql` 造数），30k 用户 |
| 场景 A | `FEED_CACHE_ENABLED=false`（无缓存，直查 PG） |
| 场景 B | 缓存开（TTL=60s），压测前已 warm |
| 缓存命中 | `feed_cache_hit_total` / `feed_cache_miss_total`（:9091/metrics） |

## 结果

| 场景 | QPS | p50 | p90 | 最大 | db_pool_wait_count | feed_cache 命中率 |
|---|---|---|---|---|---|---|
| M3 基线（7 行，无缓存） | 12,219 | - | - | - | ~120k | - |
| **1M 无缓存** | **6,576** | 5.4ms | 14.1ms | 220ms | **58,706** | - |
| **1M 有缓存** | **11,337** | 3.1ms | 6.9ms | 218ms | **0** | **99.999%**（113,378/1） |

## 观察与结论

1. **1M 数据让 DB 路径慢了 ~46%**（12.2k → 6.6k QPS）：索引深度 + 数据体积，符合预期。M3 的"7 行基线"确实不能代表真实规模。
2. **缓存把 DB 压力归零**：db_pool_wait_count 从 58,706 → 0，连接池从瓶颈变成空闲。
3. **缓存后的新瓶颈是 Redis GET + JSON 反序列化**（~11.3k QPS 封顶，命中路径每次请求一次 Get + 一次 Unmarshal）：对比 7 行基线还略低，说明现在测的是 Redis 吞吐而非 DB。进一步优化方向（不进 M4）：命中时直接返回原始字节、或预序列化 bytes 缓存。
4. **99.999% 命中率是 10s 压测的假象**：TTL 60s 意味着每 60s 有一次 miss + 回填。真实流量下周期性击穿的防护靠 singleflight（已实现）+ double-checked locking。
5. **压测结论**：M4 目标达成——QPS 提升 1.72x（6.6k→11.3k），延迟减半，DB 完全脱出热路径。

## 可复现命令

```bash
# 造数（幂等）
docker exec -i tokmall-postgres-e2e psql -U tokmall -d tokmall < scripts/seed_perf.sql

# 场景 A：无缓存
FEED_CACHE_ENABLED=false DB_PORT=5433 JWT_SECRET=test MINIO_ENDPOINT=localhost:9000 ./tokmall-api &
vegeta attack -targets=/tmp/feed-targets.txt -duration=10s -rate=0 -max-workers=50 | vegeta report

# 场景 B：有缓存（先 warm 两次再压）
DB_PORT=5433 JWT_SECRET=test MINIO_ENDPOINT=localhost:9000 ./tokmall-api &
curl -s localhost:8080/api/v1/feed > /dev/null && curl -s localhost:8080/api/v1/feed > /dev/null
vegeta attack -targets=/tmp/feed-targets.txt -duration=10s -rate=0 -max-workers=50 | vegeta report
```
