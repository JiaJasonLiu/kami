#!/usr/bin/env bash
#
# quickstart.sh — build and run kami-gateway locally (no root, no systemd).
#
# This is the "simple OpenClaw" path: one binary, one machine, your home dir.
# For the hardened production install (dedicated user + locked-down systemd
# unit), use ./setup.sh instead.

set -euo pipefail
cd "$(dirname "$0")"

echo "=== kami-gateway quick start ==="
echo

# 1. Go toolchain -----------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
    echo "Go is not installed. Install Go 1.21+ from https://go.dev/dl/ and re-run." >&2
    exit 1
fi

# 2. Build ------------------------------------------------------------------
echo "Building kami-gateway…"
go build -o kami-gateway .

# 3. What you need before the wizard ---------------------------------------
cat <<'TXT'

You'll need two things for the wizard:
  • a Telegram bot token — message @BotFather, /newbot, copy the token
  • a Gemini API key      — https://aistudio.google.com/apikey

How do you want to talk to it?

  A) Direct messages (simplest) — one agent at a time, switch with
     "/agent use <name>". Just DM your bot.

  B) Forum group (one agent per topic) — each Telegram topic gets its own
     agent automatically. Set this up first:
        1. @BotFather → /mybots → your bot → Bot Settings
           → Group Privacy → Turn OFF   (so it can read every message)
        2. Create a Telegram group; in its settings, enable "Topics"
        3. Add your bot to the group

When the wizard asks which chat to use:
  • for A, send your bot a direct message, then press Enter
  • for B, send any message IN THE GROUP, then press Enter
    (it will detect the group's negative chat id)

TXT

# 4. First-run setup wizard -------------------------------------------------
if [ ! -f state/config.json ]; then
    ./kami-gateway setup
else
    echo "Existing config found in state/config.json — skipping the wizard."
    echo "(Run './kami-gateway setup' yourself to reconfigure.)"
    echo
fi

# 5. Run --------------------------------------------------------------------
echo "Starting gateway — press Ctrl-C to stop."
exec ./kami-gateway
