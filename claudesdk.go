package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"kami-gateway/internal/claudesdk"
)

// The claude-sdk provider. Unlike every other backend, this one does not call a
// metered API — it drives the Claude Agent SDK (the headless `claude` CLI) via
// the loopback sidecar in internal/claudesdk, which runs under the operator's
// Claude subscription login. Turns therefore consume **session usage** on that
// seat rather than API credits.
//
// Two things make it different from the other providers, and both are
// deliberate:
//
//   - **The SDK owns the conversation.** It persists history under a session id
//     and resumes it with --resume, so the gateway does not replay history.json
//     into the request. Instead it keeps one session id per (agent, topic) in
//     state/claude_sessions.json and hands it back each turn.
//   - **The SDK owns the agent loop.** It runs its own tools internally, so the
//     gateway's tool registry is not offered to it. Only the last user turn and
//     the agent's SOUL.md cross the boundary.
//
// callClaudeSDK therefore takes the internal gRequest like any other client,
// but reads only what it needs from it and returns a plain text gResponse with
// no function calls — which makes the agent loop in agent.go finish in one step.

const claudeSessionsFile = "claude_sessions.json"

// defaultClaudeSDKURL is where the host-level Claude SDK sidecar listens. It is
// loopback-only by design: the sidecar holds a logged-in `claude` CLI, so it
// must never be exposed off the machine.
const defaultClaudeSDKURL = "http://127.0.0.1:8081/claude"

// claudeSessions maps a routing key — "<agent>:<thread>" — to the SDK session
// id that holds that conversation. Guarded by sessionMu because cron turns and
// bot turns both reach it (they are already serialised by turnMu, but the store
// is cheap to lock and this keeps it correct if that ever changes).
var (
	claudeSessions = map[string]string{}
	sessionMu      sync.Mutex
)

// claudeSessionKey identifies a conversation the same way the gateway routes
// messages: per agent, per Telegram forum topic.
func claudeSessionKey(agent string, thread int64) string {
	return fmt.Sprintf("%s:%d", agent, thread)
}

// loadClaudeSessions restores the session map at startup so a restart of the
// gateway does not lose the SDK conversations it was in the middle of.
func loadClaudeSessions() {
	b, err := os.ReadFile(statePath(claudeSessionsFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("could not read %s: %v", claudeSessionsFile, err)
		}
		return
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("ignoring malformed %s: %v", claudeSessionsFile, err)
		return
	}
	sessionMu.Lock()
	claudeSessions = m
	sessionMu.Unlock()
}

// saveClaudeSessions persists the map. Errors are logged, not returned: losing
// a session id only costs the conversation's memory, never the reply.
func saveClaudeSessions() {
	sessionMu.Lock()
	b, _ := json.MarshalIndent(claudeSessions, "", "  ")
	sessionMu.Unlock()
	if err := os.WriteFile(statePath(claudeSessionsFile), b, 0o600); err != nil {
		log.Printf("could not save %s: %v", claudeSessionsFile, err)
	}
}

func getClaudeSession(key string) string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	return claudeSessions[key]
}

func setClaudeSession(key, id string) {
	sessionMu.Lock()
	changed := claudeSessions[key] != id
	if changed {
		claudeSessions[key] = id
	}
	sessionMu.Unlock()
	if changed {
		saveClaudeSessions()
	}
}

// resetClaudeSession forgets the session for one conversation, so the next turn
// starts a fresh SDK session. Called by /new so wiping the gateway's memory
// wipes the SDK's too — otherwise /new would appear to do nothing under this
// provider.
func resetClaudeSession(key string) {
	sessionMu.Lock()
	_, had := claudeSessions[key]
	delete(claudeSessions, key)
	sessionMu.Unlock()
	if had {
		saveClaudeSessions()
	}
}

// claudeSessionSummary renders the live sessions for /claude status.
func claudeSessionSummary() string {
	sessionMu.Lock()
	keys := make([]string, 0, len(claudeSessions))
	for k := range claudeSessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %s → %s", k, truncate(claudeSessions[k], 12))
	}
	sessionMu.Unlock()
	if b.Len() == 0 {
		return "\n  (none yet)"
	}
	return b.String()
}

// lastUserText pulls the most recent plain user turn out of the internal
// request. The SDK keeps its own history, so that single turn is all it needs.
func lastUserText(req gRequest) string {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c.Role != "user" {
			continue
		}
		if t := strings.TrimSpace(partsText(c.Parts)); t != "" {
			return t
		}
	}
	return ""
}

// callClaudeSDK runs one turn through the Claude Agent SDK sidecar. It resolves
// the session for the current (agent, topic), sends the user's message and the
// agent's SOUL.md, and stores whatever session id comes back.
func callClaudeSDK(req gRequest) (*gResponse, error) {
	prompt := lastUserText(req)
	if prompt == "" {
		return nil, fmt.Errorf("claude-sdk: no user message to send")
	}

	key := claudeSessionKey(activeAgent, currentTopic)
	var soul string
	if req.SystemInstruction != nil {
		soul = partsText(req.SystemInstruction.Parts)
	}

	out, err := claudesdk.Run(claudesdk.Request{
		Prompt:       prompt,
		SessionID:    getClaudeSession(key),
		SystemPrompt: soul,
		Model:        cfg.ClaudeSDKModel,
		MaxTurns:     cfg.ClaudeSDKMaxTurns,
		Cwd:          workspaceRoot(),
	})
	if err != nil {
		// A resume can fail because the SDK expired or pruned the session. Drop
		// the stale id so the user's next message starts a clean conversation
		// instead of failing forever on the same dead session.
		if getClaudeSession(key) != "" && isStaleSessionErr(err) {
			resetClaudeSession(key)
			return nil, fmt.Errorf("%w (that session is gone; the next message will start a fresh one)", err)
		}
		return nil, err
	}
	if out.SessionID != "" {
		setClaudeSession(key, out.SessionID)
	}

	text := strings.TrimSpace(out.Result)
	if text == "" {
		text = "(the Claude SDK returned no text)"
	}
	return &gResponse{Candidates: []gCandidate{{
		Content: gContent{Role: "model", Parts: []gPart{{Text: text}}},
	}}}, nil
}

// isStaleSessionErr guesses whether a failure was caused by an unresumable
// session id rather than by something that retrying would not fix.
func isStaleSessionErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "session") &&
		(strings.Contains(s, "not found") || strings.Contains(s, "no conversation") ||
			strings.Contains(s, "does not exist") || strings.Contains(s, "expired"))
}

// ---------------------------------------------------------------------------
// /claude — the Telegram slash command
// ---------------------------------------------------------------------------

// previousProvider remembers what the gateway was using before /claude switched
// it, so /claude off can put it back rather than guessing at a default.
var previousProvider string

// handleClaudeCommand implements the /claude family:
//
//	/claude              switch this gateway to the Claude Agent SDK
//	/claude status       show provider, model, sidecar and live sessions
//	/claude off          switch back to the previous provider
//	/claude model <m>    set the SDK model (alias like opus/sonnet, or an id)
//	/claude url <u>      point at a different sidecar (loopback only)
//	/claude reset        forget this conversation's SDK session
//
// Provider is global (like set_config), so switching here affects every agent
// and topic — the reply says so, to avoid a surprise.
func handleClaudeCommand(cmd string) string {
	fields := strings.Fields(cmd)
	sub := ""
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(cmd, "/claude")), sub))

	switch sub {
	case "":
		if cfg.Provider == claudeSDKProvider {
			return "Already using the Claude Agent SDK.\n" + claudeStatus()
		}
		previousProvider = orDefault(cfg.Provider, "gemini")
		cfg.Provider = claudeSDKProvider
		if err := saveConfig(); err != nil {
			cfg.Provider = previousProvider
			return "⚠️ couldn't save config: " + err.Error()
		}
		return "🤖 Switched to the Claude Agent SDK — this uses your Claude subscription's session usage, not an API key.\n" +
			"(Provider is global, so every agent and topic uses it now. /claude off goes back to " + previousProvider + ".)\n" +
			claudeStatus()

	case "off":
		if cfg.Provider != claudeSDKProvider {
			return "Not currently using the Claude Agent SDK (provider is " + orDefault(cfg.Provider, "gemini") + ")."
		}
		back := orDefault(previousProvider, "gemini")
		cfg.Provider = back
		if err := saveConfig(); err != nil {
			cfg.Provider = claudeSDKProvider
			return "⚠️ couldn't save config: " + err.Error()
		}
		return "↩️ Back to provider " + back + "."

	case "status":
		return claudeStatus()

	case "model":
		if arg == "" {
			return "Usage: /claude model <alias-or-id>   e.g. /claude model opus"
		}
		cfg.ClaudeSDKModel = arg
		if err := saveConfig(); err != nil {
			return "⚠️ couldn't save config: " + err.Error()
		}
		return "Claude SDK model set to " + arg + "."

	case "url":
		if arg == "" {
			return "Usage: /claude url http://127.0.0.1:8081/claude"
		}
		if !isLoopbackURL(arg) {
			return "⚠️ refused: the sidecar holds a logged-in Claude session, so it must stay on loopback (127.0.0.1 or localhost)."
		}
		cfg.ClaudeSDKURL = arg
		if err := saveConfig(); err != nil {
			return "⚠️ couldn't save config: " + err.Error()
		}
		return "Claude SDK sidecar set to " + arg + "."

	case "reset":
		key := claudeSessionKey(activeAgent, currentTopic)
		resetClaudeSession(key)
		return "🧹 Forgot the Claude SDK session for " + key + ". The next message starts a new one."

	default:
		return "Unknown /claude subcommand " + sub + ".\nUsage: /claude | /claude status | /claude off | /claude model <m> | /claude url <u> | /claude reset"
	}
}

// claudeSDKProvider is the cfg.Provider value selecting this backend.
const claudeSDKProvider = "claude-sdk"

// claudeStatus renders the current SDK settings and live sessions.
func claudeStatus() string {
	var b strings.Builder
	b.WriteString("Claude Agent SDK\n")
	fmt.Fprintf(&b, "  active provider: %s\n", orDefault(cfg.Provider, "gemini"))
	fmt.Fprintf(&b, "  billing: your Claude subscription (session usage), no API key\n")
	fmt.Fprintf(&b, "  model: %s\n", orDefault(cfg.ClaudeSDKModel, "(SDK default)"))
	fmt.Fprintf(&b, "  sidecar: %s\n", orDefault(cfg.ClaudeSDKURL, defaultClaudeSDKURL))
	fmt.Fprintf(&b, "  sessions:%s", claudeSessionSummary())
	return b.String()
}

// isLoopbackURL keeps the sidecar address pinned to the local machine. The
// sidecar runs a logged-in `claude` CLI, so pointing it at a remote host would
// hand the operator's subscription to someone else.
func isLoopbackURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
