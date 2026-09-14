# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this project is

`kami-gateway` is a tiny, privacy-first AI agent you talk to over Telegram
(one user, one chat). It supports multiple AI providers (Gemini, OpenAI,
Anthropic, OpenRouter, the Claude Agent SDK, and any OpenAI-compatible local
server), selected
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

The `claude-sdk` provider follows the same rule for the same reason. Driving
the Claude Agent SDK means running the `claude` CLI, which the gateway must
not do — and under systemd it could not reach the operator's OAuth login in
`~/.claude` anyway (`ProtectHome=yes`). So `internal/claudesdk` is an HTTP
client to a host-level sidecar (`sidecar/claude-sdk-service.js`) that owns
the process and the login. Neither `internal/claudesdk` nor any gateway file
may import `os/exec`; if the SDK ever needs a new capability, add it to the
sidecar's JSON contract, not to the gateway.

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
docs/adr/                  architecture decision records (why, not what)
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
`activeModel()` and dispatches to one of four clients — `callGemini`
(gemini), the OpenAI-compatible client (openai, openrouter, local — they
differ only by base URL/key), the Anthropic client, or `callClaudeSDK`
(claude-sdk). Each client translates
the internal shape to/from its own wire format, **including tool calls**:
OpenAI and Anthropic link a call to its result by id, so translation mints
synthetic ids while walking the history and matches results to calls by name
(`toolRef`/`popByName`). When adding a provider, add a case to `activeModel`
and, if it isn't OpenAI- or Anthropic-shaped, a new client + translation pair;
never make the agent loop aware of provider specifics. Provider is global and
switchable at runtime through `set_config` (or `/claude` in chat); each
provider keeps its own key/model in config so switching never drops
credentials.

### The claude-sdk provider (subscription session usage)

`claude-sdk` is the one provider that is **not** a metered API call. It drives
the Claude Agent SDK through the loopback sidecar, which runs the `claude` CLI
under the operator's subscription login, so turns consume that seat's **session
usage** instead of API credits. Three consequences shape its code, and all
three are intentional:

- **No API key.** `activeModel()` validates nothing for this provider — there
  is no key to validate. Never add one; a key in the environment would silently
  redirect billing away from the subscription (the sidecar strips
  `ANTHROPIC_API_KEY` from the child process for exactly this reason, and must
  never pass `--bare`, which disables OAuth).
- **The SDK owns the conversation.** It persists history under a session id, so
  the gateway does not replay `history.json` into the request. It keeps one
  session id per `(agent, thread)` in `state/claude_sessions.json` and resumes
  it each turn. `/new` must therefore clear the session too, or the model keeps
  remembering after the user asked it to forget.
- **The SDK owns the agent loop.** It runs its own tools internally, so the
  gateway's tool registry is not offered to it and `callClaudeSDK` returns
  plain text with no function calls — which makes the loop in `agent.go` finish
  in a single step. `callModel` deliberately skips `withRetry` here: one call is
  a whole agentic session, so a blind retry could repeat side effects.

The sidecar URL is pinned to loopback by `isLoopbackURL`, in both `/claude url`
and `set_config`. Keep that check: the sidecar can spend the operator's
subscription, so it must never be pointed at a remote host.

**Sandboxing the SDK.** The sidecar runs as the operator's user, so confining
it is the sidecar's job, and it does so in three layers: `Bash`/`WebFetch`/
`WebSearch` denied (a shell cannot be confined to a directory by path rules),
Read/Write/Edit scoped to the workspace, and — the layer that actually holds —
a kernel sandbox making the filesystem read-only apart from the workspace —
bubblewrap on Linux, `sandbox-exec` (Seatbelt) on macOS, chosen by platform in
`SANDBOX_KIND`. The macOS path is implemented but was written on Linux and
never executed there: `node sidecar/claude-sdk-service.js --selftest` verifies
it on the machine that matters.

That third layer is load-bearing, not belt-and-braces. Permission rules alone
**cannot** confine writes: scoped `Write(<ws>/**)` allows do not stop a write
elsewhere, and `Write(//**)` — the only rule that does — also matches the
workspace's own absolute path, since deny beats allow with no carve-out
(verified against CLI 2.1.179). Do not "simplify" the bwrap wrapper away in
favour of rules; re-test with a write to an absolute path outside the workspace
before changing any of this. The measurements behind this, and the separate
story for non-SDK providers, are in
`docs/adr/0001-claude-sdk-provider-and-filesystem-confinement.md`.

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
make run       # gateway + Claude SDK sidecar (stops the sidecar on exit)
make run-gateway / make sidecar   # the two halves separately
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
- The gateway and every package under `internal/` must remain free of
  `os/exec`; subprocess work belongs behind a loopback HTTP boundary.
- Tool handlers have the signature
  `func(args map[string]interface{}) (string, error)`; register them in the
  `handlers` map in `tools.go` and declare them in `defaultTools` (and bump
  the enabled-tool count in `main_test.go`).
- Tool errors are returned to the model as `error: ...` strings, never
  panics — the agent loop must survive any tool failure.
- Secrets (API keys) are written with mode `0600` and masked with `mask()`
  before being shown in tool output.
