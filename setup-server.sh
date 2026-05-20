#!/usr/bin/env bash
# DNS Messenger — Server Setup Script
# Run as root (or with sudo) on a Linux VPS

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()    { echo -e "${BLUE}[*]${NC} $*"; }
success() { echo -e "${GREEN}[✓]${NC} $*"; }
warn()    { echo -e "${YELLOW}[!]${NC} $*"; }
die()     { echo -e "${RED}[✗]${NC} $*"; exit 1; }

echo ""
echo -e "${BLUE}╔══════════════════════════════════════╗${NC}"
echo -e "${BLUE}║       DNS Messenger — Server Setup   ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════╝${NC}"
echo ""

# ── 1. Check root ──────────────────────────────────────────────────────────────
if [ "$EUID" -ne 0 ]; then
  die "Please run as root: sudo bash setup-server.sh"
fi

# ── 2. Collect config ──────────────────────────────────────────────────────────
read -rp "Domain for DNS Messenger (e.g. chat.example.com): " DOMAIN
[ -z "$DOMAIN" ] && die "Domain is required"

read -rsp "Shared passphrase (clients need this to connect): " PASSPHRASE
echo ""
[ -z "$PASSPHRASE" ] && die "Passphrase is required"

read -rp "DNS listen address [default: :53]: " DNS_ADDR
DNS_ADDR="${DNS_ADDR:-:53}"

read -rp "HTTP web UI port [default: 8080]: " HTTP_PORT
HTTP_PORT="${HTTP_PORT:-8080}"

read -rp "Max messages per room in memory [default: 100]: " MAX_MSGS
MAX_MSGS="${MAX_MSGS:-100}"

INSTALL_DIR="/opt/dnsmessenger"
DATA_DIR="/var/lib/dnsmessenger"

echo ""
info "Will install to: $INSTALL_DIR"
info "Data directory:  $DATA_DIR"
echo ""

# ── 3. Install Go if missing ───────────────────────────────────────────────────
if ! command -v go &>/dev/null; then
  info "Go not found — installing Go 1.22..."
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64)  GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    armv*)   GOARCH="armv6l" ;;
    *)       die "Unsupported arch: $ARCH" ;;
  esac
  GO_VERSION="1.22.4"
  GO_TAR="go${GO_VERSION}.${OS}-${GOARCH}.tar.gz"
  curl -fsSL "https://go.dev/dl/${GO_TAR}" -o "/tmp/${GO_TAR}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "/tmp/${GO_TAR}"
  rm "/tmp/${GO_TAR}"
  export PATH="/usr/local/go/bin:$PATH"
  success "Go ${GO_VERSION} installed"
else
  success "Go $(go version | awk '{print $3}') already installed"
fi

export PATH="/usr/local/go/bin:$PATH"

# ── 4. Build binaries ──────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
info "Building server binary..."
cd "$SCRIPT_DIR"
go build -ldflags="-s -w" -o /tmp/dnsmsg-server ./cmd/server || die "Build failed"
success "Server binary built"

# ── 5. Install files ───────────────────────────────────────────────────────────
info "Installing files..."
mkdir -p "$INSTALL_DIR" "$DATA_DIR"

cp /tmp/dnsmsg-server "$INSTALL_DIR/dnsmsg-server"
chmod +x "$INSTALL_DIR/dnsmsg-server"

# Copy static web UI
cp -r "$SCRIPT_DIR/internal/web/static" "$INSTALL_DIR/static"

# Create rooms.txt if not present in data dir
if [ ! -f "$DATA_DIR/rooms.txt" ]; then
  cp "$SCRIPT_DIR/rooms.txt" "$DATA_DIR/rooms.txt"
  success "rooms.txt copied to $DATA_DIR/"
else
  warn "rooms.txt already exists in $DATA_DIR/ — not overwritten"
fi

# Write .env for reference
cat > "$DATA_DIR/.env" <<EOF
DOMAIN=$DOMAIN
DNS_ADDR=$DNS_ADDR
HTTP_PORT=$HTTP_PORT
MAX_MSGS=$MAX_MSGS
EOF
# Store passphrase separately with restricted permissions
echo "$PASSPHRASE" > "$DATA_DIR/.passphrase"
chmod 600 "$DATA_DIR/.passphrase"

success "Files installed to $INSTALL_DIR"

# ── 6. Create systemd service ──────────────────────────────────────────────────
info "Creating systemd service..."

cat > /etc/systemd/system/dnsmessenger.service <<EOF
[Unit]
Description=DNS Messenger Server
After=network.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/dnsmsg-server \\
  --domain $DOMAIN \\
  --passphrase $(cat $DATA_DIR/.passphrase) \\
  --dns-addr $DNS_ADDR \\
  --http-addr :$HTTP_PORT \\
  --rooms $DATA_DIR/rooms.txt \\
  --max-messages $MAX_MSGS \\
  --allow-manage
WorkingDirectory=$INSTALL_DIR
Restart=on-failure
RestartSec=5
# Allow binding to port 53
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable dnsmessenger
success "systemd service created and enabled"

# ── 7. Open firewall ports ─────────────────────────────────────────────────────
if command -v ufw &>/dev/null; then
  info "Opening ports in ufw..."
  ufw allow 53/udp  comment "DNS Messenger DNS" 2>/dev/null || true
  ufw allow "$HTTP_PORT/tcp" comment "DNS Messenger HTTP" 2>/dev/null || true
  success "ufw rules added (53/udp and $HTTP_PORT/tcp)"
elif command -v firewall-cmd &>/dev/null; then
  info "Opening ports in firewalld..."
  firewall-cmd --permanent --add-port=53/udp 2>/dev/null || true
  firewall-cmd --permanent --add-port="${HTTP_PORT}/tcp" 2>/dev/null || true
  firewall-cmd --reload 2>/dev/null || true
  success "firewalld rules added"
else
  warn "No firewall tool found — make sure ports 53/udp and $HTTP_PORT/tcp are open"
fi

# ── 8. Start service ───────────────────────────────────────────────────────────
info "Starting DNS Messenger..."
systemctl start dnsmessenger
sleep 2
if systemctl is-active --quiet dnsmessenger; then
  success "DNS Messenger is running!"
else
  warn "Service may have failed. Check with: journalctl -u dnsmessenger -n 30"
fi

# ── 9. Print summary ───────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║               Setup Complete!                       ║${NC}"
echo -e "${GREEN}╠══════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║${NC}  Domain    : ${BLUE}$DOMAIN${NC}"
echo -e "${GREEN}║${NC}  DNS       : ${BLUE}this-server-ip:53 (UDP)${NC}"
echo -e "${GREEN}║${NC}  Web UI    : ${BLUE}http://this-server-ip:$HTTP_PORT${NC}"
echo -e "${GREEN}║${NC}  Rooms     : ${BLUE}$DATA_DIR/rooms.txt${NC}"
echo -e "${GREEN}╠══════════════════════════════════════════════════════╣${NC}"
echo -e "${GREEN}║${NC}  Useful commands:${NC}"
echo -e "${GREEN}║${NC}   systemctl status dnsmessenger${NC}"
echo -e "${GREEN}║${NC}   journalctl -u dnsmessenger -f${NC}"
echo -e "${GREEN}║${NC}   nano $DATA_DIR/rooms.txt${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${YELLOW}DNS record required:${NC}"
echo -e "  Add an NS record pointing ${BLUE}$DOMAIN${NC} → this server's IP"
echo -e "  OR use this server's IP directly as resolver in the client."
echo ""
echo -e "  ${YELLOW}Client connection:${NC}"
echo -e "  Domain     : ${BLUE}$DOMAIN${NC}"
echo -e "  Passphrase : ${BLUE}(what you entered)${NC}"
echo -e "  Resolver   : ${BLUE}this-server-ip:53${NC}"
echo ""
