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

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type tgUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

func tgAPI(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", cfg.TelegramToken, method)
}

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

func tgSend(chatID int64, text string) {
	for _, chunk := range chunk(text, 4000) {
		payload, _ := json.Marshal(map[string]interface{}{"chat_id": chatID, "text": chunk})
		resp, err := httpClient.Post(tgAPI("sendMessage"), "application/json", bytes.NewReader(payload))
		if err != nil {
			log.Printf("sendMessage error: %v", err)
			return
		}
		resp.Body.Close()
	}
}

func tgSendTyping(chatID int64) {
	payload, _ := json.Marshal(map[string]interface{}{"chat_id": chatID, "action": "typing"})
	resp, err := httpClient.Post(tgAPI("sendChatAction"), "application/json", bytes.NewReader(payload))
	if err == nil {
		resp.Body.Close()
	}
}

func loadOffset() int64 {
	b, err := os.ReadFile(statePath(offsetFile))
	if err != nil {
		return 0
	}
	var n int64
	fmt.Sscanf(string(b), "%d", &n)
	return n
}

func saveOffset(n int64) {
	_ = os.WriteFile(statePath(offsetFile), []byte(fmt.Sprintf("%d", n)), 0o644)
}

func runBot() {
	log.Printf("kami-gateway up. model=%s chat=%d", cfg.GeminiModel, cfg.TelegramChatID)
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
			if up.Message == nil || up.Message.Text == "" {
				continue
			}
			if up.Message.Chat.ID != cfg.TelegramChatID {
				log.Printf("ignoring message from unauthorised chat %d", up.Message.Chat.ID)
				continue
			}
			text := up.Message.Text
			log.Printf("user: %s", truncate(text, 120))
			tgSendTyping(cfg.TelegramChatID)
			reply := handleUserMessage(text)
			tgSend(cfg.TelegramChatID, reply)
		}
	}
}
