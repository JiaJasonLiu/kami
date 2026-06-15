package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSafePath(t *testing.T) {
	home = "/tmp/kamitest"
	good := []string{"notes.txt", "a/b/c.md", "./x.txt"}
	for _, g := range good {
		if _, err := safePath(g); err != nil {
			t.Errorf("expected %q allowed, got %v", g, err)
		}
	}
	bad := []string{"../escape.txt", "../../etc/passwd", "/etc/passwd", "a/../../b", ""}
	for _, b := range bad {
		if _, err := safePath(b); err == nil {
			t.Errorf("expected %q rejected, but it was allowed", b)
		}
	}
}

func TestChunk(t *testing.T) {
	if got := chunk("hello", 4000); len(got) != 1 {
		t.Errorf("short string should be 1 chunk, got %d", len(got))
	}
	big := make([]byte, 9000)
	for i := range big {
		big[i] = 'x'
	}
	if got := chunk(string(big), 4000); len(got) != 3 {
		t.Errorf("9000 bytes / 4000 should be 3 chunks, got %d", len(got))
	}
}

func TestScaffoldAndCommands(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	// default tools.json must parse and yield enabled declarations
	decls, err := enabledDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 10 {
		t.Errorf("expected 10 enabled tools, got %d", len(decls))
	}
	// offline commands shouldn't touch the network
	if got := handleUserMessage("/help"); got == "" {
		t.Error("/help returned empty")
	}
	if got := handleUserMessage("/new"); got == "" {
		t.Error("/new returned empty")
	}
	// write_file then read_file round-trips inside the workspace
	if _, err := tWriteFile(map[string]interface{}{"path": "note.txt", "content": "hi"}); err != nil {
		t.Fatal(err)
	}
	got, err := tReadFile(map[string]interface{}{"path": "note.txt"})
	if err != nil || got != "hi" {
		t.Errorf("round-trip failed: %q %v", got, err)
	}
	// write_tools rejects invalid json
	if _, err := tWriteTools(map[string]interface{}{"content": "{not json"}); err == nil {
		t.Error("expected invalid tools.json to be rejected")
	}
}

func TestTrimHistory(t *testing.T) {
	// build 200 user/model text turns
	var h []gContent
	for i := 0; i < 200; i++ {
		h = append(h, gContent{Role: "user", Parts: []gPart{{Text: "hello there friend"}}})
		h = append(h, gContent{Role: "model", Parts: []gPart{{Text: "hi back to you"}}})
	}
	out := trimHistory(h)
	if len(out) > maxHistoryEntries {
		t.Errorf("trim left %d entries, over cap %d", len(out), maxHistoryEntries)
	}
	if !isUserText(out[0]) {
		t.Errorf("trimmed history must start on a user-text turn, got role=%q", out[0].Role)
	}
}

func TestTrimNeverOrphansToolPair(t *testing.T) {
	// a tool sequence: user text, model functionCall, user functionResponse
	mkPad := func() gContent { return gContent{Role: "user", Parts: []gPart{{Text: strings_repeat("x", 2000)}}} }
	var h []gContent
	for i := 0; i < 40; i++ {
		h = append(h, mkPad())
	}
	h = append(h,
		gContent{Role: "user", Parts: []gPart{{Text: "do a thing"}}},
		gContent{Role: "model", Parts: []gPart{{FunctionCall: &gFunctionCall{Name: "read_file"}}}},
		gContent{Role: "user", Parts: []gPart{{FunctionResponse: &gFunctionResp{Name: "read_file", Response: map[string]interface{}{"result": "ok"}}}}},
	)
	out := trimHistory(h)
	// first kept entry must not be a bare functionResponse
	for _, p := range out[0].Parts {
		if p.FunctionResponse != nil {
			t.Fatal("trimmed history starts with an orphaned functionResponse")
		}
	}
}

func strings_repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestCallGeminiRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":503,"status":"UNAVAILABLE","message":"try later"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)
	}))
	defer srv.Close()

	geminiBaseURL = srv.URL
	geminiBackoffBase = time.Millisecond
	cfg.GeminiModel = "test"
	cfg.GeminiAPIKey = "k"

	resp, err := callGemini(gRequest{Contents: []gContent{{Role: "user", Parts: []gPart{{Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content.Parts[0].Text != "hi" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCallGeminiNonRetryable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad key"}}`)
	}))
	defer srv.Close()

	geminiBaseURL = srv.URL
	geminiBackoffBase = time.Millisecond
	if _, err := callGemini(gRequest{}); err == nil {
		t.Fatal("expected error on 400")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("400 should not retry, but made %d calls", calls)
	}
}
