package mock

import (
	"encoding/json"
	"net/http"
	"strings"
)

// anthropicVersionWithoutSampling is the first API version that returns 400 for
// temperature, top_p, and top_k on every model.
const anthropicVersionWithoutSampling = "2026-10-21"

// anthropicWire follows the Anthropic Messages format as published in the
// Anthropic API documentation (checked 2026-11-05).
type anthropicWire struct{}

func (anthropicWire) parse(r *http.Request, body []byte) (request, *apiError) {
	if r.Header.Get("X-Api-Key") == "" {
		return request{}, &apiError{status: http.StatusUnauthorized, message: "missing x-api-key header"}
	}
	version := r.Header.Get("Anthropic-Version")
	if version == "" {
		return request{}, &apiError{status: http.StatusBadRequest, message: "missing anthropic-version header"}
	}
	var in struct {
		Model     string `json:"model"`
		MaxTokens *int   `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		StopSequences []string `json:"stop_sequences"`
		Stream        bool     `json:"stream"`
		Temperature   *float64 `json:"temperature"`
		TopP          *float64 `json:"top_p"`
		TopK          *int     `json:"top_k"`
	}
	if err := decodeStrict(body, &in); err != nil {
		return request{}, &apiError{status: http.StatusBadRequest, message: "invalid request: " + err.Error()}
	}
	if version >= anthropicVersionWithoutSampling && (in.Temperature != nil || in.TopP != nil || in.TopK != nil) {
		return request{}, &apiError{status: http.StatusBadRequest, message: "sampling parameters are not accepted on this API version"}
	}
	if in.MaxTokens == nil || *in.MaxTokens < 1 {
		return request{}, &apiError{status: http.StatusBadRequest, message: "max_tokens is required"}
	}
	if len(in.Messages) == 0 {
		return request{}, &apiError{status: http.StatusBadRequest, message: "messages is required"}
	}
	texts := []string{in.System}
	for _, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return request{}, &apiError{status: http.StatusBadRequest, message: "unexpected role: " + m.Role}
		}
		texts = append(texts, m.Content)
	}
	if in.Messages[len(in.Messages)-1].Role != "user" {
		return request{}, &apiError{status: http.StatusBadRequest, message: "the conversation must end with a user message"}
	}
	return request{
		model:        in.Model,
		stream:       in.Stream,
		includeUsage: true,
		maxOutput:    *in.MaxTokens,
		promptTokens: countPromptTokens(texts...),
	}, nil
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusConflict:
		return "conflict_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusInternalServerError:
		return "api_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func (anthropicWire) writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_mock")
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(mustMarshal(map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": anthropicErrorType(e.status), "message": e.message},
		"request_id": "req_mock",
	}))
}

func anthropicStopReason(resp response) string {
	if resp.limited {
		return "max_tokens"
	}
	return "end_turn"
}

func (anthropicWire) completion(resp response) []byte {
	type textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	}
	out := struct {
		ID           string      `json:"id"`
		Type         string      `json:"type"`
		Role         string      `json:"role"`
		Content      []textBlock `json:"content"`
		Model        string      `json:"model"`
		StopReason   string      `json:"stop_reason"`
		StopSequence *string     `json:"stop_sequence"`
		Usage        *usage      `json:"usage,omitempty"`
	}{
		ID:         "msg_mock_" + resp.scenario.label,
		Type:       "message",
		Role:       "assistant",
		Content:    []textBlock{{Type: "text", Text: resp.text()}},
		Model:      resp.scenario.label,
		StopReason: anthropicStopReason(resp),
	}
	switch resp.scenario.usage {
	case usageReported:
		out.Usage = &usage{resp.promptTokens, resp.tokens}
	case usageInconsistent:
		out.Usage = &usage{-1, resp.tokens}
	}
	return mustMarshal(out)
}

func (a anthropicWire) event(sc scenario, name string, body map[string]any) []byte {
	body["type"] = name
	return sc.frame(name, mustMarshal(body))
}

func (a anthropicWire) stream(resp response) streamParts {
	sc := resp.scenario

	message := map[string]any{
		"id":            "msg_mock_" + sc.label,
		"type":          "message",
		"role":          "assistant",
		"content":       []any{},
		"model":         sc.label,
		"stop_reason":   nil,
		"stop_sequence": nil,
	}
	finalUsage := map[string]any{"output_tokens": resp.tokens}
	switch sc.usage {
	case usageReported:
		message["usage"] = map[string]any{"input_tokens": resp.promptTokens, "output_tokens": 1}
	case usageInconsistent:
		// Usage in message_delta is cumulative, so counts must never fall.
		message["usage"] = map[string]any{"input_tokens": resp.promptTokens, "output_tokens": resp.tokens + 5}
	}

	parts := streamParts{
		prelude: [][]byte{
			a.event(sc, "message_start", map[string]any{"message": message}),
			a.event(sc, "content_block_start", map[string]any{
				"index":         0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
			a.event(sc, "ping", map[string]any{}),
		},
		textFrame: func(i int) []byte { return a.textDelta(sc, sc.delta(i)) },
	}

	delta := map[string]any{"delta": map[string]any{"stop_reason": anthropicStopReason(resp), "stop_sequence": nil}}
	if sc.usage != usageNone {
		delta["usage"] = finalUsage
	}
	parts.epilogue = [][]byte{
		a.event(sc, "content_block_stop", map[string]any{"index": 0}),
		a.event(sc, "message_delta", delta),
	}
	parts.terminator = [][]byte{a.event(sc, "message_stop", map[string]any{})}
	return parts
}

func (a anthropicWire) textDelta(sc scenario, text string) []byte {
	return a.event(sc, "content_block_delta", map[string]any{
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (a anthropicWire) streamErrorFrame(sc scenario) []byte {
	return a.event(sc, "error", map[string]any{
		"error": map[string]any{"type": "overloaded_error", "message": "Overloaded"},
	})
}

func (anthropicWire) malformedFrame(sc scenario) []byte {
	return sc.frame("content_block_delta", json.RawMessage(`{"type":"content_block_delta","delta":{`))
}

func (a anthropicWire) oversizedFrame(sc scenario) []byte {
	return a.textDelta(sc, strings.Repeat("x", sc.size))
}
