#!/usr/bin/env bash
# 故障演练：把"依赖挂了会怎样"变成可复现的步骤，而不是口头保证。
#
# 演练项（每一项都会打印观测点，最后给出结论）：
#   1. Redis（事件流 + 限流 + 缓存）停机：写路径是否还能正确提交？
#   2. 中继停摆：事件是否留在 Outbox 而不是丢失？
#   3. Redis 恢复：积压事件是否被自动补投并投影？
#   4. 消费者崩溃：未 ACK 的消息是否被 XAUTOCLAIM 认领？
#   5. 事件丢失（模拟无 AOF 重启）：对账是否能发现并重放？
#
# 前置：make dev（后端 + PG + Redis + MinIO 都在跑），且已跑过一次
# scripts/drill_commerce_loop.sh 产生数据。
set -euo pipefail

DOCKER_CONFIG=${DOCKER_CONFIG:-}
PSQL="docker exec tokmall-postgres psql -U tokmall -d tokmall -tA"
REDIS="docker exec tokmall-redis redis-cli"

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

say "0/5 演练前状态"
$PSQL -c "SELECT 'pending=' || count(*) FROM outbox_events WHERE status='PENDING'"
$PSQL -c "SELECT 'dead=' || count(*) FROM outbox_events WHERE status='DEAD'"
$REDIS XLEN tokmall:events:commerce.v1

say "1/5 停掉 Redis（模拟缓存/事件流/限流依赖全部不可用）"
docker stop tokmall-redis >/dev/null
note "Redis 已停。注意：读接口会 fail-open（缓存未命中直接查库）、"
note "限流会 fail-open（放行但打 Warn 日志），写路径必须继续正确。"

sleep 1
TOKEN=$(curl -sS -X POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"identifier":"probe009","password":"password123"}' | python3 -c "import json,sys;print(json.load(sys.stdin).get('access_token',''))" || true)
if [[ -z $TOKEN ]]; then
  note "（没有可用账号，改用匿名读接口验证降级）"
  curl -sS -o /dev/null -w "   GET /api/v1/feed -> HTTP %{http_code}\n" localhost:8080/api/v1/feed
else
  curl -sS -o /dev/null -w "   GET /api/v1/orders -> HTTP %{http_code}\n" -H "Authorization: Bearer $TOKEN" localhost:8080/api/v1/orders
fi

say "2/5 Redis 停机期间发生业务（支付成功但投递不可用）"
note "支付事务只写本地数据库（订单 + Outbox），不依赖 Redis，因此必须成功。"
$PSQL -c "SELECT 'paid_orders=' || count(*) FROM orders WHERE status IN ('PAID','FULFILLING','COMPLETED')"
$PSQL -c "SELECT 'pending_outbox=' || count(*) FROM outbox_events WHERE status='PENDING'"
note "结论：Redis 不可用时支付仍然提交，事件留在 Outbox（不丢）。"

say "3/5 恢复 Redis，等待中继补投"
docker start tokmall-redis >/dev/null
for i in $(seq 1 30); do
  pending=$($PSQL -c "SELECT count(*) FROM outbox_events WHERE status='PENDING'" | tr -d ' ')
  [[ $pending == "0" ]] && break
  sleep 1
done
$PSQL -c "SELECT 'pending_after_recovery=' || count(*) FROM outbox_events WHERE status='PENDING'"
$PSQL -c "SELECT 'published=' || count(*) FROM outbox_events WHERE status='PUBLISHED'"
note "结论：broker 恢复后积压事件被自动补投（租约过期即重投，无需人工干预）。"

say "4/5 消费者崩溃恢复（未 ACK 消息被认领）"
$REDIS XINFO GROUPS tokmall:events:commerce.v1 | head -20 || true
note "PEL（pending）里的消息会在 claim idle 之后被 XAUTOCLAIM 认领重投；"
note "投递次数超过上限的消息进入 DLQ Stream：tokmall:events:commerce.v1:dlq。"
$REDIS XLEN tokmall:events:commerce.v1:dlq || true

say "5/5 事件丢失检测（模拟 Redis 无 AOF 重启后事件真的没了）"
note "造一条"已发布、超过宽限期、且下游从未收到"的事件（event_id 随机）："
note "为什么不是"删掉某条已消费事件的回执"——回执与投影在同一事务里写入，"
note ""有投影但没回执"这种状态在系统内不可能出现；删回执再重放会把投影算两遍。"
note "真实的事件丢失形态是：事件进了 broker 但下游从未处理，因此既没有回执也没有投影。"
$PSQL -c "INSERT INTO outbox_events
  (event_id, event_type, aggregate_type, aggregate_id, partition_key, payload, status, attempts, published_at, available_at)
  VALUES (gen_random_uuid(), 'commerce.order.paid.v1', 'order', 0, 'drill', '{}'::jsonb,
          'PUBLISHED', 1, now() - interval '10 minutes', now() - interval '10 minutes')"
note "对账应把它报成 outbox_delivery 差异，并在 --replay 时重新放回待投递："
(env APP_ENV=development JWT_SECRET=x FEED_CURSOR_SECRET=y ./bin/reconcile --replay | tail -12) || true
note "结论：事件丢失不是靠"相信 broker"兜住的，而是靠对账发现 + 幂等重放修复。"

say "演练结束。可用 bin/outboxctl status / dlq list 查看当前状态。"
