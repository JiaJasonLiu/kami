package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// safePath resolves a relative path inside the workspace and rejects any path that would
// escape the sandbox via absolute paths or ".." traversal. This is a classic path traversal
// defence: filepath.Clean normalises the path, then a prefix check on the absolute form
// confirms it stays inside the workspace root.
func safePath(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute paths are not allowed; use a path relative to the workspace")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes the workspace")
	}
	root, err := filepath.Abs(workspaceRoot())
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, clean)
	rootWithSep := root + string(os.PathSeparator)
	if joined != root && !strings.HasPrefix(joined, rootWithSep) {
		return "", errors.New("path escapes the workspace")
	}
	return joined, nil
}

// mask partially redacts a secret string for display, showing only the first and last
// three characters. This is safer than printing the full value in logs or tool output.
func mask(s string) string {
	if len(s) <= 6 {
		if s == "" {
			return "(unset)"
		}
		return "******"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

// truncate shortens a string to at most n bytes, appending an ellipsis when clipped.
// Used to keep log lines readable when printing large API payloads.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// chunk splits a string into segments of at most size bytes, preferring to break at the
// last newline in the window (when it falls in the second half) to avoid splitting mid-line.
// This is used to stay within Telegram's per-message character limit.
func chunk(s string, size int) []string {
	if len(s) <= size {
		return []string{s}
	}
	var out []string
	for len(s) > size {
		cut := size
		if i := strings.LastIndex(s[:size], "\n"); i > size/2 {
			cut = i
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}
