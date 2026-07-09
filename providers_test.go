package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// resetProviderCfg clears provider config between tests so leftover globals
// from another test can't leak in.
func resetProviderCfg() {
	cfg = Config{}
	modelBackoffBase = time.Millisecond
}

// sampleReq is a small request exercising system prompt, a plain user turn, a
// model tool call, and the matching tool result — the four shapes translation
// must handle.
func sampleReq() gRequest {
	return gRequest{
		SystemInstruction: &gSystemInstruction{Parts: []gPart{{Text: "be brief"}}},
		Contents: []gContent{
			{Role: "user", Parts: []gPart{{Text: "read note.txt"}}},
			{Role: "model", Parts: []gPart{{FunctionCall: &gFunctionCall{Name: "read_file", Args: map[string]interface{}{"path": "note.txt"}}}}},
			{Role: "user", Parts: []gPart{{FunctionResponse: &gFunctionResp{Name: "read_file", Response: map[string]interface{}{"result": "hi"}}}}},
		},
		Tools: []gToolDecl{{FunctionDeclarations: []gFunctionDecl{{Name: "read_file", Description: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}}},
	}
}

func TestActiveModelSelection(t *testing.T) {
	resetProviderCfg()
	// Unset provider defaults to gemini and reports what's missing.
	if _, err := activeModel(); err == nil {
		t.Error("expected error when gemini key/model unset")
	}
	cfg.GeminiAPIKey, cfg.GeminiModel = "k", "gemini-2.0-flash"
	if mc, err := activeModel(); err != nil || mc.kind != "gemini" {
		t.Errorf("gemini: got kind=%q err=%v", mc.kind, err)
	}

	cfg.Provider = "openrouter"
	cfg.OpenRouterAPIKey, cfg.OpenRouterModel = "k", "openai/gpt-4o-mini"
	mc, err := activeModel()
	if err != nil || mc.kind != "openai" || mc.baseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("openrouter: got %+v err=%v", mc, err)
	}

	cfg.Provider = "local"
	cfg.LocalModel = "llama3.1"
	mc, err = activeModel()
	if err != nil || mc.baseURL != "http://localhost:11434/v1" {
		t.Errorf("local default base: got %+v err=%v", mc, err)
	}

	cfg.Provider = "bogus"
	if _, err := activeModel(); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestToOpenAITranslation(t *testing.T) {
	req := toOpenAI(sampleReq(), "gpt-4o-mini")
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages (system,user,assistant,tool), got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Errorf("unexpected leading roles: %+v", req.Messages[:2])
	}
	asst := req.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool-call turn malformed: %+v", asst)
	}
	tool := req.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != asst.ToolCalls[0].ID {
		t.Errorf("tool result id %q not linked to call id %q", tool.ToolCallID, asst.ToolCalls[0].ID)
	}
	if tool.Content != "hi" {
		t.Errorf("tool content = %q, want hi", tool.Content)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read_file" {
		t.Errorf("tools not translated: %+v", req.Tools)
	}
}

func TestToAnthropicTranslation(t *testing.T) {
	req := toAnthropic(sampleReq(), "claude-3-5-sonnet-latest")
	if req.System != "be brief" {
		t.Errorf("system = %q", req.System)
	}
	if req.MaxTokens == 0 {
		t.Error("max_tokens must be set for Anthropic")
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}
	asst := req.Messages[1]
	if asst.Role != "assistant" || asst.Content[0].Type != "tool_use" {
		t.Fatalf("assistant tool_use malformed: %+v", asst)
	}
	res := req.Messages[2]
	if res.Content[0].Type != "tool_result" || res.Content[0].ToolUseID != asst.Content[0].ID {
		t.Errorf("tool_result id %q not linked to tool_use id %q", res.Content[0].ToolUseID, asst.Content[0].ID)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Errorf("tools not translated: %+v", req.Tools)
	}
}

func TestOpenAIProviderHTTP(t *testing.T) {
	resetProviderCfg()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg.Provider = "openai"
	cfg.OpenAIBaseURL = srv.URL
	cfg.OpenAIAPIKey = "sk-test"
	cfg.OpenAIModel = "gpt-4o-mini"

	resp, err := callModel(sampleReq())
	if err != nil {
		t.Fatalf("callModel: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("endpoint path = %q", gotPath)
	}
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 2 || parts[0].Text != "hello" || parts[1].FunctionCall == nil {
		t.Fatalf("unexpected parts: %+v", parts)
	}
	if parts[1].FunctionCall.Name != "read_file" || parts[1].FunctionCall.Args["path"] != "a" {
		t.Errorf("tool call not parsed: %+v", parts[1].FunctionCall)
	}
}

func TestAnthropicProviderHTTP(t *testing.T) {
	resetProviderCfg()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "ant-key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic headers: %v", r.Header)
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"sure"},{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"b"}}],"stop_reason":"tool_use"}`)
	}))
	defer srv.Close()

	cfg.Provider = "anthropic"
	cfg.AnthropicBaseURL = srv.URL
	cfg.AnthropicAPIKey = "ant-key"
	cfg.AnthropicModel = "claude-3-5-sonnet-latest"

	resp, err := callModel(sampleReq())
	if err != nil {
		t.Fatalf("callModel: %v", err)
	}
	parts := resp.Candidates[0].Content.Parts
	if len(parts) != 2 || parts[0].Text != "sure" || parts[1].FunctionCall == nil {
		t.Fatalf("unexpected parts: %+v", parts)
	}
	if parts[1].FunctionCall.Args["path"] != "b" {
		t.Errorf("tool input not parsed: %+v", parts[1].FunctionCall)
	}
}

func TestOpenAIRetriesThenSucceeds(t *testing.T) {
	resetProviderCfg()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"try later"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	cfg.Provider = "openai"
	cfg.OpenAIBaseURL = srv.URL
	cfg.OpenAIAPIKey = "k"
	cfg.OpenAIModel = "m"

	resp, err := callModel(sampleReq())
	if err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "ok" {
		t.Errorf("unexpected response: %+v", resp.Candidates)
	}
}

func TestOpenAINonRetryable(t *testing.T) {
	resetProviderCfg()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad model"}}`)
	}))
	defer srv.Close()

	cfg.Provider = "openai"
	cfg.OpenAIBaseURL = srv.URL
	cfg.OpenAIAPIKey = "k"
	cfg.OpenAIModel = "m"

	if _, err := callModel(sampleReq()); err == nil {
		t.Fatal("expected error on HTTP 400")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("400 should not retry, made %d calls", calls)
	}
}
