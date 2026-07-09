package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// Provider selects the active AI backend: gemini (default), openai,
	// anthropic, openrouter, or local. Each backend keeps its own key/model
	// below so switching never discards credentials.
	Provider string `json:"provider,omitempty"`

	// Gemini (Google AI Studio).
	GeminiAPIKey string `json:"gemini_api_key"`
	GeminiModel  string `json:"gemini_model"`

	// OpenAI. Base URL is overridable for OpenAI-compatible gateways.
	OpenAIAPIKey  string `json:"openai_api_key,omitempty"`
	OpenAIModel   string `json:"openai_model,omitempty"`
	OpenAIBaseURL string `json:"openai_base_url,omitempty"`

	// Anthropic (Claude Messages API).
	AnthropicAPIKey  string `json:"anthropic_api_key,omitempty"`
	AnthropicModel   string `json:"anthropic_model,omitempty"`
	AnthropicBaseURL string `json:"anthropic_base_url,omitempty"`

	// OpenRouter (OpenAI-compatible aggregator).
	OpenRouterAPIKey string `json:"openrouter_api_key,omitempty"`
	OpenRouterModel  string `json:"openrouter_model,omitempty"`

	// Local OpenAI-compatible server (Ollama, LM Studio, llama.cpp, vLLM…).
	LocalBaseURL string `json:"local_base_url,omitempty"`
	LocalModel   string `json:"local_model,omitempty"`
	LocalAPIKey  string `json:"local_api_key,omitempty"`

	TelegramToken  string `json:"telegram_token"`
	TelegramChatID int64  `json:"telegram_chat_id"`

	// BraveAPIKey enables the web_search tool (Brave Search API). Web access is
	// optional: web_fetch works without it, and if it is empty web_search
	// returns a configuration hint instead of searching.
	BraveAPIKey string `json:"brave_api_key,omitempty"`
}

// configComplete reports whether the gateway has enough config to run: a
// Telegram token + chat id, and a fully specified active provider.
func configComplete() error {
	if cfg.TelegramToken == "" || cfg.TelegramChatID == 0 {
		return errors.New("telegram token or chat id is missing")
	}
	if _, err := activeModel(); err != nil {
		return err
	}
	return nil
}

var cfg Config

// loadConfig reads and JSON-decodes the config file into the global cfg variable.
// It returns (false, nil) when the file simply doesn't exist yet — a common Go pattern
// for distinguishing "not found" from a real I/O error using errors.Is.
func loadConfig() (bool, error) {
	b, err := os.ReadFile(statePath(configFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, err
	}
	return true, nil
}

// saveConfig serialises cfg to indented JSON and writes it with mode 0o600 (owner read/write only)
// so the file containing API keys is not world-readable.
func saveConfig() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(statePath(configFile), b, 0o600)
}

// runSetup walks the user through an interactive first-run wizard on stdin.
// bufio.NewReader wraps os.Stdin so we can read whole lines efficiently;
// ReadString('\n') blocks until the user presses Enter.
func runSetup() error {
	in := bufio.NewReader(os.Stdin)
	fmt.Println("=== kami-gateway setup ===")
	fmt.Println("(everything is stored locally under ./state — nothing leaves this machine except calls to your AI provider, Telegram, and any web pages the agent looks up)")
	fmt.Println()

	cfg.Provider = strings.ToLower(strings.TrimSpace(prompt(in, "AI provider (gemini/openai/anthropic/openrouter/local)", orDefault(cfg.Provider, "gemini"))))
	switch cfg.Provider {
	case "", "gemini":
		cfg.Provider = "gemini"
		cfg.GeminiAPIKey = strings.TrimSpace(prompt(in, "Gemini API key", cfg.GeminiAPIKey))
		cfg.GeminiModel = strings.TrimSpace(prompt(in, "Gemini model", orDefault(cfg.GeminiModel, "gemini-2.0-flash")))
	case "openai":
		cfg.OpenAIAPIKey = strings.TrimSpace(prompt(in, "OpenAI API key", cfg.OpenAIAPIKey))
		cfg.OpenAIModel = strings.TrimSpace(prompt(in, "OpenAI model", orDefault(cfg.OpenAIModel, "gpt-4o-mini")))
		cfg.OpenAIBaseURL = strings.TrimSpace(prompt(in, "OpenAI base URL (blank for api.openai.com)", cfg.OpenAIBaseURL))
	case "anthropic":
		cfg.AnthropicAPIKey = strings.TrimSpace(prompt(in, "Anthropic API key", cfg.AnthropicAPIKey))
		cfg.AnthropicModel = strings.TrimSpace(prompt(in, "Anthropic model", orDefault(cfg.AnthropicModel, "claude-3-5-sonnet-latest")))
	case "openrouter":
		cfg.OpenRouterAPIKey = strings.TrimSpace(prompt(in, "OpenRouter API key", cfg.OpenRouterAPIKey))
		cfg.OpenRouterModel = strings.TrimSpace(prompt(in, "OpenRouter model", orDefault(cfg.OpenRouterModel, "openai/gpt-4o-mini")))
	case "local":
		cfg.LocalBaseURL = strings.TrimSpace(prompt(in, "Local server base URL", orDefault(cfg.LocalBaseURL, "http://localhost:11434/v1")))
		cfg.LocalModel = strings.TrimSpace(prompt(in, "Local model name", orDefault(cfg.LocalModel, "llama3.1")))
		cfg.LocalAPIKey = strings.TrimSpace(prompt(in, "Local API key (blank if none)", cfg.LocalAPIKey))
	default:
		fmt.Printf("Unknown provider %q — falling back to gemini.\n", cfg.Provider)
		cfg.Provider = "gemini"
		cfg.GeminiAPIKey = strings.TrimSpace(prompt(in, "Gemini API key", cfg.GeminiAPIKey))
		cfg.GeminiModel = strings.TrimSpace(prompt(in, "Gemini model", orDefault(cfg.GeminiModel, "gemini-2.0-flash")))
	}
	cfg.TelegramToken = strings.TrimSpace(prompt(in, "Telegram bot token (from @BotFather)", cfg.TelegramToken))
	cfg.BraveAPIKey = strings.TrimSpace(prompt(in, "Brave Search API key for web_search (optional, blank to skip)", cfg.BraveAPIKey))

	if cfg.TelegramChatID == 0 {
		fmt.Println()
		fmt.Println("Now I need to learn which chat you'll talk to me from.")
		fmt.Println("• Direct messages: open your bot in Telegram and send it any message.")
		fmt.Println("• Forum group (one agent per topic): add the bot to the group and")
		fmt.Println("  disable its privacy mode in @BotFather, then send a message in the group.")
		fmt.Print("Then press Enter here to detect it (or type a chat id manually): ")
		line, _ := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			fmt.Sscanf(line, "%d", &cfg.TelegramChatID)
		} else {
			id, err := detectChatID()
			if err != nil {
				return err
			}
			cfg.TelegramChatID = id
			fmt.Printf("Detected chat id: %d\n", id)
		}
	}

	if err := saveConfig(); err != nil {
		return err
	}
	if err := ensureScaffold(); err != nil {
		return err
	}
	fmt.Println("\n✅ Setup complete. Run the binary again with no arguments to start the gateway.")
	return nil
}

// detectChatID polls Telegram for up to 60 seconds waiting for the user to send the bot
// any message, then returns the chat ID from the first message received.
// This avoids making the user look up their own chat ID manually.
func detectChatID() (int64, error) {
	deadline := time.Now().Add(60 * time.Second)
	offset := int64(0)
	for time.Now().Before(deadline) {
		updates, err := tgGetUpdates(offset+1, 10)
		if err != nil {
			return 0, err
		}
		for _, up := range updates {
			offset = up.UpdateID
			if up.Message != nil && up.Message.Chat.ID != 0 {
				saveOffset(up.UpdateID)
				return up.Message.Chat.ID, nil
			}
		}
	}
	return 0, errors.New("timed out waiting for a message; send your bot a message and re-run setup")
}

// prompt prints a labelled question to stdout and reads one line of user input.
// If the user presses Enter without typing, the provided default value is returned —
// a common CLI pattern for showing the current/suggested value in brackets.
func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return def
	}
	return line
}

// orDefault returns def when s is the empty string. It mirrors the common pattern
// of falling back to a default, similar to how environment variables are often handled.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
