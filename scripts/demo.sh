#!/usr/bin/env bash
# Runs the local demonstration against the mock provider. It makes no external
# network calls and needs no credentials.
#
# Ports default to 18300 (gateway) and 18301 (mock). Override them with
# LLMGATE_DEMO_GATEWAY_PORT and LLMGATE_DEMO_MOCK_PORT if those are in use.
set -euo pipefail

cd "$(dirname "$0")/.."

GATEWAY_PORT="${LLMGATE_DEMO_GATEWAY_PORT:-18300}"
MOCK_PORT="${LLMGATE_DEMO_MOCK_PORT:-18301}"
GATEWAY="http://127.0.0.1:${GATEWAY_PORT}"
MOCK="http://127.0.0.1:${MOCK_PORT}"

for port in "$GATEWAY_PORT" "$MOCK_PORT"; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "port $port is in use. Set LLMGATE_DEMO_GATEWAY_PORT or LLMGATE_DEMO_MOCK_PORT." >&2
    exit 1
  fi
done

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

section() { printf '\n== %s\n' "$1"; }

wait_for() {
  for _ in $(seq 1 100); do
    if curl -fsS -o /dev/null "$1" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  echo "timed out waiting for $1" >&2
  exit 1
}

section "Building"
go build -o "$WORK/llmgate" ./cmd/llmgate
go build -o "$WORK/llmgate-mock" ./cmd/llmgate-mock

sed -e "s/127.0.0.1:18300/127.0.0.1:${GATEWAY_PORT}/" \
    -e "s/127.0.0.1:18301/127.0.0.1:${MOCK_PORT}/" configs/demo.yaml > "$WORK/config.yaml"

"$WORK/llmgate-mock" -listen "127.0.0.1:${MOCK_PORT}" 2>"$WORK/mock.log" &
PIDS+=($!)
LLMGATE_DEV_KEY=demo-dev-key LLMGATE_MOCK_PROVIDER_KEY=demo-provider-key \
  "$WORK/llmgate" serve -config "$WORK/config.yaml" 2>"$WORK/gateway.log" &
PIDS+=($!)

wait_for "$MOCK/__mock/stats"
wait_for "$GATEWAY/healthz"

section "Gateway health"
curl -sS "$GATEWAY/healthz"
echo

section "Mock provider, OpenAI format"
curl -sS "$MOCK/v1/chat/completions" -H 'Authorization: Bearer mock' \
  -d '{"model":"alpha;tokens=3","messages":[{"role":"user","content":"hello"}]}'

section "Mock provider, Anthropic format, streaming"
curl -sS -N "$MOCK/v1/messages" -H 'x-api-key: mock' -H 'anthropic-version: 2026-10-21' \
  -d '{"model":"beta;tokens=2","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}'

section "Mock provider, injected 429"
curl -sS -i "$MOCK/v1/chat/completions" -H 'Authorization: Bearer mock' \
  -d '{"model":"alpha;fail=status;status=429;retry_after=2","messages":[{"role":"user","content":"hello"}]}' \
  | grep -iE '^(HTTP/|retry-after|\{)'

section "Mock counters"
curl -sS "$MOCK/__mock/stats"

section "Done"
