# ADR 0004: Routing and failure handling

**Status:** Accepted, 2026-11-05

## Context

A gateway that retries everything turns a provider's bad afternoon into a retry storm, hides configuration mistakes, and can bill a client's invalid request several times. A gateway that retries nothing gives up the main reason to have more than one route. The behavior has to depend on what actually went wrong.

Fallback also interacts with two other decisions. It stops at the commitment point in [ADR 0003](0003-downstream-commitment-point.md), and every attempt can cost money under [ADR 0002](0002-postgres-is-the-budget-authority.md).

## Decision

### Choosing routes

For each request the gateway makes a plan:

1. Remove routes that can't honor the request. A request with `temperature` skips a route whose API rejects it.
2. Order by `tier`. Lower tiers are tried first.
3. Within a tier, choose by `weight`, at random without replacement.

Whether a route is usable is decided when it's offered, because time passes between attempts. A route is passed over if its breaker is open or a rate limit hold is active. Each skip is counted with a small fixed set of reasons: `capability`, `breaker_open`, and `rate_limit_hold`.

If no route could ever honor the request, that's a `400 no_capable_route`, because the client can fix it. If routes exist but none is usable right now, that's a fast `503 no_route_available` with `Retry-After`.

### What each failure means

| Failure | Try another route? | Effect on the route's health |
|---|---|---|
| Connection failure, an attempt's own timeout, provider 5xx, overload, interrupted stream, malformed response | Yes | Failure |
| Rejected credentials, unknown model, billing or quota | Yes | Failure |
| Rate limited | Yes | Neutral, and holds the route if the provider gave a hint |
| Provider rejected the request | No | Neutral |
| Client canceled | No | Neutral |
| The request's overall deadline passed | No time left | Neutral |

A provider rejecting a request says something about the request, so another attempt would probably fail the same way and could bill twice. A rate limit means the provider is working and asked for fewer requests, so it's respected without being treated as an outage. Credentials and billing failures do count against health, because every request sent to that route will fail until an operator acts, and fast failure is better than repeating it.

A client's behavior never affects a provider's health. That includes canceling, stalling, sending something invalid, or asking for an output long enough to outlast the request's deadline. Otherwise one client could open a breaker for every team. An attempt's own timers are what catch a provider that has hung: the first-event timeout and the idle timeout for streams, and the completion timeout for non-streaming requests.

### Attempts and deadlines

- **Finite attempts.** `max_attempts` limits upstream attempts per request, defaulting to one per route, with a configured maximum of 10.
- **One deadline.** `request_timeout` covers the whole logical request, including every attempt and any wait. No attempt starts after it passes.
- **Separate timers.** Each attempt has its own timer: `first_event_timeout` for a stream, and `completion_timeout` for a non-streaming request, which defaults to half of `request_timeout`. A slow first attempt can't use up the next one's window.
- **Separate records.** Each attempt is recorded when it ends, with its own outcome, usage, and reason (`primary`, `fallback`, or `retry_after`). A fallback's cost is never merged into the attempt that served the response.

### Retry hints

A `Retry-After` from a rate limit holds that route for all requests in this process, capped at 60 seconds, so that a bad hint can't take a route out of service for long.

A request only waits for a hint it was given itself, when every route has been tried, attempts remain, and the hint fits within the deadline. Then it retries only the route that gave the hint. Otherwise it fails immediately and passes the hint to the client. A hold from another request's hint is a reason to skip a route, not to wait for it.

There is no other same-route retry. Retrying a 5xx on the same route is left to clients, who can see the failure, so that retries in two layers don't multiply.

### Circuit breakers

Each route, meaning a provider and model, has one breaker per process, shared by the aliases that use it.

| Property | Behavior | Default |
|---|---|---|
| Sampling window | Sliding, in 10 buckets | 60s |
| Minimum evidence | Outcomes in the window before it can open | 10 |
| Opening condition | Failures reach this share of outcomes | 0.5 |
| Cooldown | Time open before probing | 30s |
| Half-open probes | Attempts allowed at once. All must succeed to close, and any failure reopens. | 2 |

Outcomes are reported when an attempt ends, so a stream that dies midway counts as a failure even though it started well. A result from before a state change is ignored. A canceled probe gives back its slot, so that a client going away can't leave a breaker stuck half-open.

**Replica behavior.** Breakers are process-local. Each replica learns about a failing route on its own, so a fleet of N replicas sends roughly N times the minimum evidence before all have opened, and up to N times the probes during recovery. Sharing state would add a dependency to every request's path, for a saving of a few failed attempts. This is not acceptable for budgets, which is why [ADR 0002](0002-postgres-is-the-budget-authority.md) keeps those in Postgres.

### Concurrency and fair admission

Each provider can have a limit on attempts running at once, because upstream capacity belongs to the account rather than to one model. When it's full, requests wait in a bounded queue.

- **Round robin between teams.** Waiting teams are served in turn, and each team's own requests in order. A team queueing hundreds of requests gets one turn in each round, the same as a team with one.
- **Per-team limits.** `max_queue_per_team` keeps one team from filling the queue and shedding everyone else. `max_in_flight_per_team` keeps one team from holding every slot, and a team at its limit doesn't hold up teams behind it.
- **Explicit shedding.** A full queue, or a wait longer than `queue_timeout`, moves on to the next route. With nowhere left to go, the client gets a fast `503 capacity_exceeded` with `Retry-After`.
- **Capacity comes before the attempt.** A route that was never called isn't an attempt, costs nothing, and doesn't affect its health. A slot comes back however an attempt ends.

These limits are per process, so a fleet's total is the limit times its replicas. Limits a team is promised across replicas live in Redis, described in [ADR 0002](0002-postgres-is-the-budget-authority.md).

## Alternatives considered

- **Failing over on every error.** Simple, and wrong for invalid requests and cancellations.
- **Same-route retries with backoff for all transient failures.** Multiplies with client retries and fallback, and makes it hard to state how many attempts a request can make.
- **Honoring `Retry-After` by always waiting.** Holds a connection and a concurrency slot for as long as a provider says, even when another route could serve immediately.
- **Counting rate limits against health.** Opens a breaker for a provider that's working, reducing capacity exactly when it's tight.

## Consequences

- API compatibility doesn't mean identical output. A fallback may answer differently, and clients can see which route served them in `X-Llmgate-Route`.
- Clients can see up to `max_attempts` times the latency of one attempt, bounded by `request_timeout`.
- A request can cost more than one attempt's worth, and budget reservations must cover each attempt before it starts.
- Provider-specific account problems that arrive as plain 400s or 429s, such as a workspace spend limit, can't always be told apart from ordinary failures.
