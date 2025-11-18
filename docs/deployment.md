# Deployment

The gateway is one binary. It needs Postgres for money and keys and Redis for team limits and the cache, and it never applies database migrations by itself.

## The image

```bash
make image        # docker build -t llmgate:dev .
```

The image is about 53 MB. It holds `llmgate` and the mock provider `llmgate-mock`, runs as the numeric non-root user 65532 on a distroless base, and pins its base images by digest. The entrypoint is `llmgate`, so the container's arguments are a command such as `serve --config /etc/llmgate/config.yaml`.

## Compose

```bash
make stack-up     # Postgres, Redis, migrations, the mock, the gateway, Prometheus, and Grafana
make stack-down
```

Everything binds to the host's loopback:

| Port | Service |
|---|---|
| 18300 | Gateway |
| 18301 | Mock provider |
| 18302 | Admin API |
| 18303 | Metrics |
| 18304 | Prometheus |
| 18305 | Grafana, with the dashboard provisioned and anonymous read-only access |

The stack runs tenant mode from `configs/compose.yaml`, against the mock only. The admin token defaults to a local placeholder. Set `LLMGATE_ADMIN_TOKEN` to change it. `make dev-up` still starts only Postgres and Redis, for tests.

## Helm

The chart in `deploy/helm/llmgate` doesn't include Postgres or Redis. Create a Secret with your credentials, and the chart reads them from it:

```bash
kubectl create secret generic llmgate \
  --from-literal=database-url='postgres://...' \
  --from-literal=redis-url='redis://...' \
  --from-literal=admin-token="$(openssl rand -hex 24)" \
  --from-literal=openai-api-key='...'

helm upgrade --install llmgate deploy/helm/llmgate -f my-values.yaml
```

`values.yaml` holds the gateway's configuration under `config`, in the same form as `docs/admin.md` describes, and maps environment variables to Secret keys under `secretEnv`. The chart never creates the Secret, so credentials stay out of values files.

| What | How |
|---|---|
| Migrations | A pre-install and pre-upgrade hook Job runs `llmgate migrate`. Turn it off with `migrate.enabled: false`. |
| Services | The gateway, the admin API, and metrics each have their own ClusterIP Service, so the admin API can be kept off any ingress. |
| Pod security | Non-root, read-only root filesystem, no privileges, and all capabilities dropped. No service account token. |
| Probes | Readiness is `/readyz`, checked every 2 seconds. Liveness is `/healthz`. Neither depends on Postgres or Redis, because a budget check already fails closed when they're down. |
| Shutdown | `server.drain_delay` (5 seconds) keeps a stopping pod serving while it reports not ready, so Services stop sending it new connections first. Then in-flight requests have `server.shutdown_timeout` to finish, within a 45-second grace period. |
| Availability | Two replicas, and a PodDisruptionBudget keeping one. |
| Mock | `mock.enabled: true` adds the mock provider, for demos and tests. |

The default values route to the mock, with made-up prices. Replace `config.providers`, `config.models`, and `config.pricing` with real ones before pointing traffic at it.

Breakers and provider concurrency limits are per replica, so with more replicas a provider sees up to that many times `max_concurrency`. Budgets and team limits hold across replicas.

## Testing the chart on kind

```bash
./scripts/kind-test.sh      # KEEP_CLUSTER=1 keeps the cluster afterwards
```

It creates the kind cluster `llmgate-dev`, names the context `kind-llmgate-dev` in every command, and installs the chart in the namespace `llmgate`, with test copies of Postgres and Redis and the mock. Then it:

1. Sends a streaming request through the chart.
2. Restarts the deployment while a pod inside the cluster sends requests for a minute, and fails if any of them failed.
3. Prints the team's usage, which should show every attempt reconciled.

In the last run, 12,271 requests went through a rolling restart without a failure, and all 12,272 attempts reconciled, with nothing left reserved.

## Checking a configuration

```bash
llmgate validate --config config.yaml
```

This checks a configuration the way `serve` would, without credentials or connections, so it can run in CI against what the chart renders.
