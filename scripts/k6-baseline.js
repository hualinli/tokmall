import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const fixture = JSON.parse(open(__ENV.FIXTURE));
const transactions = new Counter('successful_transactions');
const failures = new Counter('business_failures');
const transactionLatency = new Trend('transaction_duration', true);
const createLatency = new Trend('create_order_duration', true);
const payLatency = new Trend('pay_order_duration', true);
const feedLatency = new Trend('feed_duration', true);
export const options = {
  scenarios: { baseline: { executor: 'constant-vus', vus: Number(__ENV.VUS),
    duration: __ENV.DURATION, gracefulStop: '15s' } },
  systemTags: ['status', 'method', 'name', 'scenario', 'expected_response'],
  summaryTrendStats: ['avg', 'med', 'p(95)', 'p(99)', 'max'],
  thresholds: { checks: ['rate==1'], http_req_failed: ['rate==0'] },
};

export default function () {
  const base = __ENV.BASE;
  if (__ENV.SCENARIO === 'feed') {
    const r = http.get(`${base}/api/v1/feed?limit=20`, { tags: { name: 'feed' } });
    feedLatency.add(r.timings.duration);
    check(r, { 'feed contains 20 videos': (v) => v.status === 200 && v.json('items').length === 20 });
    return;
  }
  const buyer = fixture.buyers[(__VU - 1) % fixture.buyers.length];
  const sku = fixture.skus[(__ITER + __VU - 1) % fixture.skus.length];
  const key = `k6-${fixture.run}-${__VU}-${__ITER}`;
  const headers = { Authorization: `Bearer ${buyer.access_token}`, 'Content-Type': 'application/json' };
  const began = Date.now();
  const created = http.post(`${base}/api/v1/orders`, JSON.stringify({
    sku_id: sku.sku_id, quantity: 1, idempotency_key: key,
    source: { video_id: sku.video_id, author_id: fixture.author_id, product_id: sku.product_id },
  }), { headers, tags: { name: 'create_order' } });
  createLatency.add(created.timings.duration);
  if (!check(created, { 'order created': (r) => r.status === 201 })) { failures.add(1); return; }
  const order = created.json();
  const paid = http.post(`${base}/api/v1/orders/${order.order_id}/pay`, JSON.stringify({
    payment_no: key, provider_trade_no: key, amount: order.total_amount,
  }), { headers, tags: { name: 'pay_order' } });
  payLatency.add(paid.timings.duration);
  if (check(paid, { 'order paid': (r) => r.status === 200 })) {
    transactions.add(1);
    transactionLatency.add(Date.now() - began);
  } else { failures.add(1); }
}

export function handleSummary(data) {
  return { [__ENV.SUMMARY]: JSON.stringify(data, null, 2) };
}
