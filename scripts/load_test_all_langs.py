#!/usr/bin/env python3
"""
Heavy, all-languages load generator for Code Runtime.

Reuses the language snippets from test_languages.py (HELLO for every language ID)
and fires N submissions cycling through all of them at high concurrency. Default
is ingestion mode (--no-wait): the api accepts + publishes to SQS as fast as
possible, which exercises api throughput AND drives KEDA worker autoscaling on
the resulting SQS backlog. Use --wait for full end-to-end execution.

Usage (from inside the cluster against the Service, or via port-forward):
    python3 load_test_all_langs.py --base-url http://api-gateway \
        --api-key '' --total 18000 --concurrency 60 --no-wait
"""
import argparse, json, sys, time, urllib.request, urllib.error
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed

# Reuse the canonical per-language snippets + names from the language test suite.
from test_languages import HELLO, LANG_NAMES  # same scripts/ dir

EMAIL = "dev@coderuntime.io"
PASSWORD = "Admin123!"
ACCEPTED = 3


def api(base, method, path, data=None, token=None, api_key=None, timeout=120):
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["X-Auth-Token"] = api_key
    elif token:
        headers["Authorization"] = f"Bearer {token}"
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(f"{base}{path}", data=body, headers=headers, method=method)
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read()), time.perf_counter() - t0
    except urllib.error.HTTPError as e:
        try:
            payload = json.loads(e.read())
        except Exception:
            payload = {}
        return e.code, payload, time.perf_counter() - t0
    except Exception as e:
        return None, {"error": str(e)}, time.perf_counter() - t0


def one(base, idx, lang_ids, token, api_key, wait, timeout):
    lang_id = lang_ids[idx % len(lang_ids)]
    path = "/submissions?wait=true" if wait else "/submissions"
    status, res, lat = api(base, "POST", path,
                           {"language_id": lang_id, "source_code": HELLO[lang_id]},
                           token=token, api_key=api_key, timeout=timeout)
    r = {"lang": LANG_NAMES.get(lang_id, str(lang_id)), "http": status, "lat": lat, "ok": False, "reason": ""}
    if status is None:
        r["reason"] = "transport"
    elif status == 429:
        r["reason"] = "429"
    elif status >= 400:
        r["reason"] = f"http{status}"
    elif not wait:
        r["ok"] = bool(res.get("token")); r["reason"] = "" if r["ok"] else "no-token"
    else:
        r["ok"] = (res.get("status") or {}).get("id") == ACCEPTED
        r["reason"] = "" if r["ok"] else "verdict:" + str((res.get("status") or {}).get("description", "?"))
    return r


def pct(v, p):
    return v[max(0, min(len(v) - 1, int(round(p / 100 * (len(v) - 1)))))] if v else 0.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://localhost:18002")
    ap.add_argument("--total", type=int, default=18000)
    ap.add_argument("--concurrency", type=int, default=60)
    ap.add_argument("--api-key", default="")
    ap.add_argument("--email", default=EMAIL)
    ap.add_argument("--password", default=PASSWORD)
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--no-wait", dest="wait", action="store_false")
    ap.add_argument("--lang-ids", default="", help="comma-separated subset; default = all")
    ap.set_defaults(wait=True)
    a = ap.parse_args()
    base = a.base_url.rstrip("/")

    lang_ids = [int(x) for x in a.lang_ids.split(",") if x.strip()] or sorted(HELLO.keys())
    token, api_key = None, (a.api_key or "").strip()
    if not api_key:
        st, res, _ = api(base, "POST", "/auth/token", {"email": a.email, "password": a.password})
        token = (res or {}).get("access_token")
        if not token:
            print(f"auth failed ({st}): {res}"); sys.exit(1)

    print(f"target={base} mode={'wait' if a.wait else 'no-wait'} total={a.total} "
          f"concurrency={a.concurrency} languages={len(lang_ids)}")

    results = []
    t0 = time.perf_counter()
    with ThreadPoolExecutor(max_workers=a.concurrency) as pool:
        futs = [pool.submit(one, base, i, lang_ids, token, api_key, a.wait, a.timeout)
                for i in range(a.total)]
        done = 0
        for f in as_completed(futs):
            results.append(f.result()); done += 1
            if done % max(1, a.total // 20) == 0 or done == a.total:
                ok = sum(1 for x in results if x["ok"])
                print(f"  [{done}/{a.total}] ok={ok} ({100*ok/done:.0f}%)")
    wall = time.perf_counter() - t0

    ok = [r for r in results if r["ok"]]
    lat = sorted(r["lat"] for r in results if r["lat"])
    print("\n" + "=" * 60)
    print(f"ALL-LANGUAGES LOAD TEST  ({'wait' if a.wait else 'ingestion'})")
    print("=" * 60)
    print(f"submissions : {len(results)}")
    print(f"accepted    : {len(ok)} ({100*len(ok)/len(results):.1f}%)")
    print(f"wall time   : {wall:.1f}s")
    print(f"throughput  : {len(results)/wall:.0f} req/s")
    print(f"latency(s)  : avg={sum(lat)/len(lat):.3f} p50={pct(lat,50):.3f} "
          f"p90={pct(lat,90):.3f} p99={pct(lat,99):.3f} max={lat[-1]:.3f}")
    reasons = Counter(r["reason"] for r in results if not r["ok"])
    if reasons:
        print("failures    :", dict(reasons.most_common()))
    return 0


if __name__ == "__main__":
    sys.exit(main())
