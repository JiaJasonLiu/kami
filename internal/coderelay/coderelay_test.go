package coderelay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withServer points ServiceURL at a test server for the duration of a test.
func withServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := ServiceURL
	ServiceURL = srv.URL
	t.Cleanup(func() { ServiceURL = old })
}

func TestRelaySuccess(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var req executeRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		if req.Prompt != "run the tests" {
			t.Errorf("prompt = %q, want %q", req.Prompt, "run the tests")
		}
		json.NewEncoder(w).Encode(executeResponse{Output: "ok: 12 passed"})
	})

	out, err := RelayToCodeService("run the tests")
	if err != nil {
		t.Fatalf("RelayToCodeService: %v", err)
	}
	if out != "ok: 12 passed" {
		t.Errorf("output = %q, want %q", out, "ok: 12 passed")
	}
}

func TestRelayServiceError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(executeResponse{Error: "compiler exploded"})
	})
	if _, err := RelayToCodeService("build it"); err == nil {
		t.Fatal("expected error when service reports one")
	}
}

func TestRelayHTTPError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	if _, err := RelayToCodeService("build it"); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestRelayRejectsEmptyPrompt(t *testing.T) {
	if _, err := RelayToCodeService("   "); err == nil {
		t.Fatal("expected empty prompt to be rejected")
	}
}

func TestRelayUnreachableService(t *testing.T) {
	old := ServiceURL
	ServiceURL = "http://127.0.0.1:1/execute" // port 1: nothing listens there
	t.Cleanup(func() { ServiceURL = old })
	if _, err := RelayToCodeService("hello"); err == nil {
		t.Fatal("expected error when nothing is listening")
	}
}
