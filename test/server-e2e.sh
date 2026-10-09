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
for svc in jokku jokku-proxy jokku-buildkitd jokku-dns; do systemctl is-active "$svc" || fail "$svc is not running"; done
sudo jokku ssh-keys:list | grep -q 'NAME="admin"' || fail "installer did not import the key"

step "commands over ssh"
jssh apps:create hello
jssh checks:set hello wait-to-retire 3
jssh apps:list | grep -qx hello || fail "apps:list"
[ "$(jssh letsencrypt:report hello --letsencrypt-computed-enabled)" = false ] || fail "a new app has Let's Encrypt on"
if jssh daemon 2>/dev/null; then fail "server-only command allowed over ssh"; fi

step "deploy with git push"
app=$(mktemp -d)
cd "$app"
git init -q -b main
# The first EXPOSE stands in for one inherited from a base image (nginx's 80):
# the app's own, newer EXPOSE is $PORT.
cat >Dockerfile <<'EOF'
FROM public.ecr.aws/docker/library/busybox:1.36
EXPOSE 80
RUN mkdir /www
EXPOSE 3000
EOF
cat >Procfile <<'EOF'
web: echo "hello ${GREETING:-nobody} from $(hostname)" > /www/index.html && echo "listening on port $PORT" >/dev/stdout && exec httpd -f -v -p "$PORT" -h /www
worker: while true; do echo "worker tick"; sleep 2; done
EOF
git add -A
git commit -qm init
out=$(git push "jokku@$host:hello" main 2>&1) || fail "git push failed: $out"
printf '%s\n' "$out"
grep -q "Application deployed" <<<"$out" || fail "no deploy summary"
grep -q '\$PORT is 3000, from EXPOSE 3000' <<<"$out" || fail "the app's EXPOSE is not \$PORT"
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

step "config:set restarts with zero downtime, even onto another port"
watch_start
jssh config:set hello GREETING=world PORT=4000
watch_stop "the restart"
get | grep -q "hello world" || fail "config change not applied"
jssh ps:report hello | grep -q "Status web.1: *healthy (v[0-9]*, [0-9.]*:4000," || fail "PORT config var not used"

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

step "router lines: one per request, naming the instance that answered"
for _ in 1 2 3; do get >/dev/null; done
eventually_router() {
  for _ in $(seq 1 20); do
    jssh logs hello -p router -n 20 | grep -qE 'app\[router\]: method=GET path="/" host=[^ ]+ status=200 duration=[0-9.]+ms bytes=[0-9]+ instance=web\.[12]@' && return 0
    sleep 1
  done
  return 1
}
eventually_router || fail "no router lines: $(jssh logs hello -p router -n 5)"
jssh logs hello -p router -n 20 | grep -v "app\[router\]" && fail "-p router shows other lines"
# Router lines are the newest, so look far enough back to find app output
# too; -p web shows only the app's own. (The output is kept, not piped:
# grep -q stops reading at its first match, which fails the pipe under
# pipefail if jssh is still writing.)
all=$(jssh logs hello -n 2000)
grep -q "app\[web" <<<"$all" || fail "app lines missing next to router lines"
jssh logs hello -p web -n 20 | grep -q "app\[router\]" && fail "-p web shows router lines"
sudo curl -fsS --unix-socket /run/jokku/jokku.sock "http://jokku/v1/requests?app=hello&tail=3" | grep -q '"type":"request"' || fail "request stream API"

step "apps find each other by name"
probe=$(mktemp -d)
(
  cd "$probe"
  git init -q -b main
  printf 'FROM public.ecr.aws/docker/library/busybox:1.36\nRUN mkdir /www\n' >Dockerfile
  # Looks up the hello app by name, fetches it through that name, and
  # resolves a public name through the node's DNS.
  printf 'web: (nslookup hello.internal; wget -qO- http://hello.internal:4000/; nslookup github.com) >/www/index.html 2>&1; exec httpd -f -p "$PORT" -h /www\n' >Procfile
  git add -A
  git commit -qm probe
  git push "jokku@$host:probe" main >/dev/null 2>&1
) || fail "deploying the probe failed"
pdomain=$(jssh domains:report probe --domains-app-vhosts)
answer=$(curl -fsS --max-time 5 -H "Host: $pdomain" http://127.0.0.1/) || fail "the probe is not reachable"
printf '%s\n' "$answer"
grep -q "hello world from hello-web-" <<<"$answer" || fail "the probe could not reach hello by name"
grep -qE "Name:[[:space:]]+github.com" <<<"$answer" || fail "the probe could not resolve a public name"
jssh apps:destroy probe --force

step "deploy an image from a registry"
out=$(jssh builder:image nginx public.ecr.aws/docker/library/nginx:alpine 2>&1) || fail "builder:image failed: $out"
printf '%s\n' "$out" | tail -n 5
ndomain=$(jssh domains:report nginx --domains-app-vhosts)
curl -fsS --max-time 5 -H "Host: $ndomain" http://127.0.0.1/ | grep -q "Welcome to nginx" || fail "the nginx image does not serve"
jssh releases nginx | grep -q "Deploy public.ecr.aws/docker/library/nginx:alpine" || fail "the release does not name the image"
[ "$(jssh builder:report nginx --builder-type)" = image ] || fail "builder:report does not say nginx is an image app"
jssh ps:rebuild nginx >/dev/null || fail "ps:rebuild of an image deploy failed"
if out=$(git push "jokku@$host:nginx" main 2>&1); then fail "a push deployed over an image app: $out"; fi
grep -q "nginx runs the image" <<<"$out" || fail "the push to an image app failed for another reason: $out"
jssh apps:destroy nginx --force

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

step "cluster commands on a single server"
sudo jokku nodes:list | grep -q "control" || fail "nodes:list does not show the control node"
sudo jokku events | grep -q "deployed v" || fail "no deploy events"
sudo jokku cluster:join-command | grep -q -- "--join .* --token JOKKU1\." || fail "cluster:join-command"
sudo jokku cluster:report | grep -q "Cluster nodes: *1 (1 ready)" || fail "cluster:report"
sudo grep -q '"module": "jokku"' /var/lib/jokku/proxy/config.json || fail "the proxy does not use the shared certificate store"
if out=$(sudo jokku top 2>&1 </dev/null); then fail "top without a terminal should fail: $out"; fi
grep -q "needs a terminal" <<<"$out" || fail "top: $out"

step "push to a new app creates and deploys it"
git push "jokku@$host:fresh" main >/dev/null 2>&1 || fail "deploying a new app failed"
jssh apps:list | grep -qx fresh || fail "push did not create the app"

step "a volume keeps data across restarts and deploys"
keep=$(mktemp -d)
cd "$keep"
git init -q -b main
# A non-root app with files at the mount path: they seed the new volume,
# which the app can then write to.
cat >Dockerfile <<'EOF'
FROM public.ecr.aws/docker/library/busybox:1.36
RUN adduser -D app && mkdir /data && echo seeded >/data/seed && chown -R app /data
USER app
EOF
# It also reopens its output by name, as nginx's error_log /dev/stderr does.
printf 'web: echo "stderr reopened as $(id -un)" >/dev/stderr; date >>/data/boots && exec httpd -f -p "$PORT" -h /data\n' >Procfile
git add -A
git commit -qm init
jssh apps:create keep
jssh storage:mount keep data:/data --size 1g | grep -q "Mounted volume data at /data in web" || fail "storage:mount"
git push "jokku@$host:keep" main >/dev/null 2>&1 || fail "deploying an app with a volume failed"
kdomain=$(jssh domains:report keep --domains-app-vhosts)
kget() { curl -fsS --max-time 5 -H "Host: $kdomain" "http://127.0.0.1/$1"; }
[ "$(kget seed)" = seeded ] || fail "the image's /data did not seed the volume: $(kget seed)"
for _ in $(seq 1 10); do
  jssh logs keep -p web -n 50 | grep -q "app\[web.1\]: stderr reopened as app" && break
  sleep 1
done
jssh logs keep -p web -n 50 | grep -q "app\[web.1\]: stderr reopened as app" || fail "a non-root app could not write to /dev/stderr: $(jssh logs keep -p web -n 50)"
[ "$(kget boots | wc -l)" -eq 1 ] || fail "expected one boot recorded: $(kget boots)"
jssh ps:restart keep >/dev/null
[ "$(kget boots | wc -l)" -eq 2 ] || fail "the volume lost data on restart: $(kget boots)"
# A deploy that changes the image restarts web (an unchanged one leaves it
# running), stopping the old instance before the new one takes the disk.
echo 'ENV VERSION=2' >>Dockerfile
git commit -qam v2
out=$(git push "jokku@$host:keep" main 2>&1) || fail "redeploying failed: $out"
grep -q "Stopping web.1 first" <<<"$out" || fail "the deploy did not stop the old instance first: $out"
[ "$(kget boots | wc -l)" -eq 3 ] || fail "the volume lost data on deploy: $(kget boots)"
git commit -q --allow-empty -m again
out=$(git push "jokku@$host:keep" main 2>&1) || fail "redeploying failed: $out"
grep -q "Unchanged, left running: web" <<<"$out" || fail "a deploy that changes nothing restarted web: $out"
[ "$(kget boots | wc -l)" -eq 3 ] || fail "a deploy that changes nothing rebooted web: $(kget boots)"
jssh storage:list keep | grep -E "^data +local +1g .* ready +web:/data" || fail "storage:list: $(jssh storage:list keep)"
if jssh ps:scale keep web=2 2>/dev/null; then fail "scaled a process with a local volume past one"; fi
sudo sh -c 'ls /var/lib/jokku/volumes/*.ext4' >/dev/null || fail "no volume disk on the server"
disk=$(jssh storage:report keep --storage-data-disk)
sudo test -f "${disk#*:}" || fail "storage:report names no disk: $disk"

step "jokku enter runs commands inside the instance"
[ "$(jssh enter keep web cat /data/seed)" = seeded ] || fail "enter could not read the volume"
[ "$(jssh enter keep web id -u)" != 0 ] || fail "enter ran as root, not the image's user"
[ "$(jssh enter keep --root web id -u)" = 0 ] || fail "enter --root did not run as root"
set +e
jssh "enter keep web sh -c 'exit 7'"
status=$?
set -e
[ "$status" -eq 7 ] || fail "enter returned $status for a command that exited 7"

step "storage:export and storage:import copy a volume's files"
backup=$(mktemp -d)
jssh storage:export keep data >"$backup/data.tar.gz" || fail "storage:export failed"
tar -tzf "$backup/data.tar.gz" | grep -qx "boots" || fail "the export lacks boots: $(tar -tzf "$backup/data.tar.gz")"
mkdir "$backup/files"
tar -xzf "$backup/data.tar.gz" -C "$backup/files"
[ "$(cat "$backup/files/seed")" = seeded ] || fail "the exported seed is wrong"
echo restored-marker >"$backup/files/boots"
tar -czf "$backup/new.tar.gz" -C "$backup/files" .
jssh storage:import keep data --clear <"$backup/new.tar.gz" || fail "storage:import failed"
# The app restarted in place, appending a boot to the restored file.
eventually_restored() {
  for _ in $(seq 1 30); do
    b=$(kget boots 2>/dev/null) && [ "$(head -n 1 <<<"$b")" = restored-marker ] && [ "$(wc -l <<<"$b")" -eq 2 ] && return 0
    sleep 1
  done
  return 1
}
eventually_restored || fail "the import was not restored, or the app did not restart: $(kget boots)"

step "backups go to an S3 bucket and restore from it"
if [ -z "${VERSITYGW:-}" ]; then
  echo "skipped: set VERSITYGW to a versitygw binary to run an S3 server for this step"
else
  s3=$(mktemp -d)
  mkdir "$s3/jokku" # a bucket
  ROOT_ACCESS_KEY=jokkuci ROOT_SECRET_KEY=ci-secret-key "$VERSITYGW" --port 127.0.0.1:9000 posix "$s3" >"$s3.log" 2>&1 &
  s3pid=$!
  for _ in $(seq 1 40); do curl -s -o /dev/null http://127.0.0.1:9000/ && break; sleep 0.25; done
  out=$(printf 'ci-secret-key\n' | jssh backups:destination-add ci --endpoint http://127.0.0.1:9000 --bucket jokku --access-key-id jokkuci --force 2>&1) ||
    fail "backups:destination-add failed: $out $(cat "$s3.log")"
  grep -q "jbk1_" <<<"$out" || fail "the backup key was not shown: $out"
  jssh backups:set keep data ci
  jssh "enter keep web sh -c 'echo backed-up >/data/boots'"
  out=$(jssh backups:run keep data 2>&1) || fail "backups:run failed: $out"
  printf '%s\n' "$out"
  grep -q "Writes were paused for" <<<"$out" || fail "the backup did not pause the app's writes: $out"
  compgen -G "$s3/jokku/jokku/keep/data/backups/*.backup" >/dev/null || fail "no backup in the bucket: $(find "$s3" | head -20)"
  jssh "enter keep web sh -c 'echo after-the-backup >/data/boots'"
  out=$(jssh backups:restore keep data --force 2>&1) || fail "backups:restore failed: $out"
  printf '%s\n' "$out"
  # The app restarted with the restored disk, appending a boot.
  eventually_backed_up() {
    for _ in $(seq 1 30); do
      b=$(kget boots 2>/dev/null) && [ "$(head -n 1 <<<"$b")" = backed-up ] && [ "$(wc -l <<<"$b")" -eq 2 ] && return 0
      sleep 1
    done
    return 1
  }
  eventually_backed_up || fail "the backup was not restored, or the app did not restart: $(kget boots)"
  # Two backups: the one restored, and the one of the data it replaced.
  [ "$(jssh backups:list keep data | grep -c '^20')" -eq 2 ] || fail "backups:list: $(jssh backups:list keep data)"
  [[ "$(jssh backups:report keep --backups-last)" == 20* ]] || fail "backups:report: $(jssh backups:report keep)"
  kill "$s3pid"
fi
jssh apps:destroy keep --force
for _ in $(seq 1 30); do
  sudo sh -c 'ls /var/lib/jokku/volumes/*.ext4' >/dev/null 2>&1 || break
  sleep 1
done
if sudo sh -c 'ls /var/lib/jokku/volumes/*.ext4' 2>/dev/null; then fail "apps:destroy left the volume's disk"; fi
cd "$GITHUB_WORKSPACE"

step "a compose file deploys as one app, a process type per service"
stack=$(mktemp -d)
cd "$stack"
git init -q -b main
printf 'FROM public.ecr.aws/docker/library/busybox:1.36\nRUN mkdir /www\n' >Dockerfile
cat >compose.yaml <<'EOF'
services:
  web:
    build: .
    ports: ["8080:3000"]
    environment:
      GREETING: hello ${WHO}
    command: ["sh", "-c", "echo \"$$GREETING from compose\" >/www/index.html; ping -c 1 -W 2 db >>/www/index.html 2>&1; exec httpd -f -p 3000 -h /www"]
    depends_on: [db]
  db:
    image: public.ecr.aws/docker/library/redis:7-alpine
    volumes: ["dbdata:/data"]
volumes:
  dbdata:
EOF
git add -A
git commit -qm stack
jssh builder:compose stack
jssh config:set --no-restart stack WHO=world
out=$(git push "jokku@$host:stack" main 2>&1) || fail "deploying the compose file failed: $out"
printf '%s\n' "$out" | grep -E "Deploying compose.yaml|Pulling|Building|Created volume|Starting" || true
grep -q "Deploying compose.yaml: web, db" <<<"$out" || fail "the compose file was not deployed: $out"
sdomain=$(jssh domains:report stack --domains-app-vhosts)
sget() { curl -fsS --max-time 5 -H "Host: $sdomain" http://127.0.0.1/; }
page=$(sget) || fail "the compose app's web service is not reachable"
printf '%s\n' "$page"
grep -q "hello world from compose" <<<"$page" || fail "\${WHO} was not filled in from the config var: $page"
# ping resolves "db" the way an app's database URL would: through the
# search list to db.stack.internal.
grep -qE "PING db \(10\.[0-9.]+\)" <<<"$page" || fail "web could not resolve db: $page"
jssh storage:list stack | grep -E "^dbdata .* db:/data" || fail "the db volume was not created and mounted: $(jssh storage:list stack)"
dbaddr=$(jssh ps:report stack --status-db.1 | grep -oE "10\.[0-9.]+:6379")
[ -n "$dbaddr" ] || fail "db is not running: $(jssh ps:report stack)"
out=$(jssh config:set stack WHO=jokku 2>&1) || fail "config:set failed: $out"
grep -q "Unchanged, left running: db" <<<"$out" || fail "the config change restarted db: $out"
sget | grep -q "hello jokku from compose" || fail "web did not pick up the config change"
[ "$(jssh ps:report stack --status-db.1 | grep -oE "10\.[0-9.]+:6379")" = "$dbaddr" ] || fail "db was replaced by a config change it does not use"
jssh apps:destroy stack --force
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
