# Running tenant mode

Tenant mode gives each team its own API keys, a monthly budget, and optional rate limits. Postgres decides every spending question, and Redis holds rate limits. See [ADR 0002](adr/0002-postgres-is-the-budget-authority.md) for why.

## Setting up

```bash
make dev-up        # Postgres on 127.0.0.1:15433, Redis on 127.0.0.1:16383

export LLMGATE_DATABASE_URL='postgres://llmgate:llmgate_dev@127.0.0.1:15433/llmgate?sslmode=disable'
export LLMGATE_REDIS_URL='redis://127.0.0.1:16383/0'
export LLMGATE_ADMIN_TOKEN="$(openssl rand -hex 24)"
export LLMGATE_MOCK_PROVIDER_KEY=mock

go run ./cmd/llmgate migrate --config configs/tenant.yaml
go run ./cmd/llmgate-mock &
go run ./cmd/llmgate serve --config configs/tenant.yaml
```

The gateway never applies migrations by itself. It refuses to start until the schema is current, so a schema change is always a deliberate step.

The admin token has to be at least 16 characters. It's only ever read from an environment variable, never from a flag, so it doesn't appear in a process listing.

## The CLI

Every command below talks to the admin API at `$LLMGATE_ADMIN_URL`, or `http://127.0.0.1:18302`, with the token in `$LLMGATE_ADMIN_TOKEN`. Add `--json` to any of them for the API's own JSON.

```bash
llmgate teams create acme --budget-usd 25.00 --requests-per-minute 600
llmgate teams list
llmgate teams show acme
llmgate teams set-limits acme --requests-per-minute 120 --max-in-flight 8

llmgate keys create acme --name ci     # The raw key is printed once, and never again
llmgate keys list acme
llmgate keys revoke lgk_0123456789abcdef

llmgate budgets set acme --limit-usd 50
llmgate budgets show acme

llmgate usage acme --attempts 20       # This month's spending, by route, and recent attempts
llmgate providers status               # Circuit breakers, as this replica sees them
llmgate reconcile                      # One reconciliation pass, now
llmgate attempts late-usage att_... --prompt-tokens 812 --completion-tokens 120
```

Amounts are exact decimal USD. Internally they're integer micro-USD, and no floating point is involved anywhere.

## What changes take effect when

| Change | Takes effect |
|---|---|
| Revoking a key | Within `auth.key_cache_ttl` on every replica. The default is 30 seconds, and the maximum is 5 minutes. The replica that served the revoke usually stops accepting the key at once, but a lookup already in progress there can put it back in the cache until the TTL runs out. |
| A team's rate or in-flight limits | Within `auth.key_cache_ttl`, because they're cached with the key. |
| A budget | At once, on every replica, because each admission reads it in the same transaction. Lowering it below what's already spent or reserved blocks new work, and never cancels work in flight. |
| Prices | Only through a new `pricing.version`. A stored price list can't change, so every charge can be recomputed from the version its attempt recorded. |

## The response cache

An alias can let clients reuse answers to identical requests:

```yaml
models:
  - alias: demo-fast
    cache:
      ttl: 10m
```

A request uses the cache only when its client also sends `X-Llmgate-Cache: allow`, because a cached answer means sampling no longer varies. Streams are never cached. The response says `X-Llmgate-Cache: hit`, `miss`, or `bypass`.

- **Teams never share.** The team is part of the key and of where an entry lives.
- **A hit costs nothing.** The lookup happens before any reservation and leaves no attempt behind, so a team that's out of budget can still read answers it already paid for. A hit still counts as a request for rate limits, but uses no tokens. Its `usage` is what the original response reported.
- **Only good answers are stored:** complete, with a stop or length finish, and with reported usage. Never failures, refusals, or partial streams.
- **Identical requests arriving together** on one replica make one upstream call, and the others wait for it.
- **Bounded.** Each entry is limited by `cache.max_entry_bytes` (256 KiB by default) and its TTL (at most 24 hours). Redis's `maxmemory` bounds the total.
- **A Redis failure is a miss.** It adds at most about 100ms to one request, and then the cache is skipped for a second.

Any change to an alias's routes changes its revision, which is part of the key, so answers from an old configuration aren't served.

## Operational requirements

- **Synchronized clocks.** Leases and grace periods are written and checked by each replica's own clock. If one replica's clock runs more than a minute behind the others, their reconcilers can release its reservations before it marks them dispatched. Nothing is overspent, because it then refuses to send, but its requests fail with 503 `accounting_unavailable`. Run NTP or an equivalent.
- **A team's admissions take turns.** Each replica admits one attempt per team at a time, and replicas take turns on the team's budget row in Postgres. A team's admission rate is therefore bounded by Postgres latency. If the queue outlasts `limits.admission_timeout` (5 seconds by default), the request fails closed with 503 `accounting_unavailable`. Other teams aren't affected.
- **Rate limit unauthenticated traffic in front of the gateway.** A well-formed key that doesn't exist costs one database query before it's remembered as unknown. Unknown keys are remembered separately from real ones, so they can't push real keys out of the cache, but a flood of distinct made-up keys still reaches Postgres.
- **Give the cache its own Redis in production**, with `cache.redis_url_env`, and a `maxmemory` policy that suits a cache. Sharing one Redis means memory pressure from cached answers could evict rate-limit state.

## The admin API

Every endpoint needs `Authorization: Bearer <admin token>`. It listens on `admin.listen`, which has to be loopback unless `admin.allow_non_loopback: true`. It's never served on the gateway's own listener, and a client key can't reach it.

| Method and path | Does |
|---|---|
| `GET /admin/v1/teams` | Lists teams |
| `POST /admin/v1/teams` | Creates a team: `{"id", "budget_limit_micros", "limits": {...}}` |
| `GET /admin/v1/teams/{team}` | A team and its budget this month |
| `PUT /admin/v1/teams/{team}/budget` | `{"limit_micros"}` |
| `PUT /admin/v1/teams/{team}/limits` | `{"requests_per_minute", "tokens_per_minute", "max_in_flight"}`, where 0 means no limit |
| `GET /admin/v1/teams/{team}/keys` | Lists keys, without anything usable as a key |
| `POST /admin/v1/teams/{team}/keys` | Creates a key, returned once as `key` |
| `DELETE /admin/v1/keys/{key}` | Revokes a key |
| `GET /admin/v1/teams/{team}/usage` | This month's position, attempts by state, and spending by route |
| `GET /admin/v1/teams/{team}/attempts?limit=50` | Recent attempts, newest first, at most 500 |
| `POST /admin/v1/attempts/{attempt}/usage` | Applies a late usage report, at most once |
| `GET /admin/v1/providers` | Each alias's routes, breakers, and rate-limit holds on this replica |
| `POST /admin/v1/reconcile` | Runs one reconciliation pass |

Request bodies are decoded strictly: an unknown field is an error, rather than a setting quietly left at zero. Errors look like `{"error": {"code", "message"}}`, and never include database details, which are logged instead. Every change is logged as an `admin action`, without any key material.

## Reading usage

`llmgate usage` shows each attempt's state:

| State | Meaning |
|---|---|
| `reconciled` | Settled. The charge is final, unless it was conservative and late usage corrects it. |
| `released` | Never sent, so it cost nothing. |
| `uncertain` | May have cost money, and nobody knows how much yet. Its reservation stays held until usage arrives or `accounting.uncertain_grace` passes, and then it's charged the full reservation. |
| `dispatched` | In flight, or its owner stopped before settling. The reconciler takes over when its lease runs out. |
| `reserved` | Admitted but not sent yet. If its owner stops, the reconciler releases it. |

A charge's kind says where it came from: `reported` from the provider's usage report, `rejected` for a refusal that cost nothing, and `conservative` for the full reservation of an attempt whose cost was never reported.
