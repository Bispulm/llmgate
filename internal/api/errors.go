package api

import (
	"encoding/json"
	"net/http"
)

// Error types, as documented in docs/compatibility.md.
const (
	errTypeInvalidRequest = "invalid_request_error"
)

// apiError is an error returned to a client.
type apiError struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e.envelope())
}
