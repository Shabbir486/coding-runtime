#!/usr/bin/env bash
# =============================================================================
# Code Runtime API — cURL Examples
#
# Demonstrates every API operation with real, runnable commands.
# All source_code, stdin, and expected_output values are standard Base64.
#
# Prerequisites:
#   brew install jq     (JSON pretty-printing)
#   export BASE_URL=http://localhost:8002  (or set below)
#
# Usage:
#   chmod +x tests/examples/curl_examples.sh
#   ./tests/examples/curl_examples.sh
#
# Each example is self-contained and labeled with a section header.
# =============================================================================
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8002}"

# Helper: print a section header
section() { echo; echo "================================================================"; echo "  $*"; echo "================================================================"; }

# Helper: base64-encode a string (portable across macOS and Linux)
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }

# =============================================================================
# 1. Authentication — obtain a JWT access token
# =============================================================================
section "1. Authenticate — POST /auth/token"

TOKEN=$(curl -s -X POST "${BASE_URL}/auth/token" \
  -H 'Content-Type: application/json' \
  -d '{
    "email":    "admin@example.com",
    "password": "admin123"
  }' | jq -r '.access_token')

echo "TOKEN=${TOKEN}"
export TOKEN

# =============================================================================
# 2. Submit Python "Hello, World!" asynchronously (wait=false)
#    Source: print('Hello, World!')  →  cHJpbnQoJ0hlbGxvLCBXb3JsZCEnKQ==
# =============================================================================
section "2. Submit Python (async, wait=false) — POST /submissions"

PYTHON_TOKEN=$(curl -s -X POST "${BASE_URL}/submissions?wait=false" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "language_id": 29,
    "source_code": "cHJpbnQoJ0hlbGxvLCBXb3JsZCEnKQ==",
    "cpu_time_limit": 5.0,
    "memory_limit": 262144
  }' | jq -r '.token')

echo "PYTHON_TOKEN=${PYTHON_TOKEN}"
export PYTHON_TOKEN

# =============================================================================
# 3. Poll Python result — GET /submissions/{token}
#    Repeat until status.id >= 3 (terminal state)
# =============================================================================
section "3. Poll Python result — GET /submissions/\$PYTHON_TOKEN"

MAX_ATTEMPTS=30
for i in $(seq 1 ${MAX_ATTEMPTS}); do
  RESULT=$(curl -s "${BASE_URL}/submissions/${PYTHON_TOKEN}" \
    -H "Authorization: Bearer ${TOKEN}")
  STATUS_ID=$(echo "${RESULT}" | jq -r '.status.id')
  STATUS_DESC=$(echo "${RESULT}" | jq -r '.status.description')
  echo "  attempt ${i}: status=${STATUS_ID} (${STATUS_DESC})"
  if [ "${STATUS_ID}" -ge 3 ] 2>/dev/null; then
    echo "${RESULT}" | jq '{token, status, stdout, time, memory}'
    break
  fi
  sleep 1
done

# =============================================================================
# 4. Submit Python synchronously (wait=true)
# =============================================================================
section "4. Submit Python (synchronous, wait=true) — POST /submissions?wait=true"

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "language_id": 29,
    "source_code": "cHJpbnQoJ0hlbGxvLCBXb3JsZCEnKQ==",
    "cpu_time_limit": 5.0,
    "memory_limit": 262144
  }' | jq '{token, status, stdout, time, memory}'

# =============================================================================
# 5. Submit C++ Fibonacci (wait=true)
#    Source: reads N from stdin, prints first N Fibonacci numbers
#    stdin: "10" → first 10 Fibonacci numbers
# =============================================================================
section "5. Submit C++ Fibonacci (wait=true)"

CPP_SRC=$(b64 '#include <iostream>
#include <vector>
using namespace std;
int main() {
    int n;
    cin >> n;
    vector<long long> fib(n + 1);
    fib[0] = 0;
    if (n >= 1) fib[1] = 1;
    for (int i = 2; i <= n; i++) fib[i] = fib[i-1] + fib[i-2];
    cout << "Fibonacci sequence (first " << n << " numbers):" << endl;
    for (int i = 0; i < n; i++) {
        cout << fib[i];
        if (i < n - 1) cout << ", ";
    }
    cout << endl;
    return 0;
}')

CPP_STDIN=$(b64 "10")

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 3,
    \"source_code\": \"${CPP_SRC}\",
    \"stdin\": \"${CPP_STDIN}\",
    \"cpu_time_limit\": 10.0,
    \"memory_limit\": 262144
  }" | jq '{token, status, stdout, compile_output, time}'

# =============================================================================
# 6. Submit Go goroutines demo (wait=true)
#    Source: producer/consumer pattern with channels
# =============================================================================
section "6. Submit Go concurrent goroutines (wait=true)"

GO_SRC=$(b64 'package main
import (
	"fmt"
	"sync"
)
func producer(ch chan<- int, wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 1; i <= 5; i++ { ch <- i }
	close(ch)
}
func consumer(id int, ch <-chan int, results chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	for v := range ch { results <- fmt.Sprintf("Worker %d processed: %d", id, v) }
}
func main() {
	ch := make(chan int, 10)
	results := make(chan string, 20)
	var prodWg, consWg sync.WaitGroup
	prodWg.Add(1)
	go producer(ch, &prodWg)
	consWg.Add(3)
	for i := 1; i <= 3; i++ { go consumer(i, ch, results, &consWg) }
	go func() { prodWg.Wait(); consWg.Wait(); close(results) }()
	fmt.Println("Goroutines and channels demonstration:")
	count := 0
	for msg := range results { fmt.Println(msg); count++ }
	fmt.Printf("Total messages processed: %d\n", count)
}')

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 13,
    \"source_code\": \"${GO_SRC}\",
    \"cpu_time_limit\": 10.0,
    \"memory_limit\": 262144
  }" | jq '{token, status, stdout, time}'

# =============================================================================
# 7. Submit Java array sorting (wait=true)
#    Reads N integers from stdin, sorts them, prints result
# =============================================================================
section "7. Submit Java sorting (wait=true)"

JAVA_SRC=$(b64 'import java.util.Arrays;
import java.util.Scanner;
public class Main {
    public static void main(String[] args) {
        Scanner scanner = new Scanner(System.in);
        int n = scanner.nextInt();
        int[] arr = new int[n];
        for (int i = 0; i < n; i++) arr[i] = scanner.nextInt();
        Arrays.sort(arr);
        StringBuilder sb = new StringBuilder("Sorted: ");
        for (int i = 0; i < n; i++) {
            sb.append(arr[i]);
            if (i < n - 1) sb.append(" ");
        }
        System.out.println(sb.toString());
    }
}')

JAVA_STDIN=$(b64 "5
42 7 3 99 1")

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 16,
    \"source_code\": \"${JAVA_SRC}\",
    \"stdin\": \"${JAVA_STDIN}\",
    \"cpu_time_limit\": 15.0,
    \"memory_limit\": 524288
  }" | jq '{token, status, stdout, compile_output, time}'

# =============================================================================
# 8. Submit TypeScript (wait=true)
# =============================================================================
section "8. Submit TypeScript (wait=true)"

TS_SRC=$(b64 'const greet = (name: string): string => `Hello, ${name}! TypeScript works.`;
const names: string[] = ["Alice", "Bob", "Charlie"];
names.forEach(n => console.log(greet(n)));')

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 35,
    \"source_code\": \"${TS_SRC}\",
    \"cpu_time_limit\": 10.0,
    \"memory_limit\": 262144
  }" | jq '{token, status, stdout, compile_output, time}'

# =============================================================================
# 9. Submit Rust (wait=true)
# =============================================================================
section "9. Submit Rust (wait=true)"

RUST_SRC=$(b64 'fn fibonacci(n: u64) -> u64 {
    match n {
        0 => 0,
        1 => 1,
        _ => fibonacci(n - 1) + fibonacci(n - 2),
    }
}
fn main() {
    println!("Rust Fibonacci:");
    for i in 0..10 {
        print!("{}", fibonacci(i));
        if i < 9 { print!(", "); }
    }
    println!();
}')

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 32,
    \"source_code\": \"${RUST_SRC}\",
    \"cpu_time_limit\": 15.0,
    \"memory_limit\": 262144
  }" | jq '{token, status, stdout, compile_output, time}'

# =============================================================================
# 10. Submit Bash script (wait=true)
# =============================================================================
section "10. Submit Bash script (wait=true)"

BASH_SRC=$(b64 '#!/usr/bin/env bash
echo "System information:"
echo "Date: $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
echo "Shell: $BASH_VERSION"

# Compute sum of 1..10
total=0
for i in $(seq 1 10); do
  total=$((total + i))
done
echo "Sum 1..10 = $total"

# String manipulation
msg="hello world"
echo "Upper: ${msg^^}"
echo "Words: $(echo "$msg" | wc -w)"')

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 1,
    \"source_code\": \"${BASH_SRC}\",
    \"cpu_time_limit\": 10.0,
    \"memory_limit\": 262144
  }" | jq '{token, status, stdout, time}'

# =============================================================================
# 11. Submit with stdin (Python reads input)
# =============================================================================
section "11. Submit with stdin — Python readline example"

STDIN_SRC=$(b64 'lines = []
try:
    while True:
        lines.append(input())
except EOFError:
    pass
print(f"Received {len(lines)} line(s):")
for i, line in enumerate(lines, 1):
    print(f"  {i}: {line}")')

STDIN_DATA=$(b64 "first line
second line
third line")

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 29,
    \"source_code\": \"${STDIN_SRC}\",
    \"stdin\": \"${STDIN_DATA}\"
  }" | jq '{token, status, stdout}'

# =============================================================================
# 12. Submit with expected_output (Wrong Answer detection)
# =============================================================================
section "12. Submit with expected_output — Wrong Answer detection"

WA_SRC=$(b64 'print("actual output")')
EXPECTED=$(b64 "expected output
")

curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 29,
    \"source_code\": \"${WA_SRC}\",
    \"expected_output\": \"${EXPECTED}\"
  }" | jq '{token, status}'
# Expected: status.id=4 (Wrong Answer)

# Correct output — should return Accepted
CORRECT_SRC=$(b64 'print("expected output")')
curl -s -X POST "${BASE_URL}/submissions?wait=true" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"language_id\": 29,
    \"source_code\": \"${CORRECT_SRC}\",
    \"expected_output\": \"${EXPECTED}\"
  }" | jq '{token, status}'
# Expected: status.id=3 (Accepted)

# =============================================================================
# 13. Batch submit — POST /submissions/batch
#     Submit 3 Python programs in a single request, get back 3 tokens
# =============================================================================
section "13. Batch submit — POST /submissions/batch"

P1=$(b64 'print("batch item 1")')
P2=$(b64 'import sys; print(sys.version.split()[0])')
P3=$(b64 'print(sum(i**2 for i in range(1, 6)))')

curl -s -X POST "${BASE_URL}/submissions/batch" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary "{
    \"submissions\": [
      {\"language_id\": 29, \"source_code\": \"${P1}\"},
      {\"language_id\": 29, \"source_code\": \"${P2}\"},
      {\"language_id\": 29, \"source_code\": \"${P3}\"}
    ]
  }" | jq '{tokens}'

# =============================================================================
# 14. List submissions — GET /submissions (paginated)
# =============================================================================
section "14. List submissions — GET /submissions?page=1&per_page=10"

curl -s "${BASE_URL}/submissions?page=1&per_page=10" \
  -H "Authorization: Bearer ${TOKEN}" \
  | jq '{total, page, per_page, count: (.submissions | length)}'

# =============================================================================
# 15. Delete a submission — DELETE /submissions/{token}
# =============================================================================
section "15. Delete submission — DELETE /submissions/\$PYTHON_TOKEN"

# Create a disposable submission first
DISPOSE_TOKEN=$(curl -s -X POST "${BASE_URL}/submissions?wait=false" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"language_id\": 29, \"source_code\": \"$(b64 'print("disposable")')\"}" \
  | jq -r '.token')

echo "Deleting token: ${DISPOSE_TOKEN}"

curl -s -X DELETE "${BASE_URL}/submissions/${DISPOSE_TOKEN}" \
  -H "Authorization: Bearer ${TOKEN}" \
  -w "\nHTTP status: %{http_code}\n"

# Verify deletion
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  "${BASE_URL}/submissions/${DISPOSE_TOKEN}" \
  -H "Authorization: Bearer ${TOKEN}")
echo "After delete — GET status: ${HTTP_CODE} (expect 404)"

# =============================================================================
# 16. Get all languages — GET /languages
# =============================================================================
section "16. Get all languages — GET /languages"

curl -s "${BASE_URL}/languages" \
  | jq '[.[] | {id, name, version, image}] | sort_by(.id)'

# =============================================================================
# 17. Get single language — GET /languages/13 (Go)
# =============================================================================
section "17. Get single language — GET /languages/13 (Go)"

curl -s "${BASE_URL}/languages/13" \
  | jq '{id, name, version, source_file, compile_command, run_command, image}'

# =============================================================================
# 18. Get all execution statuses — GET /statuses
# =============================================================================
section "18. Get all statuses — GET /statuses"

curl -s "${BASE_URL}/statuses" \
  | jq '[.[] | {id, description}]'

# =============================================================================
# 19. Health check — GET /health
# =============================================================================
section "19. Health check — GET /health"

curl -s "${BASE_URL}/health" \
  | jq '{status, services}'

# =============================================================================
# 20. Readiness check — GET /readyz
# =============================================================================
section "20. Readiness check — GET /readyz"

curl -s -o /dev/null -w "HTTP %{http_code}\n" "${BASE_URL}/readyz"

# =============================================================================
# Summary
# =============================================================================
section "Done"
echo "All examples completed successfully."
echo "Final PYTHON_TOKEN=${PYTHON_TOKEN}"