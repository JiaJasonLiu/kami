package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Multi-provider support. The agent loop speaks one internal request/response
// shape (the gRequest / gResponse types, which mirror Gemini — the original
// backend). callModel dispatches that request to whichever provider is
// configured, translating to and from each provider's own wire format.
//
// Three client implementations cover five providers:
//   - gemini      -> callGemini (gemini.go)
//   - openai      -\
//   - openrouter   -> openaiGenerate (OpenAI Chat Completions format)
//   - local       -/  (Ollama / LM Studio / llama.cpp / vLLM, etc.)
//   - anthropic   -> anthropicGenerate (Anthropic Messages format)
//
// Provider selection is global (cfg.Provider) and switchable at runtime via
// set_config. Each provider keeps its own key/model in config so switching
// back and forth never loses credentials.

var (
	modelMaxAttempts = 3
	modelBackoffBase = 2 * time.Second
)

// modelConfig is the resolved, ready-to-call description of the active model:
// which client kind to use, the endpoint base, the API key, and the model id.
type modelConfig struct {
	kind    string // "gemini" | "openai" | "anthropic"
	baseURL string
	apiKey  string
	model   string
}

// activeModel resolves cfg.Provider into a concrete modelConfig, filling in
// default base URLs and validating that the required key/model are present.
func activeModel() (modelConfig, error) {
	switch cfg.Provider {
	case "", "gemini":
		mc := modelConfig{kind: "gemini", apiKey: cfg.GeminiAPIKey, model: cfg.GeminiModel}
		if mc.apiKey == "" || mc.model == "" {
			return mc, fmt.Errorf("gemini provider needs gemini_api_key and gemini_model")
		}
		return mc, nil
	case "openai":
		mc := modelConfig{kind: "openai", baseURL: orDefault(cfg.OpenAIBaseURL, "https://api.openai.com/v1"), apiKey: cfg.OpenAIAPIKey, model: cfg.OpenAIModel}
		if mc.apiKey == "" || mc.model == "" {
			return mc, fmt.Errorf("openai provider needs openai_api_key and openai_model")
		}
		return mc, nil
	case "openrouter":
		mc := modelConfig{kind: "openai", baseURL: "https://openrouter.ai/api/v1", apiKey: cfg.OpenRouterAPIKey, model: cfg.OpenRouterModel}
		if mc.apiKey == "" || mc.model == "" {
			return mc, fmt.Errorf("openrouter provider needs openrouter_api_key and openrouter_model")
		}
		return mc, nil
	case "local":
		// A generic OpenAI-compatible local server. The default targets
		// Ollama; point local_base_url elsewhere for LM Studio, llama.cpp,
		// vLLM, etc. The API key is usually irrelevant for local servers.
		mc := modelConfig{kind: "openai", baseURL: orDefault(cfg.LocalBaseURL, "http://localhost:11434/v1"), apiKey: cfg.LocalAPIKey, model: cfg.LocalModel}
		if mc.model == "" {
			return mc, fmt.Errorf("local provider needs local_model (and local_base_url if not Ollama's default)")
		}
		return mc, nil
	case "anthropic":
		mc := modelConfig{kind: "anthropic", baseURL: orDefault(cfg.AnthropicBaseURL, "https://api.anthropic.com"), apiKey: cfg.AnthropicAPIKey, model: cfg.AnthropicModel}
		if mc.apiKey == "" || mc.model == "" {
			return mc, fmt.Errorf("anthropic provider needs anthropic_api_key and anthropic_model")
		}
		return mc, nil
	default:
		return modelConfig{}, fmt.Errorf("unknown provider %q (use gemini, openai, anthropic, openrouter, or local)", cfg.Provider)
	}
}

// callModel routes an internal request to the active provider's client.
func callModel(req gRequest) (*gResponse, error) {
	mc, err := activeModel()
	if err != nil {
		return nil, err
	}
	switch mc.kind {
	case "gemini":
		return callGemini(req)
	case "openai":
		return withRetry(orDefault(cfg.Provider, "openai"), func() (*gResponse, bool, error) { return openaiOnce(mc, req) })
	case "anthropic":
		return withRetry("anthropic", func() (*gResponse, bool, error) { return anthropicOnce(mc, req) })
	default:
		return nil, fmt.Errorf("unsupported model kind %q", mc.kind)
	}
}

// withRetry runs a single-attempt call with the same transient-error backoff
// policy as callGemini: retry only on rate-limit / 5xx, up to modelMaxAttempts.
func withRetry(name string, once func() (*gResponse, bool, error)) (*gResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= modelMaxAttempts; attempt++ {
		gr, retryable, err := once()
		if err == nil {
			return gr, nil
		}
		lastErr = err
		if !retryable || attempt == modelMaxAttempts {
			break
		}
		backoff := time.Duration(attempt) * modelBackoffBase
		log.Printf("%s transient error (attempt %d/%d): %v; retrying in %s", name, attempt, modelMaxAttempts, err, backoff)
		time.Sleep(backoff)
	}
	return nil, lastErr
}

// ---------------------------------------------------------------------------
// Shared translation helpers
// ---------------------------------------------------------------------------

// toolRef pairs a synthetic tool-call id with the function name it belongs to.
// The internal message shape has no ids, but both OpenAI and Anthropic require
// a tool call and its result to be linked by id, so we mint ids while walking
// the history and match each result back to its call by name.
type toolRef struct{ id, name string }

// popByName removes and returns the id of the first pending call with the
// given name, so a result can be linked to the call that produced it.
func popByName(pending *[]toolRef, name string) string {
	for i, t := range *pending {
		if t.name == name {
			*pending = append((*pending)[:i], (*pending)[i+1:]...)
			return t.id
		}
	}
	return "call_" + name
}

// partsText concatenates the text of every text part in a slice.
func partsText(ps []gPart) string {
	var b strings.Builder
	for _, p := range ps {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// funcRespContent renders a tool result as a plain string for the model. Our
// tools wrap output as {"result": "..."}; prefer that, else marshal the map.
func funcRespContent(fr *gFunctionResp) string {
	if r, ok := fr.Response["result"].(string); ok {
		return r
	}
	b, _ := json.Marshal(fr.Response)
	return string(b)
}

// orEmptyObj normalises a nil arg map to an empty object so it serialises as
// "{}" rather than "null" (which some providers reject as tool arguments).
func orEmptyObj(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// splitParts separates a content entry's parts into text, function calls and
// function responses in one pass.
func splitParts(c gContent) (text string, calls []*gFunctionCall, resps []*gFunctionResp) {
	text = partsText(c.Parts)
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			calls = append(calls, p.FunctionCall)
		}
		if p.FunctionResponse != nil {
			resps = append(resps, p.FunctionResponse)
		}
	}
	return
}

// ---------------------------------------------------------------------------
// OpenAI-compatible client (openai, openrouter, local)
// ---------------------------------------------------------------------------

type oaiFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function oaiFn  `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiTool struct {
	Type     string    `json:"type"`
	Function oaiToolFn `json:"function"`
}

type oaiRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Tools    []oaiTool    `json:"tools,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string        `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// toOpenAI translates the internal request into OpenAI Chat Completions form,
// minting tool-call ids and linking tool results back to their calls.
func toOpenAI(req gRequest, model string) oaiRequest {
	out := oaiRequest{Model: model}
	if req.SystemInstruction != nil {
		if s := partsText(req.SystemInstruction.Parts); s != "" {
			out.Messages = append(out.Messages, oaiMessage{Role: "system", Content: s})
		}
	}
	counter := 0
	var pending []toolRef
	for _, c := range req.Contents {
		text, calls, resps := splitParts(c)
		switch {
		case len(resps) > 0:
			for _, fr := range resps {
				out.Messages = append(out.Messages, oaiMessage{Role: "tool", ToolCallID: popByName(&pending, fr.Name), Content: funcRespContent(fr)})
			}
		case len(calls) > 0:
			pending = nil
			var tcs []oaiToolCall
			for _, fc := range calls {
				id := fmt.Sprintf("call_%d", counter)
				counter++
				args, _ := json.Marshal(orEmptyObj(fc.Args))
				tcs = append(tcs, oaiToolCall{ID: id, Type: "function", Function: oaiFn{Name: fc.Name, Arguments: string(args)}})
				pending = append(pending, toolRef{id, fc.Name})
			}
			out.Messages = append(out.Messages, oaiMessage{Role: "assistant", Content: text, ToolCalls: tcs})
		default:
			role := "user"
			if c.Role == "model" {
				role = "assistant"
			}
			out.Messages = append(out.Messages, oaiMessage{Role: role, Content: text})
		}
	}
	for _, t := range req.Tools {
		for _, fd := range t.FunctionDeclarations {
			out.Tools = append(out.Tools, oaiTool{Type: "function", Function: oaiToolFn{Name: fd.Name, Description: fd.Description, Parameters: fd.Parameters}})
		}
	}
	return out
}

// fromOpenAI translates an OpenAI response back into the internal shape.
func fromOpenAI(r oaiResponse) *gResponse {
	var parts []gPart
	if len(r.Choices) > 0 {
		m := r.Choices[0].Message
		if m.Content != "" {
			parts = append(parts, gPart{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			var args map[string]interface{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			parts = append(parts, gPart{FunctionCall: &gFunctionCall{Name: tc.Function.Name, Args: args}})
		}
	}
	return &gResponse{Candidates: []gCandidate{{Content: gContent{Role: "model", Parts: parts}}}}
}

func openaiOnce(mc modelConfig, req gRequest) (*gResponse, bool, error) {
	body, _ := json.Marshal(toOpenAI(req, mc.model))
	endpoint := strings.TrimRight(mc.baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if mc.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+mc.apiKey)
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var or oaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, retryable, fmt.Errorf("decoding OpenAI response (HTTP %d): %v\n%s", resp.StatusCode, err, truncate(string(raw), 500))
	}
	if or.Error != nil {
		return nil, retryable, fmt.Errorf("OpenAI error (HTTP %d): %s", resp.StatusCode, or.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryable, fmt.Errorf("OpenAI HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return fromOpenAI(or), false, nil
}

// ---------------------------------------------------------------------------
// Anthropic client (Messages API)
// ---------------------------------------------------------------------------

const anthropicMaxTokens = 4096
const anthropicVersion = "2023-06-01"

type antBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type antMessage struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antRequest struct {
	Model     string       `json:"model"`
	System    string       `json:"system,omitempty"`
	Messages  []antMessage `json:"messages"`
	Tools     []antTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens"`
}

type antResponse struct {
	Content []struct {
		Type  string                 `json:"type"`
		Text  string                 `json:"text"`
		ID    string                 `json:"id"`
		Name  string                 `json:"name"`
		Input map[string]interface{} `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// toAnthropic translates the internal request into the Anthropic Messages
// format, minting tool_use ids and linking tool_result blocks back to them.
func toAnthropic(req gRequest, model string) antRequest {
	out := antRequest{Model: model, MaxTokens: anthropicMaxTokens}
	if req.SystemInstruction != nil {
		out.System = partsText(req.SystemInstruction.Parts)
	}
	counter := 0
	var pending []toolRef
	for _, c := range req.Contents {
		text, calls, resps := splitParts(c)
		switch {
		case len(resps) > 0:
			var blocks []antBlock
			for _, fr := range resps {
				blocks = append(blocks, antBlock{Type: "tool_result", ToolUseID: popByName(&pending, fr.Name), Content: funcRespContent(fr)})
			}
			out.Messages = append(out.Messages, antMessage{Role: "user", Content: blocks})
		case len(calls) > 0:
			pending = nil
			var blocks []antBlock
			if text != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: text})
			}
			for _, fc := range calls {
				id := fmt.Sprintf("call_%d", counter)
				counter++
				blocks = append(blocks, antBlock{Type: "tool_use", ID: id, Name: fc.Name, Input: orEmptyObj(fc.Args)})
				pending = append(pending, toolRef{id, fc.Name})
			}
			out.Messages = append(out.Messages, antMessage{Role: "assistant", Content: blocks})
		default:
			role := "user"
			if c.Role == "model" {
				role = "assistant"
			}
			out.Messages = append(out.Messages, antMessage{Role: role, Content: []antBlock{{Type: "text", Text: text}}})
		}
	}
	for _, t := range req.Tools {
		for _, fd := range t.FunctionDeclarations {
			schema := fd.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out.Tools = append(out.Tools, antTool{Name: fd.Name, Description: fd.Description, InputSchema: schema})
		}
	}
	return out
}

// fromAnthropic translates an Anthropic response back into the internal shape.
func fromAnthropic(r antResponse) *gResponse {
	var parts []gPart
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, gPart{Text: b.Text})
			}
		case "tool_use":
			parts = append(parts, gPart{FunctionCall: &gFunctionCall{Name: b.Name, Args: b.Input}})
		}
	}
	return &gResponse{Candidates: []gCandidate{{Content: gContent{Role: "model", Parts: parts}}}}
}

func anthropicOnce(mc modelConfig, req gRequest) (*gResponse, bool, error) {
	body, _ := json.Marshal(toAnthropic(req, mc.model))
	endpoint := strings.TrimRight(mc.baseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", mc.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var ar antResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, retryable, fmt.Errorf("decoding Anthropic response (HTTP %d): %v\n%s", resp.StatusCode, err, truncate(string(raw), 500))
	}
	if ar.Error != nil {
		return nil, retryable, fmt.Errorf("Anthropic error (HTTP %d): %s", resp.StatusCode, ar.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryable, fmt.Errorf("Anthropic HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return fromAnthropic(ar), false, nil
}
