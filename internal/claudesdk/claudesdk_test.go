package claudesdk

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

func TestRunSuccessPassesThroughSessionAndSoul(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		var got Request
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("request body is not valid JSON: %v", err)
		}
		if got.Prompt != "what changed?" {
			t.Errorf("prompt = %q", got.Prompt)
		}
		if got.SessionID != "sess-1" {
			t.Errorf("session_id = %q, want sess-1", got.SessionID)
		}
		if got.SystemPrompt != "be brief" {
			t.Errorf("system_prompt = %q, want the SOUL text", got.SystemPrompt)
		}
		if got.Cwd != "/tmp/ws" {
			t.Errorf("cwd = %q", got.Cwd)
		}
		json.NewEncoder(w).Encode(Response{Result: "nothing yet", SessionID: "sess-1", NumTurns: 2, TotalCostUSD: 0.01})
	})

	out, err := Run(Request{Prompt: "what changed?", SessionID: "sess-1", SystemPrompt: "be brief", Cwd: "/tmp/ws"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Result != "nothing yet" {
		t.Errorf("result = %q", out.Result)
	}
	if out.SessionID != "sess-1" {
		t.Errorf("session id = %q, want it echoed back for the next turn", out.SessionID)
	}
}

// A brand-new conversation sends no session id and learns the one the SDK made.
func TestRunReportsNewSessionID(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got Request
		json.Unmarshal(body, &got)
		if got.SessionID != "" {
			t.Errorf("a first turn must not send a session id, got %q", got.SessionID)
		}
		json.NewEncoder(w).Encode(Response{Result: "hi", SessionID: "fresh-uuid"})
	})
	out, err := Run(Request{Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.SessionID != "fresh-uuid" {
		t.Errorf("session id = %q, want fresh-uuid", out.SessionID)
	}
}

func TestRunServiceError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Error: "claude binary not found"})
	})
	if _, err := Run(Request{Prompt: "hi"}); err == nil {
		t.Fatal("expected error when the service reports one")
	}
}

// is_error is how the SDK envelope reports its own failures (auth, budget).
func TestRunSDKIsError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{IsError: true, Result: "Invalid API key / not logged in"})
	})
	_, err := Run(Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error when is_error is set")
	}
}

func TestRunHTTPError(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	if _, err := Run(Request{Prompt: "hi"}); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestRunRejectsEmptyPrompt(t *testing.T) {
	if _, err := Run(Request{Prompt: "   "}); err == nil {
		t.Fatal("expected empty prompt to be rejected")
	}
}

func TestRunUnreachableService(t *testing.T) {
	old := ServiceURL
	ServiceURL = "http://127.0.0.1:1/claude" // port 1: nothing listens there
	t.Cleanup(func() { ServiceURL = old })
	if _, err := Run(Request{Prompt: "hello"}); err == nil {
		t.Fatal("expected error when nothing is listening")
	}
}

func TestRunBadJSON(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json at all")
	})
	if _, err := Run(Request{Prompt: "hi"}); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
