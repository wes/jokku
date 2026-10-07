#!/bin/bash
# End-to-end check on a fresh Ubuntu machine with systemd, sshd and KVM (a
# GitHub Actions runner): install, deploy an app into Firecracker microVMs
# over real ssh and git push, change config, scale, update jokku without
# dropping requests, and roll back a broken release.
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
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  sudo journalctl -u jokku -u jokku-proxy -u jokku-buildkitd -n 60 --no-pager >&2 || true
  exit 1
}
server_version() { sudo jokku version | sed -n 's/^server version \([^ ]*\).*/\1/p'; }

step "sshd and a key that can already log in (the installer should import it)"
ls -l /dev/kvm
sudo systemctl start ssh 2>/dev/null || sudo systemctl start sshd
mkdir -p ~/.ssh && chmod 700 ~/.ssh
ssh-keygen -q -t ed25519 -N "" -C e2e -f "$key"
cat "$key.pub" >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
export GIT_SSH_COMMAND="ssh -i $key -o StrictHostKeyChecking=accept-new -o BatchMode=yes"
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
jssh() { $GIT_SSH_COMMAND "jokku@$host" "$@"; }

step "install"
sudo JOKKU_DOWNLOAD_URL="file://$dist/old" sh ./install.sh
[ "$(server_version)" = ci-old ] || fail "expected ci-old to be running"
for svc in jokku jokku-proxy jokku-buildkitd; do systemctl is-active "$svc" || fail "$svc is not running"; done
sudo jokku ssh-keys:list | grep -q 'NAME="admin"' || fail "installer did not import the key"

step "commands over ssh"
jssh apps:create hello
jssh checks:set hello wait-to-retire 3
jssh apps:list | grep -qx hello || fail "apps:list"
if jssh daemon 2>/dev/null; then fail "server-only command allowed over ssh"; fi

step "deploy with git push"
app=$(mktemp -d)
cd "$app"
git init -q -b main
cat >Dockerfile <<'EOF'
FROM public.ecr.aws/docker/library/busybox:1.36
RUN mkdir /www
EOF
cat >Procfile <<'EOF'
web: echo "hello ${GREETING:-nobody} from $(hostname)" > /www/index.html && echo "listening on port $PORT" && exec httpd -f -v -p "$PORT" -h /www
worker: while true; do echo "worker tick"; sleep 2; done
EOF
git add -A
git commit -qm init
out=$(git push "jokku@$host:hello" main 2>&1) || fail "git push failed: $out"
printf '%s\n' "$out"
grep -q "Application deployed" <<<"$out" || fail "no deploy summary"
domain=$(jssh domains:report hello --domains-app-vhosts)
[ -n "$domain" ] || fail "the app got no default domain"
get() { curl -fsS --max-time 5 -H "Host: $domain" "http://127.0.0.1/"; }
body=$(get) || fail "the app is not reachable through the proxy"
grep -q "hello nobody from hello-web-1" <<<"$body" || fail "unexpected body: $body"

# watch_start/watch_stop count failed requests while something happens.
watch_start() {
  rm -f /tmp/failures
  (while :; do get >/dev/null 2>&1 || echo x >>/tmp/failures; sleep 0.2; done) &
  watcher=$!
}
watch_stop() {
  kill "$watcher"
  wait "$watcher" 2>/dev/null || true
  failures=0
  [ ! -f /tmp/failures ] || failures=$(wc -l </tmp/failures)
  [ "$failures" -eq 0 ] || fail "$failures requests failed during $1"
}

step "config:set restarts with zero downtime"
watch_start
jssh config:set hello GREETING=world
watch_stop "the restart"
get | grep -q "hello world" || fail "config change not applied"

step "scale out"
jssh ps:scale hello web=2 worker=1
report=$(jssh ps:report hello)
printf '%s\n' "$report"
for p in web.1 web.2 worker.1; do grep -q "Status $p: *healthy" <<<"$report" || fail "$p is not healthy"; done
seen=""
for _ in $(seq 1 10); do seen="$seen $(get)"; done
grep -q "hello-web-1" <<<"$seen" && grep -q "hello-web-2" <<<"$seen" || fail "requests were not spread over both web instances"

step "logs"
sleep 3
logs=$(jssh logs hello -n 200)
grep -q "app\[web.1\]: listening on port" <<<"$logs" || fail "web output missing from logs: $logs"
grep -q "app\[worker.1\]: worker tick" <<<"$logs" || fail "worker output missing from logs"
[ "$(jssh logs hello -p worker -q -n 1)" = "worker tick" ] || fail "logs -p worker -q"

step "a failing deploy keeps the old release serving"
printf 'web: echo "crashing" && exit 3\n' >Procfile
git commit -qam broken
if out=$(git push "jokku@$host:hello" main 2>&1); then fail "a crashing app deployed: $out"; fi
printf '%s\n' "$out"
grep -q "crashing" <<<"$out" || fail "the failing instance's output was not shown"
get | grep -q "hello world" || fail "the old release stopped serving"
git reset -q --hard HEAD~1

step "ps:stop and ps:start"
jssh ps:stop hello
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $domain" http://127.0.0.1/)
[ "$code" = 502 ] || fail "stopped app answered $code, want 502"
jssh ps:start hello
get | grep -q "hello world" || fail "app did not come back after ps:start"

step "push to a new app creates and deploys it"
git push "jokku@$host:fresh" main >/dev/null 2>&1 || fail "deploying a new app failed"
jssh apps:list | grep -qx fresh || fail "push did not create the app"
cd "$GITHUB_WORKSPACE"

step "running install.sh again changes nothing"
out=$(sudo JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./install.sh)
grep -q "already installed" <<<"$out" || fail "reinstall should point to update.sh"
[ "$(server_version)" = ci-old ] || fail "install.sh must not upgrade"

step "update while the app serves traffic"
keys_before=$(sudo jokku ssh-keys:list | wc -l)
watch_start
sudo JOKKU_VERSION=ci-new JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./update.sh
watch_stop "the update"
[ "$(server_version)" = ci-new ] || fail "expected ci-new after update"
[ "$(jssh config:get hello GREETING)" = "world" ] || fail "state lost on update"
[ "$(sudo jokku ssh-keys:list | wc -l)" -eq "$keys_before" ] || fail "keys changed on update"
sudo ls -d /var/lib/jokku/backups/*-ci-old >/dev/null || fail "no backup taken"

step "jokku update asks first"
out=$(echo n | sudo jokku update --version ci-newer 2>&1 || true)
printf '%s\n' "$out"
grep -q "Jokku ci-newer is available" <<<"$out" || fail "jokku update did not report the new version"
grep -q "needs confirmation" <<<"$out" || fail "jokku update without a terminal should require --yes"

step "update to the same version is a no-op"
out=$(sudo JOKKU_VERSION=ci-new JOKKU_DOWNLOAD_URL="file://$dist/new" sh ./update.sh)
grep -q "up to date" <<<"$out" || fail "expected 'up to date': $out"

step "a release that fails to start is rolled back"
if sudo JOKKU_VERSION=ci-broken JOKKU_DOWNLOAD_URL="file://$dist/broken" sh ./update.sh; then
  fail "updating to a broken release should fail"
fi
[ "$(server_version)" = ci-new ] || fail "expected the rollback to restore ci-new"
get | grep -q "hello world" || fail "the app stopped serving after the rollback"

step "apps:destroy removes the microVMs"
systemctl list-units --plain --no-legend --full 'jokku-vm-*' | grep -q "Jokku fresh " || fail "fresh has no running VM"
jssh apps:destroy fresh --force
for _ in $(seq 1 30); do
  systemctl list-units --plain --no-legend --full 'jokku-vm-*' | grep -q "Jokku fresh " || break
  sleep 1
done
if systemctl list-units --plain --no-legend --full 'jokku-vm-*' | grep -q "Jokku fresh "; then fail "fresh's VM is still running"; fi

printf '\nAll server checks passed.\n'
