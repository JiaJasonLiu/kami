package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
