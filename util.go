package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

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

func mask(s string) string {
	if len(s) <= 6 {
		if s == "" {
			return "(unset)"
		}
		return "******"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

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
