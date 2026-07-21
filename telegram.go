package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// tgForumTopic is the payload Telegram attaches to the service message sent
// when a forum topic is created, and echoed on reply_to_message for early
// messages within a topic. Only the name is needed to derive an agent.
type tgForumTopic struct {
	Name string `json:"name"`
}

type tgMessage struct {
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
	// MessageThreadID identifies the forum topic; absent (0) in DMs and in
	// the group's General topic.
	MessageThreadID int64 `json:"message_thread_id"`
	// IsTopicMessage is true only for messages inside a non-General topic.
	IsTopicMessage bool `json:"is_topic_message"`
	// ForumTopicCreated is set on the service message announcing a new topic.
	ForumTopicCreated *tgForumTopic `json:"forum_topic_created"`
	// ReplyToMessage lets us recover a topic's title from an ordinary message
	// when we never saw the creation event (e.g. topic made while offline).
	ReplyToMessage *tgMessage `json:"reply_to_message"`
}

// topicID returns the forum thread this message belongs to, or 0 for a DM or
// the General topic. Only genuine topic messages are treated as threaded, so
// plain reply-chains in non-forum groups don't accidentally route by thread.
func (m *tgMessage) topicID() int64 {
	if m.IsTopicMessage {
		return m.MessageThreadID
	}
	return 0
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// tgAPI constructs the full Telegram Bot API URL for a given method name,
// embedding the bot token from config. The token acts as both identifier and auth credential.
func tgAPI(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", cfg.TelegramToken, method)
}

// tgGetUpdates fetches new messages via Telegram's long-polling getUpdates API.
// The timeout parameter is the server-side long-poll duration in seconds; the HTTP client
// timeout is set slightly longer to avoid the client cutting off a valid server response.
// defer resp.Body.Close() is critical to release the connection back to the HTTP pool.
func tgGetUpdates(offset int64, timeout int) ([]tgUpdate, error) {
	u := fmt.Sprintf("%s?timeout=%d&offset=%d", tgAPI("getUpdates"), timeout, offset)
	client := &http.Client{Timeout: time.Duration(timeout+15) * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r tgUpdatesResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if !r.OK {
		return nil, errors.New("telegram getUpdates returned ok=false (check your bot token)")
	}
	return r.Result, nil
}

// tgSend sends a text message to the chat (no specific topic). It is a thin
// wrapper over tgSendToThread used for gateway-level notices like the
// online/offline banners.
func tgSend(chatID int64, text string) { tgSendToThread(chatID, 0, text) }

// tgSendToThread sends a text message to Telegram, into a specific forum
// topic when thread is non-zero, splitting it into chunks if it exceeds
// Telegram's 4096-character message limit. Errors are logged but not returned
// because a failed send is not fatal — the bot loop continues regardless.
func tgSendToThread(chatID, thread int64, text string) {
	for _, chunk := range chunk(text, 4000) {
		payload := map[string]interface{}{"chat_id": chatID, "text": chunk}
		if thread != 0 {
			payload["message_thread_id"] = thread
		}
		body, _ := json.Marshal(payload)
		resp, err := httpClient.Post(tgAPI("sendMessage"), "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("sendMessage error: %v", err)
			return
		}
		resp.Body.Close()
	}
}

// tgSendTyping sends a "typing..." indicator into the given topic so the user
// knows the bot is working. Errors are silently ignored — this is a
// best-effort UX enhancement, not a critical operation.
func tgSendTyping(chatID, thread int64) {
	payload := map[string]interface{}{"chat_id": chatID, "action": "typing"}
	if thread != 0 {
		payload["message_thread_id"] = thread
	}
	body, _ := json.Marshal(payload)
	resp, err := httpClient.Post(tgAPI("sendChatAction"), "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

// loadOffset reads the last-seen Telegram update ID from disk so the bot resumes
// from where it left off after a restart, rather than reprocessing old messages.
func loadOffset() int64 {
	b, err := os.ReadFile(statePath(offsetFile))
	if err != nil {
		return 0
	}
	var n int64
	fmt.Sscanf(string(b), "%d", &n)
	return n
}

// saveOffset persists the latest processed update ID to disk. Errors are swallowed
// because a failed write just means we might re-process one message on next restart.
func saveOffset(n int64) {
	_ = os.WriteFile(statePath(offsetFile), []byte(fmt.Sprintf("%d", n)), 0o644)
}

// tgSetCommands registers the bot's slash commands with Telegram so they appear in the
// command menu in the client UI. A local struct is defined inline here — Go allows
// type declarations inside functions, which is useful for one-off JSON shapes.
func tgSetCommands() {
	type tgCommand struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"commands": []tgCommand{
			{"new", "Wipe conversation memory and start fresh"},
			{"agents", "List agent profiles"},
			{"agent", "Create/switch/delete agent profiles"},
			{"help", "List available commands"},
		},
	})
	resp, err := httpClient.Post(tgAPI("setMyCommands"), "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("setMyCommands error: %v", err)
		return
	}
	resp.Body.Close()
}

// runBot is the main event loop: it long-polls Telegram for messages and dispatches each one
// to handleUserMessage. Signal handling is done in a goroutine (go func()) that blocks on
// a channel receive (<-sig); when a SIGINT/SIGTERM arrives the goroutine sends a goodbye
// message and exits. make(chan os.Signal, 1) uses a buffered channel so the OS signal
// delivery never blocks even if the goroutine hasn't reached the receive yet.
func runBot() {
	provider := orDefault(cfg.Provider, "gemini")
	model := "?"
	if mc, err := activeModel(); err == nil {
		model = mc.model
	}
	log.Printf("kami-gateway up. provider=%s model=%s chat=%d agent=%s", provider, model, cfg.TelegramChatID, activeAgent)
	tgSetCommands()
	go cronLoop()
	tgSend(cfg.TelegramChatID, "👋 Gateway online. Say something, or /new to reset.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		tgSend(cfg.TelegramChatID, "💤 Gateway going offline.")
		os.Exit(0)
	}()

	offset := loadOffset()
	for {
		updates, err := tgGetUpdates(offset+1, 30)
		if err != nil {
			log.Printf("getUpdates: %v (retrying in 5s)", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, up := range updates {
			offset = up.UpdateID
			saveOffset(offset)
			if up.Message == nil {
				continue
			}
			if up.Message.Chat.ID != cfg.TelegramChatID {
				log.Printf("ignoring message from unauthorised chat %d", up.Message.Chat.ID)
				continue
			}

			// A new forum topic: provision and bind an agent, then greet it.
			if tc := up.Message.ForumTopicCreated; tc != nil {
				thread := up.Message.MessageThreadID
				name, created, err := onForumTopicCreated(thread, tc.Name)
				if err != nil {
					log.Printf("forum topic %q: %v", tc.Name, err)
					continue
				}
				verb := "bound to existing"
				if created {
					verb = "created"
				}
				log.Printf("topic %q (%d) -> agent %q (%s)", tc.Name, thread, name, verb)
				tgSendToThread(cfg.TelegramChatID, thread, fmt.Sprintf("🧵 This topic now belongs to agent %q. Say hi!", name))
				continue
			}

			if up.Message.Text == "" {
				continue
			}

			// Recover a topic's agent lazily if we never saw its creation
			// event but the message still carries the topic title.
			thread := up.Message.topicID()
			if thread != 0 {
				if _, bound := topicBindings[thread]; !bound {
					if rt := up.Message.ReplyToMessage; rt != nil && rt.ForumTopicCreated != nil {
						if _, _, err := onForumTopicCreated(thread, rt.ForumTopicCreated.Name); err != nil {
							log.Printf("lazy topic bind (%d): %v", thread, err)
						}
					}
				}
			}

			// Route this message to the agent that owns its topic. The
			// per-turn routing globals are set inside runAgentTurn under a
			// lock so a concurrent cron run can't interleave with it.
			agent := agentForThread(thread)
			text := up.Message.Text
			log.Printf("user[%s]: %s", agent, truncate(text, 120))
			tgSendTyping(cfg.TelegramChatID, thread)
			reply := runAgentTurn(agent, thread, text)
			tgSendToThread(cfg.TelegramChatID, thread, reply)
		}
	}
}
