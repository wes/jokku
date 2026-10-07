#!/bin/sh
# Installs (or upgrades) Jokku on a Linux server:
#
#   curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
#
# Environment:
#   JOKKU_VERSION=v0.0.1   install a specific release (default: latest)
#   JOKKU_IMPORT_KEYS=0    don't copy your SSH keys into jokku on first install
#   JOKKU_DOWNLOAD_URL=... fetch binaries from elsewhere (a mirror, file:///dist)
set -eu

REPO=wes/jokku
VERSION=${JOKKU_VERSION:-latest}
BIN=/usr/local/bin/jokku
DATA_DIR=/var/lib/jokku
SOCKET=/run/jokku/jokku.sock

say()  { printf -- '-----> %s\n' "$*"; }
info() { printf '       %s\n' "$*"; }
warn() { printf ' !     %s\n' "$*" >&2; }
die()  { warn "$*"; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Run as root: curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh"
[ "$(uname -s)" = Linux ] || die "Jokku runs on Linux servers (this is $(uname -s))"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "Unsupported CPU architecture: $(uname -m)" ;;
esac

# Packages: git for pushes, sshd for git push and remote commands.
missing=""
command -v git >/dev/null 2>&1 || missing="$missing git"
command -v curl >/dev/null 2>&1 || missing="$missing curl"
[ -x /usr/sbin/sshd ] || command -v sshd >/dev/null 2>&1 || missing="$missing openssh-server"
if [ -n "$missing" ]; then
  say "Installing$missing"
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null
    # shellcheck disable=SC2086
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq $missing >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    # shellcheck disable=SC2086
    dnf install -y -q $missing
  else
    die "Install$missing and run this again"
  fi
fi

# The binary, verified against the release's checksums.
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
info "$("$BIN" version | head -n 1)"

# The jokku user: sshd runs git pushes and remote commands as it, and every key
# in its authorized_keys is pinned to "jokku ssh-command".
if ! id jokku >/dev/null 2>&1; then
  say "Creating the jokku user"
  useradd --system --user-group --create-home --home-dir /home/jokku --shell /bin/sh jokku
fi
# No password login, but not "locked" either: sshd refuses key logins for
# locked accounts on systems without PAM.
usermod -p '*' jokku
install -d -m 0700 -o jokku -g jokku /home/jokku/.ssh
install -d -m 0755 "$DATA_DIR"
install -d -m 0755 -o jokku -g jokku "$DATA_DIR/git"

if [ ! -d /run/systemd/system ]; then
  warn "systemd is not running, so the jokku service was not installed."
  warn "Start the server yourself with: jokku daemon"
  exit 0
fi

cat >/etc/systemd/system/jokku.service <<EOF
[Unit]
Description=Jokku
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN daemon
Restart=always
RestartSec=2
RuntimeDirectory=jokku

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
if systemctl is-active --quiet jokku; then
  say "Restarting the jokku service"
  systemctl restart jokku
else
  say "Starting the jokku service"
  systemctl enable --now jokku >/dev/null 2>&1
fi

i=0
until [ -S "$SOCKET" ] && "$BIN" apps:list >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -le 30 ] || die "The jokku service did not start. Check: journalctl -u jokku"
  sleep 1
done

# On first install, give the keys that can already log in here (yours, via
# sudo, or root's) access to jokku, so git push works right away.
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
printf '\n'
printf '=====> Jokku is installed\n'
info "Add a key:     cat ~/.ssh/id_ed25519.pub | ssh root@$ip jokku ssh-keys:add <name>"
info "Deploy:        git remote add jokku jokku@$ip:myapp && git push jokku main"
info "Run commands:  ssh jokku@$ip apps:list   (or jokku apps:list on this server)"
[ -e /dev/kvm ] || warn "/dev/kvm is missing: microVMs need a machine with KVM (bare metal or nested virtualization)."
warn "Early development: pushes are received and checked, but building and running apps arrives in milestone 1."
