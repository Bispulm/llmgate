// Package provider defines how the gateway talks to an upstream LLM API.
package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
)

// Provider performs one upstream attempt. It does not retry: deciding whether
// another attempt is safe and affordable belongs to the caller.
type Provider interface {
	// Complete performs a non-streaming attempt. Failures are *Error.
	Complete(ctx context.Context, req *chat.Request, model string) (*chat.Completion, error)

	// Stream starts a streaming attempt. A nil error means the upstream accepted
	// the request and is sending an event stream, but not that any content has
	// arrived. Failures are *Error.
	//
	// The stream is bound to ctx. Canceling ctx cancels the upstream request.
	Stream(ctx context.Context, req *chat.Request, model string) (Stream, error)
}

// Stream is an in-progress streaming attempt. It must be closed, which cancels
// the upstream request if it hasn't finished.
type Stream interface {
	// Next returns the next event. It returns io.EOF only after the upstream
	// completed the stream correctly. Anything else, including the connection
	// ending without a completion marker, is an *Error.
	Next() (chat.StreamEvent, error)

	Close() error
}

// Kind classifies a failure. Callers branch on this to decide whether to fail
// over, because treating these alike would retry a bad credential or count a
// client's cancellation against a provider's health.
type Kind string

// Failure kinds.
const (
	KindConnection     Kind = "connection"      // No HTTP response was received.
	KindTimeout        Kind = "timeout"         // A deadline passed, or the upstream reported a timeout.
	KindCanceled       Kind = "canceled"        // The caller canceled the request.
	KindRateLimited    Kind = "rate_limited"    // The upstream asked for fewer requests.
	KindOverloaded     Kind = "overloaded"      // The upstream reported it is overloaded.
	KindServer         Kind = "server"          // The upstream failed.
	KindAuth           Kind = "auth"            // The upstream rejected the gateway's credentials.
	KindNotFound       Kind = "not_found"       // The upstream doesn't know the model or path.
	KindInvalidRequest Kind = "invalid_request" // The upstream rejected the request itself.
	KindProtocol       Kind = "protocol"        // The upstream's response broke the wire format.
	KindInterrupted    Kind = "interrupted"     // A response began but never completed.
)

// Delivery says what the upstream may have seen, which accounting needs: only a
// request known not to have been sent can be released with certainty.
type Delivery string

// Delivery states.
const (
	DeliveryNotSent   Delivery = "not_sent"  // The upstream can't have received the request.
	DeliveryUncertain Delivery = "uncertain" // It may have, and may be billing for it.
	DeliveryConfirmed Delivery = "confirmed" // It did: a response arrived.
)

// Error is a failed attempt. Its text holds no credentials, prompts, or provider
// error messages, which can contain account details, so it is safe to log.
type Error struct {
	Kind     Kind
	Delivery Delivery

	// Status is the HTTP status, or 0 if none was received.
	Status int
	// RetryAfter is the upstream's hint, or 0 if it gave none.
	RetryAfter time.Duration
	// UpstreamCode is the provider's error identifier, if it gave a short one.
	UpstreamCode string
	// Err is the local cause, if any.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("provider attempt failed: ")
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " status=%d", e.Status)
	}
	if e.UpstreamCode != "" {
		fmt.Fprintf(&b, " code=%s", e.UpstreamCode)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// NewHTTPClient returns the client adapters use for upstream calls.
//
// It follows no redirects, because that would send a credential to a host the
// operator never configured. It ignores proxy environment variables for the same
// reason. Compression stays off so that stream events aren't held in a
// decompressor. Request deadlines come from the context of each call.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        1024,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
