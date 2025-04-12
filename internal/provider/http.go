package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	maxErrorBodyBytes = 64 << 10
	maxDrainBytes     = 64 << 10
	// drainTimeout bounds waiting for the end of a finished stream's body.
	drainTimeout = 100 * time.Millisecond
	// maxIdentifierLength bounds an upstream error code kept from a response.
	maxIdentifierLength = 64
)

// TransportError classifies a failure to get a response. Only a failure to connect
// proves that nothing was sent.
func TransportError(ctx context.Context, err error) *Error {
	delivery := DeliveryUncertain
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		delivery = DeliveryNotSent
	}
	kind := KindConnection
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = KindCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = KindTimeout
	}
	return &Error{Kind: kind, Delivery: delivery, Err: err}
}

// ReadError classifies a failure partway through a response. The upstream certainly
// received the request, so a cancellation here may still be billed.
func ReadError(ctx context.Context, err error) *Error {
	kind := KindInterrupted
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = KindCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = KindTimeout
	}
	return &Error{Kind: kind, Delivery: DeliveryConfirmed, Err: err}
}

// ProtocolError reports a response that broke the wire format. The message must not
// contain anything from the response.
func ProtocolError(message string) *Error {
	return &Error{Kind: KindProtocol, Delivery: DeliveryConfirmed, Err: errors.New(message)}
}

// KindForStatus classifies a non-200 HTTP status.
func KindForStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusPaymentRequired:
		return KindBilling
	case status == http.StatusNotFound:
		return KindNotFound
	case status == http.StatusRequestTimeout:
		return KindTimeout
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status == http.StatusServiceUnavailable, status == 529:
		return KindOverloaded
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindInvalidRequest
	default:
		// A redirect or an unexpected success. Redirects aren't followed.
		return KindProtocol
	}
}

// ParseRetryAfter reads a Retry-After header, in seconds or as an HTTP date. An
// unusable or past value gives 0.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// IsIdentifier reports whether a provider's error code is short and machine-like
// enough to keep. Anything else may be free text holding account details.
func IsIdentifier(s string) bool {
	if s == "" || len(s) > maxIdentifierLength {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// ReadErrorBody reads a bounded amount of an error response.
func ReadErrorBody(r io.Reader) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	return body
}

// ResponseStream owns the HTTP response behind a Stream. It has a context of its
// own, which lets Close cancel the upstream, and bound draining, without closing
// the body while a read may be in progress.
type ResponseStream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	body      io.ReadCloser
	completed bool
	closed    bool
}

// NewResponseStream returns a ResponseStream for a response made with ctx. It calls
// cancel when closed.
func NewResponseStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser) *ResponseStream {
	return &ResponseStream{ctx: ctx, cancel: cancel, body: body}
}

// ReadError classifies a failure reading this stream.
func (s *ResponseStream) ReadError(err error) *Error {
	return ReadError(s.ctx, err)
}

// MarkCompleted records that the upstream finished correctly, so that Close can
// prepare the connection for reuse.
func (s *ResponseStream) MarkCompleted() { s.completed = true }

// Close implements part of Stream. After a completed stream it reads the tail of
// the body, because HTTP/1.1 can't reuse a connection otherwise.
func (s *ResponseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	defer s.cancel()
	if s.completed {
		timer := time.AfterFunc(drainTimeout, s.cancel)
		defer timer.Stop()
		_, _ = io.CopyN(io.Discard, s.body, maxDrainBytes)
	}
	if err := s.body.Close(); err != nil {
		return fmt.Errorf("closing upstream response: %w", err)
	}
	return nil
}
