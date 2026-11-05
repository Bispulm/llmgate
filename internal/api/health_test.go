package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthEndpoints(t *testing.T) {
	s := New(Options{})

	if rec := serve(s, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("readyz before draining = %d", rec.Code)
	}

	s.SetDraining()

	if rec := serve(s, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz while draining = %d, want 200: the process is still alive", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining = %d, want 503", rec.Code)
	}
}

func TestUnknownPathsUseTheErrorEnvelope(t *testing.T) {
	rec := serve(New(Options{}), http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("the error has no X-Request-Id")
	}
	var got struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Message == "" || got.Error.Type != "invalid_request_error" || *got.Error.Code != "not_found" {
		t.Fatalf("error = %+v", got.Error)
	}
}
