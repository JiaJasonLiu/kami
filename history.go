package main

import (
	"encoding/json"
	"os"
)

const (
	maxHistoryEntries = 60
	maxHistoryBytes   = 48000
)

func loadHistory() []gContent {
	b, err := os.ReadFile(statePath(historyFile))
	if err != nil {
		return nil
	}
	var h []gContent
	_ = json.Unmarshal(b, &h)
	return h
}

func saveHistory(h []gContent) {
	h = trimHistory(h)
	b, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(statePath(historyFile), b, 0o644)
}

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

func clearHistory() {
	_ = os.Remove(statePath(historyFile))
}
