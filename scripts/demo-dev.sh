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
  "$WORK/llmgate" serve --config "$WORK/config.yaml" 2>"$WORK/gateway.log" &
PIDS+=($!)

wait_for "$MOCK/__mock/stats"
wait_for "$GATEWAY/healthz"

KEY=demo-dev-key
ask() { # alias, extra JSON fields
  curl -sS -N -D "$WORK/headers" "$GATEWAY/v1/chat/completions" -H "Authorization: Bearer $KEY" \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]$2}"
  echo
}
status() { sed -n 1p "$WORK/headers" | tr -d '\r'; }

section "Non-streaming request through the gateway"
ask demo-fast ''
status
grep -iE '^x-llmgate-' "$WORK/headers" | tr -d '\r'

section "Streaming request, with usage"
ask demo-fast ',"stream":true,"stream_options":{"include_usage":true}'

section "An unsupported field is rejected by name, and never reaches the provider"
ask demo-fast ',"tools":[]'
status

section "The first route fails before any content: the request falls back"
ask demo-failover ',"stream":true'
status
grep -iE '^x-llmgate-(route|attempts)' "$WORK/headers" | tr -d '\r'

section "Every route fails before any content: an ordinary HTTP error"
ask demo-fails-before-content ',"stream":true'
status

section "The provider fails after content: one error event, and no [DONE]"
ask demo-fails-midstream ',"stream":true'
status

section "The provider is rate limiting: the status and retry hint are passed on"
ask demo-rate-limited ''
status
grep -i '^retry-after' "$WORK/headers" | tr -d '\r'

section "A client that disconnects cancels the upstream request"
curl -sS -N --max-time 0.5 -o /dev/null "$GATEWAY/v1/chat/completions" -H "Authorization: Bearer $KEY" \
  -d '{"model":"demo-slow","stream":true,"messages":[{"role":"user","content":"hello"}]}' 2>/dev/null || true
sleep 0.3
printf 'mock counters for the canceled stream: '
curl -sS "$MOCK/__mock/stats" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["slow"])'

section "What the gateway recorded for each attempt"
python3 -I - "$WORK/gateway.log" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r.get("msg") != "upstream attempt":
        continue
    print("{alias:28} {outcome:16} committed={committed!s:5} usage={usage_state:8} completion_tokens={tokens} text_events={text_events}".format(
        tokens=r.get("completion_tokens", "-"), **r))
PY

section "Done"
