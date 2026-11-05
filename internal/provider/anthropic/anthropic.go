// Package anthropic is the adapter for the Anthropic Messages API.
//
// The wire format follows Anthropic's API documentation, checked 2026-11-05 for API
// version 2026-10-21. That version returns 400 for temperature and top_p on every
// model, so the adapter reports that it can't honor them rather than dropping them.
//
// Thinking can happen on any request, because it's on by default for some models.
// It counts against max_tokens and in output_tokens, so it costs money, but it is
// never treated as response text.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/sse"
)

// APIVersion is the version sent in the anthropic-version header.
const APIVersion = "2026-10-21"

const systemSeparator = "\n\n"

// Options configures a Provider.
type Options struct {
	// BaseURL is the API root, such as https://api.anthropic.com, without /v1.
	BaseURL string
	APIKey  string

	MaxResponseBytes int64
	MaxEventBytes    int

	// Client defaults to provider.NewHTTPClient.
	Client *http.Client
}

// Provider is the Anthropic API. Use New to create one.
type Provider struct {
	endpoint         string
	apiKey           string
	maxResponseBytes int64
	maxEventBytes    int
	client           *http.Client
}

var _ provider.Provider = (*Provider)(nil)

// New returns a Provider for the given options.
func New(opts Options) (*Provider, error) {
	if opts.APIKey == "" {
		return nil, errors.New("anthropic: an API key is required")
	}
	if opts.MaxResponseBytes <= 0 || opts.MaxEventBytes <= 0 {
		return nil, errors.New("anthropic: response and event size limits are required")
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("anthropic: the base URL must be absolute")
	}
	client := opts.Client
	if client == nil {
		client = provider.NewHTTPClient()
	}
	return &Provider{
		endpoint:         strings.TrimRight(opts.BaseURL, "/") + "/v1/messages",
		apiKey:           opts.APIKey,
		maxResponseBytes: opts.MaxResponseBytes,
		maxEventBytes:    opts.MaxEventBytes,
		client:           client,
	}, nil
}

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Sampling: false}
}

// CloseIdleConnections releases idle upstream connections.
func (p *Provider) CloseIdleConnections() { p.client.CloseIdleConnections() }

type wireMessage struct {
	Role    chat.Role `json:"role"`
	Content string    `json:"content"`
}

type wireRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int64         `json:"max_tokens"`
	System        string        `json:"system,omitempty"`
	Messages      []wireMessage `json:"messages"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

func refuse(message string) *provider.Error {
	return &provider.Error{Kind: provider.KindInvalidRequest, Delivery: provider.DeliveryNotSent, Err: errors.New(message)}
}

// encodeRequest translates a request. It refuses anything it can't honor, because
// sending the request without it would change what the client asked for.
func encodeRequest(req *chat.Request, model string, stream bool) ([]byte, *provider.Error) {
	if req.Temperature != nil || req.TopP != nil {
		return nil, refuse("this provider does not accept temperature or top_p")
	}
	if req.MaxOutputTokens <= 0 {
		return nil, refuse("this provider requires an output limit")
	}

	out := wireRequest{
		Model:         model,
		MaxTokens:     req.MaxOutputTokens,
		StopSequences: req.Stop,
		Stream:        stream,
	}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case chat.RoleSystem, chat.RoleDeveloper:
			system = append(system, m.Content)
		default:
			out.Messages = append(out.Messages, wireMessage(m))
		}
	}
	out.System = strings.Join(system, systemSeparator)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, refuse("the request could not be encoded")
	}
	return buf.Bytes(), nil
}

// send performs the request and returns a successful response. The caller owns
// closing its body.
func (p *Provider) send(ctx context.Context, req *chat.Request, model string, stream bool) (*http.Response, *provider.Error) {
	body, refusal := encodeRequest(req, model, stream)
	if refusal != nil {
		return nil, refusal
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindConnection, Delivery: provider.DeliveryNotSent, Err: errors.New("building the upstream request")}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", p.apiKey)
	httpReq.Header.Set("Anthropic-Version", APIVersion)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, provider.TransportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return resp, nil
}

func statusError(resp *http.Response) *provider.Error {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	code := ""
	if json.Unmarshal(provider.ReadErrorBody(resp.Body), &envelope) == nil {
		code = errorType(envelope.Error)
	}
	return &provider.Error{
		Kind:         provider.KindForStatus(resp.StatusCode),
		Delivery:     provider.DeliveryConfirmed,
		Status:       resp.StatusCode,
		RetryAfter:   provider.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		UpstreamCode: code,
	}
}

// errorType extracts the type from an error object. Provider messages are
// deliberately ignored, because they can contain account details.
func errorType(errorJSON []byte) string {
	var e struct {
		Type any `json:"type"`
	}
	if len(errorJSON) == 0 || json.Unmarshal(errorJSON, &e) != nil {
		return ""
	}
	if s, ok := e.Type.(string); ok && provider.IsIdentifier(s) {
		return s
	}
	return ""
}

// finishReason maps a stop reason. tool_use and pause_turn fall outside the text
// contract, and an unknown value could mean anything, so all three are errors.
func finishReason(stop string) (chat.FinishReason, bool) {
	switch stop {
	case "end_turn", "stop_sequence":
		return chat.FinishStop, true
	case "max_tokens", "model_context_window_exceeded":
		return chat.FinishLength, true
	case "refusal":
		return chat.FinishContentFilter, true
	}
	return "", false
}

// isIgnoredBlock reports content that is billed but isn't part of the response.
func isIgnoredBlock(blockType string) bool {
	return blockType == "thinking" || blockType == "redacted_thinking"
}

// Complete implements provider.Provider.
func (p *Provider) Complete(ctx context.Context, req *chat.Request, model string) (*chat.Completion, error) {
	resp, perr := p.send(ctx, req, model, false)
	if perr != nil {
		return nil, perr
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxResponseBytes+1))
	if err != nil {
		return nil, provider.ReadError(ctx, err)
	}
	if int64(len(body)) > p.maxResponseBytes {
		return nil, provider.ProtocolError("response exceeds the size limit")
	}
	return decodeCompletion(body)
}

func decodeCompletion(body []byte) (*chat.Completion, error) {
	// Decoding would otherwise replace invalid bytes with U+FFFD.
	if !utf8.Valid(body) {
		return nil, provider.ProtocolError("response is not valid UTF-8")
	}
	var in struct {
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
		StopReason *string         `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, provider.ProtocolError("response is not the expected JSON")
	}
	if in.StopReason == nil {
		return nil, provider.ProtocolError("response has no stop reason")
	}
	finish, ok := finishReason(*in.StopReason)
	if !ok {
		return nil, provider.ProtocolError("response has an unsupported stop reason")
	}

	var text strings.Builder
	for _, block := range in.Content {
		switch {
		case block.Type == "text":
			if block.Text == nil {
				return nil, provider.ProtocolError("response has a text block with no text")
			}
			text.WriteString(*block.Text)
		case isIgnoredBlock(block.Type):
		default:
			return nil, provider.ProtocolError("response has a content block outside the contract")
		}
	}

	var usage usageTracker
	if hasValue(in.Usage) {
		usage.apply(in.Usage, false)
		usage.finished = true
	}
	return &chat.Completion{Content: text.String(), FinishReason: finish, Usage: usage.snapshot()}, nil
}

func hasValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// maxTokenCount bounds a reported count, so that summing them can't overflow.
const maxTokenCount = 1 << 60

// usageTracker merges usage across a stream. message_start reports input and an
// initial output count, and message_delta reports cumulative values.
type usageTracker struct {
	seen      bool
	finished  bool // a final output count arrived
	malformed bool
	decreased bool

	input, cacheRead, cacheWrite, output, thinking *int64

	startRaw, deltaRaw json.RawMessage
}

func (t *usageTracker) apply(raw json.RawMessage, isDelta bool) {
	t.seen = true
	if isDelta {
		t.deltaRaw = bytes.Clone(raw)
	} else {
		t.startRaw = bytes.Clone(raw)
	}

	var in struct {
		Input      *int64 `json:"input_tokens"`
		Output     *int64 `json:"output_tokens"`
		CacheRead  *int64 `json:"cache_read_input_tokens"`
		CacheWrite *int64 `json:"cache_creation_input_tokens"`
		Details    *struct {
			Thinking *int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &in) != nil {
		t.malformed = true
		return
	}
	for _, v := range []*int64{in.Input, in.Output, in.CacheRead, in.CacheWrite} {
		if v != nil && (*v < 0 || *v > maxTokenCount) {
			t.malformed = true
			return
		}
	}
	if in.Input != nil {
		t.input = in.Input
	}
	if in.CacheRead != nil {
		t.cacheRead = in.CacheRead
	}
	if in.CacheWrite != nil {
		t.cacheWrite = in.CacheWrite
	}
	if in.Output != nil {
		if t.output != nil && *in.Output < *t.output {
			t.decreased = true
		}
		t.output = in.Output
		if isDelta {
			t.finished = true
		}
	}
	if in.Details != nil && in.Details.Thinking != nil {
		t.thinking = in.Details.Thinking
	}
}

// snapshot returns usage normalized to match the other adapter: prompt tokens
// include cache tokens, which Anthropic reports separately, and completion tokens
// include thinking. It returns nil if nothing was reported.
func (t *usageTracker) snapshot() *chat.Usage {
	if !t.seen {
		return nil
	}
	out := &chat.Usage{State: chat.UsageMalformed, Raw: t.raw()}
	if t.malformed || t.input == nil || t.output == nil {
		return out
	}

	prompt := *t.input
	if t.cacheRead != nil {
		prompt += *t.cacheRead
	}
	if t.cacheWrite != nil {
		prompt += *t.cacheWrite
	}
	out.PromptTokens = prompt
	out.CompletionTokens = *t.output
	out.TotalTokens = prompt + *t.output
	out.CachedPromptTokens = t.cacheRead
	out.CacheWritePromptTokens = t.cacheWrite
	out.ReasoningTokens = t.thinking

	switch {
	case t.decreased:
		out.State = chat.UsageInconsistent
	case t.finished:
		out.State = chat.UsageReported
	default:
		out.State = chat.UsagePartial
	}
	return out
}

// raw keeps exactly what the provider said, for audit.
func (t *usageTracker) raw() json.RawMessage {
	if t.deltaRaw == nil {
		return t.startRaw
	}
	if t.startRaw == nil {
		return t.deltaRaw
	}
	combined, err := json.Marshal(map[string]json.RawMessage{"message_start": t.startRaw, "message_delta": t.deltaRaw})
	if err != nil {
		return t.deltaRaw
	}
	return combined
}

// Stream implements provider.Provider.
func (p *Provider) Stream(ctx context.Context, req *chat.Request, model string) (provider.Stream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	resp, perr := p.send(streamCtx, req, model, true)
	if perr != nil {
		cancel()
		return nil, perr
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		return nil, provider.ProtocolError("stream response is not an event stream")
	}
	return &stream{
		ResponseStream: provider.NewResponseStream(streamCtx, cancel, resp.Body),
		decoder:        sse.NewDecoder(resp.Body, p.maxEventBytes),
	}, nil
}

type wireEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
	Delta *struct {
		Type       string  `json:"type"`
		Text       *string `json:"text"`
		StopReason *string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type stream struct {
	*provider.ResponseStream
	decoder *sse.Decoder
	usage   usageTracker

	finished bool // a stop reason arrived
	done     bool // message_stop arrived
}

// Next implements provider.Stream.
func (s *stream) Next() (chat.StreamEvent, error) {
	if s.done {
		return chat.StreamEvent{}, io.EOF
	}
	for {
		ev, err := s.decoder.Next()
		if err != nil {
			return chat.StreamEvent{}, s.decodeError(err)
		}
		out, err := s.parseEvent(ev.Data)
		if err != nil {
			return chat.StreamEvent{}, err
		}
		if out != (chat.StreamEvent{}) {
			return out, nil
		}
	}
}

func (s *stream) decodeError(err error) error {
	if errors.Is(err, sse.ErrEventTooLarge) {
		return provider.ProtocolError("stream event exceeds the size limit")
	}
	if errors.Is(err, io.EOF) {
		// The connection ended cleanly, but with no message_stop the output can't be
		// trusted to be whole.
		err = errors.New("stream ended without message_stop")
	}
	return s.ReadError(err)
}

// parseEvent handles one event. An event carrying nothing for the gateway, such as
// a ping or thinking delta, gives the zero StreamEvent. A completed stream gives
// io.EOF.
func (s *stream) parseEvent(data []byte) (chat.StreamEvent, error) {
	var out chat.StreamEvent
	// Decoding would otherwise replace invalid bytes with U+FFFD.
	if !utf8.Valid(data) {
		return out, provider.ProtocolError("stream event is not valid UTF-8")
	}
	var ev wireEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return out, provider.ProtocolError("stream event is not the expected JSON")
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil && hasValue(ev.Message.Usage) {
			s.usage.apply(ev.Message.Usage, false)
			out.Usage = s.usage.snapshot()
		}
	case "content_block_start":
		if ev.ContentBlock == nil {
			return out, provider.ProtocolError("stream event has no content block")
		}
		switch {
		case ev.ContentBlock.Type == "text":
			out.Content = ev.ContentBlock.Text
		case isIgnoredBlock(ev.ContentBlock.Type):
		default:
			return out, provider.ProtocolError("stream has a content block outside the contract")
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return out, provider.ProtocolError("stream event has no delta")
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != nil {
				out.Content = *ev.Delta.Text
			}
		case "thinking_delta", "signature_delta":
		default:
			// Unlike an unknown event, this could be response content, which must not be
			// dropped silently.
			return out, provider.ProtocolError("stream has a content delta outside the contract")
		}
		if s.finished && out.Content != "" {
			return out, provider.ProtocolError("stream event has content after the stop reason")
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != nil {
			if s.finished {
				return out, provider.ProtocolError("stream event repeats the stop reason")
			}
			finish, ok := finishReason(*ev.Delta.StopReason)
			if !ok {
				return out, provider.ProtocolError("stream event has an unsupported stop reason")
			}
			out.FinishReason = finish
			s.finished = true
		}
		if hasValue(ev.Usage) {
			s.usage.apply(ev.Usage, true)
			out.Usage = s.usage.snapshot()
		}
	case "message_stop":
		if !s.finished {
			return out, provider.ProtocolError("stream completed without a stop reason")
		}
		s.done = true
		s.MarkCompleted()
		return out, io.EOF
	case "error":
		return out, streamError(ev.Error)
	default:
		// Includes ping and content_block_stop. The API may add event types, and the
		// documentation says to tolerate them.
	}
	return out, nil
}

func streamError(errorJSON []byte) *provider.Error {
	code := errorType(errorJSON)
	return &provider.Error{Kind: kindForErrorType(code), Delivery: provider.DeliveryConfirmed, UpstreamCode: code}
}

func kindForErrorType(errType string) provider.Kind {
	switch errType {
	case "overloaded_error":
		return provider.KindOverloaded
	case "rate_limit_error":
		return provider.KindRateLimited
	case "timeout_error":
		return provider.KindTimeout
	case "billing_error":
		return provider.KindBilling
	case "authentication_error", "permission_error":
		return provider.KindAuth
	case "not_found_error":
		return provider.KindNotFound
	case "invalid_request_error", "request_too_large":
		return provider.KindInvalidRequest
	default:
		return provider.KindServer
	}
}
