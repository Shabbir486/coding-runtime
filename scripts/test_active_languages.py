#!/usr/bin/env python3
"""
Test ONLY the active languages of Code Runtime, end-to-end, with a real test
case each: given an input, assert the produced output.

Active set: C, C++, Java, JavaScript, TypeScript, Python 3, MySQL, PostgreSQL,
and Web (HTML/CSS/JS). The imperative languages read from STDIN (input "3 4" ->
"7") to prove input handling. SQL runs a query. Web runs a calculator suite and
a QUnit suite (framework bundled in the runtime — no CDN).

Each case submits with ?wait=true (+ stdin) and asserts status == "Accepted"
and that the expected substring appears in stdout. Exit 0 iff all pass. Also
(re)generates a Postman collection with the same requests + assertions.

Usage:
    python3 scripts/test_active_languages.py
    python3 scripts/test_active_languages.py --base-url http://localhost:18002
    python3 scripts/test_active_languages.py --postman-only
"""
import argparse
import json
import sys
import urllib.error
import urllib.request

DEFAULT_BASE = "http://aa6f9d7b568b4425f84ea155a296db33-71808099.ap-southeast-2.elb.amazonaws.com"
EMAIL = "dev@coderuntime.io"
PASSWORD = "Admin123!"
POSTMAN_OUT = "scripts/code_runtime_active.postman_collection.json"

# ---------------------------------------------------------------------------
# Test cases: (name, language_id, source_code, stdin, expected_substring)
# Imperative languages read two integers from STDIN and print their sum.
# ---------------------------------------------------------------------------
CASES = [
    ("C (GCC)", 2,
     '#include <stdio.h>\n'
     'int main(){ int a,b; if(scanf("%d %d",&a,&b)!=2) return 1; printf("%d\\n", a+b); return 0; }',
     "3 4", "7"),

    ("C++ (GCC)", 3,
     '#include <iostream>\n'
     'int main(){ int a,b; std::cin >> a >> b; std::cout << (a+b) << std::endl; }',
     "3 4", "7"),

    ("Java (OpenJDK 21)", 16,
     'import java.util.Scanner;\n'
     'public class Main { public static void main(String[] a){\n'
     '  Scanner s = new Scanner(System.in);\n'
     '  System.out.println(s.nextInt() + s.nextInt());\n'
     '} }',
     "3 4", "7"),

    ("JavaScript (Node 22)", 17,
     'const [a,b] = require("fs").readFileSync(0,"utf8").trim().split(/\\s+/).map(Number);\n'
     'console.log(a + b);',
     "3 4", "7"),

    ("TypeScript (5.6)", 35,
     'const nums: number[] = require("fs").readFileSync(0,"utf8").trim().split(/\\s+/).map(Number);\n'
     'console.log(nums[0] + nums[1]);',
     "3 4", "7"),

    ("Python (3.12)", 29,
     'a, b = map(int, input().split())\nprint(a + b)',
     "3 4", "7"),

    # SQL has no stdin; the query IS the test case (sum two values).
    ("MySQL (8.0)", 38, "SELECT 3 + 4 AS result;", "", "7"),
    ("PostgreSQL (16)", 39, "SELECT 3 + 4 AS result;", "", "7"),

    # Web: the calculator's own test cases.
    ("Web (HTML/CSS/JS) — calculator", 41, r'''<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <style>#display{font-size:24px;padding:12px;border:1px solid #ccc}</style>
</head>
<body>
  <div id="display">0</div>
  <script>
    const Calculator = {
      add:      (a, b) => a + b,
      subtract: (a, b) => a - b,
      multiply: (a, b) => a * b,
      divide:   (a, b) => (b === 0 ? "Error: divide by zero" : a / b),
    };
    document.getElementById("display").textContent = String(Calculator.add(2, 3));
    const tests = [
      ["add(2,3)",       Calculator.add(2, 3),       5],
      ["subtract(10,4)", Calculator.subtract(10, 4), 6],
      ["multiply(3,4)",  Calculator.multiply(3, 4),  12],
      ["divide(20,5)",   Calculator.divide(20, 5),   4],
      ["divide(1,0)",    Calculator.divide(1, 0),    "Error: divide by zero"],
    ];
    let passed = 0;
    for (const [name, got, want] of tests) {
      const ok = got === want;
      if (ok) passed++;
      console.log((ok ? "PASS " : "FAIL ") + name + " => " + got);
    }
    console.log("CALC TESTS: " + passed + "/" + tests.length + " passed");
  </script>
</body>
</html>''', "", "CALC TESTS: 5/5 passed"),

    # Web + QUnit: candidate code + QUnit tests (framework auto-injected).
    ("Web (HTML/CSS/JS) — QUnit", 41, r'''<!DOCTYPE html>
<html>
<head></head>
<body>
  <script>
    function add(a, b) { return a + b; }
    function isEven(n) { return n % 2 === 0; }
    QUnit.test("add works", function (t) {
      t.equal(add(2, 3), 5, "2+3");
      t.equal(add(-1, 1), 0, "-1+1");
    });
    QUnit.test("isEven works", function (t) {
      t.ok(isEven(4), "4 even");
      t.notOk(isEven(3), "3 odd");
    });
  </script>
</body>
</html>''', "", "PASS"),
]


def api(base, method, path, data=None, token=None, timeout=180):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    body = json.dumps(data).encode() if data is not None else None
    req = urllib.request.Request(f"{base}{path}", data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read())
        except Exception:
            return e.code, {}
    except Exception as e:
        return None, {"error": str(e)}


def get_token(base):
    st, res = api(base, "POST", "/auth/token", {"email": EMAIL, "password": PASSWORD})
    tok = (res or {}).get("access_token")
    if not tok:
        print(f"Auth failed ({st}): {res}")
        sys.exit(1)
    return tok


# ---------------------------------------------------------------------------
# Postman collection builder
# ---------------------------------------------------------------------------
def _submission_item(name, lang_id, code, stdin, expected):
    payload = {"language_id": lang_id, "source_code": code}
    if stdin:
        payload["stdin"] = stdin
    return {
        "name": f"{name} (id={lang_id})",
        "event": [{"listen": "test", "script": {"type": "text/javascript", "exec": [
            "var r = pm.response.json();",
            "pm.test('accepted', () => pm.expect((r.status||{}).description).to.eql('Accepted'));",
            f"pm.test('output', () => pm.expect(r.stdout||'').to.include({json.dumps(expected)}));",
        ]}}],
        "request": {
            "method": "POST",
            "header": [
                {"key": "Content-Type", "value": "application/json"},
                {"key": "Authorization", "value": "Bearer {{token}}"},
            ],
            "url": {"raw": "{{base_url}}/submissions?wait=true", "host": ["{{base_url}}"],
                    "path": ["submissions"], "query": [{"key": "wait", "value": "true"}]},
            "body": {"mode": "raw", "raw": json.dumps(payload, indent=2),
                     "options": {"raw": {"language": "json"}}},
        },
    }


def _get(path_parts, query=None, auth=True):
    url = {"raw": "{{base_url}}/" + "/".join(path_parts), "host": ["{{base_url}}"], "path": path_parts}
    if query:
        url["raw"] += "?" + "&".join(f"{k}={v}" for k, v in query)
        url["query"] = [{"key": k, "value": v} for k, v in query]
    hdr = [{"key": "Authorization", "value": "Bearer {{token}}"}] if auth else []
    return {"request": {"method": "GET", "header": hdr, "url": url}}


def _patch_active(lang_id, active):
    return {
        "name": f"Admin – {'Activate' if active else 'Deactivate'} language (id={lang_id})",
        "request": {
            "method": "PATCH",
            "header": [{"key": "Content-Type", "value": "application/json"},
                       {"key": "Authorization", "value": "Bearer {{token}}"}],
            "url": {"raw": f"{{{{base_url}}}}/languages/{lang_id}", "host": ["{{base_url}}"],
                    "path": ["languages", str(lang_id)]},
            "body": {"mode": "raw", "raw": json.dumps({"is_active": active}, indent=2),
                     "options": {"raw": {"language": "json"}}},
        },
    }


def build_postman_collection(base):
    submissions = [_submission_item(n, i, c, s, e) for (n, i, c, s, e) in CASES]
    return {
        "info": {
            "name": "Code Runtime – Active Languages",
            "description": "Auth + active-language submissions (with stdin test cases) + assertions. Run 'Auth – Get Token' first; it stores {{token}} for the rest.",
            "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
        },
        "variable": [
            {"key": "base_url", "value": base, "type": "string"},
            {"key": "token", "value": "", "type": "string"},
            {"key": "email", "value": EMAIL, "type": "string"},
            {"key": "password", "value": PASSWORD, "type": "string"},
        ],
        "item": [
            {
                "name": "Auth – Get Token",
                "event": [{"listen": "test", "script": {"type": "text/javascript", "exec": [
                    "var r = pm.response.json();",
                    "pm.collectionVariables.set('token', r.access_token);",
                    "pm.test('got token', () => pm.expect(r.access_token).to.be.a('string'));",
                ]}}],
                "request": {
                    "method": "POST",
                    "header": [{"key": "Content-Type", "value": "application/json"}],
                    "url": {"raw": "{{base_url}}/auth/token", "host": ["{{base_url}}"], "path": ["auth", "token"]},
                    "body": {"mode": "raw",
                             "raw": json.dumps({"email": "{{email}}", "password": "{{password}}"}, indent=2),
                             "options": {"raw": {"language": "json"}}},
                },
            },
            {"name": "System – Health", **_get(["health"], auth=False)},
            {"name": "Languages – List active", **_get(["languages"], [("isActive", "true")])},
            {"name": "Languages – List inactive", **_get(["languages"], [("isActive", "false")])},
            _patch_active(41, False),
            _patch_active(41, True),
            {"name": "Submissions (active languages, with stdin)", "item": submissions},
        ],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default=DEFAULT_BASE)
    ap.add_argument("--timeout", type=int, default=200)
    ap.add_argument("--postman-only", action="store_true")
    a = ap.parse_args()
    base = a.base_url.rstrip("/")

    with open(POSTMAN_OUT, "w") as f:
        json.dump(build_postman_collection(base), f, indent=2)
    print(f"Postman collection written: {POSTMAN_OUT}\n")
    if a.postman_only:
        return 0

    token = get_token(base)
    print(f"target={base}  cases={len(CASES)}\n")

    results = []
    for name, lang_id, code, stdin, expected in CASES:
        payload = {"language_id": lang_id, "source_code": code}
        if stdin:
            payload["stdin"] = stdin
        st, res = api(base, "POST", "/submissions?wait=true", payload, token=token, timeout=a.timeout)
        status = ((res or {}).get("status") or {}).get("description", f"HTTP {st}")
        stdout = (res or {}).get("stdout") or ""
        ok = status == "Accepted" and expected in stdout
        results.append(ok)
        mark = "✓ PASS" if ok else "✗ FAIL"
        in_disp = repr(stdin) if stdin else "(none)"
        out_disp = repr(stdout.strip()[:60])
        print(f"{mark}  {name:<34} id={lang_id:<3} status={status}")
        print(f"         input={in_disp}  ->  output={out_disp}")
        if not ok:
            print(f"         expected substring: {expected!r}")
            if (res or {}).get("stderr"):
                print(f"         stderr: {str(res.get('stderr'))[:200]!r}")
            if (res or {}).get("compile_output"):
                print(f"         compile: {str(res.get('compile_output'))[:200]!r}")

    passed = sum(results)
    print("\n" + "=" * 60)
    print(f"RESULT: {passed}/{len(results)} passed")
    print("=" * 60)
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
