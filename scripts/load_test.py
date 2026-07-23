#!/usr/bin/env python3
"""
Load test a subset of languages against the QA Code Runtime API.

Languages exercised: Python, Java, JavaScript, TypeScript, MySQL, PostgreSQL,
C, C++. Each request runs a realistic self-checking "top customer by spend"
workload that must emit `top=bob total=69075 grand=175199` and exit 0.

Auth: uses an API key (X-Auth-Token header) by default — no JWT login needed.
Pass --api-key '' to fall back to email/password login.

Usage:
    python3 scripts/load_test.py                       # 100 requests, 10 concurrent, wait=true
    python3 scripts/load_test.py --total 500 --concurrency 25
    python3 scripts/load_test.py --no-wait             # measure ingestion only (don't wait for result)
    python3 scripts/load_test.py --base-url http://localhost:8002
    python3 scripts/load_test.py --api-key <key>       # override the API key
    python3 scripts/load_test.py --api-key ''          # use email/password JWT instead
    python3 scripts/load_test.py --insecure            # skip TLS cert verification

Exit code: 0 if every request passed, 1 otherwise.
"""

import argparse
import json
import ssl
import sys
import time
import urllib.error
import urllib.request
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
BASE = "http://localhost:18002"
EMAIL = "dev@coderuntime.io"
PASSWORD = "Admin123!"
# API key auth (sent via the X-Auth-Token header). When set, the script skips
# the /auth/token JWT login and authenticates every request with this key.
API_KEY = "ec5e1b406fcdf6b4f51935525a5fbd455a5496fe7bf93c2bad62c5eb34a66592"

EXPECT = "top=bob total=69075 grand=175199"
ACCEPTED_STATUS_ID = 3  # API "Accepted" verdict

# ---------------------------------------------------------------------------
# Realistic self-checking snippets (same dataset across languages).
#   alice=39575  bob=69075 (top)  carol=65049  dave=1500  grand=175199
# Every program prints: top=bob total=69075 grand=175199  then OK / FAIL.
# ---------------------------------------------------------------------------

SALES_C = r"""
#include <stdio.h>
#include <string.h>
int main(void) {
    const char *cust[] = {"alice","bob","alice","carol","bob","alice","dave","carol","bob"};
    int amt[]          = {12050, 25000, 7525, 49999, 3000, 20000, 1500, 15050, 41075};
    int n = 9;
    const char *names[8]; int totals[8] = {0}; int nn = 0; int grand = 0;
    for (int i = 0; i < n; i++) {
        grand += amt[i];
        int j;
        for (j = 0; j < nn; j++) if (strcmp(names[j], cust[i]) == 0) { totals[j] += amt[i]; break; }
        if (j == nn) { names[nn] = cust[i]; totals[nn] = amt[i]; nn++; }
    }
    const char *top = names[0]; int topv = totals[0];
    for (int i = 1; i < nn; i++) if (totals[i] > topv) { top = names[i]; topv = totals[i]; }
    printf("top=%s total=%d grand=%d\n", top, topv, grand);
    if (strcmp(top, "bob") != 0 || topv != 69075 || grand != 175199) { puts("FAIL"); return 1; }
    puts("OK");
    return 0;
}
"""

SALES_CPP = r"""
#include <iostream>
#include <map>
#include <string>
#include <vector>
using namespace std;
int main() {
    vector<pair<string,int>> orders = {
        {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
        {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}};
    map<string,int> totals; int grand = 0;
    for (auto &o : orders) { totals[o.first] += o.second; grand += o.second; }
    string top; int topv = 0;
    for (auto &p : totals) if (p.second > topv) { top = p.first; topv = p.second; }
    cout << "top=" << top << " total=" << topv << " grand=" << grand << endl;
    if (top != "bob" || topv != 69075 || grand != 175199) { cout << "FAIL" << endl; return 1; }
    cout << "OK" << endl;
    return 0;
}
"""

SALES_JAVA = r"""
import java.util.*;
import java.util.stream.*;
public class Main {
    public static void main(String[] args) {
        record Order(String c, int a) {}
        var orders = List.of(
            new Order("alice",12050), new Order("bob",25000), new Order("alice",7525),
            new Order("carol",49999), new Order("bob",3000), new Order("alice",20000),
            new Order("dave",1500),   new Order("carol",15050), new Order("bob",41075));
        var totals = orders.stream().collect(Collectors.groupingBy(Order::c, Collectors.summingInt(Order::a)));
        var top    = totals.entrySet().stream().max(Map.Entry.comparingByValue()).orElseThrow();
        int grand  = totals.values().stream().mapToInt(Integer::intValue).sum();
        System.out.printf("top=%s total=%d grand=%d%n", top.getKey(), top.getValue(), grand);
        if (!top.getKey().equals("bob") || top.getValue() != 69075 || grand != 175199) {
            System.out.println("FAIL"); System.exit(1);
        }
        System.out.println("OK");
    }
}
"""

SALES_JS = r"""
const orders = [
  ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
  ["bob",3000],["alice",20000],["dave",1500],["carol",15050],["bob",41075]
];
const totals = orders.reduce((m, [c, a]) => (m[c] = (m[c]||0) + a, m), {});
const grand  = Object.values(totals).reduce((s, v) => s + v, 0);
const [top, topv] = Object.entries(totals).reduce((b, e) => e[1] > b[1] ? e : b);
console.log(`top=${top} total=${topv} grand=${grand}`);
if (top === "bob" && topv === 69075 && grand === 175199) console.log("OK");
else { console.log("FAIL"); process.exit(1); }
"""

SALES_TS = r"""
type Order = [string, number];
const orders: Order[] = [
    ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
    ["bob",3000],   ["alice",20000],["dave",1500],["carol",15050],["bob",41075]
];
const totals: Record<string, number> = {};
let grand = 0;
for (const [c, a] of orders) { totals[c] = (totals[c] ?? 0) + a; grand += a; }
const [top, topv] = Object.entries(totals).reduce((b, e) => e[1] > b[1] ? e : b);
console.log(`top=${top} total=${topv} grand=${grand}`);
if (top === "bob" && topv === 69075 && grand === 175199) console.log("OK");
else { console.log("FAIL"); process.exit(1); }
"""

SALES_PYTHON3 = r"""
from collections import Counter
orders = [
    ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
    ("bob",3000),   ("alice",20000),("dave",1500),("carol",15050),("bob",41075)
]
totals = Counter()
grand  = 0
for c, a in orders:
    totals[c] += a
    grand     += a
top, topv = totals.most_common(1)[0]
print(f"top={top} total={topv} grand={grand}")
if top == "bob" and topv == 69075 and grand == 175199:
    print("OK")
else:
    print("FAIL"); raise SystemExit(1)
"""

SALES_MYSQL = r"""
CREATE DATABASE IF NOT EXISTS test;
USE test;
DROP TABLE IF EXISTS orders;
CREATE TABLE orders (customer VARCHAR(16), amount INT);
INSERT INTO orders VALUES
    ('alice',12050),('bob',25000),('alice',7525),('carol',49999),
    ('bob',3000),  ('alice',20000),('dave',1500),('carol',15050),('bob',41075);
WITH per_cust AS (
    SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer
),
agg AS (
    SELECT customer, total, (SELECT SUM(amount) FROM orders) AS grand
    FROM per_cust ORDER BY total DESC LIMIT 1
)
SELECT CONCAT('top=', customer, ' total=', total, ' grand=', grand) AS line FROM agg
UNION ALL
SELECT IF(customer='bob' AND total=69075 AND grand=175199, 'OK', 'FAIL') FROM agg;
"""

SALES_POSTGRES = r"""
CREATE TABLE orders (customer TEXT, amount INT);
INSERT INTO orders VALUES
    ('alice',12050),('bob',25000),('alice',7525),('carol',49999),
    ('bob',3000),  ('alice',20000),('dave',1500),('carol',15050),('bob',41075);
WITH per_cust AS (
    SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer
),
agg AS (
    SELECT customer, total, (SELECT SUM(amount) FROM orders) AS grand
    FROM per_cust ORDER BY total DESC LIMIT 1
)
SELECT 'top=' || customer || ' total=' || total || ' grand=' || grand AS line FROM agg
UNION ALL
SELECT CASE WHEN customer='bob' AND total=69075 AND grand=175199
            THEN 'OK' ELSE 'FAIL' END FROM agg;
"""

# (id, name, source) — ids match the API's language table.
LANGUAGES = [
    (29, "Python",     SALES_PYTHON3),
    (16, "Java",       SALES_JAVA),
    (17, "JavaScript", SALES_JS),
    (35, "TypeScript", SALES_TS),
    (38, "MySQL",      SALES_MYSQL),
    (39, "PostgreSQL", SALES_POSTGRES),
    (2,  "C",          SALES_C),
    (3,  "C++",        SALES_CPP),
]

# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

_SSL_CTX = None  # set to unverified context with --insecure


def api(method, path, data=None, token=None, timeout=120):
    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["X-Auth-Token"] = API_KEY  # API-key auth (preferred)
    elif token:
        headers["Authorization"] = f"Bearer {token}"  # JWT fallback
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(f"{BASE}{path}", data=body, headers=headers, method=method)
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=_SSL_CTX) as r:
            payload = json.loads(r.read())
            return r.status, payload, time.perf_counter() - started
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        try:
            payload = json.loads(raw)
        except Exception:
            payload = {"error": raw}
        return e.code, payload, time.perf_counter() - started
    except Exception as e:
        return None, {"error": str(e)}, time.perf_counter() - started


def get_token():
    status, res, _ = api("POST", "/auth/token", {"email": EMAIL, "password": PASSWORD})
    tok = (res or {}).get("access_token")
    if not tok:
        print(f"Auth failed (status={status}): {res}")
        sys.exit(1)
    return tok


# ---------------------------------------------------------------------------
# Single load request
# ---------------------------------------------------------------------------

def run_one(idx, token, wait, timeout):
    lang_id, name, code = LANGUAGES[idx % len(LANGUAGES)]
    path = "/submissions?wait=true" if wait else "/submissions"
    status, res, latency = api("POST", path,
                               {"language_id": lang_id, "source_code": code},
                               token=token, timeout=timeout)

    result = {
        "i": idx, "lang": name, "lang_id": lang_id,
        "http": status, "latency": latency, "ok": False, "reason": "",
    }

    if status is None:
        result["reason"] = f"transport: {res.get('error', 'unknown')[:80]}"
        return result
    if status == 429:
        result["reason"] = "rate_limited (429)"
        return result
    if status >= 400:
        result["reason"] = f"http {status}: {str(res.get('error', res))[:80]}"
        return result

    if not wait:
        # Ingestion-only mode: success = accepted into the queue (token returned).
        if res.get("token"):
            result["ok"] = True
        else:
            result["reason"] = f"no token in response: {str(res)[:80]}"
        return result

    # wait=true: judge the execution verdict + expected output.
    sid = (res.get("status") or {}).get("id")
    stdout = res.get("stdout") or ""
    if sid != ACCEPTED_STATUS_ID:
        sdesc = (res.get("status") or {}).get("description", "?")
        result["reason"] = f"verdict={sdesc}"
        return result
    if EXPECT not in stdout:
        result["reason"] = f"stdout missing '{EXPECT}'"
        return result
    result["ok"] = True
    return result


# ---------------------------------------------------------------------------
# Stats
# ---------------------------------------------------------------------------

def pct(sorted_vals, p):
    if not sorted_vals:
        return 0.0
    k = max(0, min(len(sorted_vals) - 1, int(round((p / 100.0) * (len(sorted_vals) - 1)))))
    return sorted_vals[k]


def summarize(results, wall):
    total = len(results)
    passed = [r for r in results if r["ok"]]
    failed = [r for r in results if not r["ok"]]
    lat = sorted(r["latency"] for r in results if r["latency"] is not None)

    print(f"\n{'='*66}")
    print("LOAD TEST SUMMARY")
    print(f"{'='*66}")
    print(f"Requests        : {total}")
    print(f"Passed          : {len(passed)} ({100*len(passed)/total:.1f}%)")
    print(f"Failed          : {len(failed)} ({100*len(failed)/total:.1f}%)")
    print(f"Wall time       : {wall:.2f}s")
    print(f"Throughput      : {total/wall:.1f} req/s")

    if lat:
        avg = sum(lat) / len(lat)
        print(f"\nLatency (s)     : min={lat[0]:.3f}  avg={avg:.3f}  max={lat[-1]:.3f}")
        print(f"                  p50={pct(lat,50):.3f}  p90={pct(lat,90):.3f}  "
              f"p95={pct(lat,95):.3f}  p99={pct(lat,99):.3f}")

    # Per-language breakdown
    by_lang = defaultdict(lambda: {"pass": 0, "fail": 0})
    for r in results:
        by_lang[r["lang"]]["pass" if r["ok"] else "fail"] += 1
    print(f"\nPer-language    :")
    for name in sorted(by_lang):
        b = by_lang[name]
        print(f"  {name:12s} pass={b['pass']:4d}  fail={b['fail']:4d}")

    # Failure reasons
    if failed:
        reasons = Counter(r["reason"].split(":")[0] for r in failed)
        print(f"\nFailure reasons :")
        for reason, n in reasons.most_common():
            print(f"  {n:4d}  {reason}")

    print(f"{'='*66}\n")
    return failed


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    global BASE, EMAIL, PASSWORD, API_KEY, _SSL_CTX

    ap = argparse.ArgumentParser(description="Load test the Code Runtime API.")
    ap.add_argument("--total", type=int, default=100, help="total requests (default 100)")
    ap.add_argument("--concurrency", type=int, default=10, help="concurrent workers (default 10)")
    ap.add_argument("--base-url", default=BASE, help=f"API base URL (default {BASE})")
    ap.add_argument("--api-key", default=API_KEY,
                    help="API key (X-Auth-Token). Set empty ('') to fall back to email/password.")
    ap.add_argument("--email", default=EMAIL)
    ap.add_argument("--password", default=PASSWORD)
    ap.add_argument("--timeout", type=int, default=120, help="per-request timeout seconds")
    ap.add_argument("--no-wait", dest="wait", action="store_false",
                    help="submit only, don't wait for execution result (ingestion throughput)")
    ap.add_argument("--insecure", action="store_true", help="skip TLS cert verification")
    ap.set_defaults(wait=True)
    args = ap.parse_args()

    BASE = args.base_url.rstrip("/")
    EMAIL, PASSWORD = args.email, args.password
    API_KEY = (args.api_key or "").strip()
    if args.insecure:
        _SSL_CTX = ssl._create_unverified_context()

    print(f"Target          : {BASE}")
    print(f"Auth            : {'API key (X-Auth-Token)' if API_KEY else 'email/password (JWT)'}")
    print(f"Mode            : {'wait=true (end-to-end execution)' if args.wait else 'no-wait (ingestion only)'}")
    print(f"Total requests  : {args.total}")
    print(f"Concurrency     : {args.concurrency}")
    print(f"Languages       : {', '.join(n for _, n, _ in LANGUAGES)}")

    # With an API key, no login round-trip is needed; otherwise fetch a JWT.
    token = None
    if API_KEY:
        print("\nUsing API key — skipping JWT login.")
    else:
        print("\nAuthenticating ...", end=" ", flush=True)
        token = get_token()
        print("OK")

    print(f"Firing {args.total} requests ...\n")
    results = []
    done = 0
    started = time.perf_counter()
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(run_one, i, token, args.wait, args.timeout)
                   for i in range(args.total)]
        for fut in as_completed(futures):
            r = fut.result()
            results.append(r)
            done += 1
            mark = "✓" if r["ok"] else "✗"
            # Live progress: one line every ~10% (or each on small runs).
            step = max(1, args.total // 10)
            if done % step == 0 or done == args.total:
                ok_n = sum(1 for x in results if x["ok"])
                print(f"  [{done:5d}/{args.total}] ok={ok_n} "
                      f"last={mark} {r['lang']} {r['latency']:.2f}s "
                      f"{('('+r['reason']+')') if not r['ok'] else ''}")
    wall = time.perf_counter() - started

    failed = summarize(results, wall)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
