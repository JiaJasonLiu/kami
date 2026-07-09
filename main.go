// kami-gateway: a very small, privacy-first AI gateway you talk to over Telegram.
//
// The model gets a SOUL.md (its system prompt, which it can edit),
// a sandboxed workspace it cannot escape, and a handful of tools defined in
// tools.json (which it can also edit). Single user, single chat, Gemini only.
package main

import (
	"log"
	"os"
	"path/filepath"
)

var home string

// statePath returns the absolute path for a named GATEWAY-LEVEL file (config.json,
// offset.txt, agent.txt) inside the top-level state directory. Per-agent files
// (SOUL.md, tools.json, history.json) resolve through agentStatePath in profiles.go.
func statePath(name string) string { return filepath.Join(home, "state", name) }

// workspaceRoot returns the path to the sandboxed folder the ACTIVE agent can
// read/write. Each agent profile has its own workspace (see profiles.go).
func workspaceRoot() string { return agentWorkspaceDir(activeAgent) }

const (
	configFile  = "config.json"
	soulFile    = "SOUL.md"
	toolsFile   = "tools.json"
	historyFile = "history.json"
	offsetFile  = "offset.txt"
)

// ensureDirs creates the state and workspace directories if they do not already exist.
// os.MkdirAll is idempotent — it succeeds even when the path already exists.
func ensureDirs() error {
	if err := os.MkdirAll(statePath(""), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(workspaceRoot(), 0o755)
}

// ensureScaffold seeds the active agent's profile with default SOUL.md and tools.json
// on first run. scaffoldAgent (profiles.go) is idempotent — it only writes files that
// are missing, so an existing personality is never overwritten.
func ensureScaffold() error {
	if err := ensureDirs(); err != nil {
		return err
	}
	return scaffoldAgent(activeAgent)
}

// main is the program entry point. It handles first-run setup, validates config,
// and then hands off to runBot() which loops forever polling Telegram.
// log.SetFlags(log.Ltime) strips date from log output, keeping lines short in terminal.
func main() {
	log.SetFlags(log.Ltime)
	home = orDefault(os.Getenv("KAMI_HOME"), ".")

	if err := ensureDirs(); err != nil {
		log.Fatalf("could not create directories: %v", err)
	}
	loadActiveAgent()
	loadTopicBindings()
	loadCronJobs()

	forceSetup := len(os.Args) > 1 && (os.Args[1] == "setup" || os.Args[1] == "--setup")
	exists, err := loadConfig()
	if err != nil {
		log.Fatalf("could not read config: %v", err)
	}

	if forceSetup || !exists {
		if err := runSetup(); err != nil {
			log.Fatalf("setup failed: %v", err)
		}
		if forceSetup {
			return
		}
	}

	if err := ensureScaffold(); err != nil {
		log.Fatalf("could not create scaffold: %v", err)
	}
	if err := configComplete(); err != nil {
		log.Fatalf("config incomplete: %v; run: %s setup", err, os.Args[0])
	}

	runBot()
}
