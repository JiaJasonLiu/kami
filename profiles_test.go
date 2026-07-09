package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetAgents gives each test a fresh $KAMI_HOME with the default agent active.
func resetAgents(t *testing.T) {
	t.Helper()
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentNameValidation(t *testing.T) {
	good := []string{"coder", "note-taker", "a", "x2", "deep_thought"}
	for _, g := range good {
		if err := validAgentName(g); err != nil {
			t.Errorf("expected %q valid, got %v", g, err)
		}
	}
	bad := []string{"", "..", "a/b", "../evil", "UPPER", "way-too-long-name-that-exceeds-the-32-char-limit", "-lead", "has space"}
	for _, b := range bad {
		if err := validAgentName(b); err == nil {
			t.Errorf("expected %q rejected", b)
		}
	}
}

func TestCreateSwitchAndIsolation(t *testing.T) {
	resetAgents(t)

	if err := createAgent("coder", "You are a terse coding assistant."); err != nil {
		t.Fatal(err)
	}
	// the new agent must have its own soul carrying the personality seed
	soul, err := os.ReadFile(filepath.Join(agentStateDir("coder"), soulFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(soul), "terse coding assistant") {
		t.Error("personality seed missing from new agent's SOUL.md")
	}

	// a file written by the default agent must be invisible to the new one
	if _, err := tWriteFile(map[string]interface{}{"path": "secret.txt", "content": "default's"}); err != nil {
		t.Fatal(err)
	}
	if err := switchAgent("coder"); err != nil {
		t.Fatal(err)
	}
	if activeAgent != "coder" {
		t.Fatalf("activeAgent = %q, want coder", activeAgent)
	}
	if out, err := tListFiles(nil); err != nil || strings.Contains(out, "secret.txt") {
		t.Errorf("coder can see the default agent's files: %q (err %v)", out, err)
	}
	// souls are independent too
	coderSoul, err := tReadSoul(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(coderSoul, "terse coding assistant") {
		t.Error("read_soul did not return the active agent's soul")
	}

	// switching persists across a simulated restart
	activeAgent = defaultAgent
	loadActiveAgent()
	if activeAgent != "coder" {
		t.Errorf("after restart activeAgent = %q, want coder", activeAgent)
	}
}

func TestCreateRejectsDuplicatesAndBadNames(t *testing.T) {
	resetAgents(t)
	if err := createAgent("twin", ""); err != nil {
		t.Fatal(err)
	}
	if err := createAgent("twin", ""); err == nil {
		t.Error("duplicate create was allowed")
	}
	if err := createAgent(defaultAgent, ""); err == nil {
		t.Error("creating the default agent was allowed")
	}
	if err := createAgent("../evil", ""); err == nil {
		t.Error("path-traversal agent name was allowed")
	}
}

func TestDeleteAgentGuards(t *testing.T) {
	resetAgents(t)
	if err := createAgent("victim", ""); err != nil {
		t.Fatal(err)
	}
	if err := switchAgent("victim"); err != nil {
		t.Fatal(err)
	}
	if err := deleteAgent("victim"); err == nil {
		t.Error("deleting the ACTIVE agent was allowed")
	}
	if err := deleteAgent(defaultAgent); err == nil {
		t.Error("deleting the default agent was allowed")
	}
	if err := switchAgent(defaultAgent); err != nil {
		t.Fatal(err)
	}
	if err := deleteAgent("victim"); err != nil {
		t.Fatalf("delete after switching away: %v", err)
	}
	if agentExists("victim") {
		t.Error("agent still exists after delete")
	}
}

func TestAgentChatCommands(t *testing.T) {
	resetAgents(t)
	if out := handleUserMessage("/agents"); !strings.Contains(out, defaultAgent) {
		t.Errorf("/agents did not list the default agent: %q", out)
	}
	if out := handleUserMessage("/agent new scribe a poetic note-taker"); !strings.Contains(out, "scribe") {
		t.Errorf("unexpected /agent new reply: %q", out)
	}
	if activeAgent != "scribe" {
		t.Errorf("activeAgent = %q after /agent new, want scribe", activeAgent)
	}
	if out := handleUserMessage("/agent use " + defaultAgent); !strings.Contains(out, defaultAgent) {
		t.Errorf("unexpected /agent use reply: %q", out)
	}
	if out := handleUserMessage("/agent scribe"); !strings.Contains(out, "scribe") {
		t.Errorf("shorthand switch failed: %q", out)
	}
	if out := handleUserMessage("/agent use nobody"); !strings.Contains(out, "does not exist") {
		t.Errorf("switching to a missing agent should fail clearly: %q", out)
	}
}
