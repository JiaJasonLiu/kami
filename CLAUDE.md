# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this project is

`kami-gateway` is a tiny, privacy-first AI agent you talk to over Telegram
(one user, one chat). It supports multiple AI providers (Gemini, OpenAI,
Anthropic, OpenRouter, and any OpenAI-compatible local server), selected
globally via config. It is run as a **simple OpenClaw-style setup**: a single
self-hosted personal-assistant gateway on one machine — no Docker, no
database, no web UI, zero external Go dependencies.

## Deployment intent (for future reference)

The agent must only ever be able to **write files inside its own
directory**. This is enforced at two independent layers, and any future
change must preserve both:

1. **Layer 1 — code-level scoping (`internal/workspace`)**: every file tool
   the model can call resolves paths through `workspace.SafeWorkspace`,
   which roots all access at the `workspace/` directory under `$KAMI_HOME`
   and rejects directory traversal (`../`), with a `filepath.Clean` +
   `strings.HasPrefix` check. Never add a tool handler that passes a
   model-chosen path straight to the `os` package — always go through
   `SafeWorkspace`.
2. **Layer 2 — OS-level isolation (`setup.sh` + systemd)**: in production
   the binary runs as the no-login system user `tg-agent` under a hardened
   unit (`ProtectSystem=strict`, `ProtectHome=yes`,
   `ReadWritePaths=/opt/tg-agent/storage`, `PrivateTmp=yes`,
   `CapabilityBoundingSet=`). The kernel makes the whole filesystem
   read-only to the process except `/opt/tg-agent/storage` — its own
   directory — even if Layer 1 were bypassed.

Command execution is likewise forbidden in-process: the agent contains **no
`os/exec` usage**. Coding/automation work is relayed over loopback HTTP to
a separate host-level code service (a Claude Code repository wrapper) at
`http://127.0.0.1:8080/execute` via `internal/coderelay`. Keep it that way.

## Layout

```
main.go                    entry point, $KAMI_HOME layout (state/ + workspace/)
agent.go                   the bounded agent loop (max 8 tool steps) +
                           runAgentTurn/turnMu (serialises bot vs cron turns)
profiles.go                agent profiles: per-agent soul/tools/history/workspace,
                           /agent chat commands (new, use, delete, list)
topics.go                  forum topic → agent bindings (state/topics.json),
                           per-message routing, topic-name slugify
tools.go                   tool registry + handlers (the model's only abilities)
web.go                     web_search (Brave Search API) + web_fetch
                           (keyless page → text); outbound GET only, no fs/exec
cron.go                    in-process cron scheduler: 5-field parser, job store
                           (state/cron.json), cron_add/list/remove tools
config.go                  config load/save + interactive setup wizard
providers.go               multi-provider layer: callModel dispatcher +
                           OpenAI-compatible & Anthropic clients (translate
                           to/from the internal Gemini-shaped request)
telegram.go / gemini.go    thin API clients (long-poll Telegram, call Gemini)
history.go                 bounded conversation memory
util.go                    small helpers (mask, truncate, chunk)
internal/workspace/        LAYER 1: SafeWorkspace filesystem sandbox
internal/coderelay/        LAYER 2: loopback HTTP relay to the code service
setup.sh                   LAYER 2: root installer — tg-agent user + hardened
                           systemd unit at /etc/systemd/system/tg-agent.service
```

Runtime state lives under `$KAMI_HOME` (default `.`; `/opt/tg-agent/storage`
in production). Gateway-level files (`config.json`, `offset.txt`,
`agent.txt`) live in `state/` and resolve through `statePath()`. Per-agent
files (`SOUL.md`, `tools.json`, `history.json`) resolve through
`agentStatePath()` and belong to the active profile: the default agent
(`kami`) uses the legacy top-level `state/` + `workspace/`, every other
agent lives under `agents/<name>/{state,workspace}`. Each agent's workspace
is its own Layer-1 sandbox root — agents cannot see each other's files.
Agent names are validated against `^[a-z0-9][a-z0-9_-]{0,31}$` before being
joined into paths; never relax that check.

Which agent a message uses is chosen per-message by the bot loop, not by a
single global switch. `activeAgent` is the current message's agent (set from
its Telegram forum topic before `handleUserMessage`); `dmAgent` (persisted in
`agent.txt`) is the default for direct messages and the group's General topic
(thread 0). Forum topics bind to agents in `state/topics.json` via
`topics.go`; `agentForThread(0)` returns `dmAgent`, a bound thread returns its
agent, an unbound thread falls back to `kami`. Binding a topic sets
`activeAgent` for that turn but must never touch `dmAgent` — that separation
is what stops a topic switch from leaking into DMs.

## AI providers

The agent loop is provider-neutral: it always builds the internal
`gRequest`/`gResponse` types (which mirror Gemini, the original backend) and
calls `callModel` in `providers.go`. `callModel` resolves `cfg.Provider` via
`activeModel()` and dispatches to one of three clients — `callGemini`
(gemini), the OpenAI-compatible client (openai, openrouter, local — they
differ only by base URL/key), or the Anthropic client. Each client translates
the internal shape to/from its own wire format, **including tool calls**:
OpenAI and Anthropic link a call to its result by id, so translation mints
synthetic ids while walking the history and matches results to calls by name
(`toolRef`/`popByName`). When adding a provider, add a case to `activeModel`
and, if it isn't OpenAI- or Anthropic-shaped, a new client + translation pair;
never make the agent loop aware of provider specifics. Provider is global and
switchable at runtime through `set_config`; each provider keeps its own
key/model in config so switching never drops credentials.

## Internet access and scheduling

Two capabilities let the agent act beyond the chat, both standard-library only
and both still respecting the no-`os/exec` rule (they make outbound HTTP GETs,
nothing more):

- **Web (`web.go`)**: `web_fetch(url)` downloads an http/https page and returns
  it as stripped plain text (keyless); `web_search(query)` calls the Brave
  Search API and needs `brave_api_key` in config (empty key → the tool returns a
  configuration hint, never a crash). Both share one short-timeout `webClient`
  and cap response/text size so a huge or slow page can't stall the loop or blow
  the history budget.
- **Cron (`cron.go`)**: `cron_add(schedule, prompt)` binds a standard 5-field
  cron expression to `(agent, thread, prompt)` captured from the current chat
  context, persisted in `state/cron.json`. `cronLoop` wakes on each minute
  boundary, and any job whose schedule matches runs its prompt as its agent and
  posts the reply into its Telegram topic. The 5-field parser supports
  `*`, ranges, lists and `*/n` steps, with Vixie-cron day-of-month/day-of-week
  OR semantics.

**Concurrency invariant**: the cron scheduler and the Telegram bot loop can both
start an agent turn, and a turn mutates the global `activeAgent`/`currentTopic`
and an agent's history files. Every turn from either source therefore goes
through `runAgentTurn` (`agent.go`), which holds `turnMu` for the whole
`handleUserMessage` call. Never call `handleUserMessage` directly from a new
concurrent path — always route through `runAgentTurn`.

## Commands

```sh
make build     # go build -o kami-gateway .
make test      # go test ./...
make fmt       # gofmt -w .
make vet       # go vet ./...
make dist      # cross-compile static binaries
sudo ./setup.sh [path-to-binary]   # install hardened systemd service
```

Always run `gofmt -w .`, `go vet ./...`, and `go test ./...` before
committing.

## Conventions

- Go 1.21+, standard library only — do not add external dependencies.
- Tool handlers have the signature
  `func(args map[string]interface{}) (string, error)`; register them in the
  `handlers` map in `tools.go` and declare them in `defaultTools` (and bump
  the enabled-tool count in `main_test.go`).
- Tool errors are returned to the model as `error: ...` strings, never
  panics — the agent loop must survive any tool failure.
- Secrets (API keys) are written with mode `0600` and masked with `mask()`
  before being shown in tool output.
