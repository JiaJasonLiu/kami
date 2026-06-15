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
	GeminiAPIKey   string `json:"gemini_api_key"`
	GeminiModel    string `json:"gemini_model"`
	TelegramToken  string `json:"telegram_token"`
	TelegramChatID int64  `json:"telegram_chat_id"`
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
	fmt.Println("(everything is stored locally under ./state — nothing leaves this machine except calls to Gemini and Telegram)")
	fmt.Println()

	cfg.GeminiAPIKey = strings.TrimSpace(prompt(in, "Gemini API key", cfg.GeminiAPIKey))
	cfg.GeminiModel = strings.TrimSpace(prompt(in, "Gemini model", orDefault(cfg.GeminiModel, "gemini-2.0-flash")))
	cfg.TelegramToken = strings.TrimSpace(prompt(in, "Telegram bot token (from @BotFather)", cfg.TelegramToken))

	if cfg.TelegramChatID == 0 {
		fmt.Println()
		fmt.Println("Now I need to learn which chat you'll talk to me from.")
		fmt.Println("Open Telegram, find your bot, and send it any message.")
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
