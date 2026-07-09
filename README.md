# kami-gateway

A tiny, privacy-first AI gateway you talk to **only over Telegram**. One user,
one chat, Gemini only. The model gets a `SOUL.md` (its system prompt, which it
can rewrite), a sandboxed `workspace/` it can't escape, and a few tools defined
in `tools.json` (which it can also edit). No Docker, no database, no web UI,
zero external Go dependencies.

Built in the "Kami" spirit: a presence that inhabits the tool, shaped by use. One bounded agent loop, no cleverness. Integrate it in your everyday tasks.

## Layout

```
$KAMI_HOME (default: the current directory)
├── kami-gateway        the binary
├── state/               (chmod 700)
│   ├── config.json      API keys + model (chmod 600)
│   ├── offset.txt       Telegram polling cursor
│   ├── agent.txt        name of the active agent profile
│   ├── SOUL.md          the DEFAULT agent's system prompt — it can edit this
│   ├── tools.json       the default agent's tool registry — it can edit this
│   └── history.json     the default agent's memory (cleared by /new)
├── workspace/           the default agent's sandbox — its ONLY writable area
└── agents/              extra agent profiles (created with /agent new)
    └── <name>/
        ├── state/       that agent's own SOUL.md, tools.json, history.json
        └── workspace/   that agent's own sandbox
```

## The sandbox ("Docker without Docker")

Every file tool resolves its path inside `workspace/` and rejects absolute
paths and `..` traversal. The model literally has no tool that can name a path
outside the workspace. `SOUL.md`, `tools.json` and config are reachable only
through dedicated tools (`read_soul`/`write_soul`, `read_tools`/`write_tools`,
`get_config`/`set_config`) — not the generic file tools.

> Note: this is path-level isolation, not a kernel sandbox. The model can only
> act through the tools it's given, and those tools are confined to the
> workspace. If you want hard OS-level isolation later, run the binary as an
> unprivileged user or inside a systemd unit with `ProtectHome`/`ReadOnlyPaths`.

## Build

Requires Go 1.21+. No modules to download.

```sh
make build
```
## Setup

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy the token.
2. Get a Gemini API key from Google AI Studio.
3. Run the wizard:

   ```sh
   ./kami-gateway setup
   ```

   It asks for your Gemini key, model (default `gemini-2.0-flash`), and bot
   token. For the chat id, it tells you to **message your bot once**, then
   auto-detects the chat. (You can also paste a chat id manually.)

## Run

```sh
./kami-gateway
```

It long-polls Telegram (no inbound port, no public IP). Message your bot and it
replies. Only your configured chat id is answered; everyone else is ignored.

## Chat commands

- `/new` — wipe conversation memory and start fresh
- `/agents` — list agent profiles (the active one is marked)
- `/agent new <name> [personality…]` — create a new agent and switch to it
- `/agent use <name>` (or just `/agent <name>`) — switch agents
- `/agent delete <name>` — delete an agent and all of its files
- `/help` — list commands
- anything else — sent to the model

## Agent profiles

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

## What the model can do out of the box

- `list_files`, `read_file`, `write_file`, `delete_file` — workspace only
- `read_soul` / `write_soul` — view and rewrite its own personality
- `read_tools` / `write_tools` — toggle/redescribe its tools (it can't invent
  new implementations — only tools the Go program already provides will run)
- `get_config` / `set_config` — read config, change `gemini_model` or
  `gemini_api_key` (Telegram settings are deliberately not self-editable so it
  can't lock itself out)
- `relay_to_code` — send a prompt to a local code service (e.g. a Claude Code
  wrapper) listening on `http://127.0.0.1:8080/execute` and get the terminal
  output back. The agent itself never runs commands (`os/exec` is not used
  anywhere); execution happens on the other side of the loopback boundary.

Try: *"Read your SOUL and give yourself a name and a dry sense of humour, then
save it."* — it'll call `read_soul` then `write_soul`.

## Adding a new tool (when you want one)

1. Write a `func(args map[string]interface{}) (string, error)` in `main.go`.
2. Register it in the `handlers` map.
3. Add a declaration to `state/tools.json` (or let the model do that part).

## Run it as a hardened system service (recommended)

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

## Run it as a user service (lighter alternative)

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

## Notes / known simplifications

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

## Build for other machines

```sh
make dist   # → dist/kami-gateway-{linux-amd64,linux-arm64,darwin-arm64}
```
