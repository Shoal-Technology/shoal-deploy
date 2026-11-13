#!/bin/sh
# Shoal Deploy installer. Alpine (OpenRC) and Debian/Ubuntu (systemd).
#
#   curl -fsSL https://raw.githubusercontent.com/Shoal-Technology/shoal-deploy/main/install.sh | sh
#   ./install.sh --public        listen on 0.0.0.0:7700 instead of loopback
#   ./install.sh --no-docker     skip the docker package install
#   ./install.sh --tls-internal  caddy issues its own certificates (no public dns)
#
# Set SHOALDEPLOY_BIN to install an already built binary instead of downloading one.
set -eu

REPO="Shoal-Technology/shoal-deploy"
LISTEN="127.0.0.1:7700"
DATA_DIR="/var/lib/shoal-deploy"
BIN_DIR="/usr/local/bin"
DO_DOCKER=1
DO_CADDY=1
TLS_INTERNAL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --public) LISTEN="0.0.0.0:7700" ;;
    --no-docker) DO_DOCKER=0 ;;
    --no-caddy) DO_CADDY=0 ;;
    --tls-internal) TLS_INTERNAL=1 ;;
    --listen) shift; LISTEN="$1" ;;
    --data-dir) shift; DATA_DIR="$1" ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

TLS_ARG=""
if [ "$TLS_INTERNAL" = 1 ]; then TLS_ARG=" --tls-internal"; fi

note() { printf '  %s\n' "$*"; }

# apk and the openrc tools live in /sbin, so a root shell with a narrow PATH
# (su without -, cron, some cloud-init consoles) finds no package manager at all.
PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"
export PATH

# The package manager and the init system answer different questions: a
# RHEL-family box has dnf with systemd, Alpine has apk with openrc. Detect both
# separately so an unknown distro is a note, not a dead end before anything ran.
if command -v apk >/dev/null 2>&1; then PKG="apk"
elif command -v apt-get >/dev/null 2>&1; then PKG="apt"
else PKG="none"; fi
if command -v rc-update >/dev/null 2>&1 && [ -d /etc/runlevels ]; then INIT="openrc"
elif command -v systemctl >/dev/null 2>&1; then INIT="systemd"
else INIT="none"; fi
OS_ID="$(sed -n 's/^ID=//p' /etc/os-release 2>/dev/null | tr -d '"' | head -1)"
[ -n "$OS_ID" ] || OS_ID="unknown"

echo "shoal-deploy install"
note "host:            $OS_ID"
note "package manager: $PKG"
note "init system:     $INIT"
note "data directory:  $DATA_DIR"
note "portal listen:   $LISTEN"

# 1. docker
# A minimal or freshly booted box can have an empty or stale package index, and
# apk then answers "no such package" for a package that does exist. Refresh the
# index once and turn a failure into a sentence a human can act on.
apk_refresh() {
  if [ "$PKG" = "apk" ] && [ "${APK_REFRESHED:-0}" = 0 ]; then
    apk update >/dev/null 2>&1 || true
    APK_REFRESHED=1
  fi
}

apk_install() {
  apk_refresh
  apk add --no-cache "$@" >/dev/null
}

# $1 = apk package name, $2 = apt package name. Exits non-zero when this host
# has no package manager we know, so callers can print something actionable.
pkg_install() {
  case "$PKG" in
    apk) apk_install "$1" ;;
    apt) apt-get install -y -qq "$2" >/dev/null ;;
    *) return 1 ;;
  esac
}

pkg_hint() { # $1 = package to install, $2 = flag that skips it
  mgr=""
  for m in dnf yum pacman zypper xbps-install; do
    if command -v "$m" >/dev/null 2>&1; then mgr="$m"; break; fi
  done
  if [ -n "$mgr" ]; then
    echo "this host uses $mgr, which this script does not drive." >&2
    echo "run '$mgr install -y $1' or install $1 by hand, then rerun with $2." >&2
  else
    echo "no package manager found; install $1 by hand, then rerun with $2." >&2
  fi
}

# Wait for the portal to answer. Sets PROBE_SECS. curl first because RHEL
# minimal images ship curl but no wget.
portal_probe() { # $1 = port
  PROBE_SECS=0
  while [ "$PROBE_SECS" -lt 60 ]; do
    if command -v curl >/dev/null 2>&1; then
      curl -fs -o /dev/null "http://127.0.0.1:$1/" 2>/dev/null && return 0
    else
      wget -q -O /dev/null "http://127.0.0.1:$1/" 2>/dev/null && return 0
    fi
    sleep 1
    PROBE_SECS=$((PROBE_SECS + 1))
  done
  return 1
}

if [ "$DO_DOCKER" = 1 ] && ! command -v docker >/dev/null 2>&1; then
  echo "installing docker"
  if [ "$PKG" = "apk" ]; then
    if ! apk_install docker; then
      echo "could not install docker from the configured apk repositories." >&2
      echo "check that /etc/apk/repositories points at a reachable mirror, then rerun." >&2
      exit 1
    fi
    apk_install docker-cli-compose || true
  elif [ "$PKG" = "apt" ]; then
    apt-get update -qq
    apt-get install -y -qq docker.io >/dev/null
  else
    pkg_hint docker --no-docker
    exit 1
  fi
fi
case "$INIT" in
  openrc)
    rc-update add docker default >/dev/null 2>&1 || true
    rc-service docker start >/dev/null 2>&1 || true ;;
  systemd) systemctl enable --now docker >/dev/null 2>&1 || true ;;
esac
command -v docker >/dev/null 2>&1 && note "docker: $(docker --version | cut -d, -f1)" || note "docker: missing (deploys will fail)"

# 1b. git, needed to clone repositories for builds
if ! command -v git >/dev/null 2>&1; then
  echo "installing git"
  if [ "$PKG" = "apk" ]; then
    apk_install git
  elif [ "$PKG" = "apt" ]; then
    apt-get install -y -qq git >/dev/null
  else
    note "git: missing — install it before deploying from a repository"
  fi
fi
if command -v git >/dev/null 2>&1; then note "git: $(git --version)"; fi

# 2. caddy (optional, for domains and TLS)
if [ "$DO_CADDY" = 1 ] && ! command -v caddy >/dev/null 2>&1; then
  echo "installing caddy"
  if [ "$PKG" = "apk" ]; then
    # the caddy package ships the binary and a Caddyfile but no init script;
    # without caddy-openrc nothing ever starts on boot and domains stay dark
    apk_install caddy caddy-openrc || apk_install caddy ||
      note "caddy: not installed (optional)"
  elif [ "$PKG" = "apt" ]; then
    apt-get install -y -qq caddy >/dev/null 2>&1 || note "caddy: not installed (optional)"
  else
    note "caddy: missing (optional) — install it yourself for domains"
  fi
fi
if command -v caddy >/dev/null 2>&1; then
  note "caddy: $(caddy version | head -1)"
  case "$INIT" in
    openrc)
      # caddy-openrc already defaults to --config /etc/caddy/Caddyfile; the missing
      # piece is only that it has to be installed, and caddy started once.
      rc-update add caddy default >/dev/null 2>&1 || true
      rc-service caddy restart >/dev/null 2>&1 || rc-service caddy start >/dev/null 2>&1 || true ;;
    systemd) systemctl enable --now caddy >/dev/null 2>&1 || true ;;
  esac
  # A binary with no working service is the worst case: the portal reports caddy
  # present while every domain answers nothing. Say it instead of pretending.
  if ! pgrep -x caddy >/dev/null 2>&1; then
    note "caddy: installed but NOT running — domains will not answer"
    note "  start it with: caddy start --config /etc/caddy/Caddyfile --adapter caddyfile"
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
  # curl is not in a bare alpine image; apk can still fetch both files, so make
  # sure one of the two exists before the download stands there with no tool.
  if ! command -v curl >/dev/null 2>&1; then
    pkg_install curl curl >/dev/null 2>&1 || {
      echo "curl is required to download the binary; install curl and rerun" >&2
      exit 1
    }
  fi
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
  bin="$(find "$tmp" -type f -name shoal-deploy -print -quit)"
  [ -n "$bin" ] || { echo "archive has no shoal-deploy binary" >&2; exit 1; }
  install -m 0755 "$bin" "$BIN_DIR/shoal-deploy"
  rm -rf "$tmp"
fi
note "binary: $BIN_DIR/shoal-deploy ($("$BIN_DIR/shoal-deploy" version))"

# 4. service
if [ "$INIT" = "openrc" ]; then
  mkdir -p /etc/init.d /etc/runlevels/default
  # unquoted heredoc: the listen address and data dir must reach the script
  cat > /etc/init.d/shoal-deploy <<EOF
#!/sbin/openrc-run
description="Shoal Deploy control plane"
command=$BIN_DIR/shoal-deploy
command_args="--listen $LISTEN --data-dir $DATA_DIR$TLS_ARG"
command_user=root:root
command_background=yes
pidfile=/run/shoal-deploy.pid
# use/after, not need: if docker is unhappy the portal must still come up so the
# operator can see what is wrong instead of finding a dead port and a log line.
depend() { use docker; after docker; }
output_log=/var/log/shoal-deploy.log
error_log=/var/log/shoal-deploy.log
EOF
  chmod 0755 /etc/init.d/shoal-deploy
  rc-update add docker default >/dev/null 2>&1 || true
  rc-update add shoal-deploy default >/dev/null
  # one start call: openrc pulls the `need docker` dependency itself, and a
  # second call answers "already starting" while the first is still working
  rc-service shoal-deploy start >/dev/null 2>&1 || true
  PORT="${LISTEN##*:}"
  if ! portal_probe "$PORT"; then
    echo "warning: portal did not answer on 127.0.0.1:$PORT after 60s" >&2
    echo "  status:  rc-service shoal-deploy status" >&2
    echo "  log:     /var/log/shoal-deploy.log" >&2
    echo "  by hand: $BIN_DIR/shoal-deploy --listen $LISTEN --data-dir $DATA_DIR$TLS_ARG" >&2
  else
    note "portal ready on 127.0.0.1:$PORT (${PROBE_SECS}s)"
  fi
elif [ "$INIT" = "systemd" ]; then
  mkdir -p /etc/systemd/system
  cat > /etc/systemd/system/shoal-deploy.service <<EOF
[Unit]
Description=Shoal Deploy control plane
After=docker.service network-online.target
Wants=docker.service

[Service]
ExecStart=$BIN_DIR/shoal-deploy --listen $LISTEN --data-dir $DATA_DIR$TLS_ARG
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now shoal-deploy >/dev/null 2>&1
  PORT="${LISTEN##*:}"
  if ! portal_probe "$PORT"; then
    echo "warning: portal did not answer on 127.0.0.1:$PORT after 60s" >&2
    echo "  status:  systemctl status shoal-deploy" >&2
    echo "  log:     journalctl -u shoal-deploy -n 50 --no-pager" >&2
    echo "  by hand: $BIN_DIR/shoal-deploy --listen $LISTEN --data-dir $DATA_DIR$TLS_ARG" >&2
  else
    note "portal ready on 127.0.0.1:$PORT (${PROBE_SECS}s)"
  fi
else
  # a container image or a stripped rootfs can have neither init system; the
  # binary still works, so say so instead of dying half way through the install
  note "no openrc or systemd here: start it by hand with"
  note "  $BIN_DIR/shoal-deploy --listen $LISTEN --data-dir $DATA_DIR$TLS_ARG"
  echo
  echo "portal: http://$LISTEN/  (user admin)"
  exit 0
fi

sleep 1
if [ -f "$DATA_DIR/initial-passphrase" ]; then
  echo
  echo "first run — the generated passphrase is in $DATA_DIR/initial-passphrase"
  echo "read it there, then change it under settings after signing in."
fi
PORT="${LISTEN##*:}"
echo
if [ "$LISTEN" = "127.0.0.1:$PORT" ]; then
  echo "portal: http://127.0.0.1:$PORT/  (user admin, loopback only)"
  echo "not reachable from another machine; reinstall with --public for that,"
  echo "or open a tunnel: ssh -L $PORT:127.0.0.1:$PORT $(hostname)"
else
  # firewalld is enabled by default on rhel/rocky/fedora and silently drops the
  # packet, so a working portal looks dead from the LAN.
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null 2>&1 &&
      firewall-cmd --reload >/dev/null 2>&1 &&
      note "firewall: opened $PORT/tcp in firewalld"
  fi
  IP="$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)"
  [ -n "$IP" ] || IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  echo "portal: http://${IP:-<this-host-ip>}:$PORT/  (user admin)"
fi
if [ "$DO_CADDY" = 1 ] && command -v caddy >/dev/null 2>&1; then
  echo "give it a name: caddy reverse_proxy 127.0.0.1:$PORT"
else
  echo "domains and TLS need caddy: apk add caddy / dnf install caddy"
fi
exit 0
