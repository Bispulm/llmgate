package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/provider/openai"
	"github.com/Bispulm/llmgate/internal/routing"
	"github.com/Bispulm/llmgate/internal/sse"
)

const (
	testAPIKey      = "client-key-value"
	testProviderKey = "provider-key-value"
	testPrompt      = "synthetic-prompt-text"
)

// recordedAttempt is an attempt and what the recorder saw of its context.
type recordedAttempt struct {
	accounting.Attempt
	ctxErr      error
	hasDeadline bool
}

// attemptRecorder keeps attempts for assertions. It optionally forwards them.
type attemptRecorder struct {
	inner    accounting.Recorder
	attempts chan recordedAttempt
}

func (r *attemptRecorder) RecordAttempt(ctx context.Context, a accounting.Attempt) {
	if r.inner != nil {
		r.inner.RecordAttempt(ctx, a)
	}
	_, hasDeadline := ctx.Deadline()
	select {
	case r.attempts <- recordedAttempt{a, ctx.Err(), hasDeadline}:
	case <-ctx.Done():
		// Follows the Recorder contract. The attempt is lost, so wait fails the test.
	}
}

func (r *attemptRecorder) wait(t *testing.T) recordedAttempt {
	t.Helper()
	select {
	case a := <-r.attempts:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("no attempt was recorded")
		return recordedAttempt{}
	}
}

type gateway struct {
	url      string
	mock     *mock.Server
	recorder *attemptRecorder
	client   *http.Client
}

func testLimits() Limits {
	return Limits{
		MaxRequestBytes:    1 << 16,
		RequestTimeout:     20 * time.Second,
		FirstEventTimeout:  10 * time.Second,
		StreamIdleTimeout:  10 * time.Second,
		ClientWriteTimeout: 5 * time.Second,
		BodyReadTimeout:    5 * time.Second,
	}
}

// newGateway runs the real stack on real sockets: client, gateway, adapter, mock.
// The alias "fast" routes to the given upstream model string.
func newGateway(t *testing.T, model string, options ...func(*Options)) *gateway {
	t.Helper()

	m := mock.New()
	upstream := httptest.NewServer(m)
	t.Cleanup(upstream.Close)

	adapter, err := openai.New(openai.Options{
		BaseURL:          upstream.URL + "/v1",
		APIKey:           testProviderKey,
		MaxResponseBytes: 1 << 20,
		MaxEventBytes:    1 << 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.CloseIdleConnections)

	return startGateway(t, adapter, model, m, options...)
}

func startGateway(t *testing.T, p provider.Provider, model string, m *mock.Server, options ...func(*Options)) *gateway {
	t.Helper()
	rec := &attemptRecorder{attempts: make(chan recordedAttempt, 64)}
	opts := Options{
		Auth:     auth.NewDevKey(testAPIKey),
		Recorder: rec,
		Limits:   testLimits(),
		Routes: routing.NewTable([]routing.Alias{{
			Name:            "fast",
			MaxOutputTokens: 100,
			Routes:          []routing.Route{{ProviderName: "mock", Provider: p, Model: model}},
		}}),
	}
	for _, f := range options {
		f(&opts)
	}
	if inner, ok := opts.Recorder.(*attemptRecorder); ok && inner != rec {
		rec = inner
	}

	ts := httptest.NewServer(New(opts))
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	return &gateway{url: ts.URL, mock: m, recorder: rec, client: client}
}

func chatBody(stream bool, extra string) string {
	body := `{"model":"fast","messages":[{"role":"user","content":"` + testPrompt + `"}]`
	if stream {
		body += `,"stream":true`
	}
	return body + extra + `}`
}

func (g *gateway) send(ctx context.Context, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return g.client.Do(req)
}

func (g *gateway) post(t *testing.T, body string) *http.Response {
	t.Helper()
	resp, err := g.send(context.Background(), body)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

type clientError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func readError(t *testing.T, resp *http.Response) clientError {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error content type = %q", ct)
	}
	var got struct {
		Error clientError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Message == "" || got.Error.Type == "" {
		t.Fatalf("incomplete error: %+v", got.Error)
	}
	return got.Error
}

func expectError(t *testing.T, resp *http.Response, status int, errType, code string) clientError {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d", resp.StatusCode, status)
	}
	got := readError(t, resp)
	if got.Type != errType {
		t.Fatalf("type = %q, want %q", got.Type, errType)
	}
	if got.Code == nil || *got.Code != code {
		t.Fatalf("code = %v, want %q", got.Code, code)
	}
	return got
}

type clientChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
			Refusal *string `json:"refusal"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error *clientError    `json:"error"`
}

type clientStream struct {
	chunks []clientChunk
	done   bool
	text   string
	finish string
	err    *clientError
}

func readStream(t *testing.T, r io.Reader) clientStream {
	t.Helper()
	var out clientStream
	d := sse.NewDecoder(r, 1<<20)
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if out.done {
			t.Fatalf("event after [DONE]: %s", ev.Data)
		}
		if string(ev.Data) == "[DONE]" {
			out.done = true
			continue
		}
		var c clientChunk
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			t.Fatalf("event %q: %v", ev.Data, err)
		}
		if c.Error != nil {
			out.err = c.Error
			continue
		}
		out.chunks = append(out.chunks, c)
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				out.text += *ch.Delta.Content
			}
			if ch.FinishReason != nil {
				out.finish = *ch.FinishReason
			}
		}
	}
}

func TestNonStreamingRoundTrip(t *testing.T) {
	g := newGateway(t, "alpha;tokens=3")
	resp := g.post(t, chatBody(false, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id")
	}
	if got := resp.Header.Get("X-Llmgate-Route"); got != "mock/alpha;tokens=3" {
		t.Errorf("route header = %q", got)
	}
	if got := resp.Header.Get("X-Llmgate-Budget-Enforcement"); got != "none" {
		t.Errorf("budget header = %q: nothing may imply that budgets are enforced yet", got)
	}

	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string  `json:"role"`
				Content string  `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"message"`
			Logprobs     *struct{} `json:"logprobs"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			Total      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.ID, "chatcmpl-") || got.Object != "chat.completion" || got.Created == 0 {
		t.Errorf("response = %+v", got)
	}
	if got.Model != "fast" {
		t.Errorf("model = %q, want the alias", got.Model)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Role != "assistant" ||
		got.Choices[0].Message.Content != "[alpha] token-1 token-2" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", got.Choices)
	}
	if got.Usage.Completion != 3 || got.Usage.Total != got.Usage.Prompt+3 {
		t.Errorf("usage = %+v", got.Usage)
	}

	a := g.recorder.wait(t)
	if a.Outcome != accounting.OutcomeCompleted || !a.Committed || a.Stream || a.Number != 1 {
		t.Errorf("attempt = %+v", a)
	}
	if a.TeamID == "" || a.KeyID == "" || a.Alias != "fast" || a.Provider != "mock" {
		t.Errorf("attempt identity = %+v", a)
	}
	if a.Usage == nil || a.Usage.CompletionTokens != 3 {
		t.Errorf("attempt usage = %+v", a.Usage)
	}
}

func TestStreamingRoundTrip(t *testing.T) {
	g := newGateway(t, "alpha;tokens=3")
	resp := g.post(t, chatBody(true, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}
	if got := resp.Header.Get("X-Llmgate-Budget-Enforcement"); got != "none" {
		t.Errorf("budget header = %q", got)
	}

	s := readStream(t, resp.Body)
	if !s.done {
		t.Fatal("no [DONE]")
	}
	if s.text != "[alpha] token-1 token-2" || s.finish != "stop" {
		t.Fatalf("text %q finish %q", s.text, s.finish)
	}

	first := s.chunks[0]
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role != "assistant" ||
		first.Choices[0].Delta.Content == nil || *first.Choices[0].Delta.Content != "" {
		t.Errorf("first chunk = %+v", first)
	}
	for i, c := range s.chunks {
		if c.Object != "chat.completion.chunk" || c.Model != "fast" || c.Created == 0 || c.ID != first.ID {
			t.Errorf("chunk %d = %+v", i, c)
		}
		if c.Usage != nil {
			t.Errorf("chunk %d has usage without include_usage: %s", i, c.Usage)
		}
	}
	last := s.chunks[len(s.chunks)-1]
	if last.Choices[0].FinishReason == nil || last.Choices[0].Delta.Content != nil {
		t.Errorf("last chunk = %+v", last)
	}

	a := g.recorder.wait(t)
	if a.Outcome != accounting.OutcomeCompleted || !a.Committed || !a.Stream || a.TextEvents != 3 {
		t.Errorf("attempt = %+v", a)
	}
	// The client didn't ask for usage, and accounting must get it anyway.
	if a.Usage == nil || a.Usage.CompletionTokens != 3 {
		t.Errorf("attempt usage = %+v", a.Usage)
	}
	if a.TimeToFirstEvent <= 0 {
		t.Errorf("time to first event = %v", a.TimeToFirstEvent)
	}
}

func TestStreamingUsageWhenRequested(t *testing.T) {
	g := newGateway(t, "alpha;tokens=2")
	resp := g.post(t, chatBody(true, `,"stream_options":{"include_usage":true}`))
	s := readStream(t, resp.Body)
	if !s.done {
		t.Fatal("no [DONE]")
	}
	last := s.chunks[len(s.chunks)-1]
	if len(last.Choices) != 0 {
		t.Fatalf("usage chunk has choices: %+v", last)
	}
	var usage struct {
		Completion int64 `json:"completion_tokens"`
	}
	if err := json.Unmarshal(last.Usage, &usage); err != nil || usage.Completion != 2 {
		t.Fatalf("usage = %s, err = %v", last.Usage, err)
	}
	for i, c := range s.chunks[:len(s.chunks)-1] {
		if string(c.Usage) != "null" {
			t.Errorf("chunk %d usage = %q, want null", i, c.Usage)
		}
	}
}

func TestMissingUsageIsNeverReportedAsZero(t *testing.T) {
	t.Run("streaming", func(t *testing.T) {
		g := newGateway(t, "alpha;usage=none")
		resp := g.post(t, chatBody(true, `,"stream_options":{"include_usage":true}`))
		s := readStream(t, resp.Body)
		if !s.done {
			t.Fatal("no [DONE]")
		}
		for _, c := range s.chunks {
			if string(c.Usage) != "null" {
				t.Fatalf("usage = %s", c.Usage)
			}
		}
		if a := g.recorder.wait(t); a.Usage != nil || a.Outcome != accounting.OutcomeCompleted {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("non-streaming", func(t *testing.T) {
		g := newGateway(t, "alpha;usage=none")
		resp := g.post(t, chatBody(false, ""))
		var got map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["usage"]; ok {
			t.Fatalf("usage = %s", got["usage"])
		}
		if a := g.recorder.wait(t); a.Usage != nil {
			t.Fatalf("attempt usage = %+v", a.Usage)
		}
	})
}

func TestOutputLimit(t *testing.T) {
	t.Run("an absent limit becomes the alias's maximum", func(t *testing.T) {
		g := newGateway(t, "alpha;tokens=1000")
		resp := g.post(t, chatBody(false, ""))
		var got struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				Completion int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Usage.Completion != 100 || got.Choices[0].FinishReason != "length" {
			t.Fatalf("response = %+v", got)
		}
	})
	t.Run("a smaller limit is honored", func(t *testing.T) {
		g := newGateway(t, "alpha;tokens=1000")
		resp := g.post(t, chatBody(true, `,"max_completion_tokens":2`))
		s := readStream(t, resp.Body)
		if s.finish != "length" || g.recorder.wait(t).TextEvents != 2 {
			t.Fatalf("stream = %+v", s)
		}
	})
	t.Run("a limit above the maximum is rejected, not lowered", func(t *testing.T) {
		g := newGateway(t, "limit-check")
		resp := g.post(t, chatBody(false, `,"max_tokens":101`))
		got := expectError(t, resp, 400, "invalid_request_error", "output_limit_exceeded")
		if got.Param == nil || *got.Param != "max_tokens" {
			t.Fatalf("param = %v", got.Param)
		}
		if requests := g.mock.Stats()["limit-check"].Requests; requests != 0 {
			t.Fatalf("upstream saw %d requests", requests)
		}
	})
}

func TestAuthentication(t *testing.T) {
	send := func(t *testing.T, g *gateway, header, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	for name, header := range map[string]string{
		"missing":   "",
		"wrong key": "Bearer not-the-key",
		"malformed": "Basic " + testAPIKey,
	} {
		t.Run(name, func(t *testing.T) {
			g := newGateway(t, "auth-check")
			resp := send(t, g, header, chatBody(false, ""))
			expectError(t, resp, 401, "authentication_error", "invalid_api_key")
			if got := resp.Header.Get("Www-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if requests := g.mock.Stats()["auth-check"].Requests; requests != 0 {
				t.Fatalf("upstream saw %d requests", requests)
			}
		})
	}
	t.Run("authentication comes before reading the body", func(t *testing.T) {
		g := newGateway(t, "auth-check")
		expectError(t, send(t, g, "", "{not json"), 401, "authentication_error", "invalid_api_key")
	})
	t.Run("the scheme is case-insensitive", func(t *testing.T) {
		g := newGateway(t, "alpha")
		if resp := send(t, g, "bearer "+testAPIKey, chatBody(false, "")); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestRequestRejection(t *testing.T) {
	g := newGateway(t, "reject-check")

	t.Run("an unsupported field is named and never reaches the upstream", func(t *testing.T) {
		resp := g.post(t, chatBody(false, `,"tools":[]`))
		got := expectError(t, resp, 400, "invalid_request_error", chat.CodeUnsupportedParameter)
		if got.Param == nil || *got.Param != "tools" {
			t.Fatalf("param = %v", got.Param)
		}
	})
	t.Run("an unknown alias", func(t *testing.T) {
		resp := g.post(t, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`)
		got := expectError(t, resp, 404, "invalid_request_error", "model_not_found")
		if got.Param == nil || *got.Param != "model" {
			t.Fatalf("param = %v", got.Param)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		resp := g.post(t, `{"model":`)
		expectError(t, resp, 400, "invalid_request_error", chat.CodeInvalidJSON)
	})
	t.Run("a body over the limit", func(t *testing.T) {
		big := strings.Repeat("x", 1<<17)
		resp := g.post(t, `{"model":"fast","messages":[{"role":"user","content":"`+big+`"}]}`)
		expectError(t, resp, 413, "invalid_request_error", "request_too_large")
	})
	t.Run("the wrong method", func(t *testing.T) {
		resp, err := g.client.Get(g.url + "/v1/chat/completions")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		expectError(t, resp, 405, "invalid_request_error", "method_not_allowed")
		if got := resp.Header.Get("Allow"); got != "POST" {
			t.Fatalf("Allow = %q", got)
		}
	})
	if requests := g.mock.Stats()["reject-check"].Requests; requests != 0 {
		t.Fatalf("upstream saw %d requests", requests)
	}
}

func TestFailuresBeforeCommitAreOrdinaryHTTPErrors(t *testing.T) {
	tests := []struct {
		name, model   string
		status        int
		errType, code string
		kind          provider.Kind
		delivery      provider.Delivery
	}{
		{"rate limited", "a;fail=status;status=429;retry_after=2", 429, "rate_limit_error", "upstream_rate_limited", provider.KindRateLimited, provider.DeliveryConfirmed},
		{"overloaded 503", "a;fail=status;status=503", 503, "upstream_error", "upstream_overloaded", provider.KindOverloaded, provider.DeliveryConfirmed},
		{"overloaded 529", "a;fail=status;status=529", 503, "upstream_error", "upstream_overloaded", provider.KindOverloaded, provider.DeliveryConfirmed},
		{"server error", "a;fail=status;status=500", 502, "upstream_error", "upstream_failed", provider.KindServer, provider.DeliveryConfirmed},
		{"provider rejected the credential", "a;fail=status;status=401", 502, "upstream_error", "upstream_misconfigured", provider.KindAuth, provider.DeliveryConfirmed},
		{"provider has no such model", "a;fail=status;status=404", 502, "upstream_error", "upstream_misconfigured", provider.KindNotFound, provider.DeliveryConfirmed},
		{"provider rejected the request", "a;fail=status;status=400", 400, "invalid_request_error", "upstream_rejected", provider.KindInvalidRequest, provider.DeliveryConfirmed},
		{"provider reported a timeout", "a;fail=status;status=408", 504, "upstream_error", "upstream_timeout", provider.KindTimeout, provider.DeliveryConfirmed},
		{"connection dropped", "a;fail=drop_before_headers", 502, "upstream_error", "upstream_failed", provider.KindConnection, provider.DeliveryUncertain},
	}
	for _, tt := range tests {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tt.name, stream), func(t *testing.T) {
				g := newGateway(t, tt.model)
				resp := g.post(t, chatBody(stream, ""))
				expectError(t, resp, tt.status, tt.errType, tt.code)
				a := g.recorder.wait(t)
				if a.Outcome != accounting.OutcomeFailed || a.Committed {
					t.Errorf("attempt = %+v", a)
				}
				if a.ErrorKind != tt.kind || a.Delivery != tt.delivery {
					t.Errorf("kind %q delivery %q", a.ErrorKind, a.Delivery)
				}
				if a.Usage != nil {
					t.Errorf("usage = %+v", a.Usage)
				}
			})
		}
	}

	t.Run("the retry hint is passed on", func(t *testing.T) {
		g := newGateway(t, "a;fail=status;status=429;retry_after=2")
		if got := g.post(t, chatBody(false, "")).Header.Get("Retry-After"); got != "2" {
			t.Fatalf("Retry-After = %q", got)
		}
	})
	t.Run("a provider's message is not forwarded", func(t *testing.T) {
		g := newGateway(t, "a;fail=status;status=429")
		got := readError(t, g.post(t, chatBody(false, "")))
		if strings.Contains(got.Message, "mock failure") {
			t.Fatalf("message = %q", got.Message)
		}
	})
}

// This is the commitment rule. A provider that accepts the request and sends only
// its prelude has told the client nothing, so the failure can still be a clean
// HTTP error, and fallback can still happen once it exists.
func TestStreamFailuresBeforeTheFirstContentDoNotCommit(t *testing.T) {
	tests := []struct {
		name, model string
		code        string
		kind        provider.Kind
	}{
		{"clean end right after headers", "a;fail=eof_before_first_event", "upstream_failed", provider.KindInterrupted},
		{"abrupt end right after headers", "a;fail=disconnect_before_first_event", "upstream_failed", provider.KindInterrupted},
		{"abrupt end after a role-only chunk", "a;fail=disconnect;after=0", "upstream_failed", provider.KindInterrupted},
		{"malformed first event", "a;fail=malformed;after=0", "upstream_protocol_error", provider.KindProtocol},
		{"oversized first event", "a;fail=oversized;after=0;size=100000", "upstream_protocol_error", provider.KindProtocol},
		{"error event first", "a;fail=stream_error;after=0", "upstream_failed", provider.KindServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, tt.model)
			resp := g.post(t, chatBody(true, ""))
			expectError(t, resp, 502, "upstream_error", tt.code)
			a := g.recorder.wait(t)
			if a.Committed || a.Outcome != accounting.OutcomeFailed || a.ErrorKind != tt.kind {
				t.Fatalf("attempt = %+v", a)
			}
			if a.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("delivery = %q: the upstream received this request", a.Delivery)
			}
		})
	}
	t.Run("a non-event-stream 200", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>proxy login</html>")
		}))
		defer ts.Close()
		adapter, err := openai.New(openai.Options{BaseURL: ts.URL, APIKey: "k", MaxResponseBytes: 1 << 16, MaxEventBytes: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.CloseIdleConnections()
		g := startGateway(t, adapter, "m", nil)
		expectError(t, g.post(t, chatBody(true, "")), 502, "upstream_error", "upstream_protocol_error")
	})
	t.Run("headers aren't sent until content arrives", func(t *testing.T) {
		const ttfb = 300 * time.Millisecond
		g := newGateway(t, "alpha;ttfb=300ms")
		start := time.Now()
		resp := g.post(t, chatBody(true, ""))
		if got := time.Since(start); got < ttfb {
			t.Fatalf("headers arrived after %v, before the first content at %v", got, ttfb)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestStreamFailuresAfterCommitEndWithoutCompletion(t *testing.T) {
	tests := []struct {
		name, model string
		wantText    string
		wantFinish  string
		kind        provider.Kind
	}{
		{"disconnect", "alpha;fail=disconnect;after=2;tokens=5", "[alpha] token-1", "", provider.KindInterrupted},
		{"malformed event", "alpha;fail=malformed;after=1", "[alpha]", "", provider.KindProtocol},
		{"oversized event", "alpha;fail=oversized;after=1;size=100000", "[alpha]", "", provider.KindProtocol},
		{"error event", "alpha;fail=stream_error;after=2", "[alpha] token-1", "", provider.KindServer},
		{"everything except the completion marker", "alpha;terminator=none;tokens=2", "[alpha] token-1", "stop", provider.KindInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, tt.model)
			resp := g.post(t, chatBody(true, ""))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			s := readStream(t, resp.Body)
			if s.done {
				t.Fatal("a failed stream got [DONE]")
			}
			if s.text != tt.wantText || s.finish != tt.wantFinish {
				t.Fatalf("text %q finish %q", s.text, s.finish)
			}
			if s.err == nil || s.err.Type != "upstream_error" || s.err.Code == nil || *s.err.Code != "stream_interrupted" {
				t.Fatalf("error event = %+v", s.err)
			}

			a := g.recorder.wait(t)
			if !a.Committed || a.Outcome != accounting.OutcomeInterrupted || a.ErrorKind != tt.kind {
				t.Fatalf("attempt = %+v", a)
			}
		})
	}

	t.Run("usage that never arrived stays unknown", func(t *testing.T) {
		g := newGateway(t, "alpha;fail=disconnect;after=2;tokens=5")
		_ = readStream(t, g.post(t, chatBody(true, "")).Body)
		a := g.recorder.wait(t)
		if a.Usage != nil {
			t.Fatalf("usage = %+v", a.Usage)
		}
		if a.TextEvents != 2 || a.OutputBytes == 0 {
			t.Fatalf("partial output = %d events, %d bytes", a.TextEvents, a.OutputBytes)
		}
	})
	t.Run("usage that did arrive is kept", func(t *testing.T) {
		g := newGateway(t, "alpha;terminator=none;tokens=2")
		_ = readStream(t, g.post(t, chatBody(true, "")).Body)
		a := g.recorder.wait(t)
		if a.Usage == nil || a.Usage.CompletionTokens != 2 {
			t.Fatalf("usage = %+v", a.Usage)
		}
	})
}

func waitForMock(t *testing.T, g *gateway, label string, ok func(mock.LabelStats) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok(g.mock.Stats()[label]) {
		if time.Now().After(deadline) {
			t.Fatalf("mock stats for %q never reached the expected state: %+v", label, g.mock.Stats()[label])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func canceledUpstream(s mock.LabelStats) bool { return s.Canceled == 1 }

func TestClientCancellationReachesTheUpstream(t *testing.T) {
	assertAccounted := func(t *testing.T, g *gateway, committed bool) recordedAttempt {
		t.Helper()
		a := g.recorder.wait(t)
		if a.Outcome != accounting.OutcomeClientCanceled || a.Committed != committed {
			t.Fatalf("attempt = %+v", a)
		}
		if a.Usage != nil {
			t.Fatalf("usage = %+v: a canceled attempt's cost is unknown, not zero", a.Usage)
		}
		// Accounting must run on a live context with its own limit, or the
		// cancellation being recorded would also prevent recording it.
		if a.ctxErr != nil || !a.hasDeadline {
			t.Fatalf("recorder context: err %v, deadline %v", a.ctxErr, a.hasDeadline)
		}
		return a
	}

	t.Run("during a stream", func(t *testing.T) {
		g := newGateway(t, "gone;tokens=1000;interval=10ms")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resp, err := g.send(ctx, chatBody(true, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := sse.NewDecoder(resp.Body, 1<<16).Next(); err != nil {
			t.Fatal(err)
		}
		cancel()
		waitForMock(t, g, "gone", canceledUpstream)
		a := assertAccounted(t, g, true)
		if g.mock.Stats()["gone"].Completed != 0 {
			t.Fatalf("mock stats = %+v", g.mock.Stats()["gone"])
		}
		if a.TextEvents == 0 {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("while waiting for the first event", func(t *testing.T) {
		g := newGateway(t, "gone;ttfb=30s")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errs := make(chan error, 1)
		go func() {
			resp, err := g.send(ctx, chatBody(true, ""))
			if err == nil {
				_ = resp.Body.Close()
			}
			errs <- err
		}()
		waitForMock(t, g, "gone", func(s mock.LabelStats) bool { return s.Requests == 1 })
		cancel()
		waitForMock(t, g, "gone", canceledUpstream)
		assertAccounted(t, g, false)
		if err := <-errs; err == nil {
			t.Error("the canceled request reported success")
		}
	})
	t.Run("during a non-streaming request", func(t *testing.T) {
		g := newGateway(t, "gone;latency=30s")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errs := make(chan error, 1)
		go func() {
			resp, err := g.send(ctx, chatBody(false, ""))
			if err == nil {
				_ = resp.Body.Close()
			}
			errs <- err
		}()
		waitForMock(t, g, "gone", func(s mock.LabelStats) bool { return s.Requests == 1 })
		cancel()
		waitForMock(t, g, "gone", canceledUpstream)
		assertAccounted(t, g, false)
		if err := <-errs; err == nil {
			t.Error("the canceled request reported success")
		}
	})
}

func TestDeadlines(t *testing.T) {
	withLimits := func(f func(*Limits)) func(*Options) {
		return func(o *Options) { f(&o.Limits) }
	}

	t.Run("no first event in time", func(t *testing.T) {
		g := newGateway(t, "slow;ttfb=30s", withLimits(func(l *Limits) { l.FirstEventTimeout = 100 * time.Millisecond }))
		resp := g.post(t, chatBody(true, ""))
		expectError(t, resp, 504, "upstream_error", "upstream_timeout")
		waitForMock(t, g, "slow", canceledUpstream)
		a := g.recorder.wait(t)
		if a.Committed || a.Outcome != accounting.OutcomeFailed || a.ErrorKind != provider.KindTimeout {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("keepalives alone don't hold the first-event window open", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for {
				_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n: ping\n\n")
				_ = http.NewResponseController(w).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}))
		defer ts.Close()
		adapter, err := openai.New(openai.Options{BaseURL: ts.URL, APIKey: "k", MaxResponseBytes: 1 << 16, MaxEventBytes: 1 << 16})
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.CloseIdleConnections()
		g := startGateway(t, adapter, "m", nil, withLimits(func(l *Limits) { l.FirstEventTimeout = 100 * time.Millisecond }))
		expectError(t, g.post(t, chatBody(true, "")), 504, "upstream_error", "upstream_timeout")
	})
	t.Run("a stall after commit", func(t *testing.T) {
		g := newGateway(t, "stall;tokens=3;interval=30s", withLimits(func(l *Limits) { l.StreamIdleTimeout = 100 * time.Millisecond }))
		start := time.Now()
		resp := g.post(t, chatBody(true, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		s := readStream(t, resp.Body)
		if s.done || s.err == nil {
			t.Fatalf("stream = %+v", s)
		}
		// The overall request deadline would end this too, much later.
		if got := time.Since(start); got > 5*time.Second {
			t.Fatalf("the stall was detected after %v", got)
		}
		waitForMock(t, g, "stall", canceledUpstream)
		a := g.recorder.wait(t)
		if !a.Committed || a.Outcome != accounting.OutcomeInterrupted || a.ErrorKind != provider.KindTimeout {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("the overall request deadline after commit", func(t *testing.T) {
		g := newGateway(t, "long;tokens=100;interval=50ms", withLimits(func(l *Limits) { l.RequestTimeout = 300 * time.Millisecond }))
		resp := g.post(t, chatBody(true, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		s := readStream(t, resp.Body)
		if s.done || s.err == nil {
			t.Fatalf("stream = %+v", s)
		}
		waitForMock(t, g, "long", canceledUpstream)
		a := g.recorder.wait(t)
		if !a.Committed || a.Outcome != accounting.OutcomeInterrupted || a.ErrorKind != provider.KindTimeout {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("the overall request deadline", func(t *testing.T) {
		g := newGateway(t, "slow;latency=30s", withLimits(func(l *Limits) { l.RequestTimeout = 100 * time.Millisecond }))
		expectError(t, g.post(t, chatBody(false, "")), 504, "upstream_error", "upstream_timeout")
		waitForMock(t, g, "slow", canceledUpstream)
		if a := g.recorder.wait(t); a.ErrorKind != provider.KindTimeout {
			t.Fatalf("attempt = %+v", a)
		}
	})
}

// A client that stops reading must stall the upstream read, not grow a buffer, and
// the write timeout must then release the upstream.
func TestStalledClientReleasesTheUpstream(t *testing.T) {
	const padded = 4096
	const tokens = 200000
	g := newGateway(t, fmt.Sprintf("stuck;tokens=%d;pad=%d", tokens, padded), func(o *Options) {
		o.Limits.ClientWriteTimeout = 300 * time.Millisecond
		o.Routes = routingWithLimit(o.Routes, tokens)
	})

	conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.(*net.TCPConn).SetReadBuffer(4096); err != nil {
		t.Fatal(err)
	}
	body := chatBody(true, "")
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
		testAPIKey, len(body), body); err != nil {
		t.Fatal(err)
	}

	// The connection is never read. The gateway's writes must fill the socket and
	// time out, which cancels the upstream.
	waitForMock(t, g, "stuck", canceledUpstream)

	a := g.recorder.wait(t)
	if a.Outcome != accounting.OutcomeClientStalled || !a.Committed {
		t.Fatalf("attempt = %+v", a)
	}
	const limit = 128 << 20
	if written := g.mock.Stats()["stuck"].BytesWritten; written > limit {
		t.Fatalf("the upstream wrote %d bytes for a client that read nothing", written)
	}
}

func routingWithLimit(table *routing.Table, limit int64) *routing.Table {
	alias, _ := table.Resolve("fast")
	updated := *alias
	updated.MaxOutputTokens = limit
	return routing.NewTable([]routing.Alias{updated})
}

// The write timeout limits how long one write can block. It must never limit how
// long a provider can take, or every response slower than it would be lost.
func TestWriteTimeoutDoesNotLimitSlowProviders(t *testing.T) {
	const writeTimeout = 100 * time.Millisecond
	short := func(o *Options) { o.Limits.ClientWriteTimeout = writeTimeout }

	t.Run("a slow non-streaming response", func(t *testing.T) {
		g := newGateway(t, "alpha;tokens=2;latency=400ms", short)
		resp := g.post(t, chatBody(false, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		if !bytes.Contains(raw, []byte("[alpha] token-1")) {
			t.Fatalf("body = %s", raw)
		}
		if a := g.recorder.wait(t); a.Outcome != accounting.OutcomeCompleted {
			t.Fatalf("attempt = %+v", a)
		}
	})
	t.Run("a failure reported after a long wait", func(t *testing.T) {
		g := newGateway(t, "a;latency=400ms;fail=status;status=500", short)
		expectError(t, g.post(t, chatBody(false, "")), 502, "upstream_error", "upstream_failed")
	})
	t.Run("a timeout reported after a long wait", func(t *testing.T) {
		g := newGateway(t, "a;ttfb=30s", short, func(o *Options) { o.Limits.FirstEventTimeout = 400 * time.Millisecond })
		expectError(t, g.post(t, chatBody(true, "")), 504, "upstream_error", "upstream_timeout")
	})
	t.Run("a slow first event", func(t *testing.T) {
		g := newGateway(t, "alpha;ttfb=400ms", short)
		if s := readStream(t, g.post(t, chatBody(true, "")).Body); !s.done {
			t.Fatalf("stream = %+v", s)
		}
	})
	t.Run("a stream lasting longer than the write timeout", func(t *testing.T) {
		g := newGateway(t, "alpha;tokens=5;interval=100ms", short)
		s := readStream(t, g.post(t, chatBody(true, "")).Body)
		if !s.done {
			t.Fatalf("stream = %+v", s)
		}
		if a := g.recorder.wait(t); a.TextEvents != 5 {
			t.Fatalf("attempt = %+v", a)
		}
	})
}

// A write deadline applies to the connection, so a stale one from an earlier
// response must not break a later request on the same connection.
func TestDeadlinesDoNotOutliveTheirRequest(t *testing.T) {
	g := newGateway(t, "alpha;tokens=2", func(o *Options) { o.Limits.ClientWriteTimeout = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)

	exchange := func(path string, method string, body string) int {
		t.Helper()
		if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			method, path, testAPIKey, len(body), body); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	if got := exchange("/v1/chat/completions", http.MethodPost, chatBody(true, "")); got != http.StatusOK {
		t.Fatalf("first request = %d", got)
	}
	time.Sleep(250 * time.Millisecond)
	if got := exchange("/healthz", http.MethodGet, ""); got != http.StatusOK {
		t.Fatalf("health check = %d", got)
	}
	time.Sleep(250 * time.Millisecond)
	if got := exchange("/v1/chat/completions", http.MethodPost, chatBody(false, "")); got != http.StatusOK {
		t.Fatalf("later request = %d", got)
	}
}

func TestSlowRequestBodyIsCutOff(t *testing.T) {
	g := newGateway(t, "alpha", func(o *Options) { o.Limits.BodyReadTimeout = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n{", testAPIKey); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("no response to a stalled body: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	expectError(t, resp, 408, "invalid_request_error", "request_timeout")

	// The unread remainder of the body must never be treated as a next request, so
	// the server has to end the connection.
	if !resp.Close {
		t.Error("the response does not announce that the connection will close")
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("connection not closed: read returned %v", err)
	}
}

// A request rejected without reading its body must not wait for that body, because
// net/http drains an unread body before replying. Without this, any caller could
// hold a connection and a goroutine open without authenticating.
func TestRejectionsDoNotWaitForAnUnreadBody(t *testing.T) {
	tests := []struct {
		name, method, path, auth string
		status                   int
	}{
		{"no credential", http.MethodPost, "/v1/chat/completions", "", 401},
		{"wrong credential", http.MethodPost, "/v1/chat/completions", "Authorization: Bearer wrong\r\n", 401},
		{"wrong method", http.MethodPut, "/v1/chat/completions", "", 405},
		{"unknown path", http.MethodPost, "/nope", "", 404},
		{"health check", http.MethodGet, "/healthz", "", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newGateway(t, "alpha", func(o *Options) { o.Limits.BodyReadTimeout = 200 * time.Millisecond })
			conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: test\r\n%sContent-Length: 1000\r\n\r\n", tt.method, tt.path, tt.auth); err != nil {
				t.Fatal(err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("no response while the body is outstanding: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			// The server must also release the connection on its own, while this client
			// keeps it open and never sends the body.
			if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("connection not released: read returned %v", err)
			}
		})
	}

	t.Run("a client that did send its body still gets a clean rejection", func(t *testing.T) {
		g := newGateway(t, "alpha")
		req, err := http.NewRequest(http.MethodPost, g.url+"/v1/chat/completions", strings.NewReader(chatBody(false, "")))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		expectError(t, resp, 401, "authentication_error", "invalid_api_key")
	})
	t.Run("requests without a body keep their connection", func(t *testing.T) {
		g := newGateway(t, "alpha")
		resp, err := g.client.Get(g.url + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.Close {
			t.Fatal("the connection was closed for a request with no body")
		}
	})
}

// Both timers start around the same moment, so the response to a stalled body must
// not need the write deadline set when the request arrived.
func TestSlowRequestBodyGetsAResponseWhenBothTimeoutsExpire(t *testing.T) {
	g := newGateway(t, "alpha", func(o *Options) {
		o.Limits.BodyReadTimeout = 300 * time.Millisecond
		o.Limits.ClientWriteTimeout = 100 * time.Millisecond
	})
	conn, err := net.Dial("tcp", strings.TrimPrefix(g.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n{", testAPIKey); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	expectError(t, resp, 408, "invalid_request_error", "request_timeout")
}

func TestStreamFramingFromUpstreamDoesNotChangeOutput(t *testing.T) {
	const base = "alpha;tokens=6;text=unicode"
	reference := func() string {
		g := newGateway(t, base)
		var got struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(g.post(t, chatBody(false, "")).Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.Choices[0].Message.Content
	}()
	if !strings.Contains(reference, "世界") {
		t.Fatalf("reference text %q has no multi-byte characters", reference)
	}

	for _, framing := range []string{";chunk=1", ";eol=crlf", ";eol=cr", ";comments=true", ";multiline=true", ";chunk=2;eol=crlf;comments=true"} {
		t.Run(framing, func(t *testing.T) {
			g := newGateway(t, base+framing)
			s := readStream(t, g.post(t, chatBody(true, "")).Body)
			if !s.done || s.text != reference {
				t.Fatalf("done %v, text %q, want %q", s.done, s.text, reference)
			}
		})
	}
}

// fakeProvider produces exact results for cases the mock can't express.
type fakeProvider struct {
	completion *chat.Completion
	events     []chat.StreamEvent
}

func (f *fakeProvider) Complete(context.Context, *chat.Request, string) (*chat.Completion, error) {
	return f.completion, nil
}

func (f *fakeProvider) Stream(context.Context, *chat.Request, string) (provider.Stream, error) {
	return &fakeStream{events: f.events}, nil
}

type fakeStream struct {
	events []chat.StreamEvent
}

func (s *fakeStream) Next() (chat.StreamEvent, error) {
	if len(s.events) == 0 {
		return chat.StreamEvent{}, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *fakeStream) Close() error { return nil }

func TestResponseEncoding(t *testing.T) {
	t.Run("text keeps its exact bytes", func(t *testing.T) {
		const text = `<b>&"é` + "\n"
		g := startGateway(t, &fakeProvider{
			completion: &chat.Completion{Content: text, FinishReason: chat.FinishStop},
		}, "m", nil)
		resp := g.post(t, chatBody(false, ""))
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(`<b>&\"é\n`)) {
			t.Fatalf("body = %s", raw)
		}
	})
	t.Run("a refusal is reported as a refusal", func(t *testing.T) {
		g := startGateway(t, &fakeProvider{
			completion: &chat.Completion{Refusal: "no", FinishReason: chat.FinishContentFilter},
		}, "m", nil)
		var got struct {
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
					Refusal *string `json:"refusal"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(g.post(t, chatBody(false, "")).Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		c := got.Choices[0]
		if c.Message.Refusal == nil || *c.Message.Refusal != "no" || c.Message.Content != nil || c.FinishReason != "content_filter" {
			t.Fatalf("choice = %+v", c)
		}
	})
	t.Run("a streamed refusal commits and is forwarded", func(t *testing.T) {
		g := startGateway(t, &fakeProvider{events: []chat.StreamEvent{
			{Refusal: "no"},
			{FinishReason: chat.FinishContentFilter},
		}}, "m", nil)
		s := readStream(t, g.post(t, chatBody(true, "")).Body)
		if !s.done || s.finish != "content_filter" {
			t.Fatalf("stream = %+v", s)
		}
		found := false
		for _, c := range s.chunks {
			for _, ch := range c.Choices {
				if ch.Delta.Refusal != nil && *ch.Delta.Refusal == "no" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("no refusal delta in %+v", s.chunks)
		}
	})
	t.Run("usage reported before the finish reason still comes last", func(t *testing.T) {
		g := startGateway(t, &fakeProvider{events: []chat.StreamEvent{
			{Content: "a", Usage: &chat.Usage{State: chat.UsageReported, PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
			{FinishReason: chat.FinishStop},
		}}, "m", nil)
		s := readStream(t, g.post(t, chatBody(true, `,"stream_options":{"include_usage":true}`)).Body)
		last := s.chunks[len(s.chunks)-1]
		if len(last.Choices) != 0 || len(last.Usage) == 0 || string(last.Usage) == "null" {
			t.Fatalf("last chunk = %+v", last)
		}
	})
	t.Run("usage details are forwarded only when reported", func(t *testing.T) {
		cached, write, reasoning := int64(4), int64(2), int64(1)
		g := startGateway(t, &fakeProvider{completion: &chat.Completion{
			Content:      "a",
			FinishReason: chat.FinishStop,
			Usage: &chat.Usage{
				State: chat.UsageReported, PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7,
				CachedPromptTokens: &cached, CacheWritePromptTokens: &write, ReasoningTokens: &reasoning,
			},
		}}, "m", nil)
		raw, err := io.ReadAll(g.post(t, chatBody(false, "")).Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"cached_tokens":4`, `"cache_write_tokens":2`, `"reasoning_tokens":1`} {
			if !bytes.Contains(raw, []byte(want)) {
				t.Errorf("no %s in %s", want, raw)
			}
		}

		plain := startGateway(t, &fakeProvider{completion: &chat.Completion{
			Content:      "a",
			FinishReason: chat.FinishStop,
			Usage:        &chat.Usage{State: chat.UsageReported, PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		}}, "m", nil)
		raw, err = io.ReadAll(plain.post(t, chatBody(false, "")).Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, unwanted := range []string{"prompt_tokens_details", "completion_tokens_details"} {
			if bytes.Contains(raw, []byte(unwanted)) {
				t.Errorf("%s in %s", unwanted, raw)
			}
		}
	})
	t.Run("malformed usage isn't shown to clients but is recorded", func(t *testing.T) {
		g := startGateway(t, &fakeProvider{completion: &chat.Completion{
			Content:      "a",
			FinishReason: chat.FinishStop,
			Usage:        &chat.Usage{State: chat.UsageMalformed, Raw: json.RawMessage(`{"nonsense":1}`)},
		}}, "m", nil)
		var got map[string]json.RawMessage
		if err := json.NewDecoder(g.post(t, chatBody(false, "")).Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["usage"]; ok {
			t.Fatalf("usage = %s", got["usage"])
		}
		if a := g.recorder.wait(t); a.Usage == nil || a.Usage.State != chat.UsageMalformed {
			t.Fatalf("attempt usage = %+v", a.Usage)
		}
	})
}

// brokenWriter fails every write, the way a connection does once a client has gone.
type brokenWriter struct {
	header http.Header
}

func (w *brokenWriter) Header() http.Header { return w.header }

func (w *brokenWriter) WriteHeader(int) {}

func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// What the upstream sent cost money whether or not the client received it.
func TestAccountingCountsTheFirstEventWhenItCannotBeDelivered(t *testing.T) {
	rec := &attemptRecorder{attempts: make(chan recordedAttempt, 1)}
	fake := &fakeProvider{events: []chat.StreamEvent{{
		Content: "hello",
		Usage:   &chat.Usage{State: chat.UsageReported, PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}}}
	server := New(Options{
		Auth:     auth.NewDevKey(testAPIKey),
		Recorder: rec,
		Limits:   testLimits(),
		Routes: routing.NewTable([]routing.Alias{{
			Name:            "fast",
			MaxOutputTokens: 100,
			Routes:          []routing.Route{{ProviderName: "fake", Provider: fake, Model: "m"}},
		}}),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(true, "")))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	server.ServeHTTP(&brokenWriter{header: http.Header{}}, req)

	a := rec.wait(t)
	if !a.Committed || a.Outcome != accounting.OutcomeClientCanceled {
		t.Fatalf("attempt = %+v", a)
	}
	if a.TextEvents != 1 || a.OutputBytes != 5 {
		t.Fatalf("output = %d events, %d bytes: the undelivered event wasn't counted", a.TextEvents, a.OutputBytes)
	}
	if a.Usage == nil || a.Usage.TotalTokens != 2 {
		t.Fatalf("usage = %+v", a.Usage)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLogsHoldNoSensitiveData(t *testing.T) {
	scenarios := map[string]struct {
		model  string
		stream bool
	}{
		"successful stream":    {"logcheck;tokens=2", true},
		"successful response":  {"logcheck;tokens=2", false},
		"failed before commit": {"logcheck;fail=status;status=500", false},
		"failed after commit":  {"logcheck;fail=disconnect;after=1", true},
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			var logs syncBuffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			rec := &attemptRecorder{inner: accounting.NewLogRecorder(logger), attempts: make(chan recordedAttempt, 4)}
			g := newGateway(t, sc.model, func(o *Options) {
				o.Logger = logger
				o.Recorder = rec
			})
			resp := g.post(t, chatBody(sc.stream, ""))
			_, _ = io.Copy(io.Discard, resp.Body)
			g.recorder.wait(t)

			out := logs.String()
			if !strings.Contains(out, "upstream attempt") {
				t.Fatalf("no attempt was logged: %s", out)
			}
			for _, secret := range []string{testPrompt, testAPIKey, testProviderKey, "token-1", "[logcheck]", "Bearer"} {
				if strings.Contains(out, secret) {
					t.Errorf("logs contain %q:\n%s", secret, out)
				}
			}
		})
	}
}

func TestConcurrentStreamsStayIsolated(t *testing.T) {
	g := newGateway(t, "alpha;tokens=20")
	const clients = 25
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for range clients {
		wg.Go(func() {
			resp, err := g.send(context.Background(), chatBody(true, ""))
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			d := sse.NewDecoder(resp.Body, 1<<16)
			var id string
			count := 0
			for {
				ev, err := d.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					errs <- err
					return
				}
				if string(ev.Data) == "[DONE]" {
					continue
				}
				var c clientChunk
				if err := json.Unmarshal(ev.Data, &c); err != nil {
					errs <- err
					return
				}
				if id == "" {
					id = c.ID
				}
				if c.ID != id {
					errs <- fmt.Errorf("chunk id changed from %s to %s", id, c.ID)
					return
				}
				count++
			}
			if count != 22 {
				errs <- fmt.Errorf("got %d chunks, want 22", count)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for range clients {
		if a := g.recorder.wait(t); a.Outcome != accounting.OutcomeCompleted {
			t.Errorf("attempt = %+v", a)
		}
	}
}
