// Package claudesdk talks to the Claude Agent SDK (the `claude` CLI running in
// headless mode) so the gateway can use a Claude **subscription seat** —
// "session usage" — instead of a metered Anthropic API key.
//
// Why this is an HTTP client and not an exec call
// -----------------------------------------------
// The SDK only bills against a subscription when it runs as the local `claude`
// binary under the operator's OAuth login (~/.claude credentials). That is
// impossible from inside the gateway process, for two independent reasons, both
// of which the project deliberately keeps in place:
//
//  1. The gateway contains no os/exec usage — and this package, like
//     internal/coderelay, never imports it either.
//  2. Under the production systemd unit (ProtectHome=yes, ProtectSystem=strict,
//     empty CapabilityBoundingSet) the process cannot read the operator's home
//     directory, so it could not reach the OAuth credentials even if it tried.
//
// So this follows the pattern internal/coderelay already established for
// exactly this problem: the prompt is relayed over the loopback interface to a
// small host-level sidecar that owns the `claude` process and the OAuth login.
// See sidecar/claude-sdk-service.js in the repository root for a reference
// implementation, and README.md for how to run it.
//
// Sessions
// --------
// The SDK persists a conversation under a session id. The gateway holds one
// session per (agent, Telegram topic), passes it back on every turn, and the
// sidecar resumes it with `--resume`. Conversation memory therefore lives on
// the SDK side of the boundary; the gateway's own history.json is not used for
// this provider.
package claudesdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ServiceURL is the endpoint of the host-level Claude SDK sidecar. It is a
// package variable (not a constant) so tests can point it at an httptest
// server and so config can override it; production defaults to loopback.
var ServiceURL = "http://127.0.0.1:8081/claude"

// httpClient is shared so connections are reused. The timeout is generous
// because an agentic SDK run with tool use can legitimately take minutes; the
// transport still fails fast when nothing is listening.
var httpClient = &http.Client{Timeout: 10 * time.Minute}

// maxResponseBytes caps how much of a reply is read into memory, protecting
// the gateway from a runaway or malicious payload.
const maxResponseBytes = 4 << 20 // 4 MiB

// Request is the JSON payload sent to the sidecar. It mirrors the headless
// flags of the `claude` CLI that the sidecar is expected to pass through.
type Request struct {
	// Prompt is the user turn, sent to the CLI over stdin by the sidecar so
	// its length is not bounded by the host's argv limit.
	Prompt string `json:"prompt"`
	// SessionID resumes an existing SDK conversation (`--resume`). Empty
	// starts a new one, and the reply reports the id that was created.
	SessionID string `json:"session_id,omitempty"`
	// SystemPrompt becomes `--append-system-prompt`, carrying the agent's
	// SOUL.md so a Claude-SDK turn keeps the same personality as every other
	// provider. It is appended, not replaced, so the SDK keeps its own tooling
	// instructions.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Model is an alias ("opus", "sonnet") or a full model id; empty lets the
	// SDK pick its configured default.
	Model string `json:"model,omitempty"`
	// MaxTurns bounds the SDK's internal agentic loop (`--max-turns`).
	MaxTurns int `json:"max_turns,omitempty"`
	// Cwd is the directory the SDK runs in — the calling agent's sandboxed
	// workspace, so SDK-side file tools stay inside it.
	Cwd string `json:"cwd,omitempty"`
}

// Response is the JSON payload expected back. The field names follow the
// `--output-format json` result envelope of the CLI so a sidecar can forward
// the envelope almost verbatim.
type Response struct {
	// Result is the assistant's final text.
	Result string `json:"result"`
	// SessionID is the id of the session that ran, to reuse on the next turn.
	SessionID string `json:"session_id,omitempty"`
	// IsError marks an SDK-reported failure (auth, budget, bad flags).
	IsError bool `json:"is_error,omitempty"`
	// NumTurns and TotalCostUSD are reported for observability.
	NumTurns     int     `json:"num_turns,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	// Error carries a sidecar-level failure message.
	Error string `json:"error,omitempty"`
}

// Run relays one turn to the Claude SDK sidecar and returns its reply. It
// never touches os/exec — the `claude` process lives entirely on the other
// side of the loopback boundary.
func Run(req Request) (*Response, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("claudesdk: prompt must not be empty")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("claudesdk: cannot encode request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, ServiceURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("claudesdk: cannot build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claudesdk: SDK service unreachable at %s: %w", ServiceURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("claudesdk: cannot read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claudesdk: SDK service returned HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 500))
	}

	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("claudesdk: response is not valid JSON: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("claudesdk: SDK service reported an error: %s", out.Error)
	}
	if out.IsError {
		return nil, fmt.Errorf("claudesdk: SDK run failed: %s", truncate(orDefault(out.Result, "no detail reported"), 500))
	}
	return &out, nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
