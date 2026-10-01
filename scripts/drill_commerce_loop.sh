#!/usr/bin/env bash
# 端到端业务闭环演练：注册 → 商品/SKU/库存 → 上传发布视频 → 挂商品 →
# 下单 → 支付 → 退款 → 校验 Outbox 投递、幂等投影与指标。
#
# 它和 Playwright e2e 的区别：这个脚本只用 curl + psql，任何一步失败都会
# 打印响应原文，便于在面试现场或故障演练时"当场看到链路"。
#
# 前置：make dev（后端 :8080、PostgreSQL :5432、Redis :6380、MinIO :9000）
# 用法：scripts/drill_commerce_loop.sh
set -euo pipefail
trap 'echo "[drill] 命令失败于第 $LINENO 行" >&2' ERR

API=${API:-http://localhost:8080}
PSQL=${PSQL:-"docker exec tokmall-postgres psql -U tokmall -d tokmall -tA"}
SUFFIX=$(date +%s)
PASS='password123'

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }

# api METHOD PATH TOKEN [BODY]：统一 curl 封装，非 2xx 直接失败并打印响应。
api() {
  local method=$1 path=$2 token=$3 body=${4:-}
  local args=(-sS -X "$method" "$API$path" -H 'Content-Type: application/json')
  if [[ -n $token ]]; then
    args+=(-H "Authorization: Bearer $token")
  fi
  if [[ -n $body ]]; then
    args+=(-d "$body")
  fi
  local response status
  response=$(curl "${args[@]}" -w '\n%{http_code}')
  status=$(tail -n1 <<<"$response")
  response=$(sed '$d' <<<"$response")
  if [[ $status -lt 200 || $status -ge 300 ]]; then
    fail "$method $path -> $status: $response"
  fi
  printf '%s' "$response"
}

json() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }

say "1/9 注册商家与买家"
MERCHANT=$(api POST /api/v1/auth/register '' "{\"email\":\"drill-merchant-$SUFFIX@example.com\",\"username\":\"dm$SUFFIX\",\"password\":\"$PASS\"}")
MERCHANT_TOKEN=$(json "['access_token']" <<<"$MERCHANT")
MERCHANT_ID=$(json "['user']['user_id']" <<<"$MERCHANT")
BUYER=$(api POST /api/v1/auth/register '' "{\"email\":\"drill-buyer-$SUFFIX@example.com\",\"username\":\"db$SUFFIX\",\"password\":\"$PASS\"}")
BUYER_TOKEN=$(json "['access_token']" <<<"$BUYER")
BUYER_ID=$(json "['user']['user_id']" <<<"$BUYER")
echo "merchant=$MERCHANT_ID buyer=$BUYER_ID"

say "2/9 创建商品与 SKU 并补库存"
PRODUCT=$(api POST /api/v1/products "$MERCHANT_TOKEN" "{\"title\":\"演练商品 $SUFFIX\",\"description\":\"drill\"}")
PRODUCT_ID=$(json "['product_id']" <<<"$PRODUCT")
SKU=$(api POST "/api/v1/products/$PRODUCT_ID/skus" "$MERCHANT_TOKEN" '{"spec":"默认","price":1999}')
SKU_ID=$(json "['sku_id']" <<<"$SKU")
api POST "/api/v1/products/$PRODUCT_ID/skus/$SKU_ID/stock" "$MERCHANT_TOKEN" '{"change":5,"idempotency_key":"drill-stock-'"$SUFFIX"'"}' >/dev/null
echo "product=$PRODUCT_ID sku=$SKU_ID stock=5"

say "3/9 申请预签名上传 URL 并直传 MinIO"
UPLOAD=$(api POST /api/v1/videos/upload-url "$MERCHANT_TOKEN" '{"filename":"drill.mp4","file_size":1048576,"content_type":"video/mp4"}')
OBJECT_KEY=$(json "['object_key']" <<<"$UPLOAD")
PUT_URL=$(json "['upload_url']" <<<"$UPLOAD")
head -c 1048576 /dev/urandom > /tmp/drill-video.mp4
curl -sS -X PUT "$PUT_URL" -H 'Content-Type: video/mp4' --data-binary @/tmp/drill-video.mp4 >/dev/null
echo "object_key=$OBJECT_KEY"

say "4/9 创建视频并发布"
VIDEO=$(api POST /api/v1/videos "$MERCHANT_TOKEN" "{\"title\":\"演练视频 $SUFFIX\",\"object_key\":\"$OBJECT_KEY\",\"duration_seconds\":10}")
VIDEO_ID=$(json "['video_id']" <<<"$VIDEO")
api POST "/api/v1/videos/$VIDEO_ID/publish" "$MERCHANT_TOKEN" >/dev/null
api POST "/api/v1/videos/$VIDEO_ID/products" "$MERCHANT_TOKEN" "{\"product_id\":$PRODUCT_ID}" >/dev/null
echo "video=$VIDEO_ID published & linked"

say "5/9 买家下单（带来源归因）"
ORDER=$(api POST /api/v1/orders "$BUYER_TOKEN" "{\"sku_id\":$SKU_ID,\"quantity\":1,\"idempotency_key\":\"drill-order-$SUFFIX\",\"source\":{\"video_id\":$VIDEO_ID,\"author_id\":$MERCHANT_ID,\"product_id\":$PRODUCT_ID}}")
ORDER_ID=$(json "['order_id']" <<<"$ORDER")
AMOUNT=$(json "['total_amount']" <<<"$ORDER")
echo "order=$ORDER_ID amount=$AMOUNT"

say "6/9 支付（业务事务 + Outbox 事件同事务提交）"
api POST "/api/v1/orders/$ORDER_ID/pay" "$BUYER_TOKEN" "{\"payment_no\":\"pay-$SUFFIX\",\"provider_trade_no\":\"trade-$SUFFIX\",\"amount\":$AMOUNT}" >/dev/null
sleep 2  # 给中继与消费者一个投递周期

say "7/9 校验 Outbox 状态与作者日销投影"
$PSQL -c "SELECT 'outbox_published=' || count(*) FROM outbox_events WHERE status='PUBLISHED' AND aggregate_id=$ORDER_ID"
$PSQL -c "SELECT 'outbox_pending_due=' || count(*) FROM outbox_events WHERE status='PENDING' AND available_at <= now()"
$PSQL -c "SELECT 'outbox_pending_delayed=' || count(*) || '（超时事件，到 expires_at 才投递）' FROM outbox_events WHERE status='PENDING' AND available_at > now()"
$PSQL -c "SELECT 'projection_gmv=' || coalesce(sum(gmv),0) FROM author_sales_daily WHERE author_id=$MERCHANT_ID"
$PSQL -c "SELECT 'receipts=' || count(*) FROM consumed_events WHERE aggregate_id=$ORDER_ID"

say "8/9 买家申请退款 → 商家同意（未发货，库存应回补）"
api POST "/api/v1/orders/$ORDER_ID/refund" "$BUYER_TOKEN" '{"reason":"演练退款"}' >/dev/null
api POST "/api/v1/merchant/orders/$ORDER_ID/refund/approve" "$MERCHANT_TOKEN" '{"remark":"同意"}' >/dev/null
sleep 2
$PSQL -c "SELECT 'order_status=' || status FROM orders WHERE id=$ORDER_ID"
$PSQL -c "SELECT 'stock=' || quantity FROM inventory WHERE sku_id=$SKU_ID"
$PSQL -c "SELECT 'projection_refunded=' || coalesce(sum(refunded_amount),0) FROM author_sales_daily WHERE author_id=$MERCHANT_ID"

say "9/9 关键指标"
curl -sS localhost:9091/metrics | grep -E '^(outbox_pending_events|outbox_dead_events|outbox_published_total|outbox_consumer_processed_total|outbox_consumer_duplicate_total)' | head -12

say "演练完成：内容 → 商品 → 下单 → 支付 → 事件 → 投影 → 退款 全部走通"
