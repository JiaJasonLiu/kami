package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const maxToolSteps = 8

// handleUserMessage is the core agent loop: it handles built-in slash commands first,
// then runs the Gemini agentic loop that may call tools multiple times before producing
// a final text reply. The loop is bounded by maxToolSteps to prevent infinite tool chains.
// strings.Builder is used to accumulate text parts efficiently without repeated string concatenation.
func handleUserMessage(text string) string {
	trimmed := strings.TrimSpace(text)
	switch trimmed {
	case "/new":
		clearHistory()
		return "🧹 Started a fresh conversation."
	case "/help":
		return "Commands:\n/new — wipe this conversation's memory\n/agents — list agent profiles\n/agent new <name> [personality…] — create an agent with its own soul and workspace\n/agent use <name> — assign an agent to this chat/topic\n/agent delete <name> — delete an agent\n/help — this message\nAnything else is sent to the model.\n\nTip: in a Telegram forum group, each topic gets its own agent automatically — create a topic and it spins up a matching agent."
	case "/start":
		return "Hi. I'm your gateway. Talk to me normally, or /new to start over."
	}
	if trimmed == "/agents" || trimmed == "/agent" || strings.HasPrefix(trimmed, "/agent ") {
		return handleAgentCommand(trimmed)
	}

	soul, err := os.ReadFile(agentStatePath(soulFile))
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
		resp, err := callModel(gRequest{SystemInstruction: sys, Contents: history, Tools: tools})
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
