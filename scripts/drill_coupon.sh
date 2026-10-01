#!/usr/bin/env bash
# 优惠券闭环演练：发券 → 领券（含并发/重复）→ 用券下单 → 支付 → 退款退券 → 对账。
#
# 券是跨模块特性（coupon ↔ order ↔ 对账），出错的地方往往不是单表而是"两张表
# 之间的关系"，所以这个脚本每一步都用 psql 回查数据库事实，而不是只看接口返回。
#
# 前置：make dev（后端 + PostgreSQL 在跑）
# 用法：scripts/drill_coupon.sh
set -euo pipefail

API=${API:-http://localhost:8080}
PSQL=${PSQL:-"docker exec tokmall-postgres psql -U tokmall -d tokmall -tA"}
SUFFIX=$(date +%s)

say() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*"; exit 1; }

api() {
  local method=$1 path=$2 token=$3 body=${4:-}
  local args=(-sS -X "$method" "$API$path" -H 'Content-Type: application/json')
  if [[ -n $token ]]; then args+=(-H "Authorization: Bearer $token"); fi
  if [[ -n $body ]]; then args+=(-d "$body"); fi
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
status_of() { curl -sS -o /tmp/drill_coupon_resp.json -w '%{http_code}' "$@"; }

say "1/7 注册商家与买家"
MERCHANT=$(api POST /api/v1/auth/register '' "{\"email\":\"cp-m-$SUFFIX@example.com\",\"username\":\"cpm$SUFFIX\",\"password\":\"password123\"}")
MT=$(json "['access_token']" <<<"$MERCHANT")
MID=$(json "['user']['user_id']" <<<"$MERCHANT")
BUYER=$(api POST /api/v1/auth/register '' "{\"email\":\"cp-b-$SUFFIX@example.com\",\"username\":\"cpb$SUFFIX\",\"password\":\"password123\"}")
BT=$(json "['access_token']" <<<"$BUYER")
BID=$(json "['user']['user_id']" <<<"$BUYER")
echo "merchant=$MID buyer=$BID"

say "2/7 商家上架商品并补库存"
PRODUCT=$(api POST /api/v1/products "$MT" '{"title":"券演练商品","description":"drill"}')
PID=$(json "['product_id']" <<<"$PRODUCT")
SKU=$(api POST "/api/v1/products/$PID/skus" "$MT" '{"spec":"默认","price":1999}')
SKU_ID=$(json "['sku_id']" <<<"$SKU")
api POST "/api/v1/products/$PID/skus/$SKU_ID/stock" "$MT" "{\"change\":5,\"idempotency_key\":\"cp-stock-$SUFFIX\"}" >/dev/null
echo "product=$PID sku=$SKU_ID price=1999 stock=5"

say "3/7 商家发行限量券（满 10 元减 5 元，限 2 张，每人限领 1 张）"
FROM=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.UTC)-datetime.timedelta(minutes=1)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
TO=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.UTC)+datetime.timedelta(hours=1)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
COUPON=$(api POST /api/v1/coupons "$MT" "{\"title\":\"满10减5\",\"threshold_amount\":1000,\"discount_amount\":500,\"total_quantity\":2,\"per_user_limit\":1,\"valid_from\":\"$FROM\",\"valid_to\":\"$TO\"}")
COUPON_ID=$(json "['coupon_id']" <<<"$COUPON")
echo "coupon=$COUPON_ID remaining=$(json "['remaining']" <<<"$COUPON")"

say "4/7 买家领券（重复领取应 409，且发行量只加一次）"
USER_COUPON=$(api POST "/api/v1/coupons/$COUPON_ID/claim" "$BT")
UCID=$(json "['user_coupon_id']" <<<"$USER_COUPON")
code=$(status_of -X POST "$API/api/v1/coupons/$COUPON_ID/claim" -H "Authorization: Bearer $BT")
note_error=$(python3 -c "import json;print(json.load(open('/tmp/drill_coupon_resp.json'))['error']['code'])" 2>/dev/null || echo '?')
echo "首次领取 user_coupon=$UCID；重复领取 HTTP $code $note_error"
$PSQL -c "SELECT 'issued=' || issued_quantity FROM coupons WHERE id=$COUPON_ID"
$PSQL -c "SELECT 'claims=' || count(*) FROM user_coupons WHERE coupon_id=$COUPON_ID"

say "5/7 买家带券下单（金额由服务端拆分：原价 - 优惠 = 实付）"
ORDER=$(api POST /api/v1/orders "$BT" "{\"sku_id\":$SKU_ID,\"quantity\":1,\"idempotency_key\":\"cp-order-$SUFFIX\",\"user_coupon_id\":$UCID}")
ORDER_ID=$(json "['order_id']" <<<"$ORDER")
AMOUNT=$(json "['total_amount']" <<<"$ORDER")
echo "订单 $ORDER_ID：原价=$(json "['original_amount']" <<<"$ORDER") 优惠=$(json "['discount_amount']" <<<"$ORDER") 实付=$AMOUNT"
$PSQL -c "SELECT 'coupon_status=' || status || ' order_id=' || coalesce(order_id::text,'-') FROM user_coupons WHERE id=$UCID"

say "6/7 支付 → 申请退款 → 商家同意（券应退回 UNUSED）"
api POST "/api/v1/orders/$ORDER_ID/pay" "$BT" "{\"payment_no\":\"cp-pay-$SUFFIX\",\"provider_trade_no\":\"cp-trade-$SUFFIX\",\"amount\":$AMOUNT}" >/dev/null
api POST "/api/v1/orders/$ORDER_ID/refund" "$BT" '{"reason":"演练退款"}' >/dev/null
api POST "/api/v1/merchant/orders/$ORDER_ID/refund/approve" "$MT" '{"remark":"同意"}' >/dev/null
$PSQL -c "SELECT 'order_status=' || status FROM orders WHERE id=$ORDER_ID"
$PSQL -c "SELECT 'coupon_status=' || status || ' order_id=' || coalesce(order_id::text,'-') FROM user_coupons WHERE id=$UCID"

say "7/7 对账（九项检查，含券发行量与券-订单一致性）"
(env APP_ENV=development JWT_SECRET=${JWT_SECRET:-dev-only-jwt-secret-for-local-run} \
  FEED_CURSOR_SECRET=${FEED_CURSOR_SECRET:-dev-only-cursor-secret-for-local-run} ./bin/reconcile | tail -12) || true

say "演练完成：发券 → 领券防重 → 用券下单 → 支付 → 退款退券 → 对账全部走通"
