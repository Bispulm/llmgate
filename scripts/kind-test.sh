#!/usr/bin/env bash
# Installs the chart on a local kind cluster and checks it serves, accounts, and
# survives a rolling restart. Only the mock provider is used, so nothing costs money.
#
# The cluster gets a kubeconfig of its own, so the default one, and its current context,
# are never changed. Every kubectl and helm call also names its context. The cluster is
# deleted at the end unless KEEP_CLUSTER=1.
set -euo pipefail

CLUSTER=llmgate-dev
CONTEXT=kind-$CLUSTER
NAMESPACE=llmgate
GATEWAY_PORT=${LLMGATE_KIND_GATEWAY_PORT:-18306}
ADMIN_PORT=${LLMGATE_KIND_ADMIN_PORT:-18307}
ADMIN_TOKEN=kind-test-admin-token
WORK=$(mktemp -d)
PIDS=()
CREATED=0
# Its own tag, so it never replaces the image the Compose stack uses.
IMAGE=llmgate:kind-test
export KUBECONFIG="$WORK/kubeconfig"

cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  # Only a cluster this run created is deleted, never one that was already there.
  if [ "$CREATED" = 1 ] && [ "${KEEP_CLUSTER:-0}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

k() { kubectl --context "$CONTEXT" -n "$NAMESPACE" "$@"; }
section() { printf '\n== %s\n' "$1"; }

section "Cluster"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind export kubeconfig --name "$CLUSTER" >/dev/null
else
  kind create cluster --name "$CLUSTER" --wait 120s
  CREATED=1
fi
docker build -q -t "$IMAGE" . >/dev/null
# Only the gateway is loaded: the cluster pulls Postgres and Redis itself, since kind
# can't load a multi-platform image that's only partly stored locally.
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
kubectl --context "$CONTEXT" create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f - >/dev/null

section "Dependencies"
k apply -f deploy/kind/dependencies.yaml >/dev/null
k rollout status deploy/postgres deploy/redis --timeout=180s
k create secret generic llmgate \
  --from-literal=database-url='postgres://llmgate:llmgate_test@postgres:5432/llmgate?sslmode=disable' \
  --from-literal=redis-url='redis://redis:6379/0' \
  --from-literal=admin-token="$ADMIN_TOKEN" \
  --from-literal=mock-provider-key=mock \
  --dry-run=client -o yaml | k apply -f - >/dev/null

section "Chart"
helm --kube-context "$CONTEXT" -n "$NAMESPACE" upgrade --install llmgate deploy/helm/llmgate \
  --set mock.enabled=true --set image.tag=kind-test --wait --timeout 4m
k rollout status deploy/llmgate --timeout=120s

k port-forward svc/llmgate "$GATEWAY_PORT:18300" >"$WORK/pf-gateway.log" 2>&1 & PIDS+=($!)
k port-forward svc/llmgate-admin "$ADMIN_PORT:18302" >"$WORK/pf-admin.log" 2>&1 & PIDS+=($!)
ADMIN_FORWARD=$!
# Port-forwards take a moment to listen. Any HTTP answer means one is ready.
ready() { curl -s -o /dev/null --max-time 2 "$1"; }
for _ in $(seq 1 100); do ready "http://127.0.0.1:$GATEWAY_PORT/healthz" && ready "http://127.0.0.1:$ADMIN_PORT/" && break; sleep 0.2; done

section "Serving and accounting"
go build -o "$WORK/llmgate" ./cmd/llmgate
cli() { LLMGATE_ADMIN_TOKEN=$ADMIN_TOKEN "$WORK/llmgate" "$@" --admin-url "http://127.0.0.1:$ADMIN_PORT"; }
cli teams create kind-team --budget-usd 5.00 >/dev/null
KEY=$(cli keys create kind-team | head -1)
ask() {
  curl -s --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions" -H "Authorization: Bearer $KEY" \
    -d '{"model":"demo-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}'
}
status=$(ask)
[ "$status" = 200 ] || { echo "request failed with $status"; exit 1; }
echo "a streaming request through the chart: $status"

section "Rolling restart under traffic"
# A port-forward follows one pod, so traffic goes through a pod inside the cluster.
k run load --image=curlimages/curl:8.11.1 --restart=Never --command -- sh -c \
  "ok=0; bad=0; end=\$((\$(date +%s)+60)); while [ \$(date +%s) -lt \$end ]; do
     code=\$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' http://llmgate:18300/v1/chat/completions -H 'Authorization: Bearer $KEY' -d '{\"model\":\"demo-fast\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}');
     if [ \"\$code\" = 200 ]; then ok=\$((ok+1)); else bad=\$((bad+1)); echo \"failed: \$code\"; fi; done; echo \"ok=\$ok bad=\$bad\"" >/dev/null
sleep 5
kill "$ADMIN_FORWARD" 2>/dev/null || true
k rollout restart deploy/llmgate >/dev/null
k rollout status deploy/llmgate --timeout=120s
k wait --for=jsonpath='{.status.phase}'=Succeeded pod/load --timeout=180s >/dev/null
result=$(k logs load | tail -1)
echo "requests during the restart: $result"
k delete pod load --wait=false >/dev/null
case "$result" in *"bad=0") ;; *) echo "requests failed during the restart"; k logs load | head -5; exit 1;; esac

section "Usage"
# A port-forward stays with the pod it started on, and the restart replaced it.
k port-forward svc/llmgate-admin "$ADMIN_PORT:18302" >"$WORK/pf-admin-2.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 100); do ready "http://127.0.0.1:$ADMIN_PORT/" && break; sleep 0.2; done
cli usage kind-team

section "A rollback restores the configuration"
config_of() { k get deploy/llmgate -o jsonpath='{.spec.template.spec.volumes[0].configMap.name}'; }
before=$(config_of)
helm --kube-context "$CONTEXT" -n "$NAMESPACE" upgrade llmgate deploy/helm/llmgate \
  --set mock.enabled=true --set image.tag=kind-test --set config.auth.key_cache_ttl=20s --wait --timeout 4m >/dev/null
changed=$(config_of)
helm --kube-context "$CONTEXT" -n "$NAMESPACE" rollback llmgate 1 --wait --timeout 4m >/dev/null
after=$(config_of)
echo "configuration: $before, then $changed, then after the rollback $after"
[ "$before" != "$changed" ] && [ "$after" = "$before" ] || { echo "the rollback didn't restore the configuration"; exit 1; }
k get configmap "$after" >/dev/null || { echo "the restored configuration doesn't exist"; exit 1; }

echo
echo "kind test passed"
