#!/usr/bin/env bash
# The tenant mode demonstration: two teams with their own keys and budgets, streaming,
# failover before commit, one team running out of budget while the other carries on, and
# the accounting for all of it. Every provider is the local mock, so nothing costs money
# and no credentials are needed.
#
# Needs Go, Docker, curl, and lsof. Postgres and Redis come from compose.yaml. Each run
# uses a fresh database called llmgate_demo, so it can be repeated.
set -euo pipefail
cd "$(dirname "$0")/.."

for port in 18300 18301 18302 18303; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is in use, and the demo needs 18300 to 18303" >&2
    exit 1
  fi
done

WORK=$(mktemp -d)
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

section() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
wait_for() { for _ in $(seq 1 150); do curl -s -o /dev/null "$1" && return; sleep 0.1; done; echo "timed out waiting for $1" >&2; exit 1; }

export LLMGATE_DATABASE_URL="postgres://llmgate:llmgate_dev@127.0.0.1:${LLMGATE_POSTGRES_PORT:-15433}/llmgate_demo?sslmode=disable"
export LLMGATE_REDIS_URL="redis://127.0.0.1:${LLMGATE_REDIS_PORT:-16383}/3"
export LLMGATE_ADMIN_TOKEN=demo-local-admin-token
export LLMGATE_MOCK_PROVIDER_KEY=mock
export LLMGATE_ADMIN_URL=http://127.0.0.1:18302

section "Starting Postgres and Redis"
docker compose up -d --wait postgres redis >/dev/null
docker compose exec -T postgres psql -U llmgate -d llmgate -qc "DROP DATABASE IF EXISTS llmgate_demo WITH (FORCE)" -c "CREATE DATABASE llmgate_demo" >/dev/null

section "Building and starting the mock and the gateway"
go build -o "$WORK/llmgate" ./cmd/llmgate
go build -o "$WORK/llmgate-mock" ./cmd/llmgate-mock
{ cat configs/tenant.yaml; printf 'metrics:\n  listen: 127.0.0.1:18303\n'; } >"$WORK/config.yaml"
"$WORK/llmgate-mock" -listen 127.0.0.1:18301 2>"$WORK/mock.log" & PIDS+=($!)
"$WORK/llmgate" migrate --config "$WORK/config.yaml"
"$WORK/llmgate" serve --config "$WORK/config.yaml" 2>"$WORK/gateway.log" & PIDS+=($!)
wait_for http://127.0.0.1:18300/healthz
llmgate() { "$WORK/llmgate" "$@"; }

section "Two teams, each with a monthly budget and its own key"
llmgate teams create acme --budget-usd 1.00
llmgate teams create startup --budget-usd 0.005
ACME=$(llmgate keys create acme --name demo | head -1)
STARTUP=$(llmgate keys create startup --name demo | head -1)
echo "acme's key:    ${ACME:0:24}..."
echo "startup's key: ${STARTUP:0:24}..."

ask() { # key, model, stream
  curl -sS -N -D "$WORK/headers" http://127.0.0.1:18300/v1/chat/completions -H "Authorization: Bearer $1" \
    -d "{\"model\":\"$2\",\"stream\":$3,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
}
header() { grep -i "^$1:" "$WORK/headers" | cut -d' ' -f2- | tr -d '\r'; }
status() { sed -n 1p "$WORK/headers" | awk '{print $2}'; }

section "acme streams a response"
ask "$ACME" demo-fast true | grep -o '"content":"[^"]*"' | tr '\n' ' '
echo
echo "status $(status), budget enforcement: $(header X-Llmgate-Budget-Enforcement)"

section "acme asks for a route that fails before any content, and the gateway fails over"
ask "$ACME" demo-failover false >/dev/null
echo "status $(status), served by $(header X-Llmgate-Route) after $(header X-Llmgate-Attempts) attempts"

section "startup keeps asking until its budget can't cover another request"
for i in $(seq 1 20); do
  body=$(ask "$STARTUP" demo-fast false)
  code=$(status)
  if [ "$code" != 200 ]; then
    echo "request $i: $code $(echo "$body" | grep -o '"code":"[^"]*"')"
    break
  fi
  echo "request $i: $code"
done

section "acme carries on, unaffected"
ask "$ACME" demo-fast false >/dev/null
echo "status $(status)"

sleep 1 # Settlement finishes as each handler returns, a moment after the client has its answer.

section "What acme spent, attempt by attempt"
llmgate usage acme --attempts 5

section "What startup spent"
llmgate usage startup
echo
echo "startup still has some budget left, but less than the most one more request could"
echo "cost, which is what each attempt has to reserve before anything is sent."

section "The same, as the gateway's metrics see it"
curl -s http://127.0.0.1:18303/metrics | grep -E '^llmgate_(requests_total|budget_refusals_total|fallbacks_total)' | sed 's/^/  /'

section "Done"
echo "Every request went to the local mock. Nothing was sent to a real provider."
