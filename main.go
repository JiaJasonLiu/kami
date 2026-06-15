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

func statePath(name string) string { return filepath.Join(home, "state", name) }
func workspaceRoot() string        { return filepath.Join(home, "workspace") }

const (
	configFile  = "config.json"
	soulFile    = "SOUL.md"
	toolsFile   = "tools.json"
	historyFile = "history.json"
	offsetFile  = "offset.txt"
)

func ensureDirs() error {
	if err := os.MkdirAll(statePath(""), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(workspaceRoot(), 0o755)
}

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
