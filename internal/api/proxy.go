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
	"github.com/Bispulm/llmgate/internal/limits"
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

	// capacityRetryAfter is a short hint, because a slot could free at any moment.
	capacityRetryAfter = time.Second
)

// Causes for canceling an attempt, which separate a timeout from a client leaving.
var (
	errFirstEventTimeout = errors.New("no content arrived within the first-event timeout")
	errStreamIdleTimeout = errors.New("the stream stalled past the idle timeout")
)

// exchange is one logical request, which may make several upstream attempts.
type exchange struct {
	w  http.ResponseWriter
	r  *http.Request
	rc *http.ResponseController

	id        string
	alias     string
	created   int64
	start     time.Time
	req       *chat.Request
	principal auth.Principal
	attempts  int
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
	body, apiErr := s.readBody(w, r)
	// Reading the body can use up the deadline set when the request arrived.
	s.extendWriteDeadline(rc)
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
	plan := alias.Plan(limited)
	if plan.NoCapableRoute() {
		writeError(w, noCapableRoute(limited))
		return
	}

	x := &exchange{
		w:         w,
		r:         r,
		rc:        rc,
		id:        id,
		alias:     alias.Name,
		created:   start.Unix(),
		start:     start,
		req:       limited,
		principal: principal,
	}
	ctx, cancel := s.requestContext(r.Context())
	defer cancel()
	s.runAttempts(ctx, x, alias, plan)
}

// noCapableRoute names the parameter that no route can honor. The client can fix
// this by removing it, which is why it's a 400 and not a provider outage.
func noCapableRoute(req *chat.Request) *apiError {
	param := "top_p"
	if req.Temperature != nil {
		param = "temperature"
	}
	return &apiError{
		Status:  http.StatusBadRequest,
		Type:    errTypeInvalidRequest,
		Code:    codeNoCapableRoute,
		Param:   param,
		Message: "no route for this model can honor " + param,
	}
}

func (s *Server) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if s.limits.RequestTimeout > 0 {
		return context.WithTimeout(parent, s.limits.RequestTimeout)
	}
	return context.WithCancel(parent)
}

// runAttempts tries routes until one serves the response, or until the attempt
// limit, the deadline, or the kind of failure says to stop. Reaching the end of this
// loop means nothing has been sent to the client. See ADR 0004.
func (s *Server) runAttempts(ctx context.Context, x *exchange, alias *routing.Alias, plan *routing.Plan) {
	var last *provider.Error
	lastRoute := ""
	reason := accounting.ReasonPrimary

	for x.attempts < alias.Attempts() {
		selected, ok := plan.Next()
		if !ok {
			// Every route was tried or is unusable. A provider's hint is only honored here,
			// and only when it fits within the attempt limit and the deadline.
			wait := plan.EarliestRetry()
			if last == nil || wait <= 0 || !fitsDeadline(ctx, wait) || !sleep(ctx, wait) {
				break
			}
			plan.Rewind()
			reason = accounting.ReasonRetryAfter
			continue
		}

		// Capacity comes before the attempt, because a route that was never called
		// isn't an attempt, costs nothing, and says nothing about the route's health.
		release, err := selected.Route.Limiter.Acquire(ctx, x.principal.TeamID)
		if err != nil {
			selected.Abandon()
			plan.NoteSaturated()
			if errors.Is(err, limits.ErrShed) {
				continue
			}
			break
		}

		x.attempts++
		lastRoute = routeName(selected.Route)
		failure := s.attempt(ctx, x, selected, reason)
		release()
		if failure == nil {
			return
		}
		last = failure
		reason = accounting.ReasonFallback
		if !routing.Classify(failure).Failover || ctx.Err() != nil {
			break
		}
	}
	s.failRequest(x, plan, last, lastRoute)
}

func routeName(route routing.Route) string { return route.ProviderName + "/" + route.Model }

func fitsDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
}

// sleep waits for d, reporting false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// attempt makes one upstream attempt and records it. A nil result means the response
// is finished with, even if it failed partway: only a failure from before commit is
// returned, because that's the only kind another route could still rescue.
func (s *Server) attempt(ctx context.Context, x *exchange, selected *routing.Selected, reason accounting.Reason) *provider.Error {
	start := time.Now()
	record := accounting.Attempt{
		RequestID: x.id,
		TeamID:    x.principal.TeamID,
		KeyID:     x.principal.KeyID,
		Alias:     x.alias,
		Provider:  selected.Route.ProviderName,
		Model:     selected.Route.Model,
		Number:    x.attempts,
		Stream:    x.req.Stream,
		Reason:    reason,
	}
	defer func() {
		record.Duration = time.Since(start)
		s.record(ctx, record)
	}()

	if x.req.Stream {
		return s.streamAttempt(ctx, x, selected, &record)
	}
	return s.completionAttempt(ctx, x, selected, &record)
}

// record reports an attempt on a context of its own: the request may already be
// canceled, and that is exactly when the record matters.
func (s *Server) record(requestCtx context.Context, record accounting.Attempt) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), recordTimeout)
	defer cancel()
	s.recorder.RecordAttempt(ctx, record)
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *apiError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.limits.MaxRequestBytes))
	if err == nil {
		return body, nil
	}

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

// noteFailure records a failure from before commit, and reports it to routing. A
// client leaving says nothing about the route, whatever the failure looks like.
func noteFailure(x *exchange, selected *routing.Selected, record *accounting.Attempt, failure *provider.Error) {
	record.ErrorKind = failure.Kind
	record.Delivery = failure.Delivery
	record.UpstreamStatus = failure.Status
	record.UpstreamCode = failure.UpstreamCode

	if x.r.Context().Err() != nil {
		record.Outcome = accounting.OutcomeClientCanceled
		selected.Abandon()
		return
	}
	record.Outcome = accounting.OutcomeFailed
	selected.Finish(failure)
}

// failRequest answers a request that no attempt served. Nothing has been sent yet.
func (s *Server) failRequest(x *exchange, plan *routing.Plan, last *provider.Error, lastRoute string) {
	if x.r.Context().Err() != nil {
		return // Nobody is left to tell.
	}
	s.extendWriteDeadline(x.rc)
	header := x.w.Header()
	header.Set(headerAttempts, strconv.Itoa(x.attempts))
	if lastRoute != "" {
		header.Set(headerRoute, lastRoute)
	}
	if last != nil {
		writeError(x.w, upstreamError(last))
		return
	}
	if plan.Skipped()[routing.SkipSaturated] > 0 {
		writeError(x.w, &apiError{
			Status:     http.StatusServiceUnavailable,
			Type:       errTypeUpstream,
			Code:       codeCapacityExceeded,
			Message:    "the gateway has no capacity for this model right now",
			RetryAfter: capacityRetryAfter,
		})
		return
	}
	writeError(x.w, &apiError{
		Status:     http.StatusServiceUnavailable,
		Type:       errTypeUpstream,
		Code:       codeNoRouteAvailable,
		Message:    "no route for this model is available right now",
		RetryAfter: plan.RetryAfter(),
	})
}

// providerFailure normalizes an attempt's error, and applies the reason the gateway
// itself canceled the attempt, which the adapter can't know.
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
// cancels the request's context, so only the error tells a stalled client from one
// that left. Neither says anything about the route.
func clientWriteFailed(selected *routing.Selected, record *accounting.Attempt, err error) {
	selected.Abandon()
	if errors.Is(err, os.ErrDeadlineExceeded) {
		record.Outcome = accounting.OutcomeClientStalled
		return
	}
	record.Outcome = accounting.OutcomeClientCanceled
}

func (x *exchange) setRouteHeaders(selected *routing.Selected) {
	header := x.w.Header()
	header.Set(headerRoute, routeName(selected.Route))
	header.Set(headerAttempts, strconv.Itoa(x.attempts))
}

func (s *Server) completionAttempt(ctx context.Context, x *exchange, selected *routing.Selected, record *accounting.Attempt) *provider.Error {
	completion, err := selected.Route.Provider.Complete(ctx, x.req, selected.Route.Model)
	if err != nil {
		failure := providerFailure(ctx, err)
		noteFailure(x, selected, record, failure)
		return failure
	}
	record.Usage = completion.Usage
	record.OutputBytes = int64(len(completion.Content) + len(completion.Refusal))
	selected.Finish(nil)

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
		failure := &provider.Error{Kind: provider.KindServer, Delivery: provider.DeliveryConfirmed, Err: err}
		record.ErrorKind = failure.Kind
		record.Delivery = failure.Delivery
		record.Outcome = accounting.OutcomeFailed
		return failure
	}

	record.Committed = true
	s.extendWriteDeadline(x.rc)
	x.setRouteHeaders(selected)
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(http.StatusOK)
	if _, err := x.w.Write(body); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			record.Outcome = accounting.OutcomeClientStalled
		} else {
			record.Outcome = accounting.OutcomeClientCanceled
		}
		return nil
	}
	record.Outcome = accounting.OutcomeCompleted
	return nil
}

// streamAttempt runs one streaming attempt. Everything before the first write can
// still become another attempt or an ordinary HTTP error. After it, this attempt's
// provider is the only one that will ever serve this response, which is why every
// path past the commitment point returns nil.
func (s *Server) streamAttempt(parent context.Context, x *exchange, selected *routing.Selected, record *accounting.Attempt) *provider.Error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	watchdog := newWatchdog(s.limits.FirstEventTimeout, func() { cancel(errFirstEventTimeout) })
	defer watchdog.stop()

	stream, err := selected.Route.Provider.Stream(ctx, x.req, selected.Route.Model)
	if err != nil {
		failure := providerFailure(ctx, err)
		noteFailure(x, selected, record, failure)
		return failure
	}
	defer func() { _ = stream.Close() }()

	first, err := awaitFirstOutput(stream, record)
	if errors.Is(err, io.EOF) {
		// Adapters report a finish reason as an event, so this breaks their contract.
		err = &provider.Error{
			Kind:     provider.KindProtocol,
			Delivery: provider.DeliveryConfirmed,
			Err:      errors.New("stream completed without any events"),
		}
	}
	if err != nil {
		failure := providerFailure(ctx, err)
		noteFailure(x, selected, record, failure)
		return failure
	}
	watchdog.stop()
	record.TimeToFirstEvent = time.Since(x.start)

	// This is the commitment point. See ADR 0003.
	record.Committed = true
	x.setRouteHeaders(selected)
	out := newStreamWriter(x, s)
	if err := out.commit(first); err != nil {
		clientWriteFailed(selected, record, err)
		return nil
	}

	idle := newWatchdog(s.limits.StreamIdleTimeout, func() { cancel(errStreamIdleTimeout) })
	defer idle.stop()
	for {
		event, err := stream.Next()
		if errors.Is(err, io.EOF) {
			idle.stop()
			selected.Finish(nil)
			if err := out.finish(record.Usage); err != nil {
				clientWriteFailed(selected, record, err)
				return nil
			}
			record.Outcome = accounting.OutcomeCompleted
			return nil
		}
		if err != nil {
			idle.stop()
			s.interruptStream(ctx, x, selected, record, out, err)
			return nil
		}
		observe(record, event)
		if !event.HasOutput() {
			continue // Not progress, so the idle timer keeps running.
		}

		// A slow client isn't the upstream stalling, so writing doesn't count as idle.
		idle.stop()
		if err := out.write(event); err != nil {
			clientWriteFailed(selected, record, err)
			return nil
		}
		idle.reset(s.limits.StreamIdleTimeout)
	}
}

// awaitFirstOutput reads until an event with output arrives, which is what may commit
// the response. The first-event timeout bounds how long this can take, however many
// usage reports arrive in the meantime.
func awaitFirstOutput(stream provider.Stream, record *accounting.Attempt) (chat.StreamEvent, error) {
	for {
		event, err := stream.Next()
		if err != nil {
			return chat.StreamEvent{}, err
		}
		observe(record, event)
		if event.HasOutput() {
			return event, nil
		}
	}
}

// interruptStream handles an upstream failure after commit. No other provider may be
// tried, and the stream must not appear to have succeeded.
func (s *Server) interruptStream(ctx context.Context, x *exchange, selected *routing.Selected, record *accounting.Attempt, out *streamWriter, err error) {
	failure := providerFailure(ctx, err)
	record.ErrorKind = failure.Kind
	record.Delivery = failure.Delivery
	record.UpstreamStatus = failure.Status
	record.UpstreamCode = failure.UpstreamCode

	if x.r.Context().Err() != nil {
		record.Outcome = accounting.OutcomeClientCanceled
		selected.Abandon()
		return
	}
	record.Outcome = accounting.OutcomeInterrupted
	selected.Finish(failure)
	if err := out.writeError(streamInterrupted()); err != nil {
		s.logger.LogAttrs(ctx, slogLevelDebug, "could not deliver stream error", attrRequestID(x.id))
	}
}

// observe updates an attempt's accounting from an event. It runs whether or not the
// event reaches the client, since the cost was incurred either way.
func observe(record *accounting.Attempt, event chat.StreamEvent) {
	if event.Content != "" {
		record.TextEvents++
	}
	record.OutputBytes += int64(len(event.Content) + len(event.Refusal))
	if event.Usage != nil {
		record.Usage = event.Usage
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
