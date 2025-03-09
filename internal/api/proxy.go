package api

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Bispulm/llmgate/internal/accounting"
	"github.com/Bispulm/llmgate/internal/auth"
	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
	"github.com/Bispulm/llmgate/internal/routing"
)

const (
	// recordTimeout bounds accounting for an attempt. It runs on its own context so
	// that the request ending, for any reason, can't prevent it.
	recordTimeout = 5 * time.Second

	headerRoute             = "X-Llmgate-Route"
	headerAttempts          = "X-Llmgate-Attempts"
	headerBudgetEnforcement = "X-Llmgate-Budget-Enforcement"

	// budgetEnforcementNone says plainly that no budget protects this request.
	budgetEnforcementNone = "none"
)

// Causes for canceling a stream, which separate a timeout from a client leaving.
var (
	errFirstEventTimeout = errors.New("no content arrived within the first-event timeout")
	errStreamIdleTimeout = errors.New("the stream stalled past the idle timeout")
)

// exchange is one request being proxied.
type exchange struct {
	w  http.ResponseWriter
	r  *http.Request
	rc *http.ResponseController

	id      string
	alias   string
	created int64
	start   time.Time
	req     *chat.Request
	route   routing.Route
	attempt accounting.Attempt
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := rand.Text()
	header := w.Header()
	header.Set("X-Request-Id", id)
	header.Set(headerBudgetEnforcement, budgetEnforcementNone)

	if r.Method != http.MethodPost {
		header.Set("Allow", http.MethodPost)
		writeError(w, &apiError{
			Status:  http.StatusMethodNotAllowed,
			Type:    errTypeInvalidRequest,
			Code:    codeMethodNotAllowed,
			Message: "this endpoint only accepts POST",
		})
		return
	}

	// Authenticate before reading the body, so an unauthenticated caller can't make
	// the gateway buffer or parse anything.
	principal, err := s.auth.Authenticate(r.Context(), auth.BearerToken(r.Header))
	if err != nil {
		header.Set("WWW-Authenticate", "Bearer")
		writeError(w, &apiError{
			Status:  http.StatusUnauthorized,
			Type:    errTypeAuthentication,
			Code:    codeInvalidAPIKey,
			Message: "the API key is missing or invalid",
		})
		return
	}

	rc := http.NewResponseController(w)
	body, apiErr := s.readBody(w, r, rc)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	req, err := chat.DecodeRequest(body)
	if err != nil {
		writeError(w, validationError(err))
		return
	}

	alias, ok := s.routes.Resolve(req.Model)
	if !ok {
		writeError(w, &apiError{
			Status:  http.StatusNotFound,
			Type:    errTypeInvalidRequest,
			Code:    codeModelNotFound,
			Param:   "model",
			Message: "the model does not exist",
		})
		return
	}
	limited, apiErr := applyOutputLimit(req, alias)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	route := alias.Routes[0]
	header.Set(headerRoute, route.ProviderName+"/"+route.Model)
	header.Set(headerAttempts, strconv.Itoa(1))

	x := &exchange{
		w:       w,
		r:       r,
		rc:      rc,
		id:      id,
		alias:   alias.Name,
		created: start.Unix(),
		start:   start,
		req:     limited,
		route:   route,
		attempt: accounting.Attempt{
			RequestID: id,
			TeamID:    principal.TeamID,
			KeyID:     principal.KeyID,
			Alias:     alias.Name,
			Provider:  route.ProviderName,
			Model:     route.Model,
			Number:    1,
			Stream:    req.Stream,
		},
	}
	defer s.record(r.Context(), x)

	ctx, cancel := s.requestContext(r.Context())
	defer cancel()
	if req.Stream {
		s.proxyStream(ctx, x)
		return
	}
	s.proxyCompletion(ctx, x)
}

func (s *Server) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if s.limits.RequestTimeout > 0 {
		return context.WithTimeout(parent, s.limits.RequestTimeout)
	}
	return context.WithCancel(parent)
}

// record reports the attempt on a context of its own: the request may already be
// canceled, and that is exactly when the record matters.
func (s *Server) record(requestCtx context.Context, x *exchange) {
	x.attempt.Duration = time.Since(x.start)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), recordTimeout)
	defer cancel()
	s.recorder.RecordAttempt(ctx, x.attempt)
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request, rc *http.ResponseController) ([]byte, *apiError) {
	if s.limits.BodyReadTimeout > 0 {
		_ = rc.SetReadDeadline(time.Now().Add(s.limits.BodyReadTimeout))
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.limits.MaxRequestBytes))
	if err == nil {
		if s.limits.BodyReadTimeout > 0 {
			// A deadline left in place would end the server's watch for the client
			// disconnecting, which is what cancels upstream work.
			_ = rc.SetReadDeadline(time.Time{})
		}
		return body, nil
	}

	// The rest of the body will never be read, so the connection can't carry
	// another request: leftover bytes would be parsed as one. The read deadline
	// stays in place because net/http tries to drain an unread body before writing
	// the response, which would otherwise wait on this client indefinitely.
	w.Header().Set("Connection", "close")

	out := &apiError{Type: errTypeInvalidRequest}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		out.Status = http.StatusRequestEntityTooLarge
		out.Code = codeRequestTooLarge
		out.Message = "the request body exceeds the size limit"
	case errors.Is(err, os.ErrDeadlineExceeded):
		out.Status = http.StatusRequestTimeout
		out.Code = codeRequestTimeout
		out.Message = "the request body was not received in time"
	default:
		out.Status = http.StatusBadRequest
		out.Code = codeRequestUnreadable
		out.Message = "the request body could not be read"
	}
	return nil, out
}

// applyOutputLimit returns a copy of req with a real output limit, because budget
// accounting needs an upper bound. A limit above the alias's maximum is rejected
// rather than quietly lowered, which would change what the client asked for.
func applyOutputLimit(req *chat.Request, alias *routing.Alias) (*chat.Request, *apiError) {
	if req.MaxOutputTokens > alias.MaxOutputTokens {
		return nil, &apiError{
			Status:  http.StatusBadRequest,
			Type:    errTypeInvalidRequest,
			Code:    codeOutputLimitExceeded,
			Param:   req.MaxOutputParam,
			Message: "the output limit exceeds this model's maximum of " + strconv.FormatInt(alias.MaxOutputTokens, 10),
		}
	}
	limited := *req
	if limited.MaxOutputTokens == 0 {
		limited.MaxOutputTokens = alias.MaxOutputTokens
	}
	return &limited, nil
}

// failAttempt records a failure with the client still uninformed, and tells the
// client if it is still there.
func (s *Server) failAttempt(x *exchange, failure *provider.Error) {
	x.attempt.ErrorKind = failure.Kind
	x.attempt.Delivery = failure.Delivery
	x.attempt.UpstreamStatus = failure.Status
	x.attempt.UpstreamCode = failure.UpstreamCode

	if x.r.Context().Err() != nil {
		x.attempt.Outcome = accounting.OutcomeClientCanceled
		return
	}
	x.attempt.Outcome = accounting.OutcomeFailed
	writeError(x.w, upstreamError(failure))
}

// providerFailure normalizes an attempt's error, and applies the reason the
// gateway itself canceled the attempt, which the adapter can't know.
func providerFailure(ctx context.Context, err error) *provider.Error {
	var failure *provider.Error
	if !errors.As(err, &failure) {
		failure = &provider.Error{Kind: provider.KindServer, Delivery: provider.DeliveryUncertain, Err: err}
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, errFirstEventTimeout) || errors.Is(cause, errStreamIdleTimeout) {
		timedOut := *failure
		timedOut.Kind = provider.KindTimeout
		return &timedOut
	}
	return failure
}

// clientWriteFailed records why a write to the client failed. Any write failure
// cancels the request's context, so only the error tells a stalled client from
// one that left.
func (x *exchange) clientWriteFailed(err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		x.attempt.Outcome = accounting.OutcomeClientStalled
		return
	}
	x.attempt.Outcome = accounting.OutcomeClientCanceled
}

func (s *Server) proxyCompletion(ctx context.Context, x *exchange) {
	completion, err := x.route.Provider.Complete(ctx, x.req, x.route.Model)
	if err != nil {
		s.failAttempt(x, providerFailure(ctx, err))
		return
	}
	x.attempt.Usage = completion.Usage
	x.attempt.OutputBytes = int64(len(completion.Content) + len(completion.Refusal))

	message := wireMessage{Role: roleAssistant}
	if completion.Content != "" || completion.Refusal == "" {
		message.Content = &completion.Content
	}
	if completion.Refusal != "" {
		message.Refusal = &completion.Refusal
	}
	body, err := encodeJSON(wireCompletion{
		ID:      completionPrefix + x.id,
		Object:  objectCompletion,
		Created: x.created,
		Model:   x.alias,
		Choices: []wireChoice{{
			Message:      message,
			FinishReason: string(completion.FinishReason),
		}},
		Usage: newWireUsage(completion.Usage),
	})
	if err != nil {
		s.failAttempt(x, &provider.Error{Kind: provider.KindServer, Delivery: provider.DeliveryConfirmed, Err: err})
		return
	}

	x.attempt.Committed = true
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(http.StatusOK)
	if _, err := x.w.Write(body); err != nil {
		x.clientWriteFailed(err)
		return
	}
	x.attempt.Outcome = accounting.OutcomeCompleted
}

// proxyStream runs a streaming attempt. Everything before the first write can
// still become an ordinary HTTP error. After it, this attempt's provider is the
// only one that will ever serve this response.
func (s *Server) proxyStream(parent context.Context, x *exchange) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	watchdog := newWatchdog(s.limits.FirstEventTimeout, func() { cancel(errFirstEventTimeout) })
	defer watchdog.stop()

	stream, err := x.route.Provider.Stream(ctx, x.req, x.route.Model)
	if err != nil {
		s.failAttempt(x, providerFailure(ctx, err))
		return
	}
	defer func() { _ = stream.Close() }()

	first, err := stream.Next()
	if errors.Is(err, io.EOF) {
		// Adapters report a finish reason as an event, so this breaks their contract.
		err = &provider.Error{
			Kind:     provider.KindProtocol,
			Delivery: provider.DeliveryConfirmed,
			Err:      errors.New("stream completed without any events"),
		}
	}
	if err != nil {
		s.failAttempt(x, providerFailure(ctx, err))
		return
	}
	watchdog.stop()
	x.attempt.TimeToFirstEvent = time.Since(x.start)

	// This is the commitment point. See ADR 0003.
	x.attempt.Committed = true
	out := newStreamWriter(x, s)
	if err := out.commit(first); err != nil {
		x.clientWriteFailed(err)
		return
	}
	x.observe(first)

	idle := newWatchdog(0, func() { cancel(errStreamIdleTimeout) })
	defer idle.stop()
	for {
		idle.reset(s.limits.StreamIdleTimeout)
		event, err := stream.Next()
		idle.stop()

		if errors.Is(err, io.EOF) {
			if err := out.finish(x.attempt.Usage); err != nil {
				x.clientWriteFailed(err)
				return
			}
			x.attempt.Outcome = accounting.OutcomeCompleted
			return
		}
		if err != nil {
			s.interruptStream(ctx, x, out, err)
			return
		}
		x.observe(event)
		if err := out.write(event); err != nil {
			x.clientWriteFailed(err)
			return
		}
	}
}

// interruptStream handles an upstream failure after commit. No other provider may
// be tried, and the stream must not appear to have succeeded.
func (s *Server) interruptStream(ctx context.Context, x *exchange, out *streamWriter, err error) {
	failure := providerFailure(ctx, err)
	x.attempt.ErrorKind = failure.Kind
	x.attempt.Delivery = failure.Delivery
	x.attempt.UpstreamStatus = failure.Status
	x.attempt.UpstreamCode = failure.UpstreamCode

	if x.r.Context().Err() != nil {
		x.attempt.Outcome = accounting.OutcomeClientCanceled
		return
	}
	x.attempt.Outcome = accounting.OutcomeInterrupted
	if err := out.writeError(streamInterrupted()); err != nil {
		s.logger.LogAttrs(ctx, slogLevelDebug, "could not deliver stream error", attrRequestID(x.id))
	}
}

// observe updates the attempt's accounting from an event. It runs whether or not
// the event reaches the client, since the cost was incurred either way.
func (x *exchange) observe(event chat.StreamEvent) {
	if event.Content != "" {
		x.attempt.TextEvents++
	}
	x.attempt.OutputBytes += int64(len(event.Content) + len(event.Refusal))
	if event.Usage != nil {
		x.attempt.Usage = event.Usage
	}
}

// watchdog runs a function if it isn't stopped in time. A zero timeout disables it.
type watchdog struct {
	timer *time.Timer
	fire  func()
}

func newWatchdog(timeout time.Duration, fire func()) *watchdog {
	w := &watchdog{fire: fire}
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, fire)
	}
	return w
}

func (w *watchdog) reset(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	if w.timer == nil {
		w.timer = time.AfterFunc(timeout, w.fire)
		return
	}
	w.timer.Reset(timeout)
}

func (w *watchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}
