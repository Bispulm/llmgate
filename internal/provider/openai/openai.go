// Package openai is the adapter for OpenAI-compatible chat completion APIs.
//
// The wire format follows the OpenAI OpenAPI specification, checked 2026-11-05.
// Compatible servers vary, so the adapter is strict about what would change the
// meaning of a response and tolerant of fields it doesn't use.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/sse"
)

const (
	maxErrorBodyBytes = 64 << 10
	maxDrainBytes     = 64 << 10
	// drainTimeout bounds waiting for the end of a finished stream's body.
	drainTimeout = 100 * time.Millisecond
	// maxIdentifierLength bounds an upstream error code kept from a response.
	maxIdentifierLength = 64

	doneMarker = "[DONE]"
)

// Options configures a Provider.
type Options struct {
	// BaseURL is the API root, such as https://api.openai.com/v1.
	BaseURL string
	APIKey  string
	// UseMaxTokens sends the limit as max_tokens, for servers that don't accept
	// max_completion_tokens.
	UseMaxTokens bool

	MaxResponseBytes int64
	MaxEventBytes    int

	// Client defaults to provider.NewHTTPClient.
	Client *http.Client
}

// Provider is an OpenAI-compatible upstream. Use New to create one.
type Provider struct {
	endpoint         string
	apiKey           string
	useMaxTokens     bool
	maxResponseBytes int64
	maxEventBytes    int
	client           *http.Client
}

var _ provider.Provider = (*Provider)(nil)

// New returns a Provider for the given options.
func New(opts Options) (*Provider, error) {
	if opts.APIKey == "" {
		return nil, errors.New("openai: an API key is required")
	}
	if opts.MaxResponseBytes <= 0 || opts.MaxEventBytes <= 0 {
		return nil, errors.New("openai: response and event size limits are required")
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("openai: the base URL must be absolute")
	}
	client := opts.Client
	if client == nil {
		client = provider.NewHTTPClient()
	}
	return &Provider{
		endpoint:         strings.TrimRight(opts.BaseURL, "/") + "/chat/completions",
		apiKey:           opts.APIKey,
		useMaxTokens:     opts.UseMaxTokens,
		maxResponseBytes: opts.MaxResponseBytes,
		maxEventBytes:    opts.MaxEventBytes,
		client:           client,
	}, nil
}

// CloseIdleConnections releases idle upstream connections.
func (p *Provider) CloseIdleConnections() { p.client.CloseIdleConnections() }

type wireMessage struct {
	Role    chat.Role `json:"role"`
	Content string    `json:"content"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireRequest struct {
	Model               string             `json:"model"`
	Messages            []wireMessage      `json:"messages"`
	MaxCompletionTokens *int64             `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int64             `json:"max_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	Stop                []string           `json:"stop,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
}

func (p *Provider) encodeRequest(req *chat.Request, model string, stream bool) ([]byte, error) {
	out := wireRequest{
		Model:       model,
		Messages:    make([]wireMessage, len(req.Messages)),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Stream:      stream,
	}
	for i, m := range req.Messages {
		out.Messages[i] = wireMessage(m)
	}
	if limit := req.MaxOutputTokens; limit > 0 {
		if p.useMaxTokens {
			out.MaxTokens = &limit
		} else {
			out.MaxCompletionTokens = &limit
		}
	}
	if stream {
		// Accounting needs usage whether or not the client asked for it.
		out.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	return buf.Bytes(), nil
}

// send performs the request and returns a successful response. The caller owns
// closing its body.
func (p *Provider) send(ctx context.Context, req *chat.Request, model string, stream bool) (*http.Response, error) {
	body, err := p.encodeRequest(req, model, stream)
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindInvalidRequest, Delivery: provider.DeliveryNotSent, Err: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.KindConnection, Delivery: provider.DeliveryNotSent, Err: errors.New("building the upstream request")}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return resp, nil
}

// transportError classifies a failure to get a response. Only a failure to
// connect proves that nothing was sent.
func transportError(ctx context.Context, err error) *provider.Error {
	delivery := provider.DeliveryUncertain
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		delivery = provider.DeliveryNotSent
	}
	kind := provider.KindConnection
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = provider.KindCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = provider.KindTimeout
	}
	return &provider.Error{Kind: kind, Delivery: delivery, Err: err}
}

// statusError classifies a non-200 response.
func statusError(resp *http.Response) *provider.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	var details errorDetails
	if json.Unmarshal(body, &envelope) == nil {
		details = parseErrorDetails(envelope.Error)
	}
	kind := kindForStatus(resp.StatusCode)
	if kind == provider.KindRateLimited && details.isBilling() {
		kind = provider.KindBilling
	}
	return &provider.Error{
		Kind:         kind,
		Delivery:     provider.DeliveryConfirmed,
		Status:       resp.StatusCode,
		RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		UpstreamCode: details.identifier(),
	}
}

func kindForStatus(status int) provider.Kind {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.KindAuth
	case status == http.StatusNotFound:
		return provider.KindNotFound
	case status == http.StatusPaymentRequired:
		return provider.KindBilling
	case status == http.StatusRequestTimeout:
		return provider.KindTimeout
	case status == http.StatusTooManyRequests:
		return provider.KindRateLimited
	case status == http.StatusServiceUnavailable, status == 529:
		return provider.KindOverloaded
	case status >= 500:
		return provider.KindServer
	case status >= 400:
		return provider.KindInvalidRequest
	default:
		// A redirect or an unexpected success. Redirects aren't followed.
		return provider.KindProtocol
	}
}

// errorDetails holds the identifiers from an upstream error object. Provider
// messages are deliberately ignored, because they can contain account details.
type errorDetails struct {
	code, errType string
}

func parseErrorDetails(errorJSON []byte) errorDetails {
	var e struct {
		Code any `json:"code"`
		Type any `json:"type"`
	}
	var out errorDetails
	if len(errorJSON) == 0 || json.Unmarshal(errorJSON, &e) != nil {
		return out
	}
	if s, ok := e.Code.(string); ok && isIdentifier(s) {
		out.code = s
	}
	if s, ok := e.Type.(string); ok && isIdentifier(s) {
		out.errType = s
	}
	return out
}

// identifier returns the most specific identifier available.
func (d errorDetails) identifier() string {
	if d.code != "" {
		return d.code
	}
	return d.errType
}

// isBilling reports an account that can't serve requests until its credits or
// limits change. OpenAI sends these as HTTP 429 and says to branch on the code,
// noting that the broader type can still be insufficient_quota. See
// https://developers.openai.com/api/docs/guides/error-codes (checked 2026-11-05).
func (d errorDetails) isBilling() bool {
	switch d.code {
	case "credit_balance_exhausted",
		"organization_spend_limit_exceeded",
		"project_spend_limit_exceeded",
		"organization_usage_limit_exceeded",
		"insufficient_quota":
		return true
	}
	return d.errType == "insufficient_quota"
}

func isIdentifier(s string) bool {
	if s == "" || len(s) > maxIdentifierLength {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func protocolError(message string) *provider.Error {
	return &provider.Error{Kind: provider.KindProtocol, Delivery: provider.DeliveryConfirmed, Err: errors.New(message)}
}

// Complete implements provider.Provider.
func (p *Provider) Complete(ctx context.Context, req *chat.Request, model string) (*chat.Completion, error) {
	resp, err := p.send(ctx, req, model, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxResponseBytes+1))
	if err != nil {
		return nil, readError(ctx, err)
	}
	if int64(len(body)) > p.maxResponseBytes {
		return nil, protocolError("response exceeds the size limit")
	}
	return decodeCompletion(body)
}

func decodeCompletion(body []byte) (*chat.Completion, error) {
	// Decoding would otherwise replace invalid bytes with U+FFFD.
	if !utf8.Valid(body) {
		return nil, protocolError("response is not valid UTF-8")
	}
	var in struct {
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content      *string         `json:"content"`
				Refusal      *string         `json:"refusal"`
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, protocolError("response is not the expected JSON")
	}
	if len(in.Choices) != 1 {
		return nil, protocolError("response does not have exactly one choice")
	}
	choice := in.Choices[0]
	if choice.Index != 0 {
		return nil, protocolError("response choice has an unexpected index")
	}
	if hasValue(choice.Message.ToolCalls) || hasValue(choice.Message.FunctionCall) {
		return nil, protocolError("response contains a tool call, which the contract excludes")
	}
	if choice.Message.Content == nil && choice.Message.Refusal == nil {
		return nil, protocolError("response has neither content nor a refusal")
	}
	if choice.FinishReason == nil {
		return nil, protocolError("response has no finish reason")
	}
	finish, ok := parseFinishReason(*choice.FinishReason)
	if !ok {
		return nil, protocolError("response has an unsupported finish reason")
	}

	out := &chat.Completion{FinishReason: finish, Usage: parseUsage(in.Usage)}
	if choice.Message.Content != nil {
		out.Content = *choice.Message.Content
	}
	if choice.Message.Refusal != nil {
		out.Refusal = *choice.Message.Refusal
	}
	return out, nil
}

func parseFinishReason(s string) (chat.FinishReason, bool) {
	switch reason := chat.FinishReason(s); reason {
	case chat.FinishStop, chat.FinishLength, chat.FinishContentFilter:
		return reason, true
	}
	return "", false
}

// hasValue reports whether an optional JSON field holds anything. Compatible
// servers may send null or an empty array to mean absent.
func hasValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]"))
}

// parseUsage reads a usage report. No report gives nil, not zeros. An unusable
// report is flagged as malformed and keeps its raw bytes for reconciliation.
func parseUsage(raw json.RawMessage) *chat.Usage {
	if !hasValue(raw) {
		return nil
	}
	out := &chat.Usage{State: chat.UsageMalformed, Raw: bytes.Clone(raw)}

	var in struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		TotalTokens      *int64 `json:"total_tokens"`
		PromptDetails    *struct {
			CachedTokens     *int64 `json:"cached_tokens"`
			CacheWriteTokens *int64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return out
	}
	if in.PromptTokens == nil || in.CompletionTokens == nil || in.TotalTokens == nil {
		return out
	}
	prompt, completion, total := *in.PromptTokens, *in.CompletionTokens, *in.TotalTokens
	if prompt < 0 || completion < 0 || total < 0 || prompt > (1<<62) || completion > (1<<62) {
		return out
	}

	out.PromptTokens, out.CompletionTokens, out.TotalTokens = prompt, completion, total
	out.State = chat.UsageReported
	if prompt+completion != total {
		out.State = chat.UsageInconsistent
	}
	if d := in.PromptDetails; d != nil {
		out.CachedPromptTokens, out.CacheWritePromptTokens = d.CachedTokens, d.CacheWriteTokens
	}
	if d := in.CompletionDetails; d != nil {
		out.ReasoningTokens = d.ReasoningTokens
	}
	return out
}

// readError classifies a failure partway through a response. The upstream
// certainly received the request, so a cancellation here may still be billed.
func readError(ctx context.Context, err error) *provider.Error {
	kind := provider.KindInterrupted
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = provider.KindCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = provider.KindTimeout
	}
	return &provider.Error{Kind: kind, Delivery: provider.DeliveryConfirmed, Err: err}
}

// Stream implements provider.Provider.
func (p *Provider) Stream(ctx context.Context, req *chat.Request, model string) (provider.Stream, error) {
	// A context of its own lets Close cancel the upstream, and bound the drain,
	// without closing the body while a read may be in progress.
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := p.send(streamCtx, req, model, true)
	if err != nil {
		cancel()
		return nil, err
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		return nil, protocolError("stream response is not an event stream")
	}
	return &stream{
		ctx:     streamCtx,
		cancel:  cancel,
		body:    resp.Body,
		decoder: sse.NewDecoder(resp.Body, p.maxEventBytes),
	}, nil
}

type wireChunk struct {
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content      *string         `json:"content"`
			Refusal      *string         `json:"refusal"`
			ToolCalls    json.RawMessage `json:"tool_calls"`
			FunctionCall json.RawMessage `json:"function_call"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

type stream struct {
	ctx     context.Context
	cancel  context.CancelFunc
	body    io.ReadCloser
	decoder *sse.Decoder

	finished bool // a finish reason arrived
	done     bool // the completion marker arrived
	closed   bool
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
		if string(ev.Data) == doneMarker {
			if !s.finished {
				return chat.StreamEvent{}, protocolError("stream completed without a finish reason")
			}
			s.done = true
			return chat.StreamEvent{}, io.EOF
		}
		out, err := s.parseChunk(ev.Data)
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
		return protocolError("stream event exceeds the size limit")
	}
	if errors.Is(err, io.EOF) {
		// The connection ended cleanly, but with no completion marker the output
		// can't be trusted to be whole.
		err = errors.New("stream ended without a completion marker")
	}
	return readError(s.ctx, err)
}

// parseChunk validates one event. An event carrying nothing, such as a role-only
// chunk, gives the zero StreamEvent.
func (s *stream) parseChunk(data []byte) (chat.StreamEvent, error) {
	var out chat.StreamEvent
	// Decoding would otherwise replace invalid bytes with U+FFFD.
	if !utf8.Valid(data) {
		return out, protocolError("stream event is not valid UTF-8")
	}
	var chunk wireChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return out, protocolError("stream event is not the expected JSON")
	}
	if hasValue(chunk.Error) {
		return out, streamError(chunk.Error)
	}
	if len(chunk.Choices) > 1 {
		return out, protocolError("stream event has more than one choice")
	}
	if len(chunk.Choices) == 1 {
		choice := chunk.Choices[0]
		if choice.Index != 0 {
			return out, protocolError("stream event has an unexpected choice index")
		}
		if hasValue(choice.Delta.ToolCalls) || hasValue(choice.Delta.FunctionCall) {
			return out, protocolError("stream event contains a tool call, which the contract excludes")
		}
		if choice.Delta.Content != nil {
			out.Content = *choice.Delta.Content
		}
		if choice.Delta.Refusal != nil {
			out.Refusal = *choice.Delta.Refusal
		}
		if s.finished && (out.Content != "" || out.Refusal != "") {
			return out, protocolError("stream event has content after the finish reason")
		}
		if choice.FinishReason != nil {
			if s.finished {
				return out, protocolError("stream event repeats the finish reason")
			}
			finish, ok := parseFinishReason(*choice.FinishReason)
			if !ok {
				return out, protocolError("stream event has an unsupported finish reason")
			}
			out.FinishReason = finish
			s.finished = true
		}
	}
	out.Usage = parseUsage(chunk.Usage)
	return out, nil
}

// streamError classifies an error sent inside a stream, after a 200 response.
func streamError(errorJSON []byte) *provider.Error {
	details := parseErrorDetails(errorJSON)
	code := details.identifier()
	kind := provider.KindServer
	switch {
	case details.isBilling():
		kind = provider.KindBilling
	case strings.Contains(code, "rate_limit"):
		kind = provider.KindRateLimited
	case strings.Contains(code, "overloaded"):
		kind = provider.KindOverloaded
	}
	return &provider.Error{Kind: kind, Delivery: provider.DeliveryConfirmed, UpstreamCode: code}
}

// Close implements provider.Stream. After a completed stream it reads the tail
// of the body, because HTTP/1.1 can't reuse a connection otherwise.
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	defer s.cancel()
	if s.done {
		timer := time.AfterFunc(drainTimeout, s.cancel)
		defer timer.Stop()
		_, _ = io.CopyN(io.Discard, s.body, maxDrainBytes)
	}
	if err := s.body.Close(); err != nil {
		return fmt.Errorf("closing upstream response: %w", err)
	}
	return nil
}
