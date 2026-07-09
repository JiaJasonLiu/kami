// Package coderelay implements SYSTEM LAYER 2 of the sandbox architecture:
// network-isolated execution. Instead of running commands on the host with
// os/exec — which this package deliberately never imports — prompts are
// relayed over the local loopback interface to a separate, host-level code
// service (e.g. a Claude Code repository wrapper) listening on
// 127.0.0.1:8080. The agent therefore behaves like an isolated
// microservice: its only path to the development tools is a local HTTP
// call, which the systemd sandbox (see setup.sh) still permits while all
// filesystem access outside the storage directory is blocked.
package coderelay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ServiceURL is the endpoint of the host-level code service. It is a
// package variable (not a constant) so tests can point it at an
// httptest.Server; production code should leave it untouched.
var ServiceURL = "http://127.0.0.1:8080/execute"

// httpClient is shared across calls so connections are reused. The long
// timeout accommodates code-generation runs, which can legitimately take
// minutes; the transport still fails fast if nothing is listening.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// maxResponseBytes caps how much of the service response is read into
// memory, protecting the agent from a runaway or malicious payload.
const maxResponseBytes = 4 << 20 // 4 MiB

// executeRequest is the JSON payload sent to the code service.
type executeRequest struct {
	Prompt string `json:"prompt"`
}

// executeResponse is the JSON payload expected back from the code service.
// Output carries the terminal output of the run; Error is set (and Output
// usually empty) when the service failed to execute the prompt.
type executeResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// RelayToCodeService packages prompt into a JSON payload, POSTs it to the
// local code service, and returns the terminal output string from the
// response. It never touches os/exec — all execution happens on the other
// side of the loopback boundary.
func RelayToCodeService(prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("coderelay: prompt must not be empty")
	}

	body, err := json.Marshal(executeRequest{Prompt: prompt})
	if err != nil {
		return "", fmt.Errorf("coderelay: cannot encode request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, ServiceURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("coderelay: cannot build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("coderelay: code service unreachable at %s: %w", ServiceURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("coderelay: cannot read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("coderelay: code service returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out executeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("coderelay: response is not valid JSON: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("coderelay: code service reported an error: %s", out.Error)
	}
	return out.Output, nil
}
