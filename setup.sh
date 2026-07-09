#!/usr/bin/env bash
#
# setup.sh — SYSTEM LAYER 2: OS-level systemd hardening for the Telegram agent.
#
# Installs the compiled Go binary as an unprivileged systemd service that:
#   * runs as the dedicated no-login system user `tg-agent`
#   * sees the entire filesystem read-only (ProtectSystem=strict, ProtectHome=yes)
#   * can write ONLY to /opt/tg-agent/storage (ReadWritePaths)
#   * gets a private /tmp and an empty capability set
#
# The agent talks to the host-level code service purely over 127.0.0.1:8080,
# so no extra filesystem or capability grants are needed.
#
# Usage:  sudo ./setup.sh [path-to-binary]      (default: ./kami-gateway)

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
readonly SERVICE_USER="tg-agent"
readonly INSTALL_DIR="/opt/tg-agent"
readonly STORAGE_DIR="${INSTALL_DIR}/storage"
readonly BINARY_NAME="kami-gateway"
readonly SERVICE_FILE="/etc/systemd/system/tg-agent.service"
readonly SOURCE_BINARY="${1:-./${BINARY_NAME}}"

log()  { printf '\033[1;32m[setup]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[setup] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
[[ "${EUID}" -eq 0 ]] || die "this script must be run as root (try: sudo $0)"
[[ -f "${SOURCE_BINARY}" ]] || die "binary not found at '${SOURCE_BINARY}' — run 'make build' first, or pass the path as the first argument"
command -v systemctl >/dev/null 2>&1 || die "systemctl not found — this script requires a systemd host"

# ---------------------------------------------------------------------------
# 1. Unprivileged system user (no login shell, no home directory login)
# ---------------------------------------------------------------------------
if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    log "system user '${SERVICE_USER}' already exists — skipping creation"
else
    log "creating system user '${SERVICE_USER}' (no login shell)"
    useradd -r -s /bin/false "${SERVICE_USER}"
fi

# ---------------------------------------------------------------------------
# 2. Installation directory and writable storage folder
# ---------------------------------------------------------------------------
log "creating ${INSTALL_DIR} and ${STORAGE_DIR}"
mkdir -p "${STORAGE_DIR}"

# ---------------------------------------------------------------------------
# 3. Install the binary with exclusive owner-only permissions
# ---------------------------------------------------------------------------
log "installing binary to ${INSTALL_DIR}/${BINARY_NAME} (chown ${SERVICE_USER}, chmod 700)"
install -m 700 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${SOURCE_BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"

# The storage folder is the ONLY writable location the service will have.
chown -R "${SERVICE_USER}:${SERVICE_USER}" "${STORAGE_DIR}"
chmod 700 "${STORAGE_DIR}"
# The install dir itself stays root-owned and read-only to the service user,
# so the agent cannot replace its own binary.
chown root:root "${INSTALL_DIR}"
chmod 755 "${INSTALL_DIR}"

# ---------------------------------------------------------------------------
# 4. Systemd unit with strict kernel isolation
# ---------------------------------------------------------------------------
log "writing hardened unit file to ${SERVICE_FILE}"
cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Sandboxed Telegram AI agent (kami-gateway)
Documentation=file://${INSTALL_DIR}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
WorkingDirectory=${STORAGE_DIR}
Environment=KAMI_HOME=${STORAGE_DIR}
Restart=on-failure
RestartSec=5

# --- Kernel isolation: the agent can write ONLY inside its storage dir ---
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=${STORAGE_DIR}
PrivateTmp=yes
CapabilityBoundingSet=
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
EOF
chmod 644 "${SERVICE_FILE}"

# ---------------------------------------------------------------------------
# 5. Activate
# ---------------------------------------------------------------------------
log "reloading systemd and enabling the service"
systemctl daemon-reload
systemctl enable tg-agent.service

log "done."
cat <<EOF

Next steps:
  1. Run the first-time setup wizard as the service user (writes API keys
     into ${STORAGE_DIR}/state, the service's only writable path):
       sudo -u ${SERVICE_USER} KAMI_HOME=${STORAGE_DIR} ${INSTALL_DIR}/${BINARY_NAME} setup
  2. Start the service:
       sudo systemctl start tg-agent.service
  3. Watch it:
       journalctl -u tg-agent.service -f
EOF
