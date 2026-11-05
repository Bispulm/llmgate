# Compatibility contract

llmgate exposes a deliberately small subset of the OpenAI Chat Completions API at `POST /v1/chat/completions`. A request either fits this contract or is rejected with a named error. Nothing is silently dropped, because a gateway that ignores a field it cannot honor has changed the meaning of the request without telling the caller.

Provider wire formats were checked on 2026-11-05 against the [OpenAI OpenAPI specification](https://github.com/openai/openai-openapi) and the [Anthropic API documentation](https://platform.claude.com/docs). Re-check them before changing an adapter.

## What compatibility does and does not mean

API compatibility means a client can use the same request shape and parse the same response shape. It does not mean different models produce the same output. A request that falls back from one model to another may get a different answer, in a different style, with a different token count. Choosing which models may back an alias is an operator decision.

## Request

| Field | Accepted | Notes |
|---|---|---|
| `model` | Non-empty string | A configured alias, not a provider model ID |
| `messages` | Non-empty array | See [Messages](#messages) |
| `max_completion_tokens` | Integer, 1 or greater | Upper bound on generated tokens |
| `max_tokens` | Integer, 1 or greater | Deprecated upstream, accepted as a synonym. Sending both is rejected. |
| `temperature` | Number, 0 to 2 | Restricts routing to providers that accept it. See [Provider capabilities](#provider-capabilities). |
| `top_p` | Number, 0 to 1 | As above |
| `stop` | String, or array of 1 to 4 non-empty strings | |
| `stream` | Boolean | |
| `stream_options.include_usage` | Boolean | Only valid with `stream: true` |

`null` is treated as absent for the optional fields above.

### Output limit

Budget accounting needs an upper bound on output, so a request never goes upstream without one.

- If the client sets a limit, it must not exceed the alias's configured `max_output_tokens`. A larger value is rejected rather than quietly lowered.
- If the client sets no limit, the alias's `max_output_tokens` is used and sent upstream explicitly. This is a real difference from calling a provider directly, where the default may be the model's maximum.

### Explicit defaults

These are accepted only at their documented default, and are not forwarded, because they have no effect at that value. Any other value is rejected.

| Field | Accepted value |
|---|---|
| `n` | `1` |
| `presence_penalty` | `0` |
| `frequency_penalty` | `0` |
| `logprobs` | `false` |
| `stream_options.include_obfuscation` | `false` |

### Rejected

Every other field is rejected with `unsupported_parameter`, naming the field. That includes `tools`, `tool_choice`, `parallel_tool_calls`, `functions`, `function_call`, `response_format`, `seed`, `user`, `safety_identifier`, `logit_bias`, `top_logprobs`, `audio`, `modalities`, `prediction`, `web_search_options`, `reasoning_effort`, `verbosity`, `service_tier`, `store`, `metadata`, `prompt_cache_key`, and any provider-specific parameter.

Also rejected:

- Bodies that are not valid UTF-8, because decoding would silently replace the invalid bytes.
- Duplicate keys in an object, because parsers disagree on which one wins.
- Data after the top-level JSON object.

### Messages

| Field | Accepted |
|---|---|
| `role` | `system`, `developer`, `user`, `assistant` |
| `content` | Non-empty string |

These ordering rules exist so that a request means the same thing on every provider:

- `system` and `developer` messages must come before all other messages.
- The last message must have the role `user`. A trailing `assistant` message, known as prefill, is not supported across current providers.

Rejected, each with a specific error:

- The `tool` and `function` roles.
- Array content, including text-only content parts, images, audio, and files.
- `name`, `tool_calls`, `function_call`, `audio`, and `refusal` on a message.

`system` and `developer` are sent unchanged to OpenAI-compatible upstreams. Providers with a single system field receive them concatenated in order.

**Known limitation:** a lone surrogate escape such as `\ud800` in a JSON string decodes to U+FFFD. This is the one case where client text can change in decoding.

## Non-streaming response

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "created": 1793894400,
  "model": "alias-from-the-request",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "text", "refusal": null },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 9, "completion_tokens": 12, "total_tokens": 21 }
}
```

- `model` is the alias the client asked for. The serving provider and model are in the `x-llmgate-route` response header.
- `id` and `created` come from the gateway, so they don't change with the provider.
- `usage` is omitted when the provider did not report it. The gateway never reports zero in place of unknown.
- `usage.prompt_tokens_details` and `usage.completion_tokens_details` appear only for fields the provider reported and that the gateway normalizes: `cached_tokens`, `cache_write_tokens`, and `reasoning_tokens`.

## Streaming response

Server-sent events with `Content-Type: text/event-stream`:

1. A first chunk with `delta: {"role":"assistant","content":""}`.
2. Zero or more chunks with `delta: {"content":"..."}` or `delta: {"refusal":"..."}`.
3. One chunk with empty `delta` and a non-null `finish_reason`.
4. If `include_usage` was set and the provider reported usage, one chunk with `choices: []` and `usage`. All earlier chunks carry `usage: null`.
5. `data: [DONE]`.

The gateway always requests usage from upstream for accounting, whether or not the client asked for it. It only forwards usage to clients that asked. If the provider never reports it, the usage chunk is omitted rather than faked.

Provider fields outside this contract, such as `obfuscation`, `system_fingerprint`, and `service_tier`, are not forwarded.

### When a stream fails

The gateway may not switch providers once it has committed the response. See [ADR 0003](adr/0003-downstream-commitment-point.md).

Before commit, another route may be tried. See [ADR 0004](adr/0004-routing-and-failure-handling.md). If none can serve the request, the client gets an ordinary HTTP error for the last failure.

After commit, the client gets one event, then the stream ends without `[DONE]`:

```text
data: {"error":{"message":"...","type":"upstream_error","param":null,"code":"stream_interrupted"}}
```

OpenAI's chat completions specification does not define an in-stream error, so this is the gateway's own documented representation. It reuses the specification's error envelope, and OpenAI's official Go SDK treats a data event with a top-level `error` key as a stream failure. A client must treat a stream with no `[DONE]` as failed, even if it already received a `finish_reason`.

## Finish reasons

| Value | Meaning |
|---|---|
| `stop` | The model finished, or hit a stop sequence |
| `length` | The output limit was reached |
| `content_filter` | The provider withheld or stopped the content, including refusals |

`tool_calls` and `function_call` can't occur for a request without tools. If an upstream returns them or any other unknown value, the attempt fails as a protocol error rather than being passed on or renamed.

## Errors

All errors use the OpenAI envelope, with `x-request-id` set on the response:

```json
{ "error": { "message": "...", "type": "invalid_request_error", "param": "tools", "code": "unsupported_parameter" } }
```

| Status | `type` | `code` | When |
|---|---|---|---|
| 400 | `invalid_request_error` | `unsupported_parameter` | A field outside this contract |
| 400 | `invalid_request_error` | `unsupported_role` | `tool` or `function` role |
| 400 | `invalid_request_error` | `unsupported_content_type` | Non-string message content |
| 400 | `invalid_request_error` | `invalid_message_order` | System message after others, or last message not from the user |
| 400 | `invalid_request_error` | `invalid_value` | Wrong type or out of range |
| 400 | `invalid_request_error` | `missing_required_parameter` | `model` or `messages` absent |
| 400 | `invalid_request_error` | `duplicate_parameter` | Repeated key |
| 400 | `invalid_request_error` | `invalid_json` | Malformed JSON or invalid UTF-8 |
| 400 | `invalid_request_error` | `output_limit_exceeded` | Limit above the alias's maximum |
| 400 | `invalid_request_error` | `no_capable_route` | No route for this alias can honor the request, such as `temperature` with only Anthropic routes |
| 401 | `authentication_error` | `invalid_api_key` | Missing or unknown key |
| 400 | `invalid_request_error` | `request_unreadable` | The body could not be read |
| 400 | `invalid_request_error` | `upstream_rejected` | Provider rejected the request, such as a prompt over its context limit |
| 404 | `invalid_request_error` | `model_not_found` | Unknown alias |
| 405 | `invalid_request_error` | `method_not_allowed` | Not a POST |
| 408 | `invalid_request_error` | `request_timeout` | The body did not arrive in time |
| 413 | `invalid_request_error` | `request_too_large` | Body over the configured limit |
| 429 | `rate_limit_error` | `upstream_rate_limited` | Provider rate limit, with `Retry-After` when known |
| 502 | `upstream_error` | `upstream_failed` | Connection failure, or provider 5xx |
| 502 | `upstream_error` | `upstream_protocol_error` | Malformed provider response |
| 502 | `upstream_error` | `upstream_misconfigured` | Provider rejected the gateway's credentials or model |
| 502 | `upstream_error` | `upstream_account_limit` | The provider account is out of credit or over a spend limit |
| 503 | `upstream_error` | `upstream_overloaded` | Provider reports overload |
| 503 | `upstream_error` | `no_route_available` | Every route is unavailable right now, with `Retry-After` when known |
| 503 | `upstream_error` | `capacity_exceeded` | Every usable route is at its concurrency limit, with `Retry-After` |
| 429 | `insufficient_quota` | `budget_exhausted` | The team's budget can't cover the most this attempt could cost. Waiting won't help, so there's no `Retry-After` |
| 429 | `rate_limit_error` | `rate_limit_exceeded` | The team is over a request, token, or in-flight limit, with `Retry-After` |
| 503 | `server_error` | `accounting_unavailable` | The budget couldn't be checked, so nothing was sent, with `Retry-After` |
| 503 | `server_error` | `auth_unavailable` | The key couldn't be checked. It may be perfectly valid. With `Retry-After` |
| 503 | `server_error` | `rate_limit_unavailable` | Team limits couldn't be checked, with `Retry-After` |
| 504 | `upstream_error` | `upstream_timeout` | A deadline passed before commit |

`upstream_rejected` is a 400 rather than a 5xx because the same request will keep failing, and client SDKs automatically retry a 5xx.

OpenAI reports exhausted credit and spend limits as HTTP 429, the same status as a rate limit. The gateway separates them by error code, because retrying can't help and the operator, not the client, has to act.

The gateway does not forward provider error messages, which can contain account details. It logs the provider's status and error code, and includes the code in the message of `upstream_rejected` when it is a short identifier.

## Response headers

| Header | Meaning |
|---|---|
| `X-Request-Id` | The gateway's ID for this request. It also appears in the logs. |
| `X-Llmgate-Route` | The serving `provider/model`, once one was chosen |
| `X-Llmgate-Attempts` | How many upstream attempts were made, including failed ones |
| `X-Llmgate-Budget-Enforcement` | `strict` when the ledger admits each attempt, and `none` in development mode, where nothing does. See [ADR 0002](adr/0002-postgres-is-the-budget-authority.md). |

## Provider capabilities

| Capability | OpenAI-compatible | Anthropic (`2026-10-21`) |
|---|---|---|
| `temperature`, `top_p` | Yes | **No.** The API returns 400 on every model. |
| `stop` | Yes | Yes, as `stop_sequences` |
| `system` and `developer` roles | Yes, unchanged | Combined in order into the top-level `system` field, separated by a blank line |
| Explicit output limit | Yes | Yes, and required |

A request with `temperature` set can't be served by an Anthropic route. The alternative, dropping the parameter and returning a successful response, is what this contract exists to prevent.

## How Anthropic responses map onto the contract

| Anthropic | Client sees |
|---|---|
| `end_turn`, `stop_sequence` | `finish_reason: "stop"` |
| `max_tokens`, `model_context_window_exceeded` | `finish_reason: "length"` |
| `refusal` | `finish_reason: "content_filter"`, possibly with no content |
| `tool_use`, `pause_turn`, anything unknown | The attempt fails as a protocol error |
| Thinking blocks | Nothing, but they count in `completion_tokens` and in `completion_tokens_details.reasoning_tokens` |
| `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens` | `prompt_tokens`, with the cache parts in `prompt_tokens_details` |

Thinking counts against the output limit on models that use it, so a small limit can produce `finish_reason: "length"` with little or no text. Models differ in whether thinking is on by default, and the gateway doesn't change that default.

Anthropic reports some account problems in ways the gateway can't separate from ordinary failures. A tier spend cap arrives as a 429 and a workspace spend limit as a 400, so those surface as `upstream_rate_limited` and `upstream_rejected`.

## Implementation status

| Area | Status |
|---|---|
| Request validation | Implemented |
| OpenAI-compatible adapter, streaming and non-streaming | Implemented, tested against the local mock only |
| Anthropic adapter, streaming and non-streaming | Implemented, tested against the local mock only |
| Capability filtering, weights, fallback, circuit breakers | Implemented |

No test in this repository has been run against a live provider. Tests use the local mock and synthetic fixtures.
