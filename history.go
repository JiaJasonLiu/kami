package main

import (
	"encoding/json"
	"os"
)

const (
	maxHistoryEntries = 60
	maxHistoryBytes   = 48000
)

// loadHistory deserialises the conversation history from disk.
// Returning nil (rather than an empty slice) when the file is missing is idiomatic Go:
// nil slices and empty slices behave identically with append and range.
func loadHistory() []gContent {
	b, err := os.ReadFile(statePath(historyFile))
	if err != nil {
		return nil
	}
	var h []gContent
	_ = json.Unmarshal(b, &h)
	return h
}

// saveHistory trims and persists the conversation history to disk.
// Errors are intentionally swallowed (blank identifier _) because a failed write is
// non-fatal — the bot keeps running, just without persistent memory across restarts.
func saveHistory(h []gContent) {
	h = trimHistory(h)
	b, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(statePath(historyFile), b, 0o644)
}

// isUserText reports whether a content entry is a genuine user text message,
// as opposed to a tool-response turn (which also has role "user" in the Gemini API).
// This distinction is important when trimming history — we want to keep the context
// anchored at a real user message, not in the middle of a tool exchange.
func isUserText(c gContent) bool {
	if c.Role != "user" {
		return false
	}
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			return false
		}
	}
	return len(c.Parts) > 0 && c.Parts[0].Text != ""
}

// trimHistory removes the oldest entries until the history fits within both the entry
// count and byte-size budgets. It always removes from the front and re-aligns to a
// user text message, preserving a coherent conversation structure for the model.
// The for-loop with slice reslicing (h = h[1:]) is a common Go pattern for consuming
// a slice from the front without allocating a new backing array each iteration.
func trimHistory(h []gContent) []gContent {
	for {
		b, _ := json.Marshal(h)
		if (len(h) <= maxHistoryEntries && len(b) <= maxHistoryBytes) || len(h) <= 2 {
			return h
		}
		h = h[1:]
		for len(h) > 2 && !isUserText(h[0]) {
			h = h[1:]
		}
	}
}

// clearHistory deletes the history file, effectively starting a fresh conversation.
// The error is ignored because if the file doesn't exist the outcome is the same: no history.
func clearHistory() {
	_ = os.Remove(statePath(historyFile))
}
