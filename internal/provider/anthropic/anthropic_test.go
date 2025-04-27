package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/mock"
	"github.com/Bispulm/llmgate/internal/provider"
)

const testKey = "test-provider-key-value"

func newProvider(t *testing.T, baseURL string, opts ...func(*Options)) *Provider {
	t.Helper()
	o := Options{BaseURL: baseURL, APIKey: testKey, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16}
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
	return newProvider(t, ts.URL, opts...), m
}

func request() *chat.Request {
	return &chat.Request{
		Model:           "alias-not-sent-upstream",
		Messages:        []chat.Message{{Role: chat.RoleUser, Content: "one two three"}},
		MaxOutputTokens: 100,
	}
}

func streamRequest() *chat.Request {
	r := request()
	r.Stream = true
	return r
}

func asProviderError(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *provider.Error", err, err)
	}
	return pe
}

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

func lastUsage(events []chat.StreamEvent) *chat.Usage {
	var u *chat.Usage
	for _, ev := range events {
		if ev.Usage != nil {
			u = ev.Usage
		}
	}
	return u
}

func TestRequestTranslation(t *testing.T) {
	var (
		mu     sync.Mutex
		fields map[string]json.RawMessage
		header http.Header
		path   string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		mu.Lock()
		fields, header, path = got, r.Header.Clone(), r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	last := func() (map[string]json.RawMessage, http.Header, string) {
		mu.Lock()
		defer mu.Unlock()
		return fields, header, path
	}

	t.Run("system messages combine in order and everything else maps across", func(t *testing.T) {
		req := &chat.Request{
			Messages: []chat.Message{
				{Role: chat.RoleSystem, Content: "first"},
				{Role: chat.RoleDeveloper, Content: "second"},
				{Role: chat.RoleUser, Content: "<b>&é</b>"},
				{Role: chat.RoleAssistant, Content: "reply"},
				{Role: chat.RoleUser, Content: "again"},
			},
			MaxOutputTokens: 9,
			Stop:            []string{"x", "y"},
		}
		_, _ = newProvider(t, ts.URL).Complete(context.Background(), req, "upstream-model")

		got, h, gotPath := last()
		if gotPath != "/v1/messages" {
			t.Errorf("path = %q", gotPath)
		}
		if h.Get("X-Api-Key") != testKey || h.Get("Anthropic-Version") != "2026-10-21" {
			t.Errorf("headers = %v", h)
		}
		if h.Get("Authorization") != "" {
			t.Error("the key was also sent as a bearer token")
		}
		want := map[string]string{
			"model":          `"upstream-model"`,
			"max_tokens":     `9`,
			"system":         `"first\n\nsecond"`,
			"messages":       `[{"role":"user","content":"<b>&é</b>"},{"role":"assistant","content":"reply"},{"role":"user","content":"again"}]`,
			"stop_sequences": `["x","y"]`,
		}
		if len(got) != len(want) {
			t.Errorf("fields = %v", got)
		}
		for k, v := range want {
			if string(got[k]) != v {
				t.Errorf("%s = %s, want %s", k, got[k], v)
			}
		}
	})
	t.Run("optional fields are left out", func(t *testing.T) {
		_, _ = newProvider(t, ts.URL).Complete(context.Background(), request(), "m")
		got, _, _ := last()
		for _, field := range []string{"system", "stop_sequences", "stream", "temperature", "top_p", "fallbacks", "thinking"} {
			if _, ok := got[field]; ok {
				t.Errorf("%s was sent: %s", field, got[field])
			}
		}
	})
	t.Run("streaming asks for a stream", func(t *testing.T) {
		if st, err := newProvider(t, ts.URL).Stream(context.Background(), streamRequest(), "m"); err == nil {
			_ = st.Close()
		}
		got, h, _ := last()
		if string(got["stream"]) != "true" || h.Get("Accept") != "text/event-stream" {
			t.Fatalf("stream = %s, Accept = %q", got["stream"], h.Get("Accept"))
		}
	})
}

func TestUnsupportedParametersAreRefusedNotDropped(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer ts.Close()
	p := newProvider(t, ts.URL)
	v := 0.5

	for name, mutate := range map[string]func(*chat.Request){
		"temperature": func(r *chat.Request) { r.Temperature = &v },
		"top_p":       func(r *chat.Request) { r.TopP = &v },
		"no limit":    func(r *chat.Request) { r.MaxOutputTokens = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			req := request()
			mutate(req)
			_, err := p.Complete(context.Background(), req, "m")
			pe := asProviderError(t, err)
			if pe.Kind != provider.KindInvalidRequest || pe.Delivery != provider.DeliveryNotSent {
				t.Fatalf("error = %+v", pe)
			}
			_, err = p.Stream(context.Background(), req, "m")
			if got := asProviderError(t, err).Delivery; got != provider.DeliveryNotSent {
				t.Fatalf("stream delivery = %q", got)
			}
		})
	}
	if hits != 0 {
		t.Fatalf("the upstream was called %d times", hits)
	}
	if p.Capabilities().Sampling {
		t.Fatal("the adapter claims to support sampling parameters")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for name, o := range map[string]Options{
		"no key":      {BaseURL: "http://x", MaxResponseBytes: 1, MaxEventBytes: 1},
		"bad URL":     {BaseURL: "::", APIKey: "k", MaxResponseBytes: 1, MaxEventBytes: 1},
		"no limits":   {BaseURL: "http://x", APIKey: "k"},
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
		if got.Content != "[alpha] token-1 token-2" || got.FinishReason != chat.FinishStop {
			t.Fatalf("got %+v", got)
		}
		u := got.Usage
		if u == nil || u.State != chat.UsageReported || u.CompletionTokens != 3 || u.TotalTokens != u.PromptTokens+3 || u.PromptTokens == 0 {
			t.Fatalf("usage = %+v", u)
		}
		if !json.Valid(u.Raw) {
			t.Fatalf("raw usage = %q", u.Raw)
		}
	})
	t.Run("stop reasons", func(t *testing.T) {
		for reason, want := range map[string]chat.FinishReason{
			"end_turn":                      chat.FinishStop,
			"stop_sequence":                 chat.FinishStop,
			"max_tokens":                    chat.FinishLength,
			"model_context_window_exceeded": chat.FinishLength,
			"refusal":                       chat.FinishContentFilter,
		} {
			got, err := p.Complete(ctx, request(), "alpha;tokens=1;finish="+reason)
			if err != nil {
				t.Fatalf("%s: %v", reason, err)
			}
			if got.FinishReason != want {
				t.Errorf("%s: finish = %q, want %q", reason, got.FinishReason, want)
			}
		}
	})
	for _, reason := range []string{"tool_use", "pause_turn", "surprise"} {
		t.Run("stop reason outside the contract: "+reason, func(t *testing.T) {
			_, err := p.Complete(ctx, request(), "alpha;tokens=1;finish="+reason)
			if got := asProviderError(t, err).Kind; got != provider.KindProtocol {
				t.Fatalf("kind = %q", got)
			}
		})
	}
	t.Run("a refusal can have no content and still cost something", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;tokens=0;finish=refusal")
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "" || got.FinishReason != chat.FinishContentFilter {
			t.Fatalf("got %+v", got)
		}
		if got.Usage == nil || got.Usage.PromptTokens == 0 {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
	t.Run("thinking isn't text, and is counted in usage", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;tokens=2;thinking=3")
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "[alpha] token-1" {
			t.Fatalf("content = %q", got.Content)
		}
		if got.Usage.CompletionTokens != 5 || got.Usage.ReasoningTokens == nil || *got.Usage.ReasoningTokens != 3 {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
	t.Run("prompt tokens include cache tokens, to match the other provider", func(t *testing.T) {
		base, err := p.Complete(ctx, request(), "alpha")
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.Complete(ctx, request(), "alpha;cache_read=7;cache_write=2")
		if err != nil {
			t.Fatal(err)
		}
		u := got.Usage
		if u.PromptTokens != base.Usage.PromptTokens+9 || *u.CachedPromptTokens != 7 || *u.CacheWritePromptTokens != 2 {
			t.Fatalf("usage = %+v, base prompt %d", u, base.Usage.PromptTokens)
		}
		if u.TotalTokens != u.PromptTokens+u.CompletionTokens {
			t.Fatalf("total = %d", u.TotalTokens)
		}
	})
	t.Run("absent usage is nil, not zero", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;usage=none")
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage != nil {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
	t.Run("unusable usage is flagged", func(t *testing.T) {
		got, err := p.Complete(ctx, request(), "alpha;usage=inconsistent")
		if err != nil {
			t.Fatal(err)
		}
		if got.Usage == nil || got.Usage.State != chat.UsageMalformed {
			t.Fatalf("usage = %+v", got.Usage)
		}
	})
	t.Run("the output limit is sent", func(t *testing.T) {
		req := request()
		req.MaxOutputTokens = 2
		got, err := p.Complete(ctx, req, "alpha;tokens=9")
		if err != nil {
			t.Fatal(err)
		}
		if got.FinishReason != chat.FinishLength || got.Usage.CompletionTokens != 2 {
			t.Fatalf("got %+v", got)
		}
	})
}

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

func TestCompleteProtocolErrors(t *testing.T) {
	usage := `"usage":{"input_tokens":1,"output_tokens":1}`
	for name, body := range map[string]string{
		"not JSON":             `<html>bad gateway</html>`,
		"invalid UTF-8":        "{\"content\":[{\"type\":\"text\",\"text\":\"\xff\"}],\"stop_reason\":\"end_turn\"}",
		"no stop reason":       `{"content":[{"type":"text","text":"a"}],"stop_reason":null,` + usage + `}`,
		"a tool call":          `{"content":[{"type":"tool_use","id":"1","name":"f","input":{}}],"stop_reason":"end_turn",` + usage + `}`,
		"an unknown block":     `{"content":[{"type":"hologram"}],"stop_reason":"end_turn",` + usage + `}`,
		"a text block no text": `{"content":[{"type":"text"}],"stop_reason":"end_turn",` + usage + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			url := staticServer(t, 200, "application/json", body)
			_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
			pe := asProviderError(t, err)
			if pe.Kind != provider.KindProtocol || pe.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("error = %+v", pe)
			}
		})
	}
	t.Run("redacted thinking is tolerated", func(t *testing.T) {
		url := staticServer(t, 200, "application/json", `{"content":[{"type":"redacted_thinking","data":"x"},{"type":"text","text":"a"}],"stop_reason":"end_turn",`+usage+`}`)
		got, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "a" {
			t.Fatalf("content = %q", got.Content)
		}
	})
	t.Run("a response over the size limit", func(t *testing.T) {
		p, _ := withMock(t, func(o *Options) { o.MaxResponseBytes = 1 << 12 })
		_, err := p.Complete(context.Background(), request(), "alpha;fail=oversized;size=100000")
		if got := asProviderError(t, err).Kind; got != provider.KindProtocol {
			t.Fatalf("kind = %q", got)
		}
	})
}

func TestCompleteClassifiesFailures(t *testing.T) {
	p, _ := withMock(t)
	tests := []struct {
		status int
		kind   provider.Kind
		code   string
	}{
		{400, provider.KindInvalidRequest, "invalid_request_error"},
		{401, provider.KindAuth, "authentication_error"},
		{402, provider.KindBilling, "billing_error"},
		{403, provider.KindAuth, "permission_error"},
		{404, provider.KindNotFound, "not_found_error"},
		{413, provider.KindInvalidRequest, "request_too_large"},
		{429, provider.KindRateLimited, "rate_limit_error"},
		{500, provider.KindServer, "api_error"},
		{504, provider.KindServer, "timeout_error"},
		{529, provider.KindOverloaded, "overloaded_error"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			_, err := p.Complete(context.Background(), request(), fmt.Sprintf("alpha;fail=status;status=%d;retry_after=2", tt.status))
			pe := asProviderError(t, err)
			if pe.Kind != tt.kind || pe.Status != tt.status || pe.UpstreamCode != tt.code || pe.Delivery != provider.DeliveryConfirmed {
				t.Fatalf("error = %+v", pe)
			}
			if pe.RetryAfter != 2*time.Second {
				t.Fatalf("RetryAfter = %v", pe.RetryAfter)
			}
		})
	}
	t.Run("a dropped connection is uncertain", func(t *testing.T) {
		_, err := p.Complete(context.Background(), request(), "alpha;fail=drop_before_headers")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindConnection || pe.Delivery != provider.DeliveryUncertain {
			t.Fatalf("error = %+v", pe)
		}
	})
	t.Run("a provider's message and the key stay out of the error", func(t *testing.T) {
		const message = "organization private details"
		url := staticServer(t, 429, "application/json", `{"type":"error","error":{"type":"rate_limit_error","message":"`+message+`"}}`)
		_, err := newProvider(t, url).Complete(context.Background(), request(), "m")
		for _, secret := range []string{message, testKey} {
			if strings.Contains(fmt.Sprintf("%v %+v", err, err), secret) {
				t.Fatalf("error contains %q: %v", secret, err)
			}
		}
	})
}

func TestStreamAgainstMock(t *testing.T) {
	p, _ := withMock(t)
	ctx := context.Background()

	want, err := p.Complete(ctx, request(), "alpha;tokens=5;text=unicode")
	if err != nil {
		t.Fatal(err)
	}

	for _, framing := range []string{"", ";eol=crlf", ";eol=cr", ";comments=true", ";multiline=true", ";chunk=1", ";chunk=3;eol=crlf;comments=true", ";unknown_events=true", ";thinking=3"} {
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
			for _, ev := range events {
				if finish != "" && ev.Content != "" {
					t.Fatal("text after the finish reason")
				}
				if ev.FinishReason != "" {
					finish = ev.FinishReason
				}
			}
			if finish != chat.FinishStop {
				t.Fatalf("finish = %q", finish)
			}
			u := lastUsage(events)
			if u == nil || u.State != chat.UsageReported || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
				t.Fatalf("usage = %+v", u)
			}
		})
	}

	t.Run("nothing before the first text event is output", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;tokens=1;thinking=3;unknown_events=true")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		for {
			ev, err := st.Next()
			if err != nil {
				t.Fatal(err)
			}
			if ev.HasOutput() {
				if ev.Content == "" {
					t.Fatalf("first output = %+v", ev)
				}
				return
			}
		}
	})
	t.Run("usage is normalized the same way as non-streaming", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;tokens=2;thinking=3;cache_read=7;cache_write=2")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		complete, err := p.Complete(ctx, request(), "alpha;tokens=2;thinking=3;cache_read=7;cache_write=2")
		if err != nil {
			t.Fatal(err)
		}
		u, c := lastUsage(events), complete.Usage
		if u.PromptTokens != c.PromptTokens || u.CompletionTokens != c.CompletionTokens || u.TotalTokens != c.TotalTokens {
			t.Fatalf("streaming %+v, non-streaming %+v", u, c)
		}
		if *u.CachedPromptTokens != 7 || *u.CacheWritePromptTokens != 2 || *u.ReasoningTokens != 3 {
			t.Fatalf("usage = %+v", u)
		}
	})
	t.Run("a stop reason with no content ends cleanly", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;tokens=0;finish=refusal")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		text, events, err := drain(st)
		if !errors.Is(err, io.EOF) || text != "" {
			t.Fatalf("text %q, err %v", text, err)
		}
		var finish chat.FinishReason
		for _, ev := range events {
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		}
		if finish != chat.FinishContentFilter {
			t.Fatalf("finish = %q", finish)
		}
	})
	t.Run("absent usage gives no usage event", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;usage=none")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if u := lastUsage(events); u != nil {
			t.Fatalf("usage = %+v", u)
		}
	})
	t.Run("counts that go backwards are flagged", func(t *testing.T) {
		st, err := p.Stream(ctx, streamRequest(), "alpha;usage=inconsistent;tokens=4")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if u := lastUsage(events); u == nil || u.State != chat.UsageInconsistent {
			t.Fatalf("usage = %+v", u)
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

	tests := []struct {
		name, model string
		wantText    int
		kind        provider.Kind
	}{
		{"clean end right after headers", "alpha;fail=eof_before_first_event", 0, provider.KindInterrupted},
		{"abrupt end right after headers", "alpha;fail=disconnect_before_first_event", 0, provider.KindInterrupted},
		{"disconnect midstream", "alpha;fail=disconnect;after=2;tokens=5", 2, provider.KindInterrupted},
		{"malformed event", "alpha;fail=malformed;after=1", 1, provider.KindProtocol},
		{"oversized event", "alpha;fail=oversized;after=1;size=100000", 1, provider.KindProtocol},
		{"overloaded error event", "alpha;fail=stream_error;after=2", 2, provider.KindOverloaded},
		{"rate limit error event", "alpha;fail=stream_error;after=0;error_type=rate_limit_error", 0, provider.KindRateLimited},
		{"timeout error event", "alpha;fail=stream_error;after=0;error_type=timeout_error", 0, provider.KindTimeout},
		{"billing error event", "alpha;fail=stream_error;after=0;error_type=billing_error", 0, provider.KindBilling},
		{"auth error event", "alpha;fail=stream_error;after=0;error_type=authentication_error", 0, provider.KindAuth},
		{"unknown error event", "alpha;fail=stream_error;after=0;error_type=surprise_error", 0, provider.KindServer},
		{"no message_stop, even after everything else", "alpha;terminator=none;tokens=3", 3, provider.KindInterrupted},
		{"a stop reason outside the contract", "alpha;tokens=1;finish=tool_use", 1, provider.KindProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := p.Stream(context.Background(), streamRequest(), tt.model)
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
			text := 0
			for _, ev := range events {
				if ev.Content != "" {
					text++
				}
			}
			if text != tt.wantText {
				t.Fatalf("got %d text events, want %d", text, tt.wantText)
			}
		})
	}

	t.Run("input usage is kept when output never finished", func(t *testing.T) {
		st, err := p.Stream(context.Background(), streamRequest(), "alpha;fail=disconnect;after=2;tokens=5")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, _ := drain(st)
		u := lastUsage(events)
		if u == nil || u.State != chat.UsagePartial || u.PromptTokens == 0 {
			t.Fatalf("usage = %+v", u)
		}
		if u.Reportable() {
			t.Fatal("partial usage must not be shown to clients")
		}
	})
	t.Run("a status failure before the body", func(t *testing.T) {
		_, err := p.Stream(context.Background(), streamRequest(), "alpha;fail=status;status=529")
		if got := asProviderError(t, err).Kind; got != provider.KindOverloaded {
			t.Fatalf("kind = %q", got)
		}
	})
	t.Run("a 200 that isn't an event stream", func(t *testing.T) {
		url := staticServer(t, 200, "text/html", "<html></html>")
		_, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		if got := asProviderError(t, err).Kind; got != provider.KindProtocol {
			t.Fatalf("kind = %q", got)
		}
	})
}

func sseBody(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", e)
	}
	return b.String()
}

func TestStreamContractViolations(t *testing.T) {
	const (
		text = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`
		stop = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`
		end  = `{"type":"message_stop"}`
	)
	for name, body := range map[string]string{
		"not JSON":              sseBody("nonsense"),
		"invalid UTF-8":         sseBody(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + "\xff" + `"}}`),
		"a tool block":          sseBody(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use"}}`),
		"an unknown block":      sseBody(`{"type":"content_block_start","index":0,"content_block":{"type":"hologram"}}`),
		"a tool input delta":    sseBody(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`),
		"an unknown delta":      sseBody(`{"type":"content_block_delta","index":0,"delta":{"type":"hologram_delta"}}`),
		"a stop with no reason": sseBody(text, end),
		"text after the stop":   sseBody(stop, text, end),
		"two stop reasons":      sseBody(stop, stop, end),
	} {
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
	t.Run("several message_delta events are allowed", func(t *testing.T) {
		url := staticServer(t, 200, "text/event-stream", sseBody(
			`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
			text,
			`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":1}}`,
			stop,
			`{"type":"message_delta","delta":{},"usage":{"output_tokens":2}}`,
			end))
		st, err := newProvider(t, url).Stream(context.Background(), streamRequest(), "m")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		_, events, err := drain(st)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if u := lastUsage(events); u == nil || u.CompletionTokens != 2 || u.PromptTokens != 3 || u.State != chat.UsageReported {
			t.Fatalf("usage = %+v", u)
		}
	})
}

func TestCancellation(t *testing.T) {
	t.Run("canceling reaches the upstream, and a partly read stream may still be billed", func(t *testing.T) {
		p, m := withMock(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st, err := p.Stream(ctx, streamRequest(), "cancel-me;tokens=1000;interval=20ms")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		for {
			ev, err := st.Next()
			if err != nil {
				t.Fatal(err)
			}
			if ev.Content != "" {
				break
			}
		}
		cancel()
		_, _, err = drain(st)
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindCanceled || pe.Delivery != provider.DeliveryConfirmed {
			t.Fatalf("error = %+v", pe)
		}
		deadline := time.Now().Add(5 * time.Second)
		for m.Stats()["cancel-me"].Canceled != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("upstream never saw the cancellation: %+v", m.Stats()["cancel-me"])
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("a deadline before the response", func(t *testing.T) {
		p, _ := withMock(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := p.Complete(ctx, request(), "alpha;latency=2s")
		pe := asProviderError(t, err)
		if pe.Kind != provider.KindTimeout || pe.Delivery != provider.DeliveryUncertain {
			t.Fatalf("error = %+v", pe)
		}
	})
}
