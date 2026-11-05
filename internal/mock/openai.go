package mock

import (
	"encoding/json"
	"net/http"
	"strings"
)

// mockCreated is a fixed timestamp, so identical requests get identical bytes.
const mockCreated = 1700000000

// openAIWire follows the OpenAI chat completions format as published in the
// OpenAI OpenAPI specification (checked 2026-11-05).
type openAIWire struct{}

func (openAIWire) parse(r *http.Request, body []byte) (request, *apiError) {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); !ok || token == "" {
		return request{}, &apiError{status: http.StatusUnauthorized, message: "missing bearer token"}
	}
	var in struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		MaxTokens           *int            `json:"max_tokens"`
		Temperature         *float64        `json:"temperature"`
		TopP                *float64        `json:"top_p"`
		Stop                json.RawMessage `json:"stop"`
		Stream              bool            `json:"stream"`
		StreamOptions       *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := decodeStrict(body, &in); err != nil {
		return request{}, &apiError{status: http.StatusBadRequest, message: "invalid request: " + err.Error()}
	}
	if in.StreamOptions != nil && !in.Stream {
		return request{}, &apiError{status: http.StatusBadRequest, message: "stream_options requires stream"}
	}
	if len(in.Messages) == 0 {
		return request{}, &apiError{status: http.StatusBadRequest, message: "messages is required"}
	}
	if in.MaxCompletionTokens != nil && in.MaxTokens != nil {
		return request{}, &apiError{status: http.StatusBadRequest, message: "set only one of max_tokens and max_completion_tokens"}
	}

	out := request{model: in.Model, stream: in.Stream}
	if in.StreamOptions != nil {
		out.includeUsage = in.StreamOptions.IncludeUsage
	}
	switch {
	case in.MaxCompletionTokens != nil:
		out.maxOutput = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		out.maxOutput = *in.MaxTokens
	}
	texts := make([]string, len(in.Messages))
	for i, m := range in.Messages {
		texts[i] = m.Content
	}
	out.promptTokens = countPromptTokens(texts...)
	return out, nil
}

func (openAIWire) writeError(w http.ResponseWriter, e *apiError) {
	errType, code := "server_error", "server_error"
	switch {
	case e.status == http.StatusUnauthorized:
		errType, code = "invalid_request_error", "invalid_api_key"
	case e.status == http.StatusTooManyRequests:
		errType, code = "rate_limit_error", "rate_limit_exceeded"
	case e.status < 500:
		errType, code = "invalid_request_error", "invalid_request"
	}
	body := map[string]any{"error": map[string]any{
		"message": e.message,
		"type":    errType,
		"param":   nil,
		"code":    code,
	}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req_mock")
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(mustMarshal(body))
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (openAIWire) usage(resp response) *openAIUsage {
	if resp.scenario.usage == usageNone {
		return nil
	}
	u := openAIUsage{
		PromptTokens:     resp.promptTokens,
		CompletionTokens: resp.tokens,
		TotalTokens:      resp.promptTokens + resp.tokens,
	}
	if resp.scenario.usage == usageInconsistent {
		u.TotalTokens += 7
	}
	return &u
}

func openAIFinishReason(resp response) string {
	if resp.limited {
		return "length"
	}
	return "stop"
}

func (o openAIWire) completion(resp response) []byte {
	type message struct {
		Role    string  `json:"role"`
		Content string  `json:"content"`
		Refusal *string `json:"refusal"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		Logprobs     *string `json:"logprobs"`
		FinishReason string  `json:"finish_reason"`
	}
	return mustMarshal(struct {
		ID      string       `json:"id"`
		Object  string       `json:"object"`
		Created int64        `json:"created"`
		Model   string       `json:"model"`
		Choices []choice     `json:"choices"`
		Usage   *openAIUsage `json:"usage,omitempty"`
	}{
		ID:      "chatcmpl-mock-" + resp.scenario.label,
		Object:  "chat.completion",
		Created: mockCreated,
		Model:   resp.scenario.label,
		Choices: []choice{{
			Message:      message{Role: "assistant", Content: resp.text()},
			FinishReason: openAIFinishReason(resp),
		}},
		Usage: o.usage(resp),
	})
}

type openAIStreamChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta"`
	Logprobs     *string         `json:"logprobs"`
	FinishReason *string         `json:"finish_reason"`
}

type openAIStreamChunk struct {
	ID          string               `json:"id"`
	Object      string               `json:"object"`
	Created     int64                `json:"created"`
	Model       string               `json:"model"`
	Choices     []openAIStreamChoice `json:"choices"`
	Usage       json.RawMessage      `json:"usage,omitempty"`
	Obfuscation string               `json:"obfuscation,omitempty"`
}

func (o openAIWire) chunk(resp response, delta string, finish *string) []byte {
	c := openAIStreamChunk{
		ID:          "chatcmpl-mock-" + resp.scenario.label,
		Object:      "chat.completion.chunk",
		Created:     mockCreated,
		Model:       resp.scenario.label,
		Obfuscation: "mockpad",
		Choices:     []openAIStreamChoice{{Delta: json.RawMessage(delta), FinishReason: finish}},
	}
	if resp.includeUsage {
		// Only the last chunk carries usage. All others carry an explicit null.
		c.Usage = json.RawMessage("null")
	}
	return mustMarshal(c)
}

func (o openAIWire) stream(resp response) streamParts {
	sc := resp.scenario
	finish := openAIFinishReason(resp)

	parts := streamParts{
		prelude: [][]byte{sc.frame("", o.chunk(resp, `{"role":"assistant","content":"","refusal":null}`, nil))},
		textFrame: func(i int) []byte {
			delta := mustMarshal(struct {
				Content string `json:"content"`
			}{sc.delta(i)})
			return sc.frame("", o.chunk(resp, string(delta), nil))
		},
	}
	parts.epilogue = append(parts.epilogue, sc.frame("", o.chunk(resp, `{}`, &finish)))
	if u := o.usage(resp); u != nil && resp.includeUsage {
		parts.epilogue = append(parts.epilogue, sc.frame("", mustMarshal(openAIStreamChunk{
			ID:      "chatcmpl-mock-" + sc.label,
			Object:  "chat.completion.chunk",
			Created: mockCreated,
			Model:   sc.label,
			Choices: []openAIStreamChoice{},
			Usage:   mustMarshal(u),
		})))
	}
	parts.terminator = [][]byte{sc.frame("", []byte("[DONE]"))}
	return parts
}

func (openAIWire) streamErrorFrame(sc scenario) []byte {
	return sc.frame("", mustMarshal(map[string]any{"error": map[string]any{
		"message": "mock stream failure",
		"type":    "server_error",
		"param":   nil,
		"code":    "server_error",
	}}))
}

func (openAIWire) malformedFrame(sc scenario) []byte {
	return sc.frame("", []byte(`{"id":"chatcmpl-mock","choices":[{"delta":`))
}

func (o openAIWire) oversizedFrame(sc scenario) []byte {
	resp := response{scenario: sc}
	delta := mustMarshal(struct {
		Content string `json:"content"`
	}{strings.Repeat("x", sc.size)})
	return sc.frame("", o.chunk(resp, string(delta), nil))
}
