# Observability

The gateway exports Prometheus metrics, OpenTelemetry traces, and structured logs. None of them contains a prompt, a completion, a raw key, a provider credential, or an authorization header, and tests check that.

## Metrics

Metrics are off unless `metrics.listen` is set. They're served at `/metrics` on that listener, never on the gateway's own. The listener has no authentication, so it has to be loopback unless `metrics.allow_non_loopback: true`.

```yaml
metrics:
  listen: 127.0.0.1:18303
```

Every label takes its values from a fixed set, or from configuration: alias, provider, and model names. A value outside the expected set is reported as `other`. No request ID, key, team, or anything a client sends becomes a label, so the number of series stays bounded.

| Metric | Labels | Meaning |
|---|---|---|
| `llmgate_requests_total` | `alias`, `status`, `stream` | Chat completion requests. `alias` is empty when the request never resolved to one, such as a 401. `status` is `0` if the client left before anything was written. |
| `llmgate_request_duration_seconds` | `alias`, `stream` | Time to finish a request, including every attempt |
| `llmgate_attempts_total` | `provider`, `model`, `outcome`, `error_kind`, `reason` | Upstream attempts. `reason` is `primary`, `fallback`, or `retry_after`. |
| `llmgate_attempt_duration_seconds` | `provider`, `model` | Time taken by each attempt |
| `llmgate_time_to_first_event_seconds` | `provider`, `model` | Time from a request arriving to the first content of a stream |
| `llmgate_fallbacks_total` | `provider`, `model` | Attempts made because an earlier one failed before the response committed |
| `llmgate_route_skips_total` | `alias`, `reason` | Routes not used: `capability`, `breaker_open`, `rate_limit_hold`, `saturated`, or `unpriced` |
| `llmgate_breaker_state` | `provider`, `model`, `state` | 1 for each route's current breaker state on this replica |
| `llmgate_route_hold_seconds` | `provider`, `model` | How much longer a provider's retry hint keeps a route out of use on this replica |
| `llmgate_limiter_in_flight`, `llmgate_limiter_queued`, `llmgate_limiter_shed_total` | `provider` | A provider's concurrency limiter on this replica |
| `llmgate_cache_requests_total` | `alias`, `result` | Requests that opted in to the cache: `hit`, `miss`, or `bypass` |
| `llmgate_team_limit_denials_total` | `reason` | Requests refused by a team limit: `requests`, `tokens`, or `in_flight` |
| `llmgate_budget_refusals_total` | | Attempts refused because a team's budget couldn't cover them |
| `llmgate_charged_micros_total` | `provider`, `model`, `kind` | Micro-USD charged as attempts settle, by `reported`, `rejected`, or `conservative` |
| `llmgate_settlement_failures_total` | | Attempts left for the reconciler because settling them failed |
| `llmgate_reconciled_attempts_total` | `action` | What the reconciler did: `released`, `marked_uncertain`, or `charged_conservatively` |
| `llmgate_dependency_errors_total` | `dependency`, `operation` | Failed calls to Postgres or Redis |

Go runtime and process metrics are included as well.

Breakers, holds, and provider queues are per replica, so each replica reports its own. Money isn't: charges here are a view of what settled, and the ledger in Postgres is the authority. Use `llmgate usage` or the admin API for exact figures.

## Traces

Traces are off unless `tracing.otlp_endpoint` is set. They're exported over OTLP/HTTP in batches, and the last ones are flushed at shutdown. Plaintext is only allowed to loopback, because spans name teams and routes.

```yaml
tracing:
  otlp_endpoint: http://127.0.0.1:4318   # /v1/traces is added to a base URL
  sample_ratio: 0.1                      # New traces only; a caller's decision is kept. 0 traces only those.
```

| Span | Parent | Attributes |
|---|---|---|
| `chat.completions` | A caller's `traceparent`, if sent. Only authenticated requests are traced, so a trace header alone can't get spans exported. | `llmgate.request_id`, `llmgate.alias`, `llmgate.stream`, `llmgate.team`, `llmgate.attempts`, `http.response.status_code` |
| `attempt` | `chat.completions` | `llmgate.provider`, `llmgate.model`, `llmgate.attempt.number`, `llmgate.attempt.reason`, `llmgate.outcome`, `llmgate.error_kind`, `llmgate.committed`, `llmgate.upstream_status`, `llmgate.charge_kind`, `llmgate.charge_micros`, and `llmgate.admitted` when the ledger refused it |
| `ledger.reserve` | `attempt` | `llmgate.reserved_micros` |
| `ledger.mark_dispatched` | `attempt` | |
| `ledger.finish` | `attempt` | `llmgate.tries`, `llmgate.uncertain` |

A failed ledger step's status says why, such as `budget_exhausted` or `unavailable`, and never carries the database's error text. The gateway doesn't send trace headers to providers.

## Dashboards

`deploy/grafana/dashboards/llmgate.json` covers traffic, latency, routing, breakers, queues, charges, refusals, reconciliation, the cache, and dependency errors. `deploy/grafana/provisioning` loads it, with a Prometheus data source at `http://prometheus:9090`. A test checks that every metric the dashboard queries is one the gateway exports.

## Logs

Logs are JSON on standard error. Each attempt is one line with its outcome, delivery, usage state, and token counts. Admin changes are logged as `admin action`, with no key material. Database and Redis errors are logged, and never returned to clients.
