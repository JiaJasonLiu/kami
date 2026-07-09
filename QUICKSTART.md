# Quick start

Get kami-gateway talking to you on Telegram in a few minutes. Everything runs
on one machine — no Docker, no database.

## 1. Get your keys

- **Telegram bot token** — open [@BotFather](https://t.me/BotFather), send
  `/newbot`, follow the prompts, copy the token it gives you.
- **An AI provider key** — the wizard asks which provider you want and only
  prompts for that one:
  - **Gemini** (default) — [Google AI Studio](https://aistudio.google.com/apikey)
  - **OpenAI** — an `sk-…` key from platform.openai.com
  - **Anthropic** — a Claude API key from console.anthropic.com
  - **OpenRouter** — a key from openrouter.ai
  - **Local** — no key needed; run [Ollama](https://ollama.com) (or LM Studio,
    llama.cpp, vLLM) and note its base URL + model name

You can add more providers later and switch with `set_config provider <name>`
right from the chat.

- **(Optional) Brave Search key** — for web search, grab a free key at
  [brave.com/search/api](https://brave.com/search/api/). The wizard asks for it
  (blank to skip); you can also add it later with `set_config brave_api_key …`.
  Page fetching (`web_fetch`) works without any key.

## 2. Choose how you'll talk to it

### Option A — Direct messages (simplest)

You DM the bot; one agent is active at a time and you switch with
`/agent use <name>`. Nothing extra to configure. Skip to step 3.

### Option B — Forum group (one agent per topic) ✨

Each Telegram **topic** becomes its own conversation with its own agent,
created automatically. Set this up before running:

1. In @BotFather: `/mybots` → your bot → **Bot Settings → Group Privacy →
   Turn off**. (A bot only sees every message in a group when privacy is off.)
2. Create a Telegram **group**, then in its settings enable **Topics** (this
   makes it a "forum").
3. **Add your bot** to the group.

## 3. Build and run (local)

```sh
./quickstart.sh
```

This checks Go, builds the binary, runs the one-time setup wizard, and starts
the gateway. When the wizard asks **which chat** to use:

- **Option A:** send your bot a direct message, then press Enter.
- **Option B:** send any message **in the group**, then press Enter — it
  detects the group's (negative) chat id.

That's it — the bot replies to `👋 Gateway online`.

## 4. Try it

- **DMs / the group's General topic:** talk normally. `/agents` lists your
  agents; `/agent new coder You are a terse coding assistant` creates one.
- **A forum topic (Option B):** open a new topic called e.g. "Research" and
  the gateway spins up a `research` agent, binds the topic to it, and greets
  you. Everything you say there goes to that agent; its memory and workspace
  are separate from every other topic. Re-point a topic with
  `/agent use <name>` from inside it.

## Chat commands

You can also just ask in plain language — e.g. *"search the web for today's
weather in Tokyo"* (web) or *"every day at 8am, send me a summary of the latest
Go releases"* (a scheduled cron job that posts itself back to this chat).

- `/new` — wipe this conversation's memory
- `/agents` — list agent profiles
- `/agent new <name> [personality…]` — create an agent (and use it here)
- `/agent use <name>` — assign an agent to this chat/topic
- `/agent delete <name>` — delete an agent and its files
- `/help` — show all commands

## Run it as a hardened service (production)

For an always-on install locked down at the OS level (dedicated `tg-agent`
user, `ProtectSystem=strict`, writes only to its own storage dir), use the
installer instead of `quickstart.sh`:

```sh
make build
sudo ./setup.sh
sudo -u tg-agent KAMI_HOME=/opt/tg-agent/storage /opt/tg-agent/kami-gateway setup
sudo systemctl start tg-agent.service
journalctl -u tg-agent.service -f
```

See [`README.md`](README.md) for the full layout and design notes.
