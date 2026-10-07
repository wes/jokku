#!/bin/sh
# Updates Jokku on a server to the latest release:
#
#   curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
#
# Backs up Jokku's database, swaps the binary and runs "jokku setup", which
# applies whatever server changes the new release needs and restarts jokku.
# If the new version fails to start, the previous binary and database are put
# back.
#
# Environment:
#   JOKKU_VERSION=v0.0.2   update to a specific release (default: latest)
#   JOKKU_FORCE=1          reinstall even if already on that version
#   JOKKU_DOWNLOAD_URL=... fetch binaries from elsewhere; requires JOKKU_VERSION
set -eu

REPO=wes/jokku
BIN=/usr/local/bin/jokku
DATA_DIR=/var/lib/jokku
BACKUPS=$DATA_DIR/backups

say()  { printf -- '-----> %s\n' "$*"; }
info() { printf '       %s\n' "$*"; }
warn() { printf ' !     %s\n' "$*" >&2; }
die()  { warn "$*"; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Run as root: curl -fsSL https://raw.githubusercontent.com/$REPO/main/update.sh | sudo sh"
[ -x "$BIN" ] || die "Jokku is not installed here. Install it with: curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "Unsupported CPU architecture: $(uname -m)" ;;
esac

current=$("$BIN" version 2>/dev/null | head -n 1 | cut -d' ' -f3)
target=${JOKKU_VERSION:-}
if [ -z "$target" ]; then
  [ -z "${JOKKU_DOWNLOAD_URL:-}" ] || die "Set JOKKU_VERSION when using JOKKU_DOWNLOAD_URL"
  # /releases/latest redirects to /releases/tag/<version>.
  target=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's|.*/tag/||')
  case "$target" in v*) ;; *) die "Could not find the latest Jokku release" ;; esac
fi
if [ "$current" = "$target" ] && [ "${JOKKU_FORCE:-0}" != 1 ]; then
  say "Jokku is up to date ($current)"
  exit 0
fi
say "Updating jokku from $current to $target"

URL=${JOKKU_DOWNLOAD_URL:-https://github.com/$REPO/releases/download/$target}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$URL/jokku-linux-$ARCH" -o "$tmp/jokku-linux-$ARCH" || die "Download failed: $URL/jokku-linux-$ARCH"
curl -fsSL "$URL/checksums.txt" -o "$tmp/checksums.txt" || die "Download failed: $URL/checksums.txt"
(cd "$tmp" && grep " jokku-linux-$ARCH\$" checksums.txt | sha256sum -c - >/dev/null) || die "Checksum mismatch for jokku-linux-$ARCH"

# Back up the database with jokku stopped, so the copy is consistent. New
# releases migrate the schema when they start; the backup is the way back.
backup="$BACKUPS/$(date -u +%Y%m%dT%H%M%SZ)-$current"
say "Backing up the database to $backup"
systemctl stop jokku
mkdir -p "$backup"
for f in "$DATA_DIR"/jokku.db "$DATA_DIR"/jokku.db-wal "$DATA_DIR"/jokku.db-shm; do
  [ ! -e "$f" ] || cp -p "$f" "$backup/"
done
cp -p "$BIN" "$backup/jokku"
# Keep the five most recent backups.
ls -1d "$BACKUPS"/*/ 2>/dev/null | sort -r | tail -n +6 | while IFS= read -r old; do rm -rf "$old"; done

rollback() {
  warn "The update failed. Restoring $current."
  systemctl stop jokku 2>/dev/null || true
  install -m 0755 "$backup/jokku" "$BIN"
  rm -f "$DATA_DIR"/jokku.db-wal "$DATA_DIR"/jokku.db-shm
  for f in "$backup"/jokku.db*; do
    [ ! -e "$f" ] || cp -p "$f" "$DATA_DIR/"
  done
  # Prefer the old version's own setup; v0.0.1 predates it.
  "$BIN" setup >/dev/null 2>&1 || systemctl restart jokku
  die "Jokku is back on $current. Logs from the failed start: journalctl -u jokku -n 50"
}

install -m 0755 "$tmp/jokku-linux-$ARCH" "$BIN"
"$BIN" setup || rollback

printf '\n=====> Jokku is updated to %s\n' "$target"
info "Release notes: https://github.com/$REPO/releases/tag/$target"
