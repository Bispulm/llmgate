# Runbooks

What the gateway does when something fails, how to recognize it, and what to do. Every behavior here is tested. Money is never adjusted by hand: the ledger in Postgres settles, reconciles, and corrects itself, and the only manual correction is applying a provider's usage report to an attempt.

## Postgres is down or slow

**What you see:** clients get 503 `accounting_unavailable`, or 503 `auth_unavailable` for keys that aren't cached. `llmgate_dependency_errors_total{dependency="postgres"}` rises. Logs say "the ledger could not admit an attempt" or "a key could not be checked".

**What the gateway does:** nothing new is sent upstream, because no attempt can reserve its cost. Each check gives up after `limits.admission_timeout` (5 seconds by default), so a database that has hung refuses requests quickly. Requests already streaming carry on. Their settlement retries briefly, and if it still can't reach Postgres, the attempt stays open with its liability held.

**What to do:**
1. Restore Postgres. The gateway doesn't need a restart.
2. Let the reconciler catch up, or run `llmgate reconcile`. An attempt left dispatched is marked uncertain when its lease runs out, and charged its full reservation after `accounting.uncertain_grace` unless usage arrives first.
3. Check `llmgate usage <team>` for attempts still open.

## Redis is down or hung

**What you see:** with the default `rate_limits.on_redis_failure: closed`, teams that have rate or in-flight limits get 503 `rate_limit_unavailable`. Cached answers stop being served, but requests still succeed. `llmgate_dependency_errors_total{dependency="redis"}` rises, and logs say team limits or the cache couldn't be reached.

**What the gateway does:** each Redis call gives up at its deadline even if Redis accepts connections and never answers. After a failure, a replica skips Redis for a second, so an outage adds almost nothing to each request. Budgets are unaffected, because they never depend on Redis. Teams without limits aren't affected.

**What to do:**
1. Restore Redis.
2. If refusing limited teams is worse than briefly looser limits, set `rate_limits.on_redis_failure: open`. Each replica then enforces each team's limits on its own, so the worst case is the limit times the number of replicas.

## A provider is failing

**What you see:** `llmgate_attempts_total` shows failures by `error_kind`, `llmgate_fallbacks_total` rises, and `llmgate_breaker_state` shows routes open or half-open. `llmgate providers status` shows each replica's breakers. Clients see 503 `no_route_available` with a Retry-After hint when every route is unusable.

**What the gateway does:** a failure before the response commits falls back to the next capable route, within the alias's attempt limit and the request's deadline. After commit it never switches provider: the stream ends with an error event and no `[DONE]`. Breakers open on sustained failures, probe after their cooldown, and close when probes succeed. Each failed attempt is accounted for separately.

**What to do:** check the provider's status page. If the failures are `billing`, the provider account is out of credit. If they're `auth` or `not_found`, its key or model name in the configuration is wrong.

## Attempts are piling up as uncertain

**What you see:** `llmgate_reconciled_attempts_total{action="marked_uncertain"}` or `{action="charged_conservatively"}` rising, and teams' reserved amounts staying high in `llmgate usage`.

**Why:** an attempt is uncertain when it may have cost money and no usage arrived. That happens when a client disconnects mid-stream, a provider omits usage, or a replica crashes. Each one keeps its full reservation, and after the grace period it's charged that reservation, which is the most it could have cost.

**What to do:**
1. Look at `llmgate usage <team> --attempts 50` for the outcomes involved.
2. If the provider's usage export has the real counts, apply them: `llmgate attempts late-usage <attempt> --prompt-tokens N --completion-tokens N`. This corrects a conservative charge once, up or down.

## A team says it's being refused for budget

**What you see:** 429 `budget_exhausted` for that team, and `llmgate_budget_refusals_total` rising.

**Why:** each attempt reserves the most it could cost before it's sent: its input bound at the highest input rate, plus its output limit at the output rate. A team can be refused while it still has budget left, if that's less than one reservation. `llmgate budgets show <team>` shows settled, reserved, and available.

**What to do:** raise the budget with `llmgate budgets set <team> --limit-usd N`, which applies at once on every replica. Or suggest the team asks for a lower `max_tokens`, which shrinks each reservation.

## A key has leaked

1. `llmgate keys list <team>`, then `llmgate keys revoke <key>`. It stops working within `auth.key_cache_ttl` on every replica, 30 seconds by default.
2. `llmgate keys create <team>` for a replacement.
3. Check `llmgate usage <team> --attempts 100` for spending you don't recognize.

## An attempt cost more than it reserved

**What you see:** a log line "an attempt cost more than was reserved", and the attempt flagged `over_reservation`.

**Why:** the provider reported more tokens than the reservation allowed for. Either it ignored the output limit, or a prompt produced more tokens than its UTF-8 byte count plus overhead. The charge is still applied in full, which can take a team slightly past its limit. That's the documented bounded risk.

**What to do:** check the route's model and prices. If it keeps happening for one provider, report it to them.

## Deploying and migrating

- Run `llmgate migrate` before starting a new version. The chart does this in a pre-upgrade hook. The gateway refuses to start on an outdated schema, rather than migrating by itself.
- Check a configuration first with `llmgate validate --config config.yaml`.
- On a rolling update, `server.drain_delay` keeps a stopping replica serving while it reports not ready and asks clients to close their connections, then in-flight requests get `server.shutdown_timeout` to finish and settle. Anything still running when that runs out is canceled, settled as uncertain, and charged its full reservation unless usage arrives. Keep the shutdown timeout above `limits.request_timeout`, and the pod's grace period above both.
- Prices change only through a new `pricing.version`. Reusing a version with different prices is refused at startup, because every past charge has to stay reproducible.

## Clocks disagree

**What you see:** one replica's requests fail with 503 `accounting_unavailable`, and its logs show attempts the ledger wouldn't mark dispatched.

**Why:** leases are written and checked with each replica's clock. If a replica runs more than a minute slow, other replicas' reconcilers can release its reservations before it sends. It refuses to send rather than overspend.

**What to do:** fix time synchronization (NTP) on that host.

## A team's requests are slow, but others' aren't

**Why:** a team's admissions take turns on each replica, and replicas take turns on the team's budget row, so one team's admission rate is bounded by Postgres latency. A provider's concurrency limit also queues a busy team's attempts behind its own earlier ones, and serves other teams in turn.

**What to do:** check the provider queues in `llmgate_limiter_queued` and the provider's `max_concurrency`. If one team needs more than its row lock allows, its traffic is beyond what strict budget enforcement on one row can admit, and the latency is the honest cost of that guarantee.
