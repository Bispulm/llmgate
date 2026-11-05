#!/usr/bin/env bash
# Measures what the gateway adds, using the same mock, payload, concurrency, duration,
# and warm-up for every run. Only the local mock is called, so nothing costs money.
#
# Results go to bench/results/<date>-<commit>/, with the conditions of the run. A run on a
# busy machine is labeled contended, and its numbers aren't a clean measurement.
#
# Needs Postgres and Redis from `make dev-up`, and jq.
set -euo pipefail

DURATION=${BENCH_DURATION:-20s}
WARMUP=${BENCH_WARMUP:-5s}
CONCURRENCY=${BENCH_CONCURRENCY:-16}
STREAMS=${BENCH_STREAMS:-200}
ADMIN_TOKEN=bench-local-admin-token
PG_CONTAINER=${BENCH_PG_CONTAINER:-llmgate-postgres-1}
DB_URL="postgres://llmgate:llmgate_dev@127.0.0.1:${LLMGATE_POSTGRES_PORT:-15433}/llmgate_bench?sslmode=disable"
REDIS_URL="redis://127.0.0.1:${LLMGATE_REDIS_PORT:-16383}/2"

SHA=$(git rev-parse --short HEAD)$(git diff --quiet && git diff --cached --quiet || echo "-dirty")
OUT=bench/results/$(date -u +%Y-%m-%d)-$SHA
WORK=$(mktemp -d)
GATEWAY_PID=""
MOCK_PID=""
mkdir -p "$OUT"

cleanup() {
  [ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

section() { printf '\n== %s\n' "$1"; }
load1() { sysctl -n vm.loadavg 2>/dev/null | awk '{print $2}' || cut -d' ' -f1 /proc/loadavg; }
cores() { sysctl -n hw.ncpu 2>/dev/null || nproc; }
wait_for() { for _ in $(seq 1 100); do curl -s -o /dev/null "$1" && return; sleep 0.1; done; echo "timed out waiting for $1" >&2; exit 1; }

section "Building"
go build -o "$WORK/llmgate" ./cmd/llmgate
go build -o "$WORK/llmgate-mock" ./cmd/llmgate-mock
go build -o "$WORK/llmgate-bench" ./cmd/llmgate-bench

LOAD_BEFORE=$(load1)
{
  echo "# Run conditions"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Date (UTC) | $(date -u +%Y-%m-%dT%H:%M:%SZ) |"
  echo "| Commit | $SHA |"
  echo "| Go | $(go version | awk '{print $3}') |"
  echo "| OS | $(uname -sr) |"
  echo "| CPU | $(sysctl -n machdep.cpu.brand_string 2>/dev/null || grep -m1 'model name' /proc/cpuinfo | cut -d: -f2) |"
  echo "| Cores | $(cores) |"
  echo "| Memory | $(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824 )) GiB |"
  echo "| Postgres and Redis | Compose containers ($(docker inspect -f '{{.Config.Image}}' "$PG_CONTAINER" 2>/dev/null), redis), through the Docker VM |"
  echo "| Workload | concurrency $CONCURRENCY, $DURATION measured after $WARMUP warm-up, $STREAMS streams for memory |"
  echo "| Load average before | $LOAD_BEFORE |"
} >"$OUT/environment.md"

section "Mock and database"
"$WORK/llmgate-mock" -listen 127.0.0.1:18301 2>"$WORK/mock.log" & MOCK_PID=$!
wait_for http://127.0.0.1:18301/__mock/stats
docker exec "$PG_CONTAINER" psql -U llmgate -d llmgate -tAc "SELECT 1 FROM pg_database WHERE datname = 'llmgate_bench'" | grep -q 1 ||
  docker exec "$PG_CONTAINER" psql -U llmgate -d llmgate -qc "CREATE DATABASE llmgate_bench"

prices() {
  for model in "$@"; do
    printf '    - {provider: mock, model: "%s", input_usd_per_mtok: "3.00", cached_input_usd_per_mtok: "0.30", cache_write_usd_per_mtok: "3.75", output_usd_per_mtok: "15.00"}\n' "$model"
  done
}
MODELS=("bench;tokens=16" "primary;fail=status;status=500" "slow;tokens=200;interval=50ms" "fair;tokens=4;latency=20ms")
common_tail() {
  cat <<YAML
limits:
  request_timeout: 60s
  first_event_timeout: 30s
  stream_idle_timeout: 30s
providers:
  - name: mock
    type: openai-compatible
    base_url: http://127.0.0.1:18301/v1
    api_key_env: BENCH_MOCK_KEY
    max_concurrency: ${1:-0}
    max_queue: ${2:-0}
    max_queue_per_team: ${3:-0}
models:
  - {alias: bench, max_output_tokens: 64, routes: [{provider: mock, model: "bench;tokens=16"}]}
  - alias: bench-failover
    max_output_tokens: 64
    routes:
      - {provider: mock, model: "primary;fail=status;status=500", tier: 0}
      - {provider: mock, model: "bench;tokens=16", tier: 1}
  - {alias: bench-slow, max_output_tokens: 1000, routes: [{provider: mock, model: "slow;tokens=200;interval=50ms"}]}
  - {alias: bench-fair, max_output_tokens: 64, routes: [{provider: mock, model: "fair;tokens=4;latency=20ms"}]}
YAML
}
dev_config() {
  cat <<YAML
server: {listen: 127.0.0.1:18300, shutdown_timeout: 5s}
auth: {dev_key_env: BENCH_DEV_KEY}
metrics: {listen: 127.0.0.1:18303}
YAML
  common_tail "$@"
}
tenant_config() {
  cat <<YAML
server: {listen: 127.0.0.1:18300, shutdown_timeout: 5s}
auth: {mode: tenant}
database: {url_env: BENCH_DATABASE_URL}
redis: {url_env: BENCH_REDIS_URL, key_prefix: "bench:"}
admin: {listen: 127.0.0.1:18302, token_env: BENCH_ADMIN_TOKEN}
metrics: {listen: 127.0.0.1:18303, pprof: true}
pricing:
  version: bench-1
  prices:
$(prices "${MODELS[@]}")
YAML
  common_tail "$@"
}
export BENCH_DEV_KEY=bench-dev-key BENCH_MOCK_KEY=mock BENCH_DATABASE_URL=$DB_URL BENCH_REDIS_URL=$REDIS_URL BENCH_ADMIN_TOKEN=$ADMIN_TOKEN

start_gateway() { # config file
  "$WORK/llmgate" serve --config "$1" 2>"$WORK/gateway.log" & GATEWAY_PID=$!
  wait_for http://127.0.0.1:18300/healthz
}
stop_gateway() { kill "$GATEWAY_PID" 2>/dev/null || true; wait "$GATEWAY_PID" 2>/dev/null || true; GATEWAY_PID=""; }
bench() { # label url key model [flags...]
  local label=$1 url=$2 key=$3 model=$4; shift 4
  LLMGATE_BENCH_KEY=$key "$WORK/llmgate-bench" -label "$label" -url "$url" -model "$model" \
    -concurrency "$CONCURRENCY" -duration "$DURATION" -warmup "$WARMUP" "$@" >"$OUT/$label.json"
}
cli() { LLMGATE_ADMIN_TOKEN=$ADMIN_TOKEN "$WORK/llmgate" "$@" --admin-url http://127.0.0.1:18302; }
team_key() { # team
  cli teams create "$1" --budget-usd 100000 >/dev/null 2>&1 || true
  cli keys create "$1" | head -1
}
metric() { curl -s http://127.0.0.1:18303/metrics | awk -v m="$1" '$1 == m {print $2}'; }
MOCK=http://127.0.0.1:18301/v1/chat/completions
GATEWAY=http://127.0.0.1:18300/v1/chat/completions

section "Overhead: the mock directly"
bench direct http://127.0.0.1:18301/v1/chat/completions mock "bench;tokens=16"
bench direct-stream "$MOCK" mock "bench;tokens=16" -stream

section "Overhead: development mode, no accounting"
dev_config >"$WORK/dev.yaml"
start_gateway "$WORK/dev.yaml"
bench dev "$GATEWAY" "$BENCH_DEV_KEY" bench
bench dev-stream "$GATEWAY" "$BENCH_DEV_KEY" bench -stream

section "Failover before commit"
bench dev-failover "$GATEWAY" "$BENCH_DEV_KEY" bench-failover

section "Memory per active stream"
IDLE_HEAP=$(metric go_memstats_heap_inuse_bytes); IDLE_GOROUTINES=$(metric go_goroutines)
# Each stream lasts about ten seconds, so the heap is sampled while all of them are open.
LLMGATE_BENCH_KEY=$BENCH_DEV_KEY "$WORK/llmgate-bench" -label streams -url "$GATEWAY" -model bench-slow -stream \
  -concurrency "$STREAMS" -duration 12s -warmup 0s >"$OUT/streams.json" & STREAMS_PID=$!
sleep 6
BUSY_HEAP=$(metric go_memstats_heap_inuse_bytes); BUSY_GOROUTINES=$(metric go_goroutines)
wait "$STREAMS_PID"
stop_gateway
{
  echo "| | Idle | $STREAMS streams | Per stream |"
  echo "|---|---|---|---|"
  awk -v i="$IDLE_HEAP" -v b="$BUSY_HEAP" -v n="$STREAMS" 'BEGIN { printf "| Go heap in use | %.1f MiB | %.1f MiB | %.1f KiB |\n", i/1048576, b/1048576, (b-i)/n/1024 }'
  awk -v i="$IDLE_GOROUTINES" -v b="$BUSY_GOROUTINES" -v n="$STREAMS" 'BEGIN { printf "| Goroutines | %d | %d | %.1f |\n", i, b, (b-i)/n }'
} >"$OUT/memory.md"

section "Overhead: tenant mode, with budgets and accounting"
tenant_config >"$WORK/tenant.yaml"
"$WORK/llmgate" migrate --config "$WORK/tenant.yaml" >/dev/null
start_gateway "$WORK/tenant.yaml"
KEY=$(team_key bench)
# A CPU profile of the gateway while it serves the non-streaming run, after warm-up.
(sleep "${WARMUP%s}"; curl -s "http://127.0.0.1:18303/debug/pprof/profile?seconds=10" >"$OUT/tenant-cpu.pprof") & PROFILE_PID=$!
bench tenant "$GATEWAY" "$KEY" bench
wait "$PROFILE_PID"
go tool pprof -top -nodecount=25 "$WORK/llmgate" "$OUT/tenant-cpu.pprof" >"$OUT/tenant-cpu.txt" 2>/dev/null || true
bench tenant-stream "$GATEWAY" "$KEY" bench -stream
stop_gateway

section "Fairness under a saturated team"
tenant_config 8 512 256 >"$WORK/fair.yaml"
start_gateway "$WORK/fair.yaml"
QUIET=$(team_key bench-quiet); NOISY=$(team_key bench-noisy)
LLMGATE_BENCH_KEY=$QUIET "$WORK/llmgate-bench" -label quiet-alone -url "$GATEWAY" -model bench-fair -concurrency 2 -duration "$DURATION" -warmup "$WARMUP" >"$OUT/quiet-alone.json"
LLMGATE_BENCH_KEY=$NOISY "$WORK/llmgate-bench" -label noisy -url "$GATEWAY" -model bench-fair -concurrency 64 -duration "$DURATION" -warmup "$WARMUP" >"$OUT/noisy.json" & NOISY_PID=$!
LLMGATE_BENCH_KEY=$QUIET "$WORK/llmgate-bench" -label quiet-beside-noisy -url "$GATEWAY" -model bench-fair -concurrency 2 -duration "$DURATION" -warmup "$WARMUP" >"$OUT/quiet-beside-noisy.json"
wait "$NOISY_PID"
stop_gateway

section "Liability and settlement"
docker exec "$PG_CONTAINER" psql -U llmgate -d llmgate_bench -c "
  SELECT team_id, state, count(*) AS attempts, sum(reserved_micros) AS reserved_micros,
         sum(charge_micros) AS charged_micros, count(*) FILTER (WHERE charge_kind = 'conservative') AS conservative,
         count(*) FILTER (WHERE over_reservation) AS over_reservation
  FROM attempts GROUP BY team_id, state ORDER BY team_id, state" >"$OUT/liability.txt"
docker exec "$PG_CONTAINER" psql -U llmgate -d llmgate_bench -tAc "
  SELECT count(*) FROM budget_periods p
  WHERE p.settled_micros <> COALESCE((SELECT sum(l.amount_micros) FROM ledger_entries l JOIN attempts a ON a.id = l.attempt_id WHERE a.budget_period_id = p.id), 0)
     OR p.reserved_micros <> COALESCE((SELECT sum(a.reserved_micros) FROM attempts a WHERE a.budget_period_id = p.id AND a.state IN ('reserved', 'dispatched', 'uncertain')), 0)" >"$WORK/mismatched"

LOAD_AFTER=$(load1)
CONTENDED=no
awk -v a="$LOAD_BEFORE" -v b="$LOAD_AFTER" -v c="$(cores)" 'BEGIN { exit !((a > c/4) || (b > c/4)) }' && CONTENDED=yes
{
  echo "| Load average after | $LOAD_AFTER |"
  echo "| Contended | $CONTENDED (a run is contended if the 1-minute load average exceeded a quarter of the cores before or after) |"
} >>"$OUT/environment.md"

section "Report"
{
  echo "# Benchmark results, $SHA"
  echo
  [ "$CONTENDED" = yes ] && { echo "**This run was contended. Other work was using the machine, so these numbers are not a clean measurement.**"; echo; }
  cat "$OUT/environment.md"
  echo
  echo "## Added latency, non-streaming"
  echo
  "$WORK/llmgate-bench" compare "$OUT/direct.json" "$OUT/dev.json" "$OUT/tenant.json"
  echo
  echo "## Added latency and time to first byte, streaming"
  echo
  "$WORK/llmgate-bench" compare "$OUT/direct-stream.json" "$OUT/dev-stream.json" "$OUT/tenant-stream.json"
  echo
  echo "The difference between development and tenant mode is the cost of authentication, team limits, and accounting."
  echo
  echo "## Failover before commit"
  echo
  echo "Every request first fails on a route answering 500, then succeeds on the next. Compared with going straight to the working route:"
  echo
  "$WORK/llmgate-bench" compare "$OUT/dev.json" "$OUT/dev-failover.json"
  echo
  echo "## Memory per active stream"
  echo
  cat "$OUT/memory.md"
  echo
  echo "## Fairness under a saturated team"
  echo
  echo "The provider allows 8 attempts at once. A noisy team keeps 64 requests in flight, and a quiet team 2. The quiet team alone, then beside the noisy one:"
  echo
  "$WORK/llmgate-bench" compare "$OUT/quiet-alone.json" "$OUT/quiet-beside-noisy.json" "$OUT/noisy.json"
  echo
  echo "## Where tenant mode spends CPU"
  echo
  echo "A 10-second CPU profile of the gateway during the non-streaming tenant run, top 25 functions:"
  echo
  echo '```'
  sed -n '1,40p' "$OUT/tenant-cpu.txt"
  echo '```'
  echo
  echo "## Liability and settlement"
  echo
  echo "Every attempt from the tenant-mode runs, by final state. Reserved is what was held before each attempt, and charged is what it cost."
  echo
  echo '```'
  cat "$OUT/liability.txt"
  echo '```'
  echo
  echo "Budget periods whose totals disagree with the ledger: $(cat "$WORK/mismatched")."
} >"$OUT/README.md"
cat "$OUT/README.md"
