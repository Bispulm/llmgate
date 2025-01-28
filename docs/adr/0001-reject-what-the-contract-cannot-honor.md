# ADR 0001: Reject what the contract cannot honor

**Status:** Accepted, 2026-11-05

## Context

Gateways usually pass unknown fields through or drop them. Both are harmful once a request can land on different providers:

- Dropping `tools` returns a confident text answer to a caller expecting a tool call.
- Passing a provider-specific field through works until a fallback sends it to a provider that returns 400, or ignores it.
- Anthropic's API version `2026-10-21` returns 400 for `temperature` and `top_p`. A gateway that strips them to make fallback succeed has quietly changed the sampling behavior the client asked for.

## Decision

llmgate accepts a small, explicitly listed request contract, documented in [compatibility.md](../compatibility.md). Everything else gets a 400 naming the field. Fields may be accepted at their documented default value because they have no effect there. A route is only eligible if it can honor everything the request declares.

The gateway's request model holds only contract fields. An adapter can't forward an unlisted field, because the field was never kept.

The mock provider shares no types with the gateway, so a misunderstanding of a wire format can't be hidden by both sides using the same struct.

## Consequences

- Some off-the-shelf clients will fail until they stop sending an unsupported field. The error names the field, so this is quick to diagnose. Explicit defaults such as `n: 1` are accepted to reduce this.
- Tools, multimodal input, and structured output need deliberate work to add, including capability checks, accounting, and fixtures for each provider.
- A request's validated form is the input for the cache key (M4), so anything that could change a response is part of that form.
