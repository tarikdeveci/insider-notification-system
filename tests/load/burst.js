// k6 burst: creates TOTAL notifications (default 10000) through the batch endpoint,
// spread evenly over sms, email and push with mixed priorities. All data is fake.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://host.docker.internal:8080';
const TOTAL = parseInt(__ENV.TOTAL || '10000', 10);
const BATCH = 1000;
const BATCHES = Math.ceil(TOTAL / BATCH);
const created = new Counter('created_items');

export const options = {
  scenarios: {
    burst: {
      executor: 'shared-iterations',
      vus: 4,
      iterations: BATCHES,
      maxDuration: '10m',
    },
  },
  thresholds: {
    http_req_failed: ['rate==0'],
    checks: ['rate==1'],
  },
};

const channels = ['sms', 'email', 'push'];
const priorities = ['high', 'normal', 'low'];

function item(n) {
  const channel = channels[n % 3];
  const priority = priorities[Math.floor(n / 3) % 3];
  let recipient;
  let content;
  if (channel === 'sms') {
    recipient = '+90555' + String(n).padStart(7, '0');
    content = 'load test sms ' + n;
  } else if (channel === 'email') {
    recipient = 'user-' + n + '@example.test';
    content = 'load test email ' + n;
  } else {
    recipient = 'device-token-' + n;
    content = 'load test push ' + n;
  }
  return { recipient, channel, content, priority };
}

export default function () {
  // __ITER is the global iteration index for shared-iterations, so ranges never overlap.
  const start = __ITER * BATCH;
  const end = Math.min(start + BATCH, TOTAL);
  const items = [];
  for (let n = start; n < end; n++) items.push(item(n));
  const res = http.post(`${BASE}/v1/notifications/batch`, JSON.stringify({ notifications: items }), {
    headers: { 'Content-Type': 'application/json' },
  });
  const ok = check(res, { 'batch created': (r) => r.status === 201 });
  if (ok) created.add(items.length);
}
