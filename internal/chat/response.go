package chat

import "encoding/json"

// FinishReason says why generation stopped.
type FinishReason string

// Supported finish reasons.
const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
)

// UsageState says how far a provider's usage report can be trusted.
type UsageState string

const (
	// UsageReported means the counts are present and add up.
	UsageReported UsageState = "reported"
	// UsageInconsistent means the counts are present but don't add up. They are
	// kept for reconciliation, and flagged.
	UsageInconsistent UsageState = "inconsistent"
	// UsageMalformed means a report arrived but its counts are unusable. Only Raw
	// is meaningful.
	UsageMalformed UsageState = "malformed"
	// UsagePartial means input counts arrived but the response never finished, so
	// output is only a lower bound. It's kept for reconciliation.
	UsagePartial UsageState = "partial"
)

// Usage is a provider's token report. A nil *Usage means the provider reported
// nothing, which is never the same as zero: the attempt may still have cost money.
type Usage struct {
	State UsageState

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64

	// Optional breakdowns, nil when the provider did not report them.
	CachedPromptTokens     *int64
	CacheWritePromptTokens *int64
	ReasoningTokens        *int64

	// Raw is the provider's report exactly as received, kept for audit.
	Raw json.RawMessage
}

// Reportable says whether the counts are safe to show a client.
func (u *Usage) Reportable() bool {
	return u != nil && (u.State == UsageReported || u.State == UsageInconsistent)
}

// Completion is the result of a non-streaming attempt.
type Completion struct {
	Content      string
	Refusal      string
	FinishReason FinishReason
	Usage        *Usage
}

// StreamEvent is one normalized stream event. Adapters only emit events that
// carry something, so role-only chunks, pings, and similar never reach the gateway.
type StreamEvent struct {
	Content      string
	Refusal      string
	FinishReason FinishReason
	Usage        *Usage
}

// HasOutput reports whether the event is part of the response itself, rather than
// only a usage report. Only output can commit a response or show that a stream is
// making progress. Some servers report usage on every chunk, including role-only
// ones sent before inference has really begun.
func (e StreamEvent) HasOutput() bool {
	return e.Content != "" || e.Refusal != "" || e.FinishReason != ""
}
