// Package accounting records what each upstream attempt did.
//
// An attempt is recorded whether it succeeded or not, because a failed attempt
// can still cost money. Nothing here enforces a budget yet, and records are only
// logged: see docs/adr/0002-postgres-is-the-budget-authority.md for the durable
// ledger that replaces this.
package accounting

import (
	"context"
	"log/slog"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
)

// Outcome is how an attempt ended.
type Outcome string

// Attempt outcomes.
const (
	// OutcomeCompleted means the upstream finished correctly and the client got
	// all of it.
	OutcomeCompleted Outcome = "completed"
	// OutcomeFailed means the attempt failed before the client was told anything.
	OutcomeFailed Outcome = "failed"
	// OutcomeInterrupted means the upstream failed after the response committed.
	OutcomeInterrupted Outcome = "interrupted"
	// OutcomeClientCanceled means the client went away.
	OutcomeClientCanceled Outcome = "client_canceled"
	// OutcomeClientStalled means the client stopped reading until a write timed out.
	OutcomeClientStalled Outcome = "client_stalled"
	// OutcomeNotSent means the attempt was reserved, but the gateway never sent it.
	OutcomeNotSent Outcome = "not_sent"
)

// Reason says why this attempt was made. The values are a small fixed set, so they
// are safe as metric labels.
type Reason string

// Attempt reasons.
const (
	// ReasonPrimary is a request's first attempt.
	ReasonPrimary Reason = "primary"
	// ReasonFallback follows a failed attempt on another route.
	ReasonFallback Reason = "fallback"
	// ReasonRetryAfter follows waiting for a provider's Retry-After hint.
	ReasonRetryAfter Reason = "retry_after"
)

// UsageAbsent is the logged usage state when a provider reported nothing.
const UsageAbsent = "absent"

// Attempt describes one upstream attempt. It has no field for prompts or
// completions, so a record can't leak them.
type Attempt struct {
	RequestID string
	TeamID    string
	KeyID     string
	Alias     string
	Provider  string
	Model     string
	Number    int
	Stream    bool
	Reason    Reason

	Outcome   Outcome
	Committed bool // the client received response headers

	ErrorKind      provider.Kind
	Delivery       provider.Delivery
	UpstreamStatus int
	UpstreamCode   string

	// Usage is nil when the provider reported nothing. That means unknown, not
	// zero: the attempt may have cost money. TextEvents and OutputBytes count the
	// generated text received, which reconciliation can use for an estimate.
	Usage       *chat.Usage
	TextEvents  int
	OutputBytes int64

	Duration         time.Duration
	TimeToFirstEvent time.Duration
}

// Recorder receives finished attempts. Callers pass a context separate from the
// request's, so that a canceled request can't prevent its own accounting.
//
// RecordAttempt runs before the request's handler returns, so an implementation
// must return by the context's deadline or it holds the response open.
type Recorder interface {
	RecordAttempt(ctx context.Context, a Attempt)
}

// LogRecorder writes attempts to a structured log.
type LogRecorder struct {
	logger *slog.Logger
}

// NewLogRecorder returns a recorder writing to logger.
func NewLogRecorder(logger *slog.Logger) *LogRecorder {
	return &LogRecorder{logger: logger}
}

// RecordAttempt implements Recorder.
func (r *LogRecorder) RecordAttempt(ctx context.Context, a Attempt) {
	attrs := []slog.Attr{
		slog.String("request_id", a.RequestID),
		slog.String("team_id", a.TeamID),
		slog.String("key_id", a.KeyID),
		slog.String("alias", a.Alias),
		slog.String("provider", a.Provider),
		slog.String("model", a.Model),
		slog.Int("attempt", a.Number),
		slog.String("reason", string(a.Reason)),
		slog.Bool("stream", a.Stream),
		slog.String("outcome", string(a.Outcome)),
		slog.Bool("committed", a.Committed),
		slog.Int("text_events", a.TextEvents),
		slog.Int64("output_bytes", a.OutputBytes),
		slog.Duration("duration", a.Duration),
		slog.Duration("time_to_first_event", a.TimeToFirstEvent),
	}
	if a.ErrorKind != "" {
		attrs = append(attrs,
			slog.String("error_kind", string(a.ErrorKind)),
			slog.String("delivery", string(a.Delivery)),
		)
	}
	if a.UpstreamStatus != 0 {
		attrs = append(attrs, slog.Int("upstream_status", a.UpstreamStatus))
	}
	if a.UpstreamCode != "" {
		attrs = append(attrs, slog.String("upstream_code", a.UpstreamCode))
	}
	attrs = append(attrs, usageAttrs(a.Usage)...)
	r.logger.LogAttrs(ctx, slog.LevelInfo, "upstream attempt", attrs...)
}

func usageAttrs(u *chat.Usage) []slog.Attr {
	if u == nil {
		return []slog.Attr{slog.String("usage_state", UsageAbsent)}
	}
	attrs := []slog.Attr{slog.String("usage_state", string(u.State))}
	if u.Reportable() {
		attrs = append(attrs,
			slog.Int64("prompt_tokens", u.PromptTokens),
			slog.Int64("completion_tokens", u.CompletionTokens),
			slog.Int64("total_tokens", u.TotalTokens),
		)
	}
	return attrs
}
