#!/usr/bin/env node
// code-service: the HIGH-privilege half of the two-container Docker setup.
//
// It implements the loopback "code service" contract that internal/coderelay
// (in the gateway) already speaks: POST /execute {"prompt": "..."} and get back
// {"output": "..."} or {"error": "..."}. Instead of running on the host, it
// runs in its own container with the whole repository bind-mounted read-write
// at /repo and with git + gh + the Claude Code CLI installed, so kami can edit
// its own source and open pull requests.
//
// Trust posture (deliberately the OPPOSITE of the claude-sdk sidecar):
//   - The gateway is confined and has no os/exec. THIS service is the one place
//     allowed to run a shell and dev tools — that is its entire job.
//   - The container IS the sandbox: only /repo (plus the injected credentials)
//     is reachable, nothing on the host. So the Claude Code CLI is run headless
//     with --dangerously-skip-permissions, which is safe because there is
//     nothing outside /repo for it to damage.
//   - It listens only inside the compose network (bound to 0.0.0.0 within the
//     container, never published to the host). Do not add a `ports:` mapping.

const http = require("http");
const { spawn } = require("child_process");

const PORT = parseInt(process.env.PORT || "8080", 10);
const REPO = process.env.REPO_DIR || "/repo";
const CLAUDE_BIN = process.env.CLAUDE_BIN || "claude";
// A single coding run can legitimately take minutes; cap it so a hung CLI
// cannot pin the service forever.
const RUN_TIMEOUT_MS = parseInt(process.env.RUN_TIMEOUT_MS || "600000", 10);
const MAX_BODY_BYTES = 1 << 20; // 1 MiB of prompt is plenty.

function readBody(req) {
  return new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on("data", (c) => {
      size += c.length;
      if (size > MAX_BODY_BYTES) {
        reject(new Error("request body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

// runClaude drives the Claude Code CLI headless in the repo. The prompt is
// passed verbatim — kami composes the full instruction (what to change, to
// branch, and to open a PR) on the gateway side via its `code` tool.
function runClaude(prompt) {
  return new Promise((resolve) => {
    const args = [
      "-p",
      prompt,
      "--output-format",
      "text",
      "--dangerously-skip-permissions",
    ];
    const child = spawn(CLAUDE_BIN, args, {
      cwd: REPO,
      env: process.env,
    });

    let out = "";
    let err = "";
    child.stdout.on("data", (d) => (out += d.toString()));
    child.stderr.on("data", (d) => (err += d.toString()));

    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      err += `\n[code-service] killed: exceeded ${RUN_TIMEOUT_MS}ms timeout`;
    }, RUN_TIMEOUT_MS);

    child.on("error", (e) => {
      clearTimeout(timer);
      resolve({ error: `failed to launch ${CLAUDE_BIN}: ${e.message}` });
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      if (code === 0) {
        resolve({ output: out.trim() || "(no output)" });
      } else {
        const detail = (err.trim() || out.trim() || "(no output)").slice(0, 8000);
        resolve({ error: `claude exited ${code}: ${detail}` });
      }
    });
  });
}

const server = http.createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/health") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  if (req.method !== "POST" || req.url !== "/execute") {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "not found; POST /execute" }));
    return;
  }

  let payload;
  try {
    const raw = await readBody(req);
    payload = JSON.parse(raw || "{}");
  } catch (e) {
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: `bad request: ${e.message}` }));
    return;
  }

  const prompt = (payload.prompt || "").trim();
  if (!prompt) {
    res.writeHead(400, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "prompt must not be empty" }));
    return;
  }

  const result = await runClaude(prompt);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify(result));
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`[code-service] listening on :${PORT}, repo=${REPO}, claude=${CLAUDE_BIN}`);
});
