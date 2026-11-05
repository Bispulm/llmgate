package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestDevKeyAuthenticator(t *testing.T) {
	a := NewDevKey("correct-key")

	p, err := a.Authenticate(context.Background(), "correct-key")
	if err != nil {
		t.Fatal(err)
	}
	if p.TeamID == "" || p.KeyID == "" {
		t.Fatalf("principal = %+v", p)
	}

	for _, wrong := range []string{"", "wrong", "correct-ke", "correct-key "} {
		if _, err := a.Authenticate(context.Background(), wrong); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("token %q: error = %v", wrong, err)
		}
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"Bearer  abc", ""},
		{"Basic abc", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"", ""},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Authorization", tt.header)
		}
		if got := BearerToken(h); got != tt.want {
			t.Errorf("header %q: got %q, want %q", tt.header, got, tt.want)
		}
	}
}
