package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestWorkspace(t *testing.T) *SafeWorkspace {
	t.Helper()
	ws, err := NewSafeWorkspace(filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatalf("NewSafeWorkspace: %v", err)
	}
	return ws
}

func TestNewSafeWorkspaceCreatesRoot(t *testing.T) {
	ws := newTestWorkspace(t)
	info, err := os.Stat(ws.RootPath)
	if err != nil {
		t.Fatalf("root was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root is not a directory")
	}
	if !filepath.IsAbs(ws.RootPath) {
		t.Fatalf("RootPath %q is not absolute", ws.RootPath)
	}
}

func TestNewSafeWorkspaceRejectsEmpty(t *testing.T) {
	if _, err := NewSafeWorkspace("  "); err == nil {
		t.Fatal("expected empty root name to be rejected")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	ws := newTestWorkspace(t)
	want := []byte("remember the milk")
	if err := ws.WriteFile("notes/projects/ideas.md", want); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ws.ReadFile("notes/projects/ideas.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
	info, err := os.Stat(filepath.Join(ws.RootPath, "notes/projects/ideas.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("file mode = %o, want 0644", perm)
	}
}

func TestTraversalIsBlocked(t *testing.T) {
	ws := newTestWorkspace(t)
	attacks := []string{
		"../escape.txt",
		"../../etc/passwd",
		"notes/../../../../etc/shadow",
		"notes/../..",
		"..",
		"",
		"   ",
	}
	for _, p := range attacks {
		if err := ws.WriteFile(p, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) was allowed, want security error", p)
		}
		if _, err := ws.ReadFile(p); err == nil {
			t.Errorf("ReadFile(%q) was allowed, want security error", p)
		}
		if _, err := ws.ListFiles(p); err == nil && strings.Contains(p, "..") {
			t.Errorf("ListFiles(%q) was allowed, want security error", p)
		}
		if err := ws.DeleteFile(p); err == nil {
			t.Errorf("DeleteFile(%q) was allowed, want security error", p)
		}
	}
}

// An absolute userPath is treated as relative to the root by filepath.Join,
// so it must stay inside the sandbox rather than reach the real path.
func TestAbsolutePathStaysInside(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile("/etc/passwd", []byte("not really")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.RootPath, "etc", "passwd")); err != nil {
		t.Fatalf("expected file inside sandbox at etc/passwd: %v", err)
	}
}

// A sibling directory sharing the root as a string prefix (e.g. /opt/agent
// vs /opt/agent-evil) must not pass the prefix check.
func TestSiblingPrefixIsBlocked(t *testing.T) {
	base := t.TempDir()
	ws, err := NewSafeWorkspace(filepath.Join(base, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("../agent-evil/pwned.txt", []byte("x")); err == nil {
		t.Fatal("write into sibling prefix directory was allowed")
	}
}

func TestListFiles(t *testing.T) {
	ws := newTestWorkspace(t)
	seed := map[string]string{
		"a.txt":         "1",
		"notes/b.md":    "2",
		"notes/deep/c":  "3",
		"other/d.trace": "4",
	}
	for p, c := range seed {
		if err := ws.WriteFile(p, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := ws.ListFiles("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(seed) {
		t.Fatalf("ListFiles(\"\") = %v, want %d files", all, len(seed))
	}
	sub, err := ws.ListFiles("notes")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("notes", "b.md"), filepath.Join("notes", "deep", "c")}
	if !reflect.DeepEqual(sub, want) {
		t.Fatalf("ListFiles(\"notes\") = %v, want %v", sub, want)
	}
}

func TestDeleteFile(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile("gone.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := ws.DeleteFile("gone.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := ws.ReadFile("gone.txt"); err == nil {
		t.Fatal("file still readable after delete")
	}
	if err := ws.DeleteFile("notes"); err == nil {
		// deleting a directory (or missing path) must error
		t.Fatal("expected error deleting a non-file path")
	}
}
