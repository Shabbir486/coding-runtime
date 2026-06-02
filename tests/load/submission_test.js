/**
 * Code Runtime — k6 Load Test
 *
 * Simulates realistic submission traffic: ramp up to 100 virtual users,
 * submit Python code asynchronously, then poll until terminal state.
 *
 * Usage:
 *   k6 run tests/load/submission_test.js
 *   k6 run --env BASE_URL=http://staging:8002 tests/load/submission_test.js
 *
 * Prerequisites:
 *   brew install k6   (or https://k6.io/docs/getting-started/installation)
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend, Counter } from 'k6/metrics';
import encoding from 'k6/encoding';

// ---------------------------------------------------------------------------
// Load shape
// ---------------------------------------------------------------------------

export const options = {
  stages: [
    { duration: '30s', target: 10 },    // warm-up: ramp to 10 VUs
    { duration: '1m',  target: 50 },    // sustained load: 50 VUs
    { duration: '30s', target: 100 },   // peak: ramp to 100 VUs
    { duration: '30s', target: 0 },     // ramp down: back to 0
  ],
  thresholds: {
    // 95th-percentile HTTP request latency under 5 s
    http_req_duration: ['p(95)<5000'],
    // Less than 1% of HTTP requests fail
    http_req_failed: ['rate<0.01'],
    // 95th-percentile end-to-end submission time (submit → terminal) under 30 s
    submission_e2e_duration: ['p(95)<30000'],
    // Submission success rate (Accepted) above 95%
    submission_success_rate: ['rate>0.95'],
  },
};

// ---------------------------------------------------------------------------
// Custom metrics
// ---------------------------------------------------------------------------

/** End-to-end duration from submission POST to terminal result (ms). */
const submissionE2EDuration = new Trend('submission_e2e_duration', true);

/** Rate of submissions that reached Accepted status (3). */
const submissionSuccessRate = new Rate('submission_success_rate');

/** Total completed submissions (any terminal status). */
const submissionsCompleted = new Counter('submissions_completed');

/** Total timed-out polls. */
const submissionTimeouts = new Counter('submission_poll_timeouts');

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8002';

/** Maximum time (ms) to poll for a terminal result before giving up. */
const POLL_TIMEOUT_MS = 60_000;

/** Polling interval (ms). */
const POLL_INTERVAL_MS = 500;

// Python programs to rotate through for variety
const PYTHON_PROGRAMS = [
  `print("Hello, World!")`,
  `import math\nprint(math.pi)`,
  `result = sum(range(1, 101))\nprint(result)`,
  `words = "the quick brown fox".split()\nprint(sorted(words))`,
  `fib = lambda n: n if n <= 1 else fib(n-1)+fib(n-2)\nprint([fib(i) for i in range(10)])`,
];

// ---------------------------------------------------------------------------
// Setup: obtain auth token once, shared across all VUs
// ---------------------------------------------------------------------------

export function setup() {
  const loginPayload = JSON.stringify({
    email:    'admin@example.com',
    password: 'admin123',
  });

  const loginRes = http.post(`${BASE_URL}/auth/token`, loginPayload, {
    headers: { 'Content-Type': 'application/json' },
    tags:    { name: 'auth_token' },
  });

  check(loginRes, {
    'setup: auth token issued (200)': (r) => r.status === 200,
    'setup: access_token present':    (r) => {
      try {
        return JSON.parse(r.body).access_token !== '';
      } catch {
        return false;
      }
    },
  });

  if (loginRes.status !== 200) {
    console.error(`setup failed — could not obtain auth token: ${loginRes.status} ${loginRes.body}`);
    return { token: '' };
  }

  const { access_token: token } = JSON.parse(loginRes.body);
  console.log(`setup complete — token obtained (${token.length} chars)`);
  return { token };
}

// ---------------------------------------------------------------------------
// Main VU scenario
// ---------------------------------------------------------------------------

export default function scenario(data) {
  const { token } = data;

  if (!token) {
    console.warn('no auth token — skipping VU iteration');
    return;
  }

  const headers = {
    'Content-Type':  'application/json',
    'Authorization': `Bearer ${token}`,
  };

  // Pick a random program for variety
  const program = PYTHON_PROGRAMS[Math.floor(Math.random() * PYTHON_PROGRAMS.length)];
  const sourceCode = encoding.b64encode(program);

  const submissionPayload = JSON.stringify({
    language_id:    29,         // Python 3.11
    source_code:    sourceCode,
    cpu_time_limit: 5.0,
    memory_limit:   262144,
  });

  // ---- 1. Submit (async) ------------------------------------------------
  const submitStart = Date.now();

  const submitRes = http.post(
    `${BASE_URL}/submissions?wait=false`,
    submissionPayload,
    { headers, tags: { name: 'submit' } },
  );

  const submitOk = check(submitRes, {
    'submit: status 200':     (r) => r.status === 200,
    'submit: token returned': (r) => {
      try {
        return JSON.parse(r.body).token !== '';
      } catch {
        return false;
      }
    },
  });

  if (!submitOk || submitRes.status !== 200) {
    console.warn(`submit failed: ${submitRes.status} ${submitRes.body}`);
    return;
  }

  const { token: submissionToken } = JSON.parse(submitRes.body);

  // ---- 2. Poll until terminal -------------------------------------------
  const pollDeadline = Date.now() + POLL_TIMEOUT_MS;
  let terminalStatus = null;

  while (Date.now() < pollDeadline) {
    sleep(POLL_INTERVAL_MS / 1000);

    const pollRes = http.get(
      `${BASE_URL}/submissions/${submissionToken}`,
      { headers, tags: { name: 'poll' } },
    );

    check(pollRes, {
      'poll: status 200': (r) => r.status === 200,
    });

    if (pollRes.status !== 200) {
      continue;
    }

    let sub;
    try {
      sub = JSON.parse(pollRes.body);
    } catch {
      continue;
    }

    const statusID = sub.status && sub.status.id;

    // Terminal states: Accepted (3), Wrong Answer (4), TLE (5),
    // Compilation Error (6), Runtime Error (7), Internal Error (8)
    if (statusID >= 3) {
      terminalStatus = statusID;
      break;
    }
  }

  // ---- 3. Record metrics ------------------------------------------------
  const e2eMs = Date.now() - submitStart;

  if (terminalStatus !== null) {
    submissionE2EDuration.add(e2eMs);
    submissionsCompleted.add(1);
    submissionSuccessRate.add(terminalStatus === 3 ? 1 : 0);

    check({ statusID: terminalStatus }, {
      'e2e: reached terminal state': (s) => s.statusID >= 3,
      'e2e: accepted':               (s) => s.statusID === 3,
    });
  } else {
    submissionTimeouts.add(1);
    submissionSuccessRate.add(0);
    console.warn(`poll timeout for token ${submissionToken} after ${POLL_TIMEOUT_MS}ms`);
  }

  // Brief think time to simulate realistic user pacing
  sleep(0.1 + Math.random() * 0.4);
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

export function teardown(data) {
  console.log('load test complete');
}

// ---------------------------------------------------------------------------
// Smoke test scenario (override with --scenario flags)
// ---------------------------------------------------------------------------

/**
 * healthScenario can be used as a canary check before the full load test:
 *   k6 run --scenario health_smoke tests/load/submission_test.js
 * (Not wired by default; add to options.scenarios to enable.)
 */
export function healthScenario() {
  const res = http.get(`${BASE_URL}/health`, {
    tags: { name: 'health' },
  });

  check(res, {
    'health: 200': (r) => r.status === 200,
    'health: status ok': (r) => {
      try {
        return JSON.parse(r.body).status === 'ok';
      } catch {
        return false;
      }
    },
  });

  sleep(1);
}