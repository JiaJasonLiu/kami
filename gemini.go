package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

type gPart struct {
	Text             string         `json:"text,omitempty"`
	FunctionCall     *gFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *gFunctionResp `json:"functionResponse,omitempty"`
	// ThoughtSignature must be echoed back verbatim for thinking-enabled models (e.g. gemini-2.5-*).
	ThoughtSignature json.RawMessage `json:"thoughtSignature,omitempty"`
}

type gFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type gFunctionResp struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gSystemInstruction struct {
	Parts []gPart `json:"parts"`
}

type gToolDecl struct {
	FunctionDeclarations []gFunctionDecl `json:"functionDeclarations"`
}

type gFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gRequest struct {
	SystemInstruction *gSystemInstruction `json:"system_instruction,omitempty"`
	Contents          []gContent          `json:"contents"`
	Tools             []gToolDecl         `json:"tools,omitempty"`
}

type gCandidate struct {
	Content      gContent `json:"content"`
	FinishReason string   `json:"finishReason"`
}

type gPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type gAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// gResponse is the internal, provider-neutral response shape. It happens to
// mirror the Gemini API (the first backend), and every other provider client
// translates its own response into this so the agent loop stays unchanged.
type gResponse struct {
	Candidates     []gCandidate     `json:"candidates"`
	PromptFeedback *gPromptFeedback `json:"promptFeedback"`
	Error          *gAPIError       `json:"error"`
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

var geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

const geminiMaxAttempts = 3

var geminiBackoffBase = 2 * time.Second

// callGemini sends a request to the Gemini API with simple exponential-ish backoff retry.
// It retries only on transient errors (rate limits, 5xx) — permanent errors are returned immediately.
// Returning a pointer (*gResponse) is idiomatic when the struct is large or may be nil on error.
func callGemini(req gRequest) (*gResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= geminiMaxAttempts; attempt++ {
		gr, retryable, err := callGeminiOnce(req)
		if err == nil {
			return gr, nil
		}
		lastErr = err
		if !retryable || attempt == geminiMaxAttempts {
			break
		}
		backoff := time.Duration(attempt) * geminiBackoffBase
		log.Printf("gemini transient error (attempt %d/%d): %v; retrying in %s", attempt, geminiMaxAttempts, err, backoff)
		time.Sleep(backoff)
	}
	return nil, lastErr
}

// callGeminiOnce makes a single HTTP POST to the Gemini generateContent endpoint.
// It returns three values: the parsed response, a boolean indicating whether the error
// is retryable, and the error itself. Returning multiple values to encode different
// failure modes is a common Go pattern instead of using exceptions or tagged unions.
// defer resp.Body.Close() ensures the HTTP body is drained and closed even if we return early.
func callGeminiOnce(req gRequest) (*gResponse, bool, error) {
	endpoint := fmt.Sprintf(
		"%s/models/%s:generateContent?key=%s",
		geminiBaseURL, url.PathEscape(cfg.GeminiModel), url.QueryEscape(cfg.GeminiAPIKey),
	)
	body, _ := json.Marshal(req)
	resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var gr gResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, retryable, fmt.Errorf("decoding Gemini response (HTTP %d): %v\n%s", resp.StatusCode, err, truncate(string(raw), 500))
	}
	if gr.Error != nil {
		return nil, retryable, fmt.Errorf("Gemini error %d (%s): %s", gr.Error.Code, gr.Error.Status, gr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryable, fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return &gr, false, nil
}
