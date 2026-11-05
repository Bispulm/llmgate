package api

import (
	"encoding/json"

	"github.com/Bispulm/llmgate/internal/chat"
)

// These types are the client-facing wire format. See docs/compatibility.md.

const (
	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"
	roleAssistant    = "assistant"
	completionPrefix = "chatcmpl-"
)

type wireUsage struct {
	PromptTokens      int64                 `json:"prompt_tokens"`
	CompletionTokens  int64                 `json:"completion_tokens"`
	TotalTokens       int64                 `json:"total_tokens"`
	PromptDetails     *wirePromptDetails    `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *wireCompletionDetail `json:"completion_tokens_details,omitempty"`
}

type wirePromptDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
}

type wireCompletionDetail struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// newWireUsage returns nil for usage that is absent or unreadable, which leaves
// the field out rather than reporting zeros.
func newWireUsage(u *chat.Usage) *wireUsage {
	if !u.Reportable() {
		return nil
	}
	out := &wireUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CachedPromptTokens != nil || u.CacheWritePromptTokens != nil {
		out.PromptDetails = &wirePromptDetails{
			CachedTokens:     u.CachedPromptTokens,
			CacheWriteTokens: u.CacheWritePromptTokens,
		}
	}
	if u.ReasoningTokens != nil {
		out.CompletionDetails = &wireCompletionDetail{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

type wireCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage,omitempty"`
}

type wireChoice struct {
	Index        int         `json:"index"`
	Message      wireMessage `json:"message"`
	Logprobs     *struct{}   `json:"logprobs"`
	FinishReason string      `json:"finish_reason"`
}

// wireMessage has nullable content because a refusal comes without any.
type wireMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	Refusal *string `json:"refusal"`
}

type wireChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []wireChunkChoice `json:"choices"`
	// Usage is left out unless the client asked for it. It is then an explicit
	// null on every chunk except the last.
	Usage json.RawMessage `json:"usage,omitempty"`
}

type wireChunkChoice struct {
	Index        int       `json:"index"`
	Delta        wireDelta `json:"delta"`
	Logprobs     *struct{} `json:"logprobs"`
	FinishReason *string   `json:"finish_reason"`
}

type wireDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
	Refusal *string `json:"refusal,omitempty"`
}
