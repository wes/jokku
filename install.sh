#!/bin/sh
# Installs Jokku on a Linux server:
#
#   curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
#
# This script only downloads the jokku binary; "jokku setup" does the rest, so
# each release brings the server changes it needs. To update later, use
# update.sh.
#
# Environment:
#   JOKKU_VERSION=v0.0.2   install a specific release (default: latest)
#   JOKKU_IMPORT_KEYS=0    don't give your SSH keys access to jokku
#   JOKKU_DOWNLOAD_URL=... fetch binaries from elsewhere (a mirror, file:///dist)
#
# To add this server to a cluster, run the command "jokku cluster:join-command"
# prints on the control node; it passes --join ADDRESS --token TOKEN. To make
# it an edge of a cluster (a public machine routing traffic to the nodes
# behind it), run the command "jokku edge:add" prints; it passes --edge BUNDLE.
set -eu

REPO=wes/jokku
VERSION=${JOKKU_VERSION:-latest}
BIN=/usr/local/bin/jokku

say()  { printf -- '-----> %s\n' "$*"; }
info() { printf '       %s\n' "$*"; }
warn() { printf ' !     %s\n' "$*" >&2; }
die()  { warn "$*"; exit 1; }

# Options: --join ADDRESS --token TOKEN [--name NAME] adds this server to an
# existing cluster as a worker (get the command from jokku cluster:join-command);
# --edge BUNDLE makes it an edge (get the command from jokku edge:add).
JOIN="" TOKEN="" NAME="" EDGE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --join | --token | --name | --edge)
      [ $# -ge 2 ] || die "$1 needs a value"
      case "$1" in
        --join) JOIN=$2 ;;
        --token) TOKEN=$2 ;;
        --name) NAME=$2 ;;
        --edge) EDGE=$2 ;;
      esac
      shift 2
      ;;
    *) die "Unknown option: $1" ;;
  esac
done
if [ -n "$JOIN" ] && [ -z "$TOKEN" ]; then die "--join needs --token"; fi
if [ -n "$JOIN" ] && [ -n "$EDGE" ]; then die "--join and --edge don't go together"; fi

[ "$(id -u)" -eq 0 ] || die "Run as root: curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh"
[ "$(uname -s)" = Linux ] || die "Jokku runs on Linux servers (this is $(uname -s))"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "Unsupported CPU architecture: $(uname -m)" ;;
esac

if [ -z "$JOIN" ] && [ -z "$EDGE" ] && [ -x "$BIN" ] && systemctl is-active --quiet jokku 2>/dev/null; then
  say "Jokku is already installed ($("$BIN" version 2>/dev/null | head -n 1 | cut -d' ' -f3))"
  info "To update: curl -fsSL https://raw.githubusercontent.com/$REPO/main/update.sh | sudo sh"
  exit 0
fi

if [ -n "${JOKKU_DOWNLOAD_URL:-}" ]; then
  URL=$JOKKU_DOWNLOAD_URL
elif [ "$VERSION" = latest ]; then
  URL="https://github.com/$REPO/releases/latest/download"
else
  URL="https://github.com/$REPO/releases/download/$VERSION"
fi
say "Downloading jokku ($VERSION, linux/$ARCH)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$URL/jokku-linux-$ARCH" -o "$tmp/jokku-linux-$ARCH" || die "Download failed: $URL/jokku-linux-$ARCH"
curl -fsSL "$URL/checksums.txt" -o "$tmp/checksums.txt" || die "Download failed: $URL/checksums.txt"
(cd "$tmp" && grep " jokku-linux-$ARCH\$" checksums.txt | sha256sum -c - >/dev/null) || die "Checksum mismatch for jokku-linux-$ARCH"
install -m 0755 "$tmp/jokku-linux-$ARCH" "$BIN"

if [ -n "$EDGE" ]; then
  "$BIN" setup --edge "$EDGE" || die "Setting up the edge failed. Fix the problem above and run this again."
  printf '\n=====> This server is an edge of the cluster\n'
  info "See it from the control node: jokku edge:list"
  info "Point your domains' DNS at this server's public address."
  exit 0
fi

if [ -n "$JOIN" ]; then
  if [ -n "$NAME" ]; then
    "$BIN" setup --join "$JOIN" --token "$TOKEN" --name "$NAME" || die "Joining failed. Fix the problem above and run this again."
  else
    "$BIN" setup --join "$JOIN" --token "$TOKEN" || die "Joining failed. Fix the problem above and run this again."
  fi
  printf '\n=====> This server joined the cluster\n'
  info "See it from the control node: jokku nodes:list"
  exit 0
fi

"$BIN" setup || die "Setup failed. Fix the problem above, then run: sudo jokku setup"

# Give the keys that can already log in here (yours, via sudo, or root's)
# access to jokku, so git push works right away.
if [ "${JOKKU_IMPORT_KEYS:-1}" = 1 ] && [ "$("$BIN" ssh-keys:list --format json)" = "[]" ]; then
  home=/root
  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
    home=$(getent passwd "$SUDO_USER" | cut -d: -f6)
  fi
  if [ -f "$home/.ssh/authorized_keys" ]; then
    say "Adding the SSH keys from $home/.ssh/authorized_keys"
    n=0
    grep -v '^[[:space:]]*\(#\|$\)' "$home/.ssh/authorized_keys" | while IFS= read -r key; do
      n=$((n + 1))
      name=admin
      [ "$n" -gt 1 ] && name="admin-$n"
      if printf '%s\n' "$key" | "$BIN" ssh-keys:add "$name" >/dev/null 2>&1; then
        info "added as $name"
      fi
    done
  fi
fi

ip=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
ip=${ip:-your-server}
printf '\n=====> Jokku is installed\n'
info "Deploy:        git remote add jokku jokku@$ip:myapp && git push jokku main"
info "Run commands:  ssh jokku@$ip apps:list   (or sudo jokku apps:list on this server)"
info "Add a key:     cat key.pub | ssh jokku@$ip ssh-keys:add <name>"
info "Update later:  sudo jokku update"
