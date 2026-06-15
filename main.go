// kami-gateway: a very small, privacy-first AI gateway you talk to over Telegram.
//
// The model gets a SOUL.md (its system prompt, which it can edit),
// a sandboxed workspace it cannot escape, and a handful of tools defined in
// tools.json (which it can also edit). Single user, single chat, Gemini only.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Paths & layout
// ---------------------------------------------------------------------------
//
//	$KAMI_HOME (default ".")
//	├── state/
//	│   ├── config.json    secrets + model (chmod 600)
//	│   ├── SOUL.md         the model's system prompt (model can edit)
//	│   ├── tools.json      tool registry (model can edit)
//	│   ├── history.json    persistent conversation (cleared by /new)
//	│   └── offset.txt      last processed Telegram update id
//	└── workspace/          the ONLY place file tools can touch

var home string

func statePath(name string) string { return filepath.Join(home, "state", name) }
func workspaceRoot() string        { return filepath.Join(home, "workspace") }

const (
	configFile  = "config.json"
	soulFile    = "SOUL.md"
	toolsFile   = "tools.json"
	historyFile = "history.json"
	offsetFile  = "offset.txt"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	GeminiAPIKey   string `json:"gemini_api_key"`
	GeminiModel    string `json:"gemini_model"`
	TelegramToken  string `json:"telegram_token"`
	TelegramChatID int64  `json:"telegram_chat_id"`
}

var cfg Config

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

func saveConfig() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// 0600: secrets stay readable only by the owner. Privacy first.
	return os.WriteFile(statePath(configFile), b, 0o600)
}

// ---------------------------------------------------------------------------
// Workspace sandbox  (the "docker without docker" bit)
// ---------------------------------------------------------------------------
//
// Every file tool path is resolved relative to workspace/ and then re-checked
// to make sure it never escapes the workspace (no absolute paths, no ".." ).

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
	// Belt and braces: confirm the result is still inside root.
	rootWithSep := root + string(os.PathSeparator)
	if joined != root && !strings.HasPrefix(joined, rootWithSep) {
		return "", errors.New("path escapes the workspace")
	}
	return joined, nil
}

// ---------------------------------------------------------------------------
// Gemini API types
// ---------------------------------------------------------------------------

type gPart struct {
	Text             string         `json:"text,omitempty"`
	FunctionCall     *gFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *gFunctionResp `json:"functionResponse,omitempty"`
}

type gFunctionCall struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type gFunctionResp struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gSystemInstruction struct {
	Parts []gPart `json:"parts"`
}

type gToolDecl struct {
	FunctionDeclarations []gFunctionDecl `json:"functionDeclarations"`
}

type gFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type gRequest struct {
	SystemInstruction *gSystemInstruction `json:"system_instruction,omitempty"`
	Contents          []gContent          `json:"contents"`
	Tools             []gToolDecl         `json:"tools,omitempty"`
}

type gResponse struct {
	Candidates []struct {
		Content      gContent `json:"content"`
		FinishReason string   `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

// Overridable so tests (and future proxy/Vertex setups) can point elsewhere.
var geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

const geminiMaxAttempts = 3

var geminiBackoffBase = 2 * time.Second

func callGemini(req gRequest) (*gResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= geminiMaxAttempts; attempt++ {
		gr, retryable, err := callGeminiOnce(req)
		if err == nil {
			return gr, nil
		}
		lastErr = err
		if !retryable || attempt == geminiMaxAttempts {
			break
		}
		backoff := time.Duration(attempt) * geminiBackoffBase
		log.Printf("gemini transient error (attempt %d/%d): %v; retrying in %s", attempt, geminiMaxAttempts, err, backoff)
		time.Sleep(backoff)
	}
	return nil, lastErr
}

// callGeminiOnce returns (response, retryable, error). retryable is true for
// network blips and 429/5xx responses.
func callGeminiOnce(req gRequest) (*gResponse, bool, error) {
	endpoint := fmt.Sprintf(
		"%s/models/%s:generateContent?key=%s",
		geminiBaseURL, url.PathEscape(cfg.GeminiModel), url.QueryEscape(cfg.GeminiAPIKey),
	)
	body, _ := json.Marshal(req)
	resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, true, err // network errors are worth a retry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500

	var gr gResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, retryable, fmt.Errorf("decoding Gemini response (HTTP %d): %v\n%s", resp.StatusCode, err, truncate(string(raw), 500))
	}
	if gr.Error != nil {
		return nil, retryable, fmt.Errorf("Gemini error %d (%s): %s", gr.Error.Code, gr.Error.Status, gr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, retryable, fmt.Errorf("Gemini HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return &gr, false, nil
}

// ---------------------------------------------------------------------------
// Tool registry
// ---------------------------------------------------------------------------
//
// tools.json holds the *declarations* (name/description/enabled/parameters) so
// the model can read and tune them. The *implementations* live here in Go and
// are matched by name. A declared tool with no matching handler simply errors
// when called — safe by construction (the model can't invent new powers).

type ToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolsFile struct {
	Tools []ToolDecl `json:"tools"`
}

type toolHandler func(args map[string]interface{}) (string, error)

var handlers = map[string]toolHandler{
	"list_files":  tListFiles,
	"read_file":   tReadFile,
	"write_file":  tWriteFile,
	"delete_file": tDeleteFile,
	"read_soul":   tReadSoul,
	"write_soul":  tWriteSoul,
	"read_tools":  tReadTools,
	"write_tools": tWriteTools,
	"get_config":  tGetConfig,
	"set_config":  tSetConfig,
}

func loadTools() (ToolsFile, error) {
	var tf ToolsFile
	b, err := os.ReadFile(statePath(toolsFile))
	if err != nil {
		return tf, err
	}
	err = json.Unmarshal(b, &tf)
	return tf, err
}

// enabledDeclarations returns the tool declarations to send to Gemini: enabled
// tools that also have a real handler behind them.
func enabledDeclarations() ([]gFunctionDecl, error) {
	tf, err := loadTools()
	if err != nil {
		return nil, err
	}
	var out []gFunctionDecl
	for _, t := range tf.Tools {
		if !t.Enabled {
			continue
		}
		if _, ok := handlers[t.Name]; !ok {
			continue
		}
		out = append(out, gFunctionDecl{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out, nil
}

func execTool(name string, args map[string]interface{}) string {
	h, ok := handlers[name]
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", name)
	}
	res, err := h(args)
	if err != nil {
		return "error: " + err.Error()
	}
	return res
}

// ---- argument helpers ----

func argStr(args map[string]interface{}, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

// ---- file tools (workspace only) ----

func tListFiles(_ map[string]interface{}) (string, error) {
	root := workspaceRoot()
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, fmt.Sprintf("%s (%d bytes)", rel, info.Size()))
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "workspace is empty", nil
	}
	return strings.Join(files, "\n"), nil
}

func tReadFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	abs, err := safePath(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tWriteFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	abs, err := safePath(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), rel), nil
}

func tDeleteFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	abs, err := safePath(rel)
	if err != nil {
		return "", err
	}
	if err := os.Remove(abs); err != nil {
		return "", err
	}
	return "deleted " + rel, nil
}

// ---- self-editing tools (SOUL.md, tools.json, config) ----

func tReadSoul(_ map[string]interface{}) (string, error) {
	b, err := os.ReadFile(statePath(soulFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tWriteSoul(args map[string]interface{}) (string, error) {
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("refusing to write an empty SOUL.md")
	}
	if err := os.WriteFile(statePath(soulFile), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "SOUL.md updated; it takes effect on your next reply", nil
}

func tReadTools(_ map[string]interface{}) (string, error) {
	b, err := os.ReadFile(statePath(toolsFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tWriteTools(args map[string]interface{}) (string, error) {
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	var probe ToolsFile
	if err := json.Unmarshal([]byte(content), &probe); err != nil {
		return "", fmt.Errorf("not valid tools.json: %v", err)
	}
	if err := os.WriteFile(statePath(toolsFile), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "tools.json updated; changes take effect on your next reply", nil
}

func tGetConfig(_ map[string]interface{}) (string, error) {
	out := map[string]interface{}{
		"gemini_model":            cfg.GeminiModel,
		"gemini_api_key":          mask(cfg.GeminiAPIKey),
		"telegram_token":          mask(cfg.TelegramToken),
		"telegram_chat_id":        cfg.TelegramChatID,
		"editable_via_set_config": []string{"gemini_model", "gemini_api_key"},
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

func tSetConfig(args map[string]interface{}) (string, error) {
	key, err := argStr(args, "key")
	if err != nil {
		return "", err
	}
	value, err := argStr(args, "value")
	if err != nil {
		return "", err
	}
	switch key {
	case "gemini_model":
		cfg.GeminiModel = value
	case "gemini_api_key":
		cfg.GeminiAPIKey = value
	default:
		// Telegram settings are intentionally not editable here to avoid the
		// agent locking itself out of its only channel.
		return "", fmt.Errorf("key %q is not editable via set_config (allowed: gemini_model, gemini_api_key)", key)
	}
	if err := saveConfig(); err != nil {
		return "", err
	}
	return fmt.Sprintf("config %s updated", key), nil
}

// ---------------------------------------------------------------------------
// Conversation history (persistent, cleared by /new)
// ---------------------------------------------------------------------------

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

// Keep conversation memory bounded so requests stay cheap and never overflow
// the context window. /new still wipes everything.
const (
	maxHistoryEntries = 60
	maxHistoryBytes   = 48000
)

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

// trimHistory drops the oldest turns when history grows too large, always
// leaving the slice starting on a real user message so we never orphan a
// functionCall/functionResponse pair (Gemini rejects a dangling one).
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

// ---------------------------------------------------------------------------
// The agent loop  (bounded — the kami way)
// ---------------------------------------------------------------------------

const maxToolSteps = 8

func handleUserMessage(text string) string {
	switch strings.TrimSpace(text) {
	case "/new":
		clearHistory()
		return "🧹 Started a fresh conversation."
	case "/help":
		return "Commands:\n/new — wipe conversation memory\n/help — this message\nAnything else is sent to the model."
	case "/start":
		return "Hi. I'm your gateway. Talk to me normally, or /new to start over."
	}

	soul, err := os.ReadFile(statePath(soulFile))
	if err != nil {
		return "⚠️ couldn't read SOUL.md: " + err.Error()
	}
	decls, err := enabledDeclarations()
	if err != nil {
		return "⚠️ couldn't read tools.json: " + err.Error()
	}

	history := loadHistory()
	history = append(history, gContent{Role: "user", Parts: []gPart{{Text: text}}})
	history = trimHistory(history)

	var tools []gToolDecl
	if len(decls) > 0 {
		tools = []gToolDecl{{FunctionDeclarations: decls}}
	}
	sys := &gSystemInstruction{Parts: []gPart{{Text: string(soul)}}}

	for step := 0; step < maxToolSteps; step++ {
		resp, err := callGemini(gRequest{SystemInstruction: sys, Contents: history, Tools: tools})
		if err != nil {
			return "⚠️ " + err.Error()
		}
		if len(resp.Candidates) == 0 {
			if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
				return "⚠️ blocked: " + resp.PromptFeedback.BlockReason
			}
			return "⚠️ the model returned no candidates"
		}

		modelContent := resp.Candidates[0].Content
		modelContent.Role = "model"
		history = append(history, modelContent)

		// Collect any function calls in this turn.
		var calls []*gFunctionCall
		var textOut strings.Builder
		for _, p := range modelContent.Parts {
			if p.FunctionCall != nil {
				calls = append(calls, p.FunctionCall)
			}
			if p.Text != "" {
				textOut.WriteString(p.Text)
			}
		}

		if len(calls) == 0 {
			saveHistory(history)
			out := strings.TrimSpace(textOut.String())
			if out == "" {
				out = "(the model replied with nothing)"
			}
			return out
		}

		// Run each tool and feed the results back as a single user turn.
		var respParts []gPart
		for _, c := range calls {
			log.Printf("tool: %s(%v)", c.Name, c.Args)
			result := execTool(c.Name, c.Args)
			respParts = append(respParts, gPart{FunctionResponse: &gFunctionResp{
				Name:     c.Name,
				Response: map[string]interface{}{"result": result},
			}})
		}
		history = append(history, gContent{Role: "user", Parts: respParts})
	}

	saveHistory(history)
	return fmt.Sprintf("⚠️ stopped after %d tool steps without a final answer.", maxToolSteps)
}

// ---------------------------------------------------------------------------
// Telegram (long polling — no inbound server, no public IP, privacy first)
// ---------------------------------------------------------------------------

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
			// Single-user lock: ignore everyone except the configured chat.
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

// ---------------------------------------------------------------------------
// Setup wizard
// ---------------------------------------------------------------------------

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
				saveOffset(up.UpdateID) // don't reprocess this message at runtime
				return up.Message.Chat.ID, nil
			}
		}
	}
	return 0, errors.New("timed out waiting for a message; send your bot a message and re-run setup")
}

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

// ---------------------------------------------------------------------------
// Scaffolding: create state/, workspace/, and default SOUL.md + tools.json
// ---------------------------------------------------------------------------

func ensureDirs() error {
	if err := os.MkdirAll(statePath(""), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(workspaceRoot(), 0o755)
}

func ensureScaffold() error {
	if err := ensureDirs(); err != nil {
		return err
	}
	if _, err := os.Stat(statePath(soulFile)); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(statePath(soulFile), []byte(defaultSoul), 0o644); err != nil {
			return err
		}
	}
	if _, err := os.Stat(statePath(toolsFile)); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(statePath(toolsFile), []byte(defaultTools), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mask(s string) string {
	if len(s) <= 6 {
		if s == "" {
			return "(unset)"
		}
		return "******"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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
		// try to break on a newline for readability
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

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ltime)
	home = orDefault(os.Getenv("KAMI_HOME"), ".")

	if err := ensureDirs(); err != nil {
		log.Fatalf("could not create directories: %v", err)
	}

	forceSetup := len(os.Args) > 1 && (os.Args[1] == "setup" || os.Args[1] == "--setup")
	exists, err := loadConfig()
	if err != nil {
		log.Fatalf("could not read config: %v", err)
	}

	if forceSetup || !exists {
		if err := runSetup(); err != nil {
			log.Fatalf("setup failed: %v", err)
		}
		if forceSetup {
			return
		}
	}

	if err := ensureScaffold(); err != nil {
		log.Fatalf("could not create scaffold: %v", err)
	}
	if cfg.GeminiAPIKey == "" || cfg.TelegramToken == "" || cfg.TelegramChatID == 0 {
		log.Fatalf("config incomplete; run: %s setup", os.Args[0])
	}

	runBot()
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

const defaultSoul = `# SOUL

You are a small, private assistant that lives on your owner's machine and talks
to them over Telegram. You have a persistent memory of this conversation until
they say /new.

## Your environment
- You have a **workspace**: a single sandboxed folder. The file tools can only
  read and write inside it. You cannot see anything else on the machine.
- This file (**SOUL.md**) is your own system prompt. You may rewrite it with the
  write_soul tool when your owner asks you to change who you are or how you act.
- **tools.json** lists the tools you can use. You may edit it with write_tools
  to enable/disable tools or improve their descriptions. (You cannot create
  brand-new abilities — only ones the program already implements will work.)

## How to behave
- Be concise and direct. This is a phone chat, not an essay.
- Use tools when they help; otherwise just answer.
- When you change SOUL.md, tools.json, or config, tell your owner what you did.
- Keep the workspace tidy. Use it as your notebook and memory store.

## Identity
You don't have a fixed personality yet. Ask your owner how they'd like you to
be, then write it into this file.
`

const defaultTools = `{
  "tools": [
    {
      "name": "list_files",
      "description": "List every file in your workspace with its size.",
      "enabled": true,
      "parameters": { "type": "object", "properties": {} }
    },
    {
      "name": "read_file",
      "description": "Read a file from your workspace.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": { "path": { "type": "string", "description": "Path relative to the workspace." } },
        "required": ["path"]
      }
    },
    {
      "name": "write_file",
      "description": "Create or overwrite a file in your workspace.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Path relative to the workspace." },
          "content": { "type": "string", "description": "The full file contents." }
        },
        "required": ["path", "content"]
      }
    },
    {
      "name": "delete_file",
      "description": "Delete a file from your workspace.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": { "path": { "type": "string", "description": "Path relative to the workspace." } },
        "required": ["path"]
      }
    },
    {
      "name": "read_soul",
      "description": "Read your current SOUL.md (your system prompt).",
      "enabled": true,
      "parameters": { "type": "object", "properties": {} }
    },
    {
      "name": "write_soul",
      "description": "Replace your SOUL.md with new content. Use when asked to change your personality or rules.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": { "content": { "type": "string", "description": "The full new SOUL.md." } },
        "required": ["content"]
      }
    },
    {
      "name": "read_tools",
      "description": "Read the current tools.json.",
      "enabled": true,
      "parameters": { "type": "object", "properties": {} }
    },
    {
      "name": "write_tools",
      "description": "Replace tools.json. Must be valid JSON in the same shape. You can toggle 'enabled' or edit descriptions, but only tools the program implements will actually run.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": { "content": { "type": "string", "description": "The full new tools.json." } },
        "required": ["content"]
      }
    },
    {
      "name": "get_config",
      "description": "Read your configuration (model, and masked API keys).",
      "enabled": true,
      "parameters": { "type": "object", "properties": {} }
    },
    {
      "name": "set_config",
      "description": "Change a config value. Allowed keys: gemini_model, gemini_api_key.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": {
          "key": { "type": "string", "description": "gemini_model or gemini_api_key" },
          "value": { "type": "string", "description": "The new value." }
        },
        "required": ["key", "value"]
      }
    }
  ]
}
`
