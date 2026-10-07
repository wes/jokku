#!/bin/bash
# End-to-end check of install.sh and update.sh on a fresh Ubuntu machine with
# systemd and sshd (a GitHub Actions runner): install, use jokku over real ssh
# the way a person would, update, and roll back a broken release.
#
#   test/server-e2e.sh <dist-dir>
#
# dist-dir holds three builds, each with jokku-linux-amd64 and checksums.txt:
# old/ (version ci-old), new/ (ci-new) and broken/ (a binary that fails).
set -euo pipefail
dist=$(cd "$1" && pwd)
host=127.0.0.1
key=$HOME/.ssh/jokku_e2e

step() { printf '\n=== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
server_version() { sudo jokku version | sed -n 's/^server version \([^ ]*\).*/\1/p'; }

step "sshd and a key that can already log in (the installer should import it)"
sudo systemctl start ssh 2>/dev/null || sudo systemctl start sshd
mkdir -p ~/.ssh && chmod 700 ~/.ssh
ssh-keygen -q -t ed25519 -N "" -C e2e -f "$key"
cat "$key.pub" >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
export GIT_SSH_COMMAND="ssh -i $key -o StrictHostKeyChecking=accept-new -o BatchMode=yes"
jssh() { $GIT_SSH_COMMAND "jokku@$host" "$@"; }

step "install"
sudo JOKKU_DOWNLOAD_URL="file://$dist/old" sh ./install.sh
systemctl is-active jokku
[ "$(server_version)" = ci-old ] || fail "expected ci-old to be running"
sudo jokku ssh-keys:list | grep -q 'NAME="admin"' || fail "installer did not import the key"

step "commands over ssh"
jssh apps:create hello
# ssh joins arguments with spaces, so values with spaces are quoted for the
# server side, as with Dokku.
jssh config:set hello "'GREETING=hello world'" PORT=8080
[ "$(jssh config:get hello GREETING)" = "hello world" ] || fail "config round trip"
jssh apps:list | grep -qx hello || fail "apps:list"
if jssh daemon 2>/dev/null; then fail "server-only command allowed over ssh"; fi

step "git push"
app=$(mktemp -d)
cd "$app"
git init -q -b main
printf 'FROM nginx:alpine\n' >Dockerfile
printf 'web: nginx -g "daemon off;"\nworker: sleep infinity\n' >Procfile
git add -A
git -c user.email=e2e@example.com -c user.name=e2e commit -qm init
if out=$(git push "jokku@$host:hello" main 2>&1); then
  fail "push should be rejected until milestone 1 builds apps: $out"
fi
printf '%s\n' "$out"
grep -q "Found Procfile: web, worker" <<<"$out" || fail "deploy output missing from push"
grep -q "milestone 1" <<<"$out" || fail "expected the milestone 1 message"

step "push to a new app creates it"
git push "jokku@$host:fresh" main >/dev/null 2>&1 || true
jssh apps:list | grep -qx fresh || fail "push did not create the app"
cd "$GITHUB_WORKSPACE"

step "running install.sh again changes nothing"
out=$(sudo JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./install.sh)
printf '%s\n' "$out"
grep -q "already installed" <<<"$out" || fail "reinstall should point to update.sh"
[ "$(server_version)" = ci-old ] || fail "install.sh must not upgrade"

step "update"
keys_before=$(sudo jokku ssh-keys:list | wc -l)
sudo JOKKU_VERSION=ci-new JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./update.sh
[ "$(server_version)" = ci-new ] || fail "expected ci-new after update"
[ "$(jssh config:get hello GREETING)" = "hello world" ] || fail "state lost on update"
[ "$(sudo jokku ssh-keys:list | wc -l)" -eq "$keys_before" ] || fail "keys changed on update"
sudo ls -d /var/lib/jokku/backups/*-ci-old >/dev/null || fail "no backup taken"

step "update to the same version is a no-op"
out=$(sudo JOKKU_VERSION=ci-new JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./update.sh)
grep -q "up to date" <<<"$out" || fail "expected 'up to date': $out"

step "a release that fails to start is rolled back"
if sudo JOKKU_VERSION=ci-broken JOKKU_DOWNLOAD_URL="file://$dist/broken" sh ./update.sh; then
  fail "updating to a broken release should fail"
fi
systemctl is-active jokku
[ "$(server_version)" = ci-new ] || fail "expected the rollback to restore ci-new"
[ "$(jssh config:get hello GREETING)" = "hello world" ] || fail "state lost on rollback"

printf '\nAll server checks passed.\n'
