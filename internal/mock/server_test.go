package mock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/llmgate/internal/sse"
)

const (
	openAIPath    = "/v1/chat/completions"
	anthropicPath = "/v1/messages"
	apiVersion    = "2026-10-21"
)

type harness struct {
	server *Server
	url    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := New()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return &harness{server: s, url: ts.URL}
}

func (h *harness) post(t *testing.T, path, body string, header map[string]string) *http.Response {
	t.Helper()
	resp, err := h.send(context.Background(), path, body, header)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) send(ctx context.Context, path, body string, header map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if header == nil {
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("X-Api-Key", "test")
		req.Header.Set("Anthropic-Version", apiVersion)
	}
	return http.DefaultClient.Do(req)
}

func openAIBody(model string, stream bool, extra string) string {
	body := `{"model":` + quote(model) + `,"messages":[{"role":"user","content":"one two three"}]`
	if stream {
		body += `,"stream":true`
	}
	return body + extra + `}`
}

func anthropicBody(model string, stream bool, extra string) string {
	body := `{"model":` + quote(model) + `,"max_tokens":100,"messages":[{"role":"user","content":"one two three"}]`
	if stream {
		body += `,"stream":true`
	}
	return body + extra + `}`
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type sseEvent struct {
	name, data string
}

// readEvents drains a response body, returning events and how the stream ended.
func readEvents(t *testing.T, r io.Reader) ([]sseEvent, error) {
	t.Helper()
	d := sse.NewDecoder(r, 1<<24)
	var out []sseEvent
	for {
		ev, err := d.Next()
		if err != nil {
			return out, err
		}
		out = append(out, sseEvent{string(ev.Name), string(ev.Data)})
	}
}

type openAIChunk struct {
	Object  string          `json:"object"`
	Model   string          `json:"model"`
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// collectOpenAI joins the text of a stream and reports whether it ended in [DONE].
func collectOpenAI(t *testing.T, events []sseEvent) (text string, chunks []openAIChunk, done bool) {
	t.Helper()
	for i, ev := range events {
		if ev.data == "[DONE]" {
			if i != len(events)-1 {
				t.Fatalf("[DONE] at %d of %d events", i, len(events))
			}
			return text, chunks, true
		}
		var c openAIChunk
		if err := json.Unmarshal([]byte(ev.data), &c); err != nil {
			t.Fatalf("event %d is not JSON: %v: %q", i, err, ev.data)
		}
		chunks = append(chunks, c)
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
	}
	return text, chunks, false
}

func TestOpenAINonStreaming(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, openAIPath, openAIBody("alpha;tokens=3", false, ""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
			Total      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || len(got.Choices) != 1 {
		t.Fatalf("response = %+v", got)
	}
	if got.Choices[0].Message.Role != "assistant" || got.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", got.Choices[0])
	}
	if !strings.Contains(got.Choices[0].Message.Content, "[alpha]") {
		t.Fatalf("content %q does not identify the model", got.Choices[0].Message.Content)
	}
	if *got.Usage.Completion != 3 || *got.Usage.Total != *got.Usage.Prompt+*got.Usage.Completion {
		t.Fatalf("usage = %d %d %d", *got.Usage.Prompt, *got.Usage.Completion, *got.Usage.Total)
	}
}

func TestResponsesAreDeterministicAndDistinctPerModel(t *testing.T) {
	h := newHarness(t)
	read := func(model string) string {
		resp := h.post(t, openAIPath, openAIBody(model, false, ""), nil)
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if a, b := read("alpha"), read("alpha"); a != b {
		t.Fatalf("not deterministic:\n%s\n%s", a, b)
	}
	if a, b := read("alpha"), read("beta"); a == b {
		t.Fatalf("models are indistinguishable: %s", a)
	}
}

func TestOutputLimitIsHonored(t *testing.T) {
	h := newHarness(t)
	t.Run("openai", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;tokens=9", true, `,"max_completion_tokens":2,"stream_options":{"include_usage":true}`), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		_, chunks, _ := collectOpenAI(t, events)
		var finish string
		for _, c := range chunks {
			for _, ch := range c.Choices {
				if ch.FinishReason != nil {
					finish = *ch.FinishReason
				}
			}
		}
		if finish != "length" {
			t.Fatalf("finish_reason = %q", finish)
		}
		var usage struct {
			Completion int64 `json:"completion_tokens"`
		}
		if err := json.Unmarshal(chunks[len(chunks)-1].Usage, &usage); err != nil {
			t.Fatal(err)
		}
		if usage.Completion != 2 {
			t.Fatalf("completion_tokens = %d", usage.Completion)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		resp := h.post(t, anthropicPath, `{"model":"alpha;tokens=9","max_tokens":2,"messages":[{"role":"user","content":"x"}]}`, nil)
		var got struct {
			StopReason string `json:"stop_reason"`
			Usage      struct {
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.StopReason != "max_tokens" || got.Usage.Output != 2 {
			t.Fatalf("response = %+v", got)
		}
	})
}

func TestOpenAIStreaming(t *testing.T) {
	h := newHarness(t)

	t.Run("usage only when requested", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;tokens=3", true, ""), nil)
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("content type = %q", ct)
		}
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		text, chunks, done := collectOpenAI(t, events)
		if !done {
			t.Fatal("missing [DONE]")
		}
		if !strings.HasPrefix(text, "[alpha]") {
			t.Fatalf("text = %q", text)
		}
		if chunks[0].Choices[0].Delta.Role != "assistant" {
			t.Fatalf("first chunk = %+v", chunks[0])
		}
		for _, c := range chunks {
			if c.Object != "chat.completion.chunk" {
				t.Fatalf("object = %q", c.Object)
			}
			if c.Usage != nil {
				t.Fatalf("usage present without include_usage: %s", c.Usage)
			}
		}
	})

	t.Run("include_usage adds a final chunk with empty choices", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;tokens=3", true, `,"stream_options":{"include_usage":true}`), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		_, chunks, done := collectOpenAI(t, events)
		if !done {
			t.Fatal("missing [DONE]")
		}
		last := chunks[len(chunks)-1]
		if len(last.Choices) != 0 {
			t.Fatalf("usage chunk has choices: %+v", last)
		}
		var usage struct {
			Completion int64 `json:"completion_tokens"`
		}
		if err := json.Unmarshal(last.Usage, &usage); err != nil || usage.Completion != 3 {
			t.Fatalf("usage = %s, err = %v", last.Usage, err)
		}
		for _, c := range chunks[:len(chunks)-1] {
			if string(c.Usage) != "null" {
				t.Fatalf("earlier chunk usage = %q, want null", c.Usage)
			}
		}
	})
}

func TestAnthropicNonStreaming(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, anthropicPath, anthropicBody("alpha;tokens=3", false, ""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input  *int64 `json:"input_tokens"`
			Output *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.StopReason != "end_turn" {
		t.Fatalf("response = %+v", got)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || !strings.HasPrefix(got.Content[0].Text, "[alpha]") {
		t.Fatalf("content = %+v", got.Content)
	}
	if *got.Usage.Output != 3 || *got.Usage.Input == 0 {
		t.Fatalf("usage = %d %d", *got.Usage.Input, *got.Usage.Output)
	}
}

func TestAnthropicStreamingEventOrder(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, anthropicPath, anthropicBody("alpha;tokens=2", true, ""), nil)
	events, err := readEvents(t, resp.Body)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream end: %v", err)
	}
	var names []string
	for _, ev := range events {
		names = append(names, ev.name)
		var body struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
			t.Fatalf("event %q: %v", ev.name, err)
		}
		if body.Type != ev.name {
			t.Fatalf("event name %q has data type %q", ev.name, body.Type)
		}
	}
	want := []string{
		"message_start", "content_block_start", "ping",
		"content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("events = %v, want %v", names, want)
	}

	var delta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-2].data), &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Delta.StopReason != "end_turn" || delta.Usage.Output != 2 {
		t.Fatalf("message_delta = %+v", delta)
	}
}

func TestAnthropicRequestRules(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name, body string
		header     map[string]string
		wantStatus int
		wantType   string
	}{
		{
			name:       "temperature is rejected on the current API version",
			body:       anthropicBody("alpha", false, `,"temperature":1`),
			wantStatus: 400, wantType: "invalid_request_error",
		},
		{
			name:       "top_p is rejected on the current API version",
			body:       anthropicBody("alpha", false, `,"top_p":0.5`),
			wantStatus: 400, wantType: "invalid_request_error",
		},
		{
			name:       "max_tokens is required",
			body:       `{"model":"alpha","messages":[{"role":"user","content":"x"}]}`,
			wantStatus: 400, wantType: "invalid_request_error",
		},
		{
			name:       "a trailing assistant message is rejected",
			body:       `{"model":"alpha","max_tokens":5,"messages":[{"role":"user","content":"x"},{"role":"assistant","content":"y"}]}`,
			wantStatus: 400, wantType: "invalid_request_error",
		},
		{
			name:       "a system role in messages is rejected",
			body:       `{"model":"alpha","max_tokens":5,"messages":[{"role":"system","content":"x"},{"role":"user","content":"y"}]}`,
			wantStatus: 400, wantType: "invalid_request_error",
		},
		{
			name:       "the api key is required",
			body:       anthropicBody("alpha", false, ""),
			header:     map[string]string{"Anthropic-Version": apiVersion},
			wantStatus: 401, wantType: "authentication_error",
		},
		{
			name:       "the version header is required",
			body:       anthropicBody("alpha", false, ""),
			header:     map[string]string{"X-Api-Key": "test"},
			wantStatus: 400, wantType: "invalid_request_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.post(t, anthropicPath, tt.body, tt.header)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			var got struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Type != "error" || got.Error.Type != tt.wantType {
				t.Fatalf("error body = %+v", got)
			}
		})
	}
}

func TestOpenAIRequestRules(t *testing.T) {
	h := newHarness(t)

	t.Run("a bearer token is required", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha", false, ""), map[string]string{"X-Other": "x"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
	t.Run("stream_options requires stream", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha", false, `,"stream_options":{"include_usage":true}`), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestUnknownScenarioOptionsAreRejected(t *testing.T) {
	h := newHarness(t)
	for _, model := range []string{"alpha;nope=1", "alpha;fail=nonsense", "alpha;tokens=abc", "alpha;latency=fast", ""} {
		resp := h.post(t, openAIPath, openAIBody(model, false, ""), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("model %q: status = %d, want 400", model, resp.StatusCode)
		}
	}
}

func TestStatusFailures(t *testing.T) {
	h := newHarness(t)

	t.Run("openai 429 carries a retry hint", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=status;status=429;retry_after=2", true, ""), nil)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") != "2" {
			t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
		}
		var got struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    *string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Message == "" || got.Error.Type == "" {
			t.Fatalf("error = %+v", got)
		}
	})
	t.Run("anthropic 529 uses its error type", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;fail=status;status=529", false, ""), nil)
		if resp.StatusCode != 529 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var got struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Error.Type != "overloaded_error" {
			t.Fatalf("type = %q", got.Error.Type)
		}
	})
}

func TestConnectionFailures(t *testing.T) {
	h := newHarness(t)

	for _, format := range []struct {
		name, path string
		body       func(string, bool, string) string
	}{
		{"openai", openAIPath, openAIBody},
		{"anthropic", anthropicPath, anthropicBody},
	} {
		t.Run(format.name, func(t *testing.T) {
			t.Run("drop before headers", func(t *testing.T) {
				resp, err := h.send(context.Background(), format.path, format.body("alpha;fail=drop_before_headers", true, ""), nil)
				if err == nil {
					_ = resp.Body.Close()
					t.Fatalf("got status %d, want a connection error", resp.StatusCode)
				}
			})
			t.Run("clean end before the first event", func(t *testing.T) {
				resp := h.post(t, format.path, format.body("alpha;fail=eof_before_first_event", true, ""), nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				events, err := readEvents(t, resp.Body)
				if !errors.Is(err, io.EOF) || len(events) != 0 {
					t.Fatalf("events = %v, err = %v", events, err)
				}
			})
			t.Run("abrupt close before the first event", func(t *testing.T) {
				resp := h.post(t, format.path, format.body("alpha;fail=disconnect_before_first_event", true, ""), nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				events, err := readEvents(t, resp.Body)
				if errors.Is(err, io.EOF) || len(events) != 0 {
					t.Fatalf("events = %v, err = %v, want an abrupt end", events, err)
				}
			})
			t.Run("disconnect midstream", func(t *testing.T) {
				resp := h.post(t, format.path, format.body("alpha;fail=disconnect;after=2;tokens=5", true, ""), nil)
				events, err := readEvents(t, resp.Body)
				if errors.Is(err, io.EOF) {
					t.Fatalf("stream ended cleanly with %d events", len(events))
				}
				if countText(events) != 2 {
					t.Fatalf("delivered %d text events before disconnect, want 2", countText(events))
				}
			})
		})
	}
}

// countText counts events carrying generated text in either wire format.
func countText(events []sseEvent) int {
	n := 0
	for _, ev := range events {
		if strings.Contains(ev.data, `"text_delta"`) || (strings.Contains(ev.data, `"content":"`) && !strings.Contains(ev.data, `"content":""`)) {
			n++
		}
	}
	return n
}

func TestNonStreamingFailures(t *testing.T) {
	h := newHarness(t)

	t.Run("disconnect mid body", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=disconnect", false, ""), nil)
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Fatal("body read succeeded, want an abrupt end")
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=malformed", false, ""), nil)
		var v any
		if err := json.NewDecoder(resp.Body).Decode(&v); err == nil {
			t.Fatalf("decoded %v, want a JSON error", v)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;fail=oversized;size=100000", false, ""), nil)
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 100000 {
			t.Fatalf("body = %d bytes", len(b))
		}
	})
	t.Run("stream_error has no non-streaming meaning", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=stream_error", false, ""), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})
}

func TestMalformedAndOversizedEvents(t *testing.T) {
	h := newHarness(t)

	t.Run("openai malformed event", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=malformed;after=1", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		last := events[len(events)-1].data
		if json.Valid([]byte(last)) {
			t.Fatalf("last event %q is valid JSON", last)
		}
	})
	t.Run("anthropic oversized event", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;fail=oversized;after=1;size=200000", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if got := len(events[len(events)-1].data); got < 200000 {
			t.Fatalf("last event = %d bytes", got)
		}
	})
}

func TestStreamErrorEvents(t *testing.T) {
	h := newHarness(t)

	t.Run("openai", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;fail=stream_error;after=1", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		var got struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		last := events[len(events)-1]
		if err := json.Unmarshal([]byte(last.data), &got); err != nil || got.Error.Message == "" {
			t.Fatalf("last event = %q, err = %v", last.data, err)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;fail=stream_error;after=1", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		last := events[len(events)-1]
		if last.name != "error" || !strings.Contains(last.data, "overloaded_error") {
			t.Fatalf("last event = %+v", last)
		}
	})
}

func TestMissingTerminator(t *testing.T) {
	h := newHarness(t)

	t.Run("openai has no [DONE]", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;terminator=none", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if _, _, done := collectOpenAI(t, events); done {
			t.Fatal("stream has [DONE]")
		}
	})
	t.Run("anthropic has no message_stop", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;terminator=none", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		if name := events[len(events)-1].name; name != "message_delta" {
			t.Fatalf("last event = %q", name)
		}
	})
}

func TestUsageScenarios(t *testing.T) {
	h := newHarness(t)

	t.Run("openai none", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;usage=none", false, ""), nil)
		var got map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got["usage"]; ok {
			t.Fatalf("usage present: %s", got["usage"])
		}
	})
	t.Run("openai stream none omits the usage chunk", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;usage=none", true, `,"stream_options":{"include_usage":true}`), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		_, chunks, done := collectOpenAI(t, events)
		if !done {
			t.Fatal("missing [DONE]")
		}
		for _, c := range chunks {
			if len(c.Usage) > 0 && string(c.Usage) != "null" {
				t.Fatalf("usage reported: %s", c.Usage)
			}
		}
	})
	t.Run("openai inconsistent totals", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("alpha;usage=inconsistent", false, ""), nil)
		var got struct {
			Usage struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
				Total      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Usage.Total == got.Usage.Prompt+got.Usage.Completion {
			t.Fatalf("usage is consistent: %+v", got.Usage)
		}
	})
	t.Run("anthropic stream none has no usage", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;usage=none", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		for _, ev := range events {
			if strings.Contains(ev.data, `"usage"`) {
				t.Fatalf("usage in %q: %s", ev.name, ev.data)
			}
		}
	})
	t.Run("anthropic stream inconsistent counts go backwards", func(t *testing.T) {
		resp := h.post(t, anthropicPath, anthropicBody("alpha;usage=inconsistent;tokens=4", true, ""), nil)
		events, err := readEvents(t, resp.Body)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("stream end: %v", err)
		}
		var start struct {
			Message struct {
				Usage struct {
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		var delta struct {
			Usage struct {
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		for _, ev := range events {
			switch ev.name {
			case "message_start":
				_ = json.Unmarshal([]byte(ev.data), &start)
			case "message_delta":
				_ = json.Unmarshal([]byte(ev.data), &delta)
			}
		}
		if delta.Usage.Output >= start.Message.Usage.Output {
			t.Fatalf("start %d, final %d", start.Message.Usage.Output, delta.Usage.Output)
		}
	})
}

func TestFramingScenarios(t *testing.T) {
	h := newHarness(t)

	raw := func(t *testing.T, model string) []byte {
		t.Helper()
		resp := h.post(t, openAIPath, openAIBody(model, true, ""), nil)
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	baseline := func(t *testing.T) string {
		t.Helper()
		events, err := readEvents(t, bytes.NewReader(raw(t, "alpha;text=unicode")))
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		text, _, _ := collectOpenAI(t, events)
		return text
	}

	t.Run("crlf", func(t *testing.T) {
		b := raw(t, "alpha;eol=crlf")
		if !bytes.Contains(b, []byte("\r\n\r\n")) || bytes.Contains(bytes.ReplaceAll(b, []byte("\r\n"), nil), []byte("\n")) {
			t.Fatalf("not CRLF framed: %q", b[:min(120, len(b))])
		}
	})
	t.Run("cr", func(t *testing.T) {
		b := raw(t, "alpha;eol=cr")
		if !bytes.Contains(b, []byte("\r\r")) || bytes.Contains(b, []byte("\n")) {
			t.Fatalf("not CR framed: %q", b[:min(120, len(b))])
		}
	})
	t.Run("comments", func(t *testing.T) {
		if b := raw(t, "alpha;comments=true"); !bytes.Contains(b, []byte(": keepalive\n")) {
			t.Fatalf("no comments: %q", b[:min(120, len(b))])
		}
	})
	t.Run("multiline data still parses", func(t *testing.T) {
		b := raw(t, "alpha;multiline=true")
		if !bytes.Contains(b, []byte("\ndata: \"")) {
			t.Fatalf("no multiline event: %q", b[:min(200, len(b))])
		}
		events, err := readEvents(t, bytes.NewReader(b))
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if _, _, done := collectOpenAI(t, events); !done {
			t.Fatal("missing [DONE]")
		}
	})
	t.Run("fragmented writes keep multi-byte text intact", func(t *testing.T) {
		events, err := readEvents(t, bytes.NewReader(raw(t, "alpha;text=unicode;chunk=1")))
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		text, _, done := collectOpenAI(t, events)
		if !done {
			t.Fatal("missing [DONE]")
		}
		if want := baseline(t); text != want {
			t.Fatalf("text = %q, want %q", text, want)
		}
		if !strings.Contains(text, "世界") {
			t.Fatalf("text %q has no multi-byte characters", text)
		}
	})
}

func TestTimingScenarios(t *testing.T) {
	h := newHarness(t)
	const delay = 60 * time.Millisecond

	t.Run("latency delays the response headers", func(t *testing.T) {
		start := time.Now()
		h.post(t, openAIPath, openAIBody("alpha;latency=60ms", true, ""), nil)
		if got := time.Since(start); got < delay {
			t.Fatalf("headers after %v, want at least %v", got, delay)
		}
	})
	t.Run("ttfb delays the first event but not the headers", func(t *testing.T) {
		start := time.Now()
		resp := h.post(t, openAIPath, openAIBody("alpha;ttfb=300ms", true, ""), nil)
		if got := time.Since(start); got >= 300*time.Millisecond {
			t.Fatalf("headers after %v", got)
		}
		d := sse.NewDecoder(resp.Body, 1<<16)
		if _, err := d.Next(); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got < 300*time.Millisecond {
			t.Fatalf("first event after %v", got)
		}
	})
	t.Run("interval spaces the text events", func(t *testing.T) {
		start := time.Now()
		resp := h.post(t, openAIPath, openAIBody("alpha;tokens=3;interval=60ms", true, ""), nil)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got < 2*delay {
			t.Fatalf("stream took %v, want at least %v", got, 2*delay)
		}
	})
}

// waitForStats polls because the mock only sees a disconnect when the server
// notices it.
func waitForStats(t *testing.T, s *Server, label string, ok func(LabelStats) bool) LabelStats {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.Stats()[label]
		if ok(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats for %q never reached the expected state: %+v", label, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStatsCountRequestsAndCancellation(t *testing.T) {
	h := newHarness(t)

	t.Run("completed", func(t *testing.T) {
		resp := h.post(t, openAIPath, openAIBody("done", true, ""), nil)
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		got := waitForStats(t, h.server, "done", func(s LabelStats) bool { return s.Completed == 1 })
		if got.Requests != 1 || got.Canceled != 0 || got.BytesWritten == 0 {
			t.Fatalf("stats = %+v", got)
		}
	})

	t.Run("a client disconnect is observed promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resp, err := h.send(ctx, openAIPath, openAIBody("gone;tokens=1000;interval=20ms", true, ""), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		d := sse.NewDecoder(resp.Body, 1<<16)
		if _, err := d.Next(); err != nil {
			t.Fatal(err)
		}
		cancel()
		got := waitForStats(t, h.server, "gone", func(s LabelStats) bool { return s.Canceled == 1 })
		if got.Completed != 0 {
			t.Fatalf("stats = %+v", got)
		}
	})

	t.Run("the HTTP endpoint reports the same counters", func(t *testing.T) {
		resp, err := http.Get(h.url + "/__mock/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got map[string]LabelStats
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got["done"].Completed != 1 {
			t.Fatalf("stats = %+v", got)
		}
	})
}

// A client that stops reading must stall the mock's writes rather than the mock
// buffering the whole stream. The gateway's backpressure tests depend on this.
func TestSlowConsumerStallsWrites(t *testing.T) {
	h := newHarness(t)
	const total = 64 << 20

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := h.send(ctx, openAIPath, openAIBody("slow;tokens=20000;pad=4096", true, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	waitForStats(t, h.server, "slow", func(s LabelStats) bool { return s.BytesWritten > 0 })
	var last int64
	for {
		time.Sleep(100 * time.Millisecond)
		got := h.server.Stats()["slow"].BytesWritten
		if got == last {
			break
		}
		last = got
	}
	if last >= total {
		t.Fatalf("mock wrote %d bytes to a client that read nothing", last)
	}
	cancel()
	waitForStats(t, h.server, "slow", func(s LabelStats) bool { return s.Canceled == 1 })
}
