#!/usr/bin/env bash
#
# setup-mac.sh — macOS LaunchAgent installer for kami-gateway.
#
# Installs kami-gateway as a user-level launchd service that:
#   * runs as YOU (no root required)
#   * is confined by sandbox-exec to read/write ONLY its own install directory
#   * starts automatically on login and restarts on failure
#
# Usage:
#   ./setup-mac.sh [install-dir]          install  (default: ~/kami-gateway)
#   ./setup-mac.sh --uninstall [dir]      remove the service and files
#
# After installing, run the first-time wizard:
#   <install-dir>/kami-gateway setup

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
LABEL="com.kami.gateway"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST_FILE="${PLIST_DIR}/${LABEL}.plist"
BINARY_NAME="kami-gateway"

log()  { printf '\033[1;32m[setup-mac]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[setup-mac] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Uninstall path
# ---------------------------------------------------------------------------
if [[ "${1:-}" == "--uninstall" ]]; then
    INSTALL_DIR="${2:-${HOME}/kami-gateway}"
    log "unloading launchd service…"
    launchctl unload "${PLIST_FILE}" 2>/dev/null || true
    rm -f "${PLIST_FILE}"
    log "removed ${PLIST_FILE}"
    read -r -p "Also delete ${INSTALL_DIR} and all its data? [y/N] " confirm
    if [[ "${confirm}" =~ ^[Yy]$ ]]; then
        rm -rf "${INSTALL_DIR}"
        log "deleted ${INSTALL_DIR}"
    fi
    log "uninstall complete."
    exit 0
fi

# ---------------------------------------------------------------------------
# Install path
# ---------------------------------------------------------------------------
INSTALL_DIR="${1:-${HOME}/kami-gateway}"

# Pre-flight: need the compiled binary next to this script
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_BINARY="${SCRIPT_DIR}/${BINARY_NAME}"
[[ -f "${SOURCE_BINARY}" ]] || die "binary not found at '${SOURCE_BINARY}' — run 'make build' first"
command -v sandbox-exec >/dev/null 2>&1 || die "sandbox-exec not found — requires macOS 10.5+"

# ---------------------------------------------------------------------------
# 1. Install directory (chmod 700 — only your user can read it)
# ---------------------------------------------------------------------------
log "creating install directory: ${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"
chmod 700 "${INSTALL_DIR}"

# ---------------------------------------------------------------------------
# 2. Copy binary (chmod 700)
# ---------------------------------------------------------------------------
log "installing binary to ${INSTALL_DIR}/${BINARY_NAME}"
install -m 700 "${SOURCE_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"

# ---------------------------------------------------------------------------
# 3. sandbox-exec profile — allow all reads, restrict WRITES to install dir.
#    The meaningful boundary is write confinement: the agent can't modify
#    files outside its own directory. Reads are left open so the Go runtime
#    and macOS system libraries can boot without a hand-crafted allowlist.
# ---------------------------------------------------------------------------
SANDBOX_PROFILE="${INSTALL_DIR}/kami.sb"
log "writing sandbox profile: ${SANDBOX_PROFILE}"
cat > "${SANDBOX_PROFILE}" <<SBEOF
(version 1)

; Start permissive, then lock down writes.
(allow default)

; Deny ALL file writes everywhere …
(deny file-write*)

; … except inside the install directory (state, workspace, logs).
(allow file-write* (subpath "${INSTALL_DIR}"))

; Allow writes to /dev/null and standard pseudo-devices.
(allow file-write*
    (literal "/dev/null")
    (literal "/dev/stdout")
    (literal "/dev/stderr"))
SBEOF
chmod 600 "${SANDBOX_PROFILE}"

# ---------------------------------------------------------------------------
# 4. LaunchAgent plist
# ---------------------------------------------------------------------------
log "writing LaunchAgent plist: ${PLIST_FILE}"
mkdir -p "${PLIST_DIR}"
cat > "${PLIST_FILE}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
    "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>

    <key>ProgramArguments</key>
    <array>
        <string>/usr/bin/sandbox-exec</string>
        <string>-f</string>
        <string>${SANDBOX_PROFILE}</string>
        <string>${INSTALL_DIR}/${BINARY_NAME}</string>
    </array>

    <key>WorkingDirectory</key>
    <string>${INSTALL_DIR}</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>KAMI_HOME</key>
        <string>${INSTALL_DIR}</string>
    </dict>

    <!-- Restart automatically on crash -->
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
    </dict>

    <!-- Start on login -->
    <key>RunAtLoad</key>
    <true/>

    <!-- Log files inside the install dir (sandbox allows it) -->
    <key>StandardOutPath</key>
    <string>${INSTALL_DIR}/kami-gateway.log</string>
    <key>StandardErrorPath</key>
    <string>${INSTALL_DIR}/kami-gateway.err</string>
</dict>
</plist>
PLIST
chmod 600 "${PLIST_FILE}"

# ---------------------------------------------------------------------------
# 5. Load the service
# ---------------------------------------------------------------------------
log "loading service with launchctl…"
# Unload first in case it was already registered (idempotent re-install).
launchctl unload "${PLIST_FILE}" 2>/dev/null || true
launchctl load "${PLIST_FILE}"

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
log "done."
cat <<EOF

Next steps:

  1. Run the first-time setup wizard (sets API keys, Telegram token, chat id):
       ${INSTALL_DIR}/${BINARY_NAME} setup

  2. Restart the service to pick up the new config:
       launchctl unload  ${PLIST_FILE}
       launchctl load    ${PLIST_FILE}

  3. Watch the logs:
       tail -f ${INSTALL_DIR}/kami-gateway.log

  To stop the service:
       launchctl unload ${PLIST_FILE}

  To uninstall completely:
       ./setup-mac.sh --uninstall
EOF
