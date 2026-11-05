# ADR 0003: The downstream commitment point

**Status:** Accepted, 2026-11-05. The boundary is implemented. Fallback across providers arrives in M2.

## Context

Failover is safe until the client has been told something. "Fail over before the first token" is the usual phrasing, but it leaves real questions open:

- Once `200 OK` and SSE headers are flushed, a later failure can no longer become an HTTP error.
- Many providers send a role-only chunk or `message_start` immediately, before inference can fail. Committing on that would give up failover when it's most useful.
- Splicing a second provider's output after the first's partial text produces an answer neither model wrote.

## Decision

**Once downstream response headers are written or flushed, or any stream bytes are sent, the gateway does not switch providers for that response.**

The gateway commits on the first **normalized event with content**: text, a refusal, or a finish reason.

The following do not commit, and consume no meaningful memory because adapters drop them without buffering:

- An upstream `200` and its headers.
- Role-only or empty chunks.
- `ping` events and comments.
- Anthropic `message_start`, `content_block_start`, and thinking prelude.

Before commit, the gateway holds at most one normalized event, whose size is limited by the SSE event limit. Everything that fails before commit, including a bad content type, a malformed first event, or a timeout waiting for it, is still eligible for failover and otherwise produces a clean HTTP error.

After commit:

- No other provider is contacted for that response.
- The gateway emits one documented error event when it can, then ends the stream without `[DONE]`.
- The attempt is recorded with partial output counts and uncertain usage, for reconciliation (M3).

### Deadlines

| Limit | Scope | Purpose |
|---|---|---|
| Request timeout | End to end, across all attempts | One deadline for the whole logical request |
| First-event timeout | From dispatch to first content event | Bounds the failover window, including a stream of only pings |
| Stream idle timeout | Between content events | Detects a stalled upstream after commit |
| Client write timeout | Per write and flush | A stalled client releases upstream resources |

Pings don't reset the first-event or idle timers, because a provider that only sends keepalives isn't making progress.

### Backpressure

The gateway holds no queue between upstream and client. Each event is read, written, and flushed in sequence. A slow client slows the upstream read through TCP flow control, and the client write timeout bounds how long that can last. `http.Server.WriteTimeout` stays unset because it would cut off long streams.

### Cancellation

The upstream request uses a context derived from the client request, so a client disconnect cancels the upstream call. Attempt recording uses a separate bounded context made with `context.WithoutCancel`, so cancellation can't prevent accounting. A canceled attempt is recorded with uncertain usage. Cancellation does not imply zero cost.

## Alternatives considered

- **Commit on the upstream `200`.** Simple, but loses failover for the common case of a provider that accepts the request and then fails.
- **Buffer several events, or the whole response, before committing.** Improves failover coverage but adds latency and per-stream memory, and defeats the purpose of streaming.
- **Replay partial output into a fallback request.** Models differ, so the result wouldn't be a real continuation, and it would bill for the prompt twice with no way for the client to know.

## Consequences

- Time to first byte for a client includes the provider's time to its first content event. A role chunk won't reach the client early. Added latency is measured in M5.
- Clients must treat a missing `[DONE]` as failure. Documented in [compatibility.md](../compatibility.md).
- A provider that fails right after its first content event gets no failover, even if the client would have preferred it. That is the cost of a precise rule.
