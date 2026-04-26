// k6 load test — proves the spec's throughput & latency budget:
//
//   "Sustain 200 concurrent trade-close events/sec for 60 seconds with
//    p95 write latency <= 150ms."
//
// Run:
//   k6 run --out json=loadtest/results/run.json loadtest/k6.js
//   k6 run --out web-dashboard=export=loadtest/results/report.html loadtest/k6.js
//
// k6 reads API_URL + JWT_SECRET from the env. Defaults assume
// `docker compose up` is running locally.
//
// We use 10 distinct seed users in rotation so the pool of writers looks
// realistic and hits multiple index partitions, not a single hot row.

import http from 'k6/http';
import { check, sleep } from 'k6';
import crypto from 'k6/crypto';
import encoding from 'k6/encoding';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const API_URL    = __ENV.API_URL    || 'http://localhost:8080';
const JWT_SECRET = __ENV.JWT_SECRET || '97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02';

// 10 seed userIds from the kickoff PDF (one per trader).
const USERS = [
  'f412f236-4edc-47a2-8f54-8763a6ed2ce8', // Alex Mercer
  'fcd434aa-2201-4060-aeb2-f44c77aa0683', // Jordan Lee
  '84a6a3dd-f2d0-4167-960b-7319a6033d49', // Sam Rivera
  '4f2f0816-f350-4684-b6c3-29bbddbb1869', // Casey Kim
  '75076413-e8e8-44ac-861f-c7acb3902d6d', // Morgan Bell
  '8effb0f2-f16b-4b5f-87ab-7ffca376f309', // Taylor Grant
  '50dd1053-73b0-43c5-8d0f-d2af88c01451', // Riley Stone
  'af2cfc5e-c132-4989-9c12-2913f89271fb', // Drew Patel
  '9419073a-3d58-4ee6-a917-be2d40aecef2', // Quinn Torres
  'e84ea28c-e5a7-49ef-ac26-a873e32667bd', // Avery Chen
];

// Pre-issue one JWT per seed user; reuse across the run.
const TOKENS = USERS.map((u) => signJWT({
  sub:  u,
  iat:  Math.floor(Date.now() / 1000),
  exp:  Math.floor(Date.now() / 1000) + 24 * 3600,
  role: 'trader',
}));

// ── k6 options ──────────────────────────────────────────────────────────────
//
// Spec target: 200 close events/sec for 60s.
// We use a constant-arrival-rate executor — k6 schedules exactly 200 RPS
// regardless of response times, which is the right model for this test.
//
// Threshold gates the run: if p95 write latency > 150ms, exit non-zero.
export const options = {
  scenarios: {
    constant_load: {
      executor:        'constant-arrival-rate',
      rate:            200,
      timeUnit:        '1s',
      duration:        '60s',
      preAllocatedVUs: 60,
      maxVUs:          200,
    },
  },
  thresholds: {
    'http_req_failed':   ['rate<0.01'],          // < 1% errors
    'http_req_duration': ['p(95)<150'],          // p95 <= 150ms (spec)
  },
};

// ── per-iteration: POST /trades closing a brand-new trade ──────────────────
export default function () {
  const idx   = Math.floor(Math.random() * USERS.length);
  const user  = USERS[idx];
  const token = TOKENS[idx];

  const tradeId   = uuidv4();
  const sessionId = uuidv4();
  const entry     = new Date(Date.now() - 60 * 60 * 1000).toISOString();
  const exit      = new Date().toISOString();

  const body = {
    tradeId,
    userId:        user,
    sessionId,
    asset:         'AAPL',
    assetClass:    'equity',
    direction:     'long',
    entryPrice:    178.45,
    exitPrice:     182.30,
    quantity:      10,
    entryAt:       entry,
    exitAt:        exit,
    status:        'closed',
    planAdherence: 4,
    emotionalState:'calm',
  };

  const res = http.post(`${API_URL}/trades`, JSON.stringify(body), {
    headers: {
      'Content-Type':  'application/json',
      'Authorization': `Bearer ${token}`,
    },
  });

  check(res, {
    'status is 200':       (r) => r.status === 200,
    'has tradeId in body': (r) => r.json('tradeId') === tradeId,
  });
}

// ── tiny HS256 signer (k6 has crypto + base64 in stdlib, no JWT lib needed) ─
function signJWT(payload) {
  const header = b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body   = b64url(JSON.stringify(payload));
  const signing = `${header}.${body}`;
  const sig = encoding.b64encode(
    crypto.hmac('sha256', JWT_SECRET, signing, 'binary'),
    'rawurl'
  );
  return `${signing}.${sig}`;
}
function b64url(s) {
  return encoding.b64encode(s, 'rawurl');
}
