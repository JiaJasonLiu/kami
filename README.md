# kami-gateway

A tiny, privacy-first AI gateway you talk to **only over Telegram**. One user,
one chat, your choice of AI provider (Gemini, OpenAI, Anthropic, OpenRouter,
or a local OpenAI-compatible model). The model gets a `SOUL.md` (its system
prompt, which it can rewrite), a sandboxed `workspace/` it can't escape, and a
few tools defined in `tools.json` (which it can also edit). No Docker, no
database, no web UI, zero external Go dependencies.

Built in the "Kami" spirit: a presence that inhabits the tool, shaped by use. One bounded agent loop, no cleverness. Integrate it in your everyday tasks.

> **In a hurry?** See [`QUICKSTART.md`](QUICKSTART.md) — or just run
> `./quickstart.sh` to build, configure, and start the gateway.

## Contents

- [Getting started](#getting-started)
  - [Build](#build)
  - [Setup](#setup)
  - [Run](#run)
- [Using the bot](#using-the-bot)
  - [Chat commands](#chat-commands)
  - [Agent profiles](#agent-profiles)
  - [Forum topics: one agent per topic](#forum-topics-one-agent-per-topic)
- [Capabilities](#capabilities)
  - [What the model can do out of the box](#what-the-model-can-do-out-of-the-box)
  - [Internet access](#internet-access)
  - [Scheduled tasks (cron)](#scheduled-tasks-cron)
- [AI providers](#ai-providers)
- [Deployment](#deployment)
  - [Hardened system service (Linux)](#hardened-system-service-linux)
  - [User service (Linux)](#user-service-linux)
  - [User service (macOS)](#user-service-macos)
- [Development](#development)
  - [Adding a new tool](#adding-a-new-tool)
  - [Build for other machines](#build-for-other-machines)
  - [Layout](#layout)
  - [The sandbox](#the-sandbox-docker-without-docker)
  - [Notes / known simplifications](#notes--known-simplifications)

---

## Getting started

### Build

Requires Go 1.21+. No modules to download.

```sh
make build
```

### Setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy the token.
2. Get a Gemini API key from Google AI Studio.
3. Run the wizard:

   ```sh
   ./kami-gateway setup
   ```

   It asks for your Gemini key, model (default `gemini-2.0-flash`), and bot
   token. For the chat id, it tells you to **message your bot once**, then
   auto-detects the chat. (You can also paste a chat id manually.)

### Run

```sh
./kami-gateway
```

It long-polls Telegram (no inbound port, no public IP). Message your bot and it
replies. Only your configured chat id is answered; everyone else is ignored.

---

## Using the bot

### Chat commands

- `/new` — wipe conversation memory and start fresh
- `/agents` — list agent profiles (the active one is marked)
- `/agent new <name> [personality…]` — create a new agent and use it here
- `/agent use <name>` (or just `/agent <name>`) — assign an agent to this chat/topic
- `/agent delete <name>` — delete an agent and all of its files
- `/help` — list commands
- anything else — sent to the model

### Agent profiles

Every agent is a self-contained personality: its **own SOUL.md**, its own
`tools.json`, its own conversation memory, and its own sandboxed workspace.
Create one in chat and it's live immediately:

```
/agent new coder You are a terse coding assistant. Prefer diffs over prose.
```

The optional personality text is written into the newborn agent's SOUL.md, so
it wakes up already in character. Switch back and forth with `/agent use` —
each agent keeps its own memory and files, and can never see another agent's
workspace. Agent names are restricted to `[a-z0-9_-]` (max 32 chars) so a
name can never smuggle a path. The default agent is called `kami` and lives
in the original top-level `state/` + `workspace/`, so existing installs keep
working unchanged.

### Forum topics: one agent per topic

Talk to the bot inside a **Telegram forum group** (a supergroup with *Topics*
turned on) and each topic becomes its own persistent conversation with its own
agent — like Slack channels, but one bot:

1. In BotFather, disable the bot's **privacy mode** (`/setprivacy` → Disable)
   so it can see every message in the group, then add it to the group.
2. Enable **Topics** in the group settings and set the group's chat id as your
   `telegram_chat_id`. Group chat ids are negative numbers (e.g. `-1001234567890`).
   To find yours, send a message in the group then run:
   ```sh
   curl "https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates"
   ```
   Look for `"chat":{"id":...}` in the response, then tell the bot:
   ```
   set_config telegram_chat_id to -1001234567890
   ```
3. Create a topic called "Coding" and the gateway spins up a matching agent
   (slugified to `coding`), binds the topic to it, and greets it. Everything
   you say in that topic goes to that agent; replies come back in the topic.

Bindings live in `state/topics.json` and survive restarts. Inside a topic,
`/agent use <name>` re-points *that topic* (not your DMs) at another agent,
and `/agent new` creates-and-binds in one step. The group's **General** topic
and ordinary **direct messages** (thread 0) always use the gateway-wide
default agent from `agent.txt`, so nothing about single-chat use changes.

> Telegram note: bots never receive each other's messages, so agents can't
> "talk to each other" in a topic on their own — every message is routed by
> the gateway. This is the single-bot design; if you'd rather each agent be a
> separate contact with its own name/avatar, run one bot token per agent
> instead (not what this build does).

---

## Capabilities

### What the model can do out of the box

- `list_files`, `read_file`, `write_file`, `delete_file` — workspace only
- `read_soul` / `write_soul` — view and rewrite its own personality
- `read_tools` / `write_tools` — toggle/redescribe its tools (it can't invent
  new implementations — only tools the Go program already provides will run)
- `get_config` / `set_config` — read config and switch provider / change any
  provider's model or API key (Telegram settings are deliberately not
  self-editable so it can't lock itself out)
- `web_search` / `web_fetch` — search the web (Brave Search API) and download a
  page as plain text. See [Internet access](#internet-access) below.
- `cron_add` / `cron_list` / `cron_remove` — schedule recurring prompts. See
  [Scheduled tasks](#scheduled-tasks-cron) below.
- `relay_to_code` — send a prompt to a local code service (e.g. a Claude Code
  wrapper) listening on `http://127.0.0.1:8080/execute` and get the terminal
  output back. The agent itself never runs commands (`os/exec` is not used
  anywhere); execution happens on the other side of the loopback boundary.

Try: *"Read your SOUL and give yourself a name and a dry sense of humour, then
save it."* — it'll call `read_soul` then `write_soul`.

### Internet access

The agent can reach the web through two tools:

- **`web_fetch(url)`** — downloads an `http`/`https` page and returns it as
  plain text (scripts, styles and tags stripped). No key required. Only `http`
  and `https` URLs are accepted, and both the download and the returned text are
  size-capped so a huge page can't stall the bot or flood its memory.
- **`web_search(query)`** — queries the [Brave Search API](https://brave.com/search/api/)
  and returns a short list of titles, URLs and snippets. This one needs a key:

  ```
  Ask the bot:  "set_config brave_api_key to <your-brave-key>"
  ```

  (Or paste it when the setup wizard asks — it's optional.) Without a key,
  `web_search` returns a friendly "not configured" hint instead of failing.

A common flow is `web_search` to find pages, then `web_fetch` to read the most
promising one. Everything is an outbound GET — the agent still runs no commands
and touches no files outside its workspace.

### Scheduled tasks (cron)

The agent can schedule work to run on its own, even when you're not chatting:

- **`cron_add(schedule, prompt)`** — `schedule` is a standard 5-field cron
  expression (`minute hour day-of-month month day-of-week`) and `prompt` is run
  exactly as if you'd typed it. The job remembers which **agent** and which
  **topic** it was created in, so a job made in your "News" topic runs as that
  agent and posts back there.
- **`cron_list()`** / **`cron_remove(id)`** — inspect and delete jobs.

Examples of schedules:

| Cron          | When |
|---------------|------|
| `0 9 * * *`   | every day at 09:00 |
| `*/30 * * * *`| every 30 minutes |
| `0 8 * * 1`   | 08:00 every Monday |
| `0 18 1 * *`  | 18:00 on the 1st of each month |

Jobs live in `state/cron.json` and survive restarts. A small in-process
scheduler wakes once a minute and runs whatever is due — no system crontab, no
extra process. Scheduled runs and live messages are serialised, so a job never
races your conversation.

Try: *"every morning at 8, search the web for the top AI news and send me a
three-bullet summary"* — it'll call `web_search` inside a `cron_add` job.

---

## AI providers

Pick a backend at setup time, or switch anytime by chatting `set_config`.
Supported providers:

| Provider     | `provider` value | Needs |
|--------------|------------------|-------|
| Google Gemini | `gemini` (default) | `gemini_api_key`, `gemini_model` |
| OpenAI        | `openai`         | `openai_api_key`, `openai_model` (opt. `openai_base_url`) |
| Anthropic     | `anthropic`      | `anthropic_api_key`, `anthropic_model` |
| OpenRouter    | `openrouter`     | `openrouter_api_key`, `openrouter_model` |
| Local         | `local`          | `local_model`, `local_base_url` (default Ollama `http://localhost:11434/v1`) |

`openai`, `openrouter`, and `local` all speak the OpenAI Chat Completions
format, so the **local** option works with Ollama, LM Studio, llama.cpp's
server, vLLM — anything OpenAI-compatible — just point `local_base_url` at it.
Tool calling is translated for every provider, so the file/soul/config tools
work the same no matter which model is answering.

Selection is **global** (one active provider for the gateway) and each
provider keeps its own key/model, so you can pre-configure several and flip
between them:

```
Ask the bot:  "set_config provider anthropic"
              "get_config"          (shows the active provider + models)
```

You can also set any key at runtime, e.g. *"use set_config to set
openai_api_key to sk-…"* — keys are stored `0600` and shown masked.

---

## Deployment

### Hardened system service (Linux)

`setup.sh` installs the binary as a locked-down systemd service: it creates a
no-login `tg-agent` system user, installs to `/opt/tg-agent` (binary
`chmod 700`), and writes a unit with `ProtectSystem=strict`, `ProtectHome=yes`,
`ReadWritePaths=/opt/tg-agent/storage`, `PrivateTmp=yes` and an empty
`CapabilityBoundingSet=` — so the kernel itself guarantees the agent can only
write inside its own storage directory.

```sh
make build
sudo ./setup.sh                 # installs + enables tg-agent.service
sudo -u tg-agent KAMI_HOME=/opt/tg-agent/storage /opt/tg-agent/kami-gateway setup
sudo systemctl start tg-agent.service
```

### User service (Linux)

```ini
# ~/.config/systemd/user/kami-gateway.service
[Unit]
Description=kami-gateway
[Service]
WorkingDirectory=%h/kami-gateway
ExecStart=%h/kami-gateway/kami-gateway
Restart=on-failure
Environment=KAMI_HOME=%h/kami-gateway
[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now kami-gateway
```

### User service (macOS)

`setup-mac.sh` installs kami-gateway as a launchd LaunchAgent confined by
`sandbox-exec`: the kernel allows file writes **only** inside the install
directory, with outbound network access for Telegram and AI API calls.

```sh
make build
./setup-mac.sh                          # installs to ~/kami-gateway (default)
~/kami-gateway/kami-gateway setup       # first-time wizard
make mac-start                          # load the service
```

Useful commands:

```sh
make mac-start      # load and start
make mac-stop       # stop
make mac-restart    # stop + start
make mac-logs       # tail the log
make mac-install    # rebuild and reinstall the binary
./setup-mac.sh --uninstall              # remove everything
```

> **Note:** `sandbox-exec` is deprecated by Apple (still works on current
> macOS but may not survive future OS updates). Layer 1 (`SafeWorkspace`) still
> enforces the path sandbox in code regardless.

---

## Development

### Adding a new tool

1. Write a `func(args map[string]interface{}) (string, error)` in `main.go`.
2. Register it in the `handlers` map.
3. Add a declaration to `state/tools.json` (or let the model do that part).

### Build for other machines

```sh
make dist   # → dist/kami-gateway-{linux-amd64,linux-arm64,darwin-arm64}
```

### Layout

```
$KAMI_HOME (default: the current directory)
├── kami-gateway        the binary
├── state/               (chmod 700)
│   ├── config.json      API keys + model (chmod 600)
│   ├── offset.txt       Telegram polling cursor
│   ├── agent.txt        name of the DM/General default agent profile
│   ├── topics.json      forum thread → agent bindings
│   ├── SOUL.md          the DEFAULT agent's system prompt — it can edit this
│   ├── tools.json       the default agent's tool registry — it can edit this
│   └── history.json     the default agent's memory (cleared by /new)
├── workspace/           the default agent's sandbox — its ONLY writable area
└── agents/              extra agent profiles (created with /agent new)
    └── <name>/
        ├── state/       that agent's own SOUL.md, tools.json, history.json
        └── workspace/   that agent's own sandbox
```

### The sandbox ("Docker without Docker")

Every file tool resolves its path inside `workspace/` and rejects absolute
paths and `..` traversal. The model literally has no tool that can name a path
outside the workspace. `SOUL.md`, `tools.json` and config are reachable only
through dedicated tools (`read_soul`/`write_soul`, `read_tools`/`write_tools`,
`get_config`/`set_config`) — not the generic file tools.

> Note: this is path-level isolation, not a kernel sandbox. The model can only
> act through the tools it's given, and those tools are confined to the
> workspace. If you want hard OS-level isolation later, run the binary as an
> unprivileged user or inside a systemd unit with `ProtectHome`/`ReadOnlyPaths`.

### Notes / known simplifications

- The Gemini function-response role is sent as `user` (broadest v1beta
  compatibility). If a future API version complains, that's the one line to
  change in `handleUserMessage`.
- The agent loop is capped at 8 tool steps per message.
- Conversation memory is bounded (~60 turns / ~48 KB); the oldest turns are
  dropped automatically, never splitting a tool call from its response. `/new`
  still wipes everything.
- Transient Gemini failures (429 / 5xx / network blips) are retried up to 3
  times with backoff.
- Telegram replies are auto-chunked to 4000 chars; a "typing…" indicator shows
  while the model works.
- `SIGINT`/`SIGTERM` shut down cleanly and send a goodbye message.
