package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"kami-gateway/internal/claudesdk"
)

// newSDKGateway points the gateway at a fake sidecar with a temp home.
func newSDKGateway(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{}
	cfg.Provider = claudeSDKProvider
	activeAgent = defaultAgent
	currentTopic = 0
	claudeSessions = map[string]string{}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.ClaudeSDKURL = srv.URL
	old := claudesdk.ServiceURL
	claudesdk.ServiceURL = srv.URL
	t.Cleanup(func() { claudesdk.ServiceURL = old })
	return srv
}

// The provider needs no API key — that's the whole point of session usage.
func TestClaudeSDKActiveModelNeedsNoKey(t *testing.T) {
	resetProviderCfg()
	cfg.Provider = claudeSDKProvider
	mc, err := activeModel()
	if err != nil {
		t.Fatalf("claude-sdk must resolve without any API key, got %v", err)
	}
	if mc.kind != "claude-sdk" {
		t.Errorf("kind = %q, want claude-sdk", mc.kind)
	}
	if mc.apiKey != "" {
		t.Errorf("claude-sdk must never carry an API key, got %q", mc.apiKey)
	}
	if mc.baseURL != defaultClaudeSDKURL {
		t.Errorf("baseURL = %q, want the loopback default", mc.baseURL)
	}
}

// A turn goes out to the sidecar and the reply comes back through the agent loop.
func TestClaudeSDKTurnThroughAgentLoop(t *testing.T) {
	var gotReq claudesdk.Request
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotReq)
		json.NewEncoder(w).Encode(claudesdk.Response{Result: "done", SessionID: "s-1"})
	})

	reply := handleUserMessage("summarise the repo")
	if reply != "done" {
		t.Errorf("reply = %q, want done", reply)
	}
	if gotReq.Prompt != "summarise the repo" {
		t.Errorf("prompt = %q", gotReq.Prompt)
	}
	if gotReq.SystemPrompt == "" {
		t.Error("SOUL.md should be forwarded as the system prompt")
	}
	if gotReq.Cwd == "" {
		t.Error("the agent's sandboxed workspace should be forwarded as cwd")
	}
	// The session id must be remembered for the next turn.
	if got := getClaudeSession(claudeSessionKey(defaultAgent, 0)); got != "s-1" {
		t.Errorf("stored session = %q, want s-1", got)
	}
}

// The second turn must resume the session the first one created.
func TestClaudeSDKResumesSession(t *testing.T) {
	var seen []string
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req claudesdk.Request
		json.Unmarshal(body, &req)
		seen = append(seen, req.SessionID)
		json.NewEncoder(w).Encode(claudesdk.Response{Result: "ok", SessionID: "s-9"})
	})

	handleUserMessage("first")
	handleUserMessage("second")
	if len(seen) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(seen))
	}
	if seen[0] != "" {
		t.Errorf("first turn sent session %q, want empty", seen[0])
	}
	if seen[1] != "s-9" {
		t.Errorf("second turn sent session %q, want s-9 (resumed)", seen[1])
	}
}

// Separate topics must not share one SDK conversation.
func TestClaudeSDKSessionsArePerTopic(t *testing.T) {
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req claudesdk.Request
		json.Unmarshal(body, &req)
		id := "topic-a"
		if req.Cwd != "" && req.SessionID == "" {
			id = "fresh"
		}
		json.NewEncoder(w).Encode(claudesdk.Response{Result: "ok", SessionID: id + "-sess"})
	})

	currentTopic = 0
	handleUserMessage("hello")
	currentTopic = 42
	handleUserMessage("hello")

	if getClaudeSession(claudeSessionKey(defaultAgent, 0)) == "" {
		t.Error("topic 0 should have its own session")
	}
	if _, ok := claudeSessions[claudeSessionKey(defaultAgent, 42)]; !ok {
		t.Error("topic 42 should have its own session entry")
	}
	if len(claudeSessions) != 2 {
		t.Errorf("expected 2 independent sessions, got %d", len(claudeSessions))
	}
	currentTopic = 0
}

// /new must clear the SDK session too, or the model keeps remembering.
func TestNewCommandClearsSDKSession(t *testing.T) {
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(claudesdk.Response{Result: "ok", SessionID: "s-x"})
	})
	handleUserMessage("hi")
	if getClaudeSession(claudeSessionKey(defaultAgent, 0)) == "" {
		t.Fatal("precondition: a session should exist")
	}
	handleUserMessage("/new")
	if got := getClaudeSession(claudeSessionKey(defaultAgent, 0)); got != "" {
		t.Errorf("/new left session %q behind", got)
	}
}

// Sessions survive a gateway restart.
func TestClaudeSessionsPersist(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	claudeSessions = map[string]string{}
	setClaudeSession("kami:0", "persisted-id")

	claudeSessions = map[string]string{}
	loadClaudeSessions()
	if got := getClaudeSession("kami:0"); got != "persisted-id" {
		t.Errorf("after reload session = %q, want persisted-id", got)
	}
}

// A dead session id is dropped so the next message can start clean.
func TestStaleSessionIsDropped(t *testing.T) {
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(claudesdk.Response{Error: "No conversation found with session ID abc"})
	})
	setClaudeSession(claudeSessionKey(defaultAgent, 0), "abc")
	reply := handleUserMessage("hello")
	if reply == "" {
		t.Fatal("expected an error reply, not an empty one")
	}
	if got := getClaudeSession(claudeSessionKey(defaultAgent, 0)); got != "" {
		t.Errorf("stale session %q should have been dropped", got)
	}
}

// A sidecar failure must surface as a message, never a panic.
func TestClaudeSDKErrorSurfacesAsReply(t *testing.T) {
	newSDKGateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(claudesdk.Response{IsError: true, Result: "not logged in"})
	})
	reply := handleUserMessage("hello")
	if reply[:3] != "⚠️"[:3] {
		t.Errorf("expected a warning reply, got %q", reply)
	}
}

func TestClaudeCommandSwitchesAndRestoresProvider(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{Provider: "gemini"}
	previousProvider = ""

	if out := handleClaudeCommand("/claude"); out == "" {
		t.Fatal("/claude returned empty")
	}
	if cfg.Provider != claudeSDKProvider {
		t.Errorf("provider = %q, want claude-sdk", cfg.Provider)
	}
	if out := handleClaudeCommand("/claude off"); out == "" {
		t.Fatal("/claude off returned empty")
	}
	if cfg.Provider != "gemini" {
		t.Errorf("after /claude off provider = %q, want gemini restored", cfg.Provider)
	}
}

func TestClaudeCommandModelAndStatus(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{}
	if out := handleClaudeCommand("/claude model opus"); out == "" {
		t.Fatal("empty reply")
	}
	if cfg.ClaudeSDKModel != "opus" {
		t.Errorf("model = %q, want opus", cfg.ClaudeSDKModel)
	}
	if out := handleClaudeCommand("/claude status"); out == "" {
		t.Fatal("/claude status returned empty")
	}
	if out := handleClaudeCommand("/claude bogus"); out == "" {
		t.Fatal("unknown subcommand should still reply")
	}
}

// The sidecar can spend the operator's subscription, so it must stay local.
func TestClaudeURLMustBeLoopback(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{}
	if out := handleClaudeCommand("/claude url http://evil.example.com/claude"); out == "" {
		t.Fatal("empty reply")
	}
	if cfg.ClaudeSDKURL != "" {
		t.Errorf("a remote sidecar URL must be refused, but got %q", cfg.ClaudeSDKURL)
	}
	if out := handleClaudeCommand("/claude url http://127.0.0.1:9999/claude"); out == "" {
		t.Fatal("empty reply")
	}
	if cfg.ClaudeSDKURL != "http://127.0.0.1:9999/claude" {
		t.Errorf("loopback URL should be accepted, got %q", cfg.ClaudeSDKURL)
	}
	// set_config enforces the same rule.
	if _, err := tSetConfig(map[string]interface{}{"key": "claude_sdk_url", "value": "http://10.0.0.5/claude"}); err == nil {
		t.Error("set_config should refuse a non-loopback sidecar URL")
	}
}

func TestSetConfigAcceptsClaudeSDKProvider(t *testing.T) {
	home = t.TempDir()
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	cfg = Config{}
	if _, err := tSetConfig(map[string]interface{}{"key": "provider", "value": "claude-sdk"}); err != nil {
		t.Fatalf("set_config provider=claude-sdk: %v", err)
	}
	if cfg.Provider != claudeSDKProvider {
		t.Errorf("provider = %q", cfg.Provider)
	}
}

func TestLastUserText(t *testing.T) {
	req := gRequest{Contents: []gContent{
		{Role: "user", Parts: []gPart{{Text: "old"}}},
		{Role: "model", Parts: []gPart{{Text: "reply"}}},
		{Role: "user", Parts: []gPart{{Text: "newest"}}},
	}}
	if got := lastUserText(req); got != "newest" {
		t.Errorf("lastUserText = %q, want newest", got)
	}
	// A trailing tool-result turn has no text; fall back to the real user turn.
	req.Contents = append(req.Contents, gContent{Role: "user", Parts: []gPart{{FunctionResponse: &gFunctionResp{Name: "x"}}}})
	if got := lastUserText(req); got != "newest" {
		t.Errorf("lastUserText with trailing tool result = %q, want newest", got)
	}
}
