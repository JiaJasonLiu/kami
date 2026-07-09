package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Agent profiles: each agent is a self-contained personality with its own
// SOUL.md, tools.json, conversation history, and sandboxed workspace. The
// default agent keeps the original legacy layout (state/ + workspace/ at the
// top of $KAMI_HOME) so existing installs upgrade in place; every other agent
// lives under agents/<name>/{state,workspace}.
//
// The active agent name is persisted in state/agent.txt (gateway-level, like
// config.json) so a restart resumes the same personality.

const (
	defaultAgent    = "kami"
	activeAgentFile = "agent.txt"
)

// activeAgent is the name of the profile all soul/tools/history/workspace
// lookups currently resolve to. It is only mutated by loadActiveAgent and
// switchAgent, both on the single bot-loop goroutine.
var activeAgent = defaultAgent

// agentNameRe restricts agent names to a safe slug. This is a security
// boundary, not cosmetics: the name is joined into filesystem paths, so it
// must never be able to carry separators or ".." segments.
var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// validAgentName rejects any name that could not safely appear in a path.
func validAgentName(name string) error {
	if !agentNameRe.MatchString(name) {
		return fmt.Errorf("invalid agent name %q: use 1-32 lowercase letters, digits, - or _, starting with a letter or digit", name)
	}
	return nil
}

// agentStateDir returns the directory holding an agent's SOUL.md, tools.json
// and history.json. The default agent maps to the legacy top-level state/.
func agentStateDir(name string) string {
	if name == defaultAgent {
		return filepath.Join(home, "state")
	}
	return filepath.Join(home, "agents", name, "state")
}

// agentWorkspaceDir returns an agent's sandboxed workspace root. The default
// agent maps to the legacy top-level workspace/.
func agentWorkspaceDir(name string) string {
	if name == defaultAgent {
		return filepath.Join(home, "workspace")
	}
	return filepath.Join(home, "agents", name, "workspace")
}

// agentStatePath resolves a per-agent state file (SOUL.md, tools.json,
// history.json) for the ACTIVE agent. Gateway-level files (config.json,
// offset.txt, agent.txt) go through statePath instead.
func agentStatePath(name string) string {
	return filepath.Join(agentStateDir(activeAgent), name)
}

// scaffoldAgent creates an agent's directories and seeds SOUL.md and
// tools.json with defaults when missing. Idempotent, so it is safe to call
// on every startup and every switch.
func scaffoldAgent(name string) error {
	if err := os.MkdirAll(agentStateDir(name), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(agentWorkspaceDir(name), 0o755); err != nil {
		return err
	}
	soul := filepath.Join(agentStateDir(name), soulFile)
	if _, err := os.Stat(soul); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(soul, []byte(defaultSoul), 0o644); err != nil {
			return err
		}
	}
	tools := filepath.Join(agentStateDir(name), toolsFile)
	if _, err := os.Stat(tools); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(tools, []byte(defaultTools), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// agentExists reports whether a profile already has a state directory.
// The default agent always exists.
func agentExists(name string) bool {
	if name == defaultAgent {
		return true
	}
	info, err := os.Stat(agentStateDir(name))
	return err == nil && info.IsDir()
}

// listAgents returns every known profile name, default first, the rest
// sorted alphabetically.
func listAgents() []string {
	agents := []string{defaultAgent}
	entries, err := os.ReadDir(filepath.Join(home, "agents"))
	if err != nil {
		return agents
	}
	var named []string
	for _, e := range entries {
		if e.IsDir() && validAgentName(e.Name()) == nil {
			named = append(named, e.Name())
		}
	}
	sort.Strings(named)
	return append(agents, named...)
}

// createAgent scaffolds a brand-new profile. When personality is non-empty
// it is written into the new agent's SOUL.md as owner-provided identity
// instructions, so the agent is born with its own character.
func createAgent(name, personality string) error {
	if err := validAgentName(name); err != nil {
		return err
	}
	if agentExists(name) {
		return fmt.Errorf("agent %q already exists", name)
	}
	if err := scaffoldAgent(name); err != nil {
		return err
	}
	if strings.TrimSpace(personality) != "" {
		soul := defaultSoul + fmt.Sprintf("\n## Instructions from your owner\n\nYour name is %q. %s\n", name, strings.TrimSpace(personality))
		if err := os.WriteFile(filepath.Join(agentStateDir(name), soulFile), []byte(soul), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// switchAgent makes an existing profile the active one and persists the
// choice so restarts resume it. The target is re-scaffolded defensively in
// case its files were hand-deleted.
func switchAgent(name string) error {
	if err := validAgentName(name); err != nil {
		return err
	}
	if !agentExists(name) {
		return fmt.Errorf("agent %q does not exist; create it with /agent new %s", name, name)
	}
	if err := scaffoldAgent(name); err != nil {
		return err
	}
	activeAgent = name
	return os.WriteFile(statePath(activeAgentFile), []byte(name), 0o600)
}

// deleteAgent permanently removes a profile's soul, tools, history AND
// workspace. The active agent and the default agent are protected.
func deleteAgent(name string) error {
	if err := validAgentName(name); err != nil {
		return err
	}
	if name == defaultAgent {
		return fmt.Errorf("the default agent %q cannot be deleted", defaultAgent)
	}
	if name == activeAgent {
		return fmt.Errorf("agent %q is active; switch away first with /agent use <other>", name)
	}
	if !agentExists(name) {
		return fmt.Errorf("agent %q does not exist", name)
	}
	return os.RemoveAll(filepath.Join(home, "agents", name))
}

// loadActiveAgent restores the persisted active agent at startup, falling
// back to the default when the file is missing, invalid, or points at a
// profile that no longer exists.
func loadActiveAgent() {
	activeAgent = defaultAgent
	b, err := os.ReadFile(statePath(activeAgentFile))
	if err != nil {
		return
	}
	name := strings.TrimSpace(string(b))
	if validAgentName(name) == nil && agentExists(name) {
		activeAgent = name
	}
}

// handleAgentCommand implements the /agents and /agent chat commands.
// It returns the reply text to send back to the owner.
func handleAgentCommand(text string) string {
	fields := strings.Fields(text)
	if fields[0] == "/agents" || len(fields) == 1 {
		var b strings.Builder
		b.WriteString("Agents:\n")
		for _, name := range listAgents() {
			marker := "  "
			if name == activeAgent {
				marker = "▶ "
			}
			fmt.Fprintf(&b, "%s%s\n", marker, name)
		}
		b.WriteString("\n/agent new <name> [personality…] — create (and switch to) a new agent\n/agent use <name> — switch agents\n/agent delete <name> — delete an agent and its files")
		return b.String()
	}

	sub, args := fields[1], fields[2:]
	switch sub {
	case "new":
		if len(args) == 0 {
			return "usage: /agent new <name> [personality…]"
		}
		name := args[0]
		personality := strings.Join(args[1:], " ")
		if err := createAgent(name, personality); err != nil {
			return "⚠️ " + err.Error()
		}
		if err := switchAgent(name); err != nil {
			return "⚠️ created, but could not switch: " + err.Error()
		}
		return fmt.Sprintf("✨ Created agent %q with its own soul, tools, memory and workspace — now active. Say hi!", name)
	case "use":
		if len(args) != 1 {
			return "usage: /agent use <name>"
		}
		if args[0] == activeAgent {
			return fmt.Sprintf("%q is already the active agent.", activeAgent)
		}
		if err := switchAgent(args[0]); err != nil {
			return "⚠️ " + err.Error()
		}
		return fmt.Sprintf("🔀 Switched to agent %q. Its own memory and workspace are now in effect.", activeAgent)
	case "delete":
		if len(args) != 1 {
			return "usage: /agent delete <name>"
		}
		if err := deleteAgent(args[0]); err != nil {
			return "⚠️ " + err.Error()
		}
		return fmt.Sprintf("🗑 Deleted agent %q and all of its files.", args[0])
	default:
		// Convenience shorthand: "/agent coder" switches to coder.
		if len(fields) == 2 {
			if err := switchAgent(sub); err != nil {
				return "⚠️ " + err.Error()
			}
			return fmt.Sprintf("🔀 Switched to agent %q.", activeAgent)
		}
		return "usage: /agent [new|use|delete] <name>"
	}
}
