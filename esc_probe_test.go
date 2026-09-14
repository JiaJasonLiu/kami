package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every provider — Gemini, OpenAI, Anthropic, OpenRouter, local — reaches the
// filesystem only through these tool handlers, so this is the boundary that
// decides what a non-Claude-SDK model can touch. The property that matters is
// not "the call errors" but "nothing outside the workspace is affected":
// traversal is rejected outright, while an absolute path is neutralised by
// filepath.Join, which treats it as relative and lands it harmlessly nested
// inside the workspace.
func TestToolsNeverTouchAnythingOutsideWorkspace(t *testing.T) {
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	canary := filepath.Join(outsideDir, "CANARY.txt")
	if err := os.WriteFile(canary, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	attacks := []string{
		"../../../../../../etc/passwd",
		"a/../../../escape.txt",
		canary,
		"/etc/hostname",
		"~/.ssh/id_rsa",
	}
	for _, a := range attacks {
		tReadFile(map[string]interface{}{"path": a})
		tWriteFile(map[string]interface{}{"path": a, "content": "PWNED"})
		tDeleteFile(map[string]interface{}{"path": a})
	}

	// The canary outside the workspace must be untouched and still present.
	b, err := os.ReadFile(canary)
	if err != nil {
		t.Fatalf("a file OUTSIDE the workspace was deleted: %v", err)
	}
	if string(b) != "original" {
		t.Fatalf("a file OUTSIDE the workspace was overwritten: %q", b)
	}

	// Nothing may have been created anywhere outside the workspace root.
	root := workspaceRoot()
	filepath.Walk(outsideDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && p != outsideDir && p != canary {
			t.Errorf("tool created something outside the workspace: %s", p)
		}
		return nil
	})
	if !strings.HasPrefix(root, home) {
		t.Errorf("workspace root %q escaped $KAMI_HOME %q", root, home)
	}
}

// Traversal specifically must be refused, not merely contained.
func TestTraversalIsRejected(t *testing.T) {
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"../escape.txt", "../../etc/passwd", "a/../../b"} {
		if _, err := tWriteFile(map[string]interface{}{"path": a, "content": "x"}); err == nil {
			t.Errorf("traversal %q should be rejected outright", a)
		}
	}
}

// One agent must not be able to reach another agent's workspace.
func TestAgentsCannotReadEachOther(t *testing.T) {
	home = t.TempDir()
	activeAgent = defaultAgent
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
	if _, err := tWriteFile(map[string]interface{}{"path": "secret.txt", "content": "kami-secret"}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, "agents", "spy", "workspace"), 0o755)
	activeAgent = "spy"
	defer func() { activeAgent = defaultAgent }()
	for _, p := range []string{
		"../../../workspace/secret.txt",
		filepath.Join(home, "workspace", "secret.txt"),
	} {
		if out, err := tReadFile(map[string]interface{}{"path": p}); err == nil && strings.Contains(out, "kami-secret") {
			t.Errorf("agent read another agent's file via %q -> %q", p, out)
		}
	}
}
