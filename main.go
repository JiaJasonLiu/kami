// kami-gateway: a very small, privacy-first AI gateway you talk to over Telegram.
//
// The model gets a SOUL.md (its system prompt, which it can edit),
// a sandboxed workspace it cannot escape, and a handful of tools defined in
// tools.json (which it can also edit). Single user, single chat, Gemini only.
package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
)

var home string

// statePath returns the absolute path for a named file inside the state directory.
// Keeping all mutable state under a single "state/" prefix makes the app easy to back up or wipe.
func statePath(name string) string { return filepath.Join(home, "state", name) }

// workspaceRoot returns the path to the sandboxed folder the AI model can read/write.
func workspaceRoot() string { return filepath.Join(home, "workspace") }

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

// ensureScaffold seeds the state directory with default SOUL.md and tools.json on first run.
// errors.Is(err, os.ErrNotExist) is the idiomatic Go way to check whether a file is simply missing
// rather than failing for some other reason (permissions, I/O error, etc.).
func ensureScaffold() error {
	if err := ensureDirs(); err != nil {
		return err
	}
	if _, err := os.Stat(statePath(soulFile)); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(statePath(soulFile), []byte(defaultSoul), 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(statePath(toolsFile)); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(statePath(toolsFile), []byte(defaultTools), 0o644); err != nil {
			return err
		}
	}
	return nil
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
	if cfg.GeminiAPIKey == "" || cfg.TelegramToken == "" || cfg.TelegramChatID == 0 {
		log.Fatalf("config incomplete; run: %s setup", os.Args[0])
	}

	runBot()
}
