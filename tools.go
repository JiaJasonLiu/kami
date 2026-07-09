package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"kami-gateway/internal/coderelay"
	"kami-gateway/internal/workspace"
)

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
	"list_files":    tListFiles,
	"read_file":     tReadFile,
	"write_file":    tWriteFile,
	"delete_file":   tDeleteFile,
	"read_soul":     tReadSoul,
	"write_soul":    tWriteSoul,
	"read_tools":    tReadTools,
	"write_tools":   tWriteTools,
	"get_config":    tGetConfig,
	"set_config":    tSetConfig,
	"relay_to_code": tRelayToCode,
}

// loadTools reads and parses tools.json from the state directory.
// Returning a value type (ToolsFile) rather than a pointer is fine here because the
// struct is only used locally; Go copies are cheap for small structs.
func loadTools() (ToolsFile, error) {
	var tf ToolsFile
	b, err := os.ReadFile(agentStatePath(toolsFile))
	if err != nil {
		return tf, err
	}
	err = json.Unmarshal(b, &tf)
	return tf, err
}

// enabledDeclarations returns only the tool declarations that are both enabled in tools.json
// AND have a corresponding handler registered in the handlers map.
// This two-gate check prevents the model from calling tools the program doesn't implement.
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

// execTool dispatches a tool call by name and always returns a plain string result.
// Errors are converted to "error: ..." strings so the model receives them as tool output
// rather than crashing the agent loop — a key resilience pattern for agentic systems.
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

// argStr extracts a required string argument from the generic args map.
// JSON unmarshalling into map[string]interface{} gives string values as interface{},
// so a type assertion v.(string) is needed — and we return an error if it fails.
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

// agentWorkspace opens the sandbox rooted at the workspace directory. All file tools
// go through the workspace package (SYSTEM LAYER 1), which is the single enforcement
// point for path validation — no tool touches the os package with a model-chosen path.
func agentWorkspace() (*workspace.SafeWorkspace, error) {
	return workspace.NewSafeWorkspace(workspaceRoot())
}

// tListFiles walks the workspace directory tree and returns a newline-separated list of
// relative paths with sizes. The blank identifier _ discards the unused args parameter,
// which is required by the toolHandler function type signature.
func tListFiles(_ map[string]interface{}) (string, error) {
	ws, err := agentWorkspace()
	if err != nil {
		return "", err
	}
	paths, err := ws.ListFiles("")
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "workspace is empty", nil
	}
	var files []string
	for _, rel := range paths {
		info, err := os.Stat(filepath.Join(ws.RootPath, rel))
		if err != nil {
			return "", err
		}
		files = append(files, fmt.Sprintf("%s (%d bytes)", rel, info.Size()))
	}
	return strings.Join(files, "\n"), nil
}

// tReadFile reads a workspace file by relative path; the workspace package validates
// the path to prevent directory traversal attacks.
func tReadFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	ws, err := agentWorkspace()
	if err != nil {
		return "", err
	}
	b, err := ws.ReadFile(rel)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// tWriteFile creates or overwrites a file in the workspace. The workspace package
// creates any intermediate directories, so the model can create files in
// subdirectories without a separate mkdir step.
func tWriteFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	ws, err := agentWorkspace()
	if err != nil {
		return "", err
	}
	if err := ws.WriteFile(rel, []byte(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), rel), nil
}

// tDeleteFile removes a single file from the workspace after a sandbox path check.
func tDeleteFile(args map[string]interface{}) (string, error) {
	rel, err := argStr(args, "path")
	if err != nil {
		return "", err
	}
	ws, err := agentWorkspace()
	if err != nil {
		return "", err
	}
	if err := ws.DeleteFile(rel); err != nil {
		return "", err
	}
	return "deleted " + rel, nil
}

// tReadSoul returns the current contents of the active agent's SOUL.md, its own system prompt.
func tReadSoul(_ map[string]interface{}) (string, error) {
	b, err := os.ReadFile(agentStatePath(soulFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// tWriteSoul replaces SOUL.md with new content, refusing to write an empty file
// as a guard against accidentally wiping the system prompt.
func tWriteSoul(args map[string]interface{}) (string, error) {
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("refusing to write an empty SOUL.md")
	}
	if err := os.WriteFile(agentStatePath(soulFile), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "SOUL.md updated; it takes effect on your next reply", nil
}

// tReadTools returns the raw JSON of tools.json so the model can inspect its own tool manifest.
func tReadTools(_ map[string]interface{}) (string, error) {
	b, err := os.ReadFile(agentStatePath(toolsFile))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// tWriteTools validates and replaces tools.json. Parsing into a probe ToolsFile first ensures
// the model can never write malformed JSON that would crash the next tool load.
func tWriteTools(args map[string]interface{}) (string, error) {
	content, err := argStr(args, "content")
	if err != nil {
		return "", err
	}
	var probe ToolsFile
	if err := json.Unmarshal([]byte(content), &probe); err != nil {
		return "", fmt.Errorf("not valid tools.json: %v", err)
	}
	if err := os.WriteFile(agentStatePath(toolsFile), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "tools.json updated; changes take effect on your next reply", nil
}

// tRelayToCode forwards a prompt to the local code service over loopback HTTP
// (SYSTEM LAYER 2). The agent never runs commands itself — no os/exec anywhere —
// so even a fully compromised prompt can only ask the external service nicely.
func tRelayToCode(args map[string]interface{}) (string, error) {
	prompt, err := argStr(args, "prompt")
	if err != nil {
		return "", err
	}
	return coderelay.RelayToCodeService(prompt)
}

// tGetConfig returns a JSON snapshot of the current config, masking sensitive API keys
// so the model can see which model is active without exposing credentials in conversation history.
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

// tSetConfig allows the model to update a whitelisted subset of config keys at runtime.
// The switch statement acts as an allowlist — any unlisted key is explicitly rejected,
// which is safer than a generic key-value setter.
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
		return "", fmt.Errorf("key %q is not editable via set_config (allowed: gemini_model, gemini_api_key)", key)
	}
	if err := saveConfig(); err != nil {
		return "", err
	}
	return fmt.Sprintf("config %s updated", key), nil
}

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
      "name": "relay_to_code",
      "description": "Send a prompt to the local code service (Claude Code wrapper on 127.0.0.1:8080) and get back the terminal output. Use for coding and automation tasks that need the development environment.",
      "enabled": true,
      "parameters": {
        "type": "object",
        "properties": { "prompt": { "type": "string", "description": "The instruction to relay to the code service." } },
        "required": ["prompt"]
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
