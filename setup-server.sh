#!/usr/bin/env bash
# DNS Messenger — Server Setup Script
# Supports: fresh install, repair/reconfigure broken install
# Usage:
#   sudo bash setup-server.sh              # fresh install
#   sudo bash setup-server.sh --reconfigure  # fix/reconfigure existing install

set -e

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[*]${NC} $*"; }
success() { echo -e "${GREEN}[✓]${NC} $*"; }
warn()    { echo -e "${YELLOW}[!]${NC} $*"; }
die()     { echo -e "${RED}[✗]${NC} $*"; exit 1; }

INSTALL_DIR="/opt/dnsmessenger"
DATA_DIR="/var/lib/dnsmessenger"
REPO_URL="https://github.com/Mysterious53/dns-messenger.git"

# ── Check root ────────────────────────────────────────────────────────────────
[ "$EUID" -ne 0 ] && die "Please run as root: sudo bash setup-server.sh"

# ── Parse flags ───────────────────────────────────────────────────────────────
RECONFIGURE=false
for arg in "$@"; do
  case "$arg" in
    --reconfigure|--repair|--fix) RECONFIGURE=true ;;
  esac
done

echo ""
echo -e "${BLUE}╔══════════════════════════════════════╗${NC}"
if $RECONFIGURE; then
  echo -e "${BLUE}║    DNS Messenger — Reconfigure       ║${NC}"
else
  echo -e "${BLUE}║    DNS Messenger — Server Setup      ║${NC}"
fi
echo -e "${BLUE}╚══════════════════════════════════════╝${NC}"
echo ""

# ── Collect config — always read from /dev/tty so curl|bash works ─────────────
# Try to load existing values as defaults
OLD_DOMAIN=""; OLD_DNS_ADDR=":53"; OLD_HTTP_PORT="8080"; OLD_MAX_MSGS="100"
if [ -f "$DATA_DIR/.env" ]; then
  source "$DATA_DIR/.env" 2>/dev/null || true
  OLD_DOMAIN="${DOMAIN:-}"
  OLD_DNS_ADDR="${DNS_ADDR:-:53}"
  OLD_HTTP_PORT="${HTTP_PORT:-8080}"
  OLD_MAX_MSGS="${MAX_MSGS:-100}"
fi

# Read interactively from /dev/tty (works with curl|bash)
exec 3</dev/tty

prompt() {
  local var_name="$1" prompt_text="$2" default="$3"
  if [ -n "$default" ]; then
    printf "%s [%s]: " "$prompt_text" "$default" >&2
  else
    printf "%s: " "$prompt_text" >&2
  fi
  read -r "$var_name" <&3
  # If empty, use default
  local val
  eval val=\$$var_name
  if [ -z "$val" ] && [ -n "$default" ]; then
    eval "$var_name=\"$default\""
  fi
}

prompt_secret() {
  local var_name="$1" prompt_text="$2"
  printf "%s: " "$prompt_text" >&2
  read -rs "$var_name" <&3
  echo "" >&2
}

prompt DOMAIN      "Domain (e.g. chat.example.com)"  "$OLD_DOMAIN"
[ -z "$DOMAIN" ] && die "Domain is required"

if $RECONFIGURE && [ -f "$DATA_DIR/.passphrase" ]; then
  printf "Passphrase [leave blank to keep current]: " >&2
  read -rs NEW_PASS <&3; echo "" >&2
  if [ -n "$NEW_PASS" ]; then
    PASSPHRASE="$NEW_PASS"
  else
    PASSPHRASE="$(cat "$DATA_DIR/.passphrase")"
    info "Keeping existing passphrase"
  fi
else
  prompt_secret PASSPHRASE "Shared passphrase"
  [ -z "$PASSPHRASE" ] && die "Passphrase is required"
fi

prompt DNS_ADDR    "DNS listen address"    "${OLD_DNS_ADDR:-:53}"
prompt HTTP_PORT   "HTTP web UI port"      "${OLD_HTTP_PORT:-8080}"
prompt MAX_MSGS    "Max messages per room" "${OLD_MAX_MSGS:-100}"

exec 3<&-

echo ""
info "Domain    : $DOMAIN"
info "DNS addr  : $DNS_ADDR"
info "HTTP port : $HTTP_PORT"
echo ""

# ── Install Go if missing ─────────────────────────────────────────────────────
export PATH="/usr/local/go/bin:$PATH"
if ! command -v go &>/dev/null; then
  info "Go not found — installing Go 1.22..."
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    armv*)   GOARCH="armv6l" ;;
    *)       die "Unsupported arch: $(uname -m)" ;;
  esac
  GO_VER="1.22.4"
  GO_TAR="go${GO_VER}.${OS}-${GOARCH}.tar.gz"
  curl -fsSL "https://go.dev/dl/${GO_TAR}" -o "/tmp/${GO_TAR}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TAR}"
  rm "/tmp/${GO_TAR}"
  export PATH="/usr/local/go/bin:$PATH"
  success "Go ${GO_VER} installed"
else
  success "Go $(go version | awk '{print $3}') already installed"
fi

# ── Get source code ───────────────────────────────────────────────────────────
# If running via curl|bash, BASH_SOURCE[0] will be empty/stdin
if $RECONFIGURE && [ -d "$INSTALL_DIR" ] && [ -f "$INSTALL_DIR/dnsmsg-server" ]; then
  info "Skipping rebuild — binary already exists (use fresh install to rebuild)"
  SKIP_BUILD=true
else
  SKIP_BUILD=false
fi

if ! $SKIP_BUILD; then
  # Detect if we have source available
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ "${BASH_SOURCE[0]}" != "/dev/stdin" ] && [ -f "$(dirname "${BASH_SOURCE[0]}")/go.mod" ]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    info "Using local source from $SCRIPT_DIR"
  else
    info "Cloning repository..."
    CLONE_DIR="/tmp/dns-messenger-src"
    rm -rf "$CLONE_DIR"
    git clone --depth=1 "$REPO_URL" "$CLONE_DIR" || die "Clone failed — check internet connection"
    SCRIPT_DIR="$CLONE_DIR"
    success "Repository cloned"
  fi

  info "Building server binary..."
  cd "$SCRIPT_DIR"
  go build -ldflags="-s -w" -o /tmp/dnsmsg-server ./cmd/server || die "Build failed"
  success "Server binary built"

  # Install binary and static files
  info "Installing files..."
  mkdir -p "$INSTALL_DIR" "$DATA_DIR"
  cp /tmp/dnsmsg-server "$INSTALL_DIR/dnsmsg-server"
  chmod +x "$INSTALL_DIR/dnsmsg-server"
  cp -r "$SCRIPT_DIR/internal/web/static" "$INSTALL_DIR/static"
  success "Files installed to $INSTALL_DIR"
fi

# ── Save config ───────────────────────────────────────────────────────────────
mkdir -p "$DATA_DIR"

if [ ! -f "$DATA_DIR/rooms.txt" ]; then
  if [ -f "${SCRIPT_DIR:-}/rooms.txt" ]; then
    cp "$SCRIPT_DIR/rooms.txt" "$DATA_DIR/rooms.txt"
  else
    cat > "$DATA_DIR/rooms.txt" <<'ROOMS'
general
tech
random
ROOMS
  fi
  success "rooms.txt created in $DATA_DIR/"
else
  warn "rooms.txt already exists — not overwritten"
fi

cat > "$DATA_DIR/.env" <<ENV
DOMAIN=$DOMAIN
DNS_ADDR=$DNS_ADDR
HTTP_PORT=$HTTP_PORT
MAX_MSGS=$MAX_MSGS
ENV

printf '%s' "$PASSPHRASE" > "$DATA_DIR/.passphrase"
chmod 600 "$DATA_DIR/.passphrase"

# ── Create / update systemd service ──────────────────────────────────────────
info "Writing systemd service..."

cat > /etc/systemd/system/dnsmessenger.service <<SERVICE
[Unit]
Description=DNS Messenger Server
After=network.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/dnsmsg-server \\
  --domain $DOMAIN \\
  --passphrase $PASSPHRASE \\
  --dns-addr $DNS_ADDR \\
  --http-addr :$HTTP_PORT \\
  --rooms $DATA_DIR/rooms.txt \\
  --max-messages $MAX_MSGS \\
  --allow-manage
WorkingDirectory=$INSTALL_DIR
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
systemctl enable dnsmessenger
success "systemd service updated"

# ── Open firewall ─────────────────────────────────────────────────────────────
if command -v ufw &>/dev/null; then
  ufw allow 53/udp comment "DNS Messenger DNS" 2>/dev/null || true
  ufw allow "${HTTP_PORT}/tcp" comment "DNS Messenger HTTP" 2>/dev/null || true
  success "ufw rules updated (53/udp, ${HTTP_PORT}/tcp)"
elif command -v firewall-cmd &>/dev/null; then
  firewall-cmd --permanent --add-port=53/udp 2>/dev/null || true
  firewall-cmd --permanent --add-port="${HTTP_PORT}/tcp" 2>/dev/null || true
  firewall-cmd --reload 2>/dev/null || true
  success "firewalld rules updated"
fi

# ── Start / restart service ───────────────────────────────────────────────────
info "Starting DNS Messenger..."
systemctl restart dnsmessenger
sleep 2

if systemctl is-active --quiet dnsmessenger; then
  success "DNS Messenger is running!"
else
  warn "Service failed to start. Showing logs:"
  journalctl -u dnsmessenger -n 20 --no-pager
  echo ""
  warn "Fix the issue then run: systemctl restart dnsmessenger"
fi

# ── Get server IP ─────────────────────────────────────────────────────────────
SERVER_IP=$(curl -s --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║               Setup Complete!                       ║${NC}"
echo -e "${GREEN}╠══════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║${NC}  Domain    : ${BLUE}${DOMAIN}${NC}"
echo -e "${GREEN}║${NC}  DNS       : ${BLUE}${SERVER_IP}:53 (UDP)${NC}"
echo -e "${GREEN}║${NC}  Web UI    : ${BLUE}http://${SERVER_IP}:${HTTP_PORT}${NC}"
echo -e "${GREEN}║${NC}  Rooms     : ${BLUE}${DATA_DIR}/rooms.txt${NC}"
echo -e "${GREEN}╠══════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║${NC}  Useful commands:"
echo -e "${GREEN}║${NC}   systemctl status dnsmessenger"
echo -e "${GREEN}║${NC}   journalctl -u dnsmessenger -f"
echo -e "${GREEN}║${NC}   nano ${DATA_DIR}/rooms.txt"
echo -e "${GREEN}║${NC}   sudo bash setup-server.sh --reconfigure"
echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${YELLOW}Client connection info:${NC}"
echo -e "  Domain     : ${BLUE}${DOMAIN}${NC}"
echo -e "  Resolver   : ${BLUE}${SERVER_IP}:53${NC}"
echo -e "  Passphrase : ${BLUE}(what you entered)${NC}"
echo ""
