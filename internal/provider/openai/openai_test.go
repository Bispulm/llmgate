package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/provider"
)

const testKey = "test-provider-key-value"

func newProvider(t *testing.T, baseURL string, opts ...func(*Options)) *Provider {
	t.Helper()
	o := Options{
		BaseURL:          baseURL,
		APIKey:           testKey,
		MaxResponseBytes: 1 << 20,
		MaxEventBytes:    1 << 16,
	}
	for _, f := range opts {
		f(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.CloseIdleConnections)
	return p
}

func withMock(t *testing.T, opts ...func(*Options)) (*Provider, *mock.Server) {
	t.Helper()
	m := mock.New()
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	return newProvider(t, ts.URL+"/v1", opts...), m
}

func request() *chat.Request {
	return &chat.Request{
		Model:           "alias-not-sent-upstream",
		Messages:        []chat.Message{{Role: chat.RoleUser, Content: "one two three"}},
		MaxOutputTokens: 100,
	}
}

func asProviderError(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *provider.Error", err, err)
	}
	return pe
}

func TestRequestTranslation(t *testing.T) {
	var (
		mu       sync.Mutex
		captured map[string]json.RawMessage
		header   http.Header
		path     string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		mu.Lock()
		captured, header, path = fields, r.Header.Clone(), r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	lastFields := func() map[string]json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
	lastHeader := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return header
	}

	temp, topP := 0.0, 0.5
	req := &chat.Request{
		Model: "alias",
		Messages: []chat.Message{
			{Role: chat.RoleSystem, Content: "s"},
			{Role: chat.RoleDeveloper, Content: "d"},
			{Role: chat.RoleUser, Content: "<b>&é</b>"},
		},
		MaxOutputTokens: 9,
		Temperature:     &temp,
		TopP:            &topP,
		Stop:            []string{"x", "y"},
	}

	t.Run("non-streaming", func(t *testing.T) {
		_, _ = newProvider(t, ts.URL+"/v1").Complete(context.Background(), req, "upstream-model")

		mu.Lock()
		gotPath := path
		mu.Unlock()
		if gotPath != "/v1/chat/completions" {
			t.Errorf("path = %q", gotPath)
		}
		if got := lastHeader().Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		fields := lastFields()
		want := map[string]string{
			"model":                 `"upstream-model"`,
			"messages":              `[{"role":"system","content":"s"},{"role":"developer","content":"d"},{"role":"user","content":"<b>&é</b>"}]`,
			"max_completion_tokens": `9`,
			"temperature":           `0`,
			"top_p":                 `0.5`,
			"stop":                  `["x","y"]`,
		}
		if len(fields) != len(want) {
			t.Errorf("fields = %v", fields)
		}
		for k, v := range want {
			if string(fields[k]) != v {
				t.Errorf("%s = %s, want %s", k, fields[k], v)
			}
		}
	})

	t.Run("streaming always asks for usage, whatever the client asked", func(t *testing.T) {
		s := *req
		s.Stream, s.IncludeUsage = true, false
		if st, err := newProvider(t, ts.URL+"/v1").Stream(context.Background(), &s, "m"); err == nil {
			_ = st.Close()
		}
		fields := lastFields()
		if string(fields["stream"]) != "true" {
			t.Errorf("stream = %s", fields["stream"])
		}
		if string(fields["stream_options"]) != `{"include_usage":true}` {
			t.Errorf("stream_options = %s", fields["stream_options"])
		}
		if got := lastHeader().Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
	})

	t.Run("optional fields are left out when unset", func(t *testing.T) {
		_, _ = newProvider(t, ts.URL+"/v1").Complete(context.Background(), request(), "m")
		fields := lastFields()
		for _, field := range []string{"temperature", "top_p", "stop", "stream", "stream_options"} {
			if _, ok := fields[field]; ok {
				t.Errorf("%s was sent: %s", field, fields[field])
			}
		}
	})

	t.Run("the limit can use the older parameter name", func(t *testing.T) {
		p := newProvider(t, ts.URL+"/v1", func(o *Options) { o.UseMaxTokens = true })
		_, _ = p.Complete(context.Background(), request(), "m")
		fields := lastFields()
		if string(fields["max_tokens"]) != "100" {
			t.Errorf("max_tokens = %s", fields["max_tokens"])
		}
		if _, ok := fields["max_completion_tokens"]; ok {
			t.Error("max_completion_tokens was also sent")
		}
	})
}

func TestNewRejectsBadOptions(t *testing.T) {
	for name, o := range map[string]Options{
		"no key":      {BaseURL: "http://x/v1", MaxResponseBytes: 1, MaxEventBytes: 1},
		"bad URL":     {BaseURL: "::", APIKey: "k", MaxResponseBytes: 1, MaxEventBytes: 1},
		"no limits":   {BaseURL: "http://x/v1", APIKey: "k"},
		"no base URL": {APIKey: "k", MaxResponseBytes: 1, MaxEventBytes: 1},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New accepted the options", name)
		}
	}
}

func TestCompleteAgainstMock(t *testing.T) {
	p, _ := withMock(t)
	ctx := context.Background()

	t.Run("text, finish reason, and usage", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;tokens=3")
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "[alpha] token-1 token-2" {
			t.Errorf("content = %q", got.Content)
		}
		if got.FinishReason != chat.FinishStop {
			t.Errorf("finish = %q", got.FinishReason)
		}
		if got.Usage == nil || got.Usage.State != chat.UsageReported {
			t.Fatalf("usage = %+v", got.Usage)
		}
		if got.Usage.CompletionTokens != 3 || got.Usage.TotalTokens != got.Usage.PromptTokens+3 {
			t.Errorf("usage = %+v", got.Usage)
		}
		if !json.Valid(got.Usage.Raw) {
			t.Errorf("raw usage = %q", got.Usage.Raw)
		}
	})
	t.Run("the output limit gives finish reason length", func(t *testing.T) {
		req := request()
		req.MaxOutputTokens = 2
		got, err := p.Complete(ctx, req, "alpha;tokens=9")
		if err != nil {
			t.Fatal(err)
		}
		if got.FinishReason != chat.FinishLength || got.Usage.CompletionTokens != 2 {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("absent usage is nil, not zero", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;usage=none")
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage != nil {
			t.Fatalf("usage = %+v, want nil", got.Usage)
		}
	})
	t.Run("inconsistent usage is kept and flagged", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;usage=inconsistent")
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage == nil || got.Usage.State != chat.UsageInconsistent {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
}

// staticServer replies with a fixed status, header, and body.
func staticServer(t *testing.T, status int, contentType, body string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func completion(choice, usage string) string {
	body := `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[` + choice + `]`
	if usage != "" {
		body += `,"usage":` + usage
	}
	return body + `}`
}

func TestCompleteParsesResponses(t *testing.T) {
	stop := `{"index":0,"message":{"role":"assistant","content":"hi","refusal":null},"logprobs":null,"finish_reason":"stop"}`
	goodUsage := `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`

	t.Run("usage details", func(t *testing.T) {
		url := staticServer(t, 200, "application/json", completion(stop, `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,
			"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":3}}`))
		got, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if err != nil {
			t.Fatal(err)
		}
		u := got.Usage
		if *u.CachedPromptTokens != 4 || *u.CacheWritePromptTokens != 2 || *u.ReasoningTokens != 3 {
			t.Fatalf("usage = %+v", u)
		}
	})
	t.Run("zero is a reported count, not absent", func(t *testing.T) {
		url := staticServer(t, 200, "application/json", completion(stop, `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`))
		got, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage == nil || got.Usage.State != chat.UsageReported {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
	t.Run("a refusal has no content", func(t *testing.T) {
		url := staticServer(t, 200, "application/json", completion(
			`{"index":0,"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"content_filter"}`, goodUsage))
		got, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if err != nil {
			t.Fatal(err)
		}
		if got.Refusal != "no" || got.FinishReason != chat.FinishContentFilter {
			t.Fatalf("got %+v", got)
		}
	})

	for name, usage := range map[string]string{
		"missing a required count":   `{"prompt_tokens":1,"total_tokens":3}`,
		"negative count":             `{"prompt_tokens":-1,"completion_tokens":2,"total_tokens":1}`,
		"counts of the wrong type":   `{"prompt_tokens":"1","completion_tokens":2,"total_tokens":3}`,
		"usage of the wrong type":    `5`,
		"counts that would overflow": `{"prompt_tokens":9223372036854775807,"completion_tokens":1,"total_tokens":1}`,
	} {
		t.Run("malformed usage is flagged, not trusted: "+name, func(t *testing.T) {
			url := staticServer(t, 200, "application/json", completion(stop, usage))
			got, err := newProvider(t, url).Complete(context.Background(), request(), "m")
			if err != nil {
				t.Fatal(err)
			}
			if got.Usage == nil || got.Usage.State != chat.UsageMalformed {
				t.Fatalf("usage = %+v", got.Usage)
			}
		})
	}

	protocolErrors := map[string]string{
		"not JSON":                    `<html>bad gateway</html>`,
		"invalid UTF-8":               "{\"choices\":[{\"index\":0,\"message\":{\"content\":\"\xff\"},\"finish_reason\":\"stop\"}]}",
		"no choices":                  completion("", goodUsage),
		"two choices":                 completion(stop+","+stop, goodUsage),
		"wrong index":                 completion(`{"index":1,"message":{"content":"x"},"finish_reason":"stop"}`, goodUsage),
		"no finish reason":            completion(`{"index":0,"message":{"content":"x"},"finish_reason":null}`, goodUsage),
		"unknown finish":              completion(`{"index":0,"message":{"content":"x"},"finish_reason":"surprise"}`, goodUsage),
		"tool_calls finish":           completion(`{"index":0,"message":{"content":"x"},"finish_reason":"tool_calls"}`, goodUsage),
		"tool calls in reply":         completion(`{"index":0,"message":{"content":null,"tool_calls":[{"id":"1"}]},"finish_reason":"stop"}`, goodUsage),
		"content and refusal missing": completion(`{"index":0,"message":{"content":null},"finish_reason":"stop"}`, goodUsage),
	}
	for name, body := range protocolErrors {
		t.Run("protocol error: "+name, func(t *testing.T) {
			url := staticServer(t, 200, "application/json", body)
			_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
			pe := asProviderError(t, err)
			if pe.Kind != provider.KindProtocol || pe.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("error = %+v", pe)
			}
		})
	}
}

func TestCompleteClassifiesStatuses(t *testing.T) {
	p, _ := withMock(t)
	tests := []struct {
		status int
		kind   provider.Kind
	}{
		{400, provider.KindInvalidRequest},
		{401, provider.KindAuth},
		{403, provider.KindAuth},
		{404, provider.KindNotFound},
		{408, provider.KindTimeout},
		{413, provider.KindInvalidRequest},
		{422, provider.KindInvalidRequest},
		{429, provider.KindRateLimited},
		{500, provider.KindServer},
		{502, provider.KindServer},
		{503, provider.KindOverloaded},
		{504, provider.KindServer},
		{529, provider.KindOverloaded},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			_, err := p.Complete(context.Background(), request(), fmt.Sprintf("alpha;fail=status;status=%d", tt.status))
			pe := asProviderError(t, err)
			if pe.Kind != tt.kind || pe.Status != tt.status || pe.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("error = %+v, want kind %q", pe, tt.kind)
			}
		})
	}
}

func TestRetryHints(t *testing.T) {
	p, _ := withMock(t)

	t.Run("seconds", func(t *testing.T) {
		_, err := p.Complete(context.Background(), request(), "alpha;fail=status;status=429;retry_after=2")
		if got := asProviderError(t, err).RetryAfter; got != 2*time.Second {
			t.Fatalf("RetryAfter = %v", got)
		}
	})
	t.Run("no hint", func(t *testing.T) {
		_, err := p.Complete(context.Background(), request(), "alpha;fail=status;status=429")
		if got := asProviderError(t, err).RetryAfter; got != 0 {
			t.Fatalf("RetryAfter = %v", got)
		}
	})
	t.Run("an unusable hint is ignored", func(t *testing.T) {
		_, err := p.Complete(context.Background(), request(), "alpha;fail=status;status=429;retry_after=soon")
		if got := asProviderError(t, err).RetryAfter; got != 0 {
			t.Fatalf("RetryAfter = %v", got)
		}
	})
	t.Run("an HTTP date", func(t *testing.T) {
		when := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", when)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer ts.Close()
		_, err := newProvider(t, ts.URL).Complete(context.Background(), request(), "m")
		if got := asProviderError(t, err).RetryAfter; got < 60*time.Second || got > 90*time.Second {
			t.Fatalf("RetryAfter = %v", got)
		}
	})
	t.Run("a date in the past", func(t *testing.T) {
		when := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", when)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer ts.Close()
		_, err := newProvider(t, ts.URL).Complete(context.Background(), request(), "m")
		if got := asProviderError(t, err).RetryAfter; got != 0 {
			t.Fatalf("RetryAfter = %v", got)
		}
	})
}

func TestUpstreamErrorCodeHandling(t *testing.T) {
	t.Run("a short code is kept", func(t *testing.T) {
		url := staticServer(t, 400, "application/json", `{"error":{"message":"details","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`)
		_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if got := asProviderError(t, err).UpstreamCode; got != "context_length_exceeded" {
			t.Fatalf("code = %q", got)
		}
	})
	t.Run("the type is used when there is no code", func(t *testing.T) {
		url := staticServer(t, 400, "application/json", `{"error":{"message":"details","type":"invalid_request_error","param":null,"code":null}}`)
		_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if got := asProviderError(t, err).UpstreamCode; got != "invalid_request_error" {
			t.Fatalf("code = %q", got)
		}
	})
	t.Run("a code that doesn't look like an identifier is dropped", func(t *testing.T) {
		url := staticServer(t, 400, "application/json", `{"error":{"message":"m","type":"t","code":"account 12345 over limit"}}`)
		_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if got := asProviderError(t, err).UpstreamCode; got != "t" {
			t.Fatalf("code = %q", got)
		}
	})
	t.Run("a non-JSON error body still classifies", func(t *testing.T) {
		url := staticServer(t, 502, "text/html", `<html>bad gateway</html>`)
		_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindServer || pe.UpstreamCode != "" {
			t.Fatalf("error = %+v", pe)
		}
	})
}

// Provider messages can hold account details, and the key must never appear in
// an error that may be logged.
func TestErrorsDoNotCarrySensitiveText(t *testing.T) {
	const upstreamMessage = "organization org-private-details exceeded quota"
	url := staticServer(t, 429, "application/json", `{"error":{"message":"`+upstreamMessage+`","type":"t","code":"c"}}`)
	_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
	if err == nil {
		t.Fatal("no error")
	}
	for _, secret := range []string{upstreamMessage, testKey} {
		if strings.Contains(fmt.Sprintf("%v %+v", err, err), secret) {
			t.Fatalf("error text contains %q: %v", secret, err)
		}
	}
}

func TestDeliveryReflectsWhatTheUpstreamMayHaveSeen(t *testing.T) {
	t.Run("connection refused means nothing was sent", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()

		_, err = newProvider(t, "http://"+addr+"/v1").Complete(context.Background(), request(), "m")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindConnection || pe.Delivery != provider.DeliveryNotSent {
			t.Fatalf("error = %+v", pe)
		}
	})
	t.Run("a connection dropped after sending is uncertain", func(t *testing.T) {
		p, _ := withMock(t)
		_, err := p.Complete(context.Background(), request(), "alpha;fail=drop_before_headers")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindConnection || pe.Delivery != provider.DeliveryUncertain {
			t.Fatalf("error = %+v", pe)
		}
	})
	t.Run("a body cut off midway is confirmed and interrupted", func(t *testing.T) {
		p, _ := withMock(t)
		_, err := p.Complete(context.Background(), request(), "alpha;fail=disconnect")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindInterrupted || pe.Delivery != provider.DeliveryConfirmed {
			t.Fatalf("error = %+v", pe)
		}
	})
}

func TestResponseSizeLimit(t *testing.T) {
	p, _ := withMock(t, func(o *Options) { o.MaxResponseBytes = 1 << 12 })
	_, err := p.Complete(context.Background(), request(), "alpha;fail=oversized;size=100000")
	pe := asProviderError(t, err)
	if pe.Kind != provider.KindProtocol {
		t.Fatalf("error = %+v", pe)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := newProvider(t, origin.URL).Complete(context.Background(), request(), "m")
	pe := asProviderError(t, err)
	if pe.Kind != provider.KindProtocol {
		t.Fatalf("error = %+v", pe)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed, sending the credential to another host")
	}
}

// drain reads a stream to its end, returning its joined output and final error.
func drain(st provider.Stream) (text string, events []chat.StreamEvent, err error) {
	for {
		ev, err := st.Next()
		if err != nil {
			return text, events, err
		}
		text += ev.Content
		events = append(events, ev)
	}
}

func streamRequest() *chat.Request {
	r := request()
	r.Stream = true
	return r
}

func TestStreamAgainstMock(t *testing.T) {
	p, _ := withMock(t)
	ctx := context.Background()

	want, err := p.Complete(ctx, request(), "alpha;tokens=5;text=unicode")
	if err != nil {
		t.Fatal(err)
	}

	framings := []string{"", ";eol=crlf", ";eol=cr", ";comments=true", ";multiline=true", ";chunk=1", ";chunk=3;eol=crlf;comments=true;multiline=true"}
	for _, framing := range framings {
		t.Run("framing"+framing, func(t *testing.T) {
			st, err := p.Stream(ctx, streamRequest(), "alpha;tokens=5;text=unicode"+framing)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			text, events, err := drain(st)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream end: %v", err)
			}
			if text != want.Content {
				t.Fatalf("text = %q, want %q", text, want.Content)
			}

			var finish chat.FinishReason
			var usage *chat.Usage
			for _, ev := range events {
				if finish != "" && ev.Content != "" {
					t.Fatal("text after the finish reason")
				}
				if ev.FinishReason != "" {
					finish = ev.FinishReason
				}
				if ev.Usage != nil {
					usage = ev.Usage
				}
			}
			if finish != chat.FinishStop {
				t.Fatalf("finish = %q", finish)
			}
			if usage == nil || usage.CompletionTokens != 5 || usage.State != chat.UsageReported {
				t.Fatalf("usage = %+v", usage)
			}
		})
	}

	t.Run("role-only chunks produce no events", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;tokens=1")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		ev, err := st.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Content == "" {
			t.Fatalf("first event has no content: %+v", ev)
		}
	})
	t.Run("absent usage ends cleanly with no usage event", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;usage=none")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		for _, ev := range events {
			if ev.Usage != nil {
				t.Fatalf("usage = %+v", ev.Usage)
			}
		}
	})
	t.Run("Next keeps returning EOF", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if _, _, err := drain(st); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if _, err := st.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("second end = %v", err)
		}
	})
}

func TestStreamFailures(t *testing.T) {
	p, _ := withMock(t, func(o *Options) { o.MaxEventBytes = 1 << 14 })
	ctx := context.Background()

	tests := []struct {
		name, model string
		wantEvents  int // text events delivered before the failure
		kind        provider.Kind
	}{
		{"clean end before the first event", "alpha;fail=eof_before_first_event", 0, provider.KindInterrupted},
		{"abrupt end before the first event", "alpha;fail=disconnect_before_first_event", 0, provider.KindInterrupted},
		{"disconnect midstream", "alpha;fail=disconnect;after=2;tokens=5", 2, provider.KindInterrupted},
		{"disconnect after all text", "alpha;fail=disconnect;after=5;tokens=5", 5, provider.KindInterrupted},
		{"malformed event", "alpha;fail=malformed;after=1", 1, provider.KindProtocol},
		{"oversized event", "alpha;fail=oversized;after=1;size=100000", 1, provider.KindProtocol},
		{"in-stream error", "alpha;fail=stream_error;after=2", 2, provider.KindServer},
		{"no terminator, even after all text and usage", "alpha;terminator=none;tokens=3", 3, provider.KindInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := p.Stream(ctx, streamRequest(), tt.model)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()

			_, events, err := drain(st)
			if errors.Is(err, io.EOF) {
				t.Fatal("stream ended cleanly")
			}
			pe := asProviderError(t, err)
			if pe.Kind != tt.kind || pe.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("error = %+v, want kind %q", pe, tt.kind)
			}
			textEvents := 0
			for _, ev := range events {
				if ev.Content != "" {
					textEvents++
				}
			}
			if textEvents != tt.wantEvents {
				t.Fatalf("delivered %d text events, want %d", textEvents, tt.wantEvents)
			}
		})
	}
}

func TestStreamRejectsFailuresBeforeTheBody(t *testing.T) {
	t.Run("a status error", func(t *testing.T) {
		p, _ := withMock(t)
		_, err := p.Stream(context.Background(), streamRequest(), "alpha;fail=status;status=503")
		if got := asProviderError(t, err).Kind; got != provider.KindOverloaded {
			t.Fatalf("kind = %q", got)
		}
	})
	t.Run("a 200 that isn't an event stream", func(t *testing.T) {
		url := staticServer(t, 200, "text/html", "<html>a proxy's login page</html>")
		_, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindProtocol || pe.Delivery != provider.DeliveryConfirmed {
			t.Fatalf("error = %+v", pe)
		}
	})
	t.Run("an event stream with parameters is accepted", func(t *testing.T) {
		url := staticServer(t, 200, "text/event-stream; charset=utf-8", "data: [DONE]\n\n")
		st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
	})
}

func sseBody(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&b, "data: %s\n\n", c)
	}
	return b.String()
}

func textChunk(s string) string {
	return `{"choices":[{"index":0,"delta":{"content":"` + s + `"},"finish_reason":null}]}`
}

const stopChunk = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

func TestStreamRejectsContractViolations(t *testing.T) {
	tests := map[string]string{
		"not JSON":            sseBody("nonsense"),
		"invalid UTF-8":       sseBody(`{"choices":[{"index":0,"delta":{"content":"` + "\xff" + `"}}]}`),
		"two choices":         sseBody(`{"choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}`),
		"wrong choice index":  sseBody(`{"choices":[{"index":3,"delta":{"content":"a"}}]}`),
		"tool call delta":     sseBody(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]}}]}`),
		"function call delta": sseBody(`{"choices":[{"index":0,"delta":{"function_call":{"name":"f"}}}]}`),
		"unknown finish":      sseBody(`{"choices":[{"index":0,"delta":{},"finish_reason":"surprise"}]}`),
		"tool_calls finish":   sseBody(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		"done with no finish": sseBody(textChunk("a"), "[DONE]"),
		"text after finish":   sseBody(stopChunk, textChunk("late"), "[DONE]"),
		"two finish reasons":  sseBody(stopChunk, stopChunk, "[DONE]"),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			url := staticServer(t, 200, "text/event-stream", body)
			st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			_, _, err = drain(st)
			if errors.Is(err, io.EOF) {
				t.Fatal("stream ended cleanly")
			}
			if got := asProviderError(t, err).Kind; got != provider.KindProtocol {
				t.Fatalf("kind = %q", got)
			}
		})
	}
}

func TestStreamAcceptsOptionalVariations(t *testing.T) {
	tests := map[string]string{
		"usage on the finish chunk": sseBody(
			`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			"[DONE]"),
		"unknown fields are ignored": sseBody(
			`{"obfuscation":"abc","new_field":{"x":1},"choices":[{"index":0,"delta":{"content":"a","obfuscation":"z"},"finish_reason":"stop"}],"usage":null}`,
			"[DONE]"),
		"empty and null deltas": sseBody(
			`{"choices":[]}`,
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null}}]}`,
			`{"choices":[{"index":0,"delta":{"content":null}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"a","tool_calls":null}}]}`,
			stopChunk, "[DONE]"),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			url := staticServer(t, 200, "text/event-stream", body)
			st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			text, _, err := drain(st)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("stream end: %v", err)
			}
			if text != "a" {
				t.Fatalf("text = %q", text)
			}
		})
	}

	t.Run("a refusal is delivered as a refusal", func(t *testing.T) {
		url := staticServer(t, 200, "text/event-stream", sseBody(
			`{"choices":[{"index":0,"delta":{"refusal":"no"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`,
			"[DONE]"))
		st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if events[0].Refusal != "no" || events[1].FinishReason != chat.FinishContentFilter {
			t.Fatalf("events = %+v", events)
		}
	})
}

func TestInStreamErrorClassification(t *testing.T) {
	tests := []struct {
		body string
		kind provider.Kind
		code string
	}{
		{`{"error":{"message":"m","type":"rate_limit_error","code":"rate_limit_exceeded"}}`, provider.KindRateLimited, "rate_limit_exceeded"},
		{`{"error":{"message":"m","type":"server_error","code":null}}`, provider.KindServer, "server_error"},
		{`{"error":"just a string"}`, provider.KindServer, ""},
	}
	for _, tt := range tests {
		url := staticServer(t, 200, "text/event-stream", sseBody(tt.body))
		st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = drain(st)
		_ = st.Close()
		pe := asProviderError(t, err)
		if pe.Kind != tt.kind || pe.UpstreamCode != tt.code {
			t.Errorf("body %s: error = %+v", tt.body, pe)
		}
	}
}

func TestCancellation(t *testing.T) {
	t.Run("canceling the context ends a blocked read and reaches the upstream", func(t *testing.T) {
		p, m := withMock(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		st, err := p.Stream(ctx, streamRequest(), "cancel-me;tokens=1000;interval=20ms")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		if _, err := st.Next(); err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		cancel()
		_, _, err = drain(st)
		if got := time.Since(start); got > 2*time.Second {
			t.Fatalf("cancellation took %v", got)
		}
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindCanceled {
			t.Fatalf("error = %+v", pe)
		}
		if pe.Delivery != provider.DeliveryConfirmed {
			t.Fatalf("delivery = %q: a canceled stream may still be billed", pe.Delivery)
		}

		deadline := time.Now().Add(5 * time.Second)
		for m.Stats()["cancel-me"].Canceled != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("upstream never saw the cancellation: %+v", m.Stats()["cancel-me"])
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("a deadline before the response is a timeout", func(t *testing.T) {
		p, _ := withMock(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := p.Complete(ctx, request(), "alpha;latency=2s")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindTimeout || pe.Delivery != provider.DeliveryUncertain {
			t.Fatalf("error = %+v", pe)
		}
	})
	t.Run("a deadline during a stream is a timeout", func(t *testing.T) {
		p, _ := withMock(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		st, err := p.Stream(ctx, streamRequest(), "alpha;ttfb=2s")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, err = st.Next()
		if got := asProviderError(t, err).Kind; got != provider.KindTimeout {
			t.Fatalf("kind = %q", got)
		}
	})
}

// Without draining the end of a finished stream, HTTP/1.1 can't reuse the
// connection, and every stream would pay for a new one.
func TestFinishedStreamsReuseConnections(t *testing.T) {
	var connections atomic.Int64
	ts := httptest.NewUnstartedServer(mock.New())
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	ts.Start()
	defer ts.Close()

	p := newProvider(t, ts.URL+"/v1")
	for range 5 {
		st, err := p.Stream(context.Background(), streamRequest(), "alpha;tokens=3")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := drain(st); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		_ = st.Close()
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("5 sequential streams used %d connections", got)
	}
}
