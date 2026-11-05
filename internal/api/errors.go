package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Bispulm/llmgate/internal/chat"
	"github.com/Bispulm/llmgate/internal/provider"
)

// Error types and codes, as documented in docs/compatibility.md.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeAuthentication = "authentication_error"
	errTypeRateLimit      = "rate_limit_error"
	errTypeUpstream       = "upstream_error"

	codeInvalidAPIKey         = "invalid_api_key"
	codeModelNotFound         = "model_not_found"
	codeMethodNotAllowed      = "method_not_allowed"
	codeRequestTooLarge       = "request_too_large"
	codeRequestTimeout        = "request_timeout"
	codeRequestUnreadable     = "request_unreadable"
	codeOutputLimitExceeded   = "output_limit_exceeded"
	codeUpstreamRateLimited   = "upstream_rate_limited"
	codeUpstreamOverloaded    = "upstream_overloaded"
	codeUpstreamTimeout       = "upstream_timeout"
	codeUpstreamFailed        = "upstream_failed"
	codeUpstreamProtocolError = "upstream_protocol_error"
	codeUpstreamRejected      = "upstream_rejected"
	codeUpstreamMisconfigured = "upstream_misconfigured"
	codeUpstreamAccountLimit  = "upstream_account_limit"
	codeStreamInterrupted     = "stream_interrupted"
	codeNoCapableRoute        = "no_capable_route"
	codeNoRouteAvailable      = "no_route_available"
)

// apiError is an error returned to a client.
type apiError struct {
	Status     int
	Type       string
	Code       string
	Param      string
	Message    string
	RetryAfter time.Duration
}

// errorEnvelope is the OpenAI error format. param and code are nullable there,
// so an empty value is written as null rather than an empty string.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func (e *apiError) envelope() errorEnvelope {
	return errorEnvelope{Error: errorBody{
		Message: e.Message,
		Type:    e.Type,
		Param:   nullable(e.Param),
		Code:    nullable(e.Code),
	}}
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeError(w http.ResponseWriter, e *apiError) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(e.RetryAfter.Seconds())), 10))
	}
	w.WriteHeader(e.Status)
	body, err := encodeJSON(e.envelope())
	if err == nil {
		_, _ = w.Write(body)
	}
}

// encodeJSON encodes without the HTML escaping encoding/json applies by default,
// which would inflate generated text containing <, >, or &.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encoding response: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func validationError(err error) *apiError {
	var ve *chat.ValidationError
	if errors.As(err, &ve) {
		return &apiError{
			Status:  http.StatusBadRequest,
			Type:    errTypeInvalidRequest,
			Code:    ve.Code,
			Param:   ve.Param,
			Message: ve.Message,
		}
	}
	return &apiError{
		Status:  http.StatusBadRequest,
		Type:    errTypeInvalidRequest,
		Code:    chat.CodeInvalidJSON,
		Message: "the request could not be parsed",
	}
}

// upstreamError turns a failed attempt into a client error. Provider messages are
// never forwarded because they can contain account details. The status reflects
// who can fix the problem: a provider rejecting the gateway's credentials is the
// operator's problem, and must not read as the client's key being wrong.
func upstreamError(e *provider.Error) *apiError {
	out := &apiError{Status: http.StatusBadGateway, Type: errTypeUpstream}
	switch e.Kind {
	case provider.KindRateLimited:
		out.Status = http.StatusTooManyRequests
		out.Type = errTypeRateLimit
		out.Code = codeUpstreamRateLimited
		out.Message = "the provider is rate limiting requests"
		out.RetryAfter = e.RetryAfter
	case provider.KindOverloaded:
		out.Status = http.StatusServiceUnavailable
		out.Code = codeUpstreamOverloaded
		out.Message = "the provider reports that it is overloaded"
		out.RetryAfter = e.RetryAfter
	case provider.KindTimeout:
		out.Status = http.StatusGatewayTimeout
		out.Code = codeUpstreamTimeout
		out.Message = "the provider did not respond in time"
	case provider.KindInvalidRequest:
		// Retrying won't help, and SDKs automatically retry a 5xx.
		out.Status = http.StatusBadRequest
		out.Type = errTypeInvalidRequest
		out.Code = codeUpstreamRejected
		out.Message = "the provider rejected the request"
		if e.UpstreamCode != "" {
			out.Message += " (" + e.UpstreamCode + ")"
		}
	case provider.KindAuth, provider.KindNotFound:
		out.Code = codeUpstreamMisconfigured
		out.Message = "the gateway's provider configuration was rejected"
	case provider.KindBilling:
		out.Code = codeUpstreamAccountLimit
		out.Message = "the gateway's provider account is out of credit or over a limit"
	case provider.KindProtocol:
		out.Code = codeUpstreamProtocolError
		out.Message = "the provider sent a malformed response"
	default:
		out.Code = codeUpstreamFailed
		out.Message = "the provider request failed"
	}
	return out
}

// streamInterrupted is the error event sent when a committed stream fails.
func streamInterrupted() *apiError {
	return &apiError{
		Type:    errTypeUpstream,
		Code:    codeStreamInterrupted,
		Message: "the provider stream failed after the response started, and this response is incomplete",
	}
}
