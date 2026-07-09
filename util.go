package main

import (
	"strings"
)

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
