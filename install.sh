#!/bin/sh
# Shoal Deploy installer. Alpine (OpenRC) and Debian/Ubuntu (systemd).
#
#   curl -fsSL https://raw.githubusercontent.com/Shoal-Technology/shoal-deploy/main/install.sh | sh
#   ./install.sh --public        listen on 0.0.0.0:7700 instead of loopback
#   ./install.sh --no-docker     skip the docker package install
#
# Set SHOALDEPLOY_BIN to install an already built binary instead of downloading one.
set -eu

REPO="Shoal-Technology/shoal-deploy"
LISTEN="127.0.0.1:7700"
DATA_DIR="/var/lib/shoal-deploy"
BIN_DIR="/usr/local/bin"
DO_DOCKER=1
DO_CADDY=1

while [ $# -gt 0 ]; do
  case "$1" in
    --public) LISTEN="0.0.0.0:7700" ;;
    --no-docker) DO_DOCKER=0 ;;
    --no-caddy) DO_CADDY=0 ;;
    --listen) shift; LISTEN="$1" ;;
    --data-dir) shift; DATA_DIR="$1" ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

note() { printf '  %s\n' "$*"; }

if [ "$(id -u)" = 0 ] && command -v apk >/dev/null 2>&1; then
  PKG="apk"
elif command -v apt-get >/dev/null 2>&1; then
  PKG="apt"
else
  echo "supported package managers: apk, apt-get" >&2
  exit 1
fi

echo "shoal-deploy install"
note "package manager: $PKG"
note "data directory:  $DATA_DIR"
note "portal listen:   $LISTEN"

# 1. docker
if [ "$DO_DOCKER" = 1 ] && ! command -v docker >/dev/null 2>&1; then
  echo "installing docker"
  if [ "$PKG" = "apk" ]; then
    apk add --no-cache docker docker-cli-compose >/dev/null
  else
    apt-get update -qq
    apt-get install -y -qq docker.io >/dev/null
  fi
fi
if [ "$PKG" = "apk" ]; then
  rc-update add docker default >/dev/null 2>&1 || true
  rc-service docker start >/dev/null 2>&1 || true
else
  systemctl enable --now docker >/dev/null 2>&1 || true
fi
command -v docker >/dev/null 2>&1 && note "docker: $(docker --version | cut -d, -f1)" || note "docker: missing (deploys will fail)"

# 1b. git, needed to clone repositories for builds
if ! command -v git >/dev/null 2>&1; then
  echo "installing git"
  if [ "$PKG" = "apk" ]; then
    apk add --no-cache git >/dev/null
  else
    apt-get install -y -qq git >/dev/null
  fi
fi
note "git: $(git --version)"

# 2. caddy (optional, for domains and TLS)
if [ "$DO_CADDY" = 1 ] && ! command -v caddy >/dev/null 2>&1; then
  echo "installing caddy"
  if [ "$PKG" = "apk" ]; then
    apk add --no-cache caddy >/dev/null 2>&1 || note "caddy: not installed (optional)"
  else
    apt-get install -y -qq caddy >/dev/null 2>&1 || note "caddy: not installed (optional)"
  fi
fi
if command -v caddy >/dev/null 2>&1; then
  note "caddy: $(caddy version | head -1)"
  if [ "$PKG" = "apk" ]; then
    rc-update add caddy default >/dev/null 2>&1 || true
    rc-service caddy start >/dev/null 2>&1 || true
  else
    systemctl enable --now caddy >/dev/null 2>&1 || true
  fi
else
  note "caddy: off, apps stay on their published port"
fi

# 3. binary
mkdir -p "$BIN_DIR" "$DATA_DIR"
chmod 700 "$DATA_DIR"
if [ -n "${SHOALDEPLOY_BIN:-}" ]; then
  echo "installing $SHOALDEPLOY_BIN"
  install -m 0755 "$SHOALDEPLOY_BIN" "$BIN_DIR/shoal-deploy"
else
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
  esac
  tag="${SHOALDEPLOY_VERSION:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)}"
  [ -n "$tag" ] || { echo "no release found; set SHOALDEPLOY_VERSION" >&2; exit 1; }
  url="https://github.com/$REPO/releases/download/$tag/shoal-deploy_${tag#v}_linux_${arch}.tar.gz"
  echo "downloading $url"
  tmp="$(mktemp -d)"
  curl -fsSL "$url" -o "$tmp/bundle.tar.gz"
  if command -v sha256sum >/dev/null 2>&1; then
    want="$(curl -fsSL "https://github.com/$REPO/releases/download/$tag/checksums.txt" | awk -v f="$(basename "$url")" '$2 == f {print $1}')"
    [ -n "$want" ] || { echo "no checksum published for $(basename "$url")" >&2; exit 1; }
    got="$(sha256sum "$tmp/bundle.tar.gz" | awk '{print $1}')"
    [ "$want" = "$got" ] || { echo "checksum mismatch: expected $want, got $got" >&2; exit 1; }
    note "checksum: ok"
  fi
  tar -xzf "$tmp/bundle.tar.gz" -C "$tmp"
  install -m 0755 "$tmp/shoal-deploy" "$BIN_DIR/shoal-deploy"
  rm -rf "$tmp"
fi
note "binary: $BIN_DIR/shoal-deploy ($("$BIN_DIR/shoal-deploy" version))"

# 4. service
if [ "$PKG" = "apk" ]; then
  # unquoted heredoc: the listen address and data dir must reach the script
  cat > /etc/init.d/shoal-deploy <<EOF
#!/sbin/openrc-run
description="Shoal Deploy control plane"
command=$BIN_DIR/shoal-deploy
command_args="--listen $LISTEN --data-dir $DATA_DIR"
command_user=root:root
command_background=yes
pidfile=/run/shoal-deploy.pid
depend() { need docker; after docker; }
output_log=/var/log/shoal-deploy.log
error_log=/var/log/shoal-deploy.log
EOF
  chmod 0755 /etc/init.d/shoal-deploy
  rc-update add shoal-deploy default >/dev/null
  rc-service shoal-deploy restart >/dev/null 2>&1 || rc-service shoal-deploy start >/dev/null
else
  cat > /etc/systemd/system/shoal-deploy.service <<EOF
[Unit]
Description=Shoal Deploy control plane
After=docker.service network-online.target
Requires=docker.service

[Service]
ExecStart=$BIN_DIR/shoal-deploy --listen $LISTEN --data-dir $DATA_DIR
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now shoal-deploy >/dev/null 2>&1
fi

sleep 1
if [ -f "$DATA_DIR/initial-passphrase" ]; then
  echo
  echo "first run — the generated passphrase is in $DATA_DIR/initial-passphrase"
  echo "read it there, then change it under settings after signing in."
fi
echo
echo "portal: http://$LISTEN/  (user admin)"
if [ "$LISTEN" = "127.0.0.1:7700" ]; then
  echo "loopback only: open a tunnel, e.g. ssh -L 7700:127.0.0.1:7700 $(hostname)"
fi
echo "put it behind caddy for a domain and TLS: reverse_proxy 127.0.0.1:7700"
exit 0
