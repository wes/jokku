#!/bin/bash
# Two-node cluster check on a GitHub Actions runner with KVM: the runner is
# the control node; a QEMU VM running Ubuntu joins it as a worker with
# install.sh --join. Checks the WireGuard mesh, that the worker's proxy
# serves an app running on the control node (traffic crossing the mesh),
# node-down detection and removal.
#
#   test/cluster-e2e.sh <dist-dir>   (jokku-linux-amd64 + checksums.txt)
set -euo pipefail
dist=$(cd "$1" && pwd)
work=$(mktemp -d)
bridge=jkt-br tap=jkt-tap
host_ip=192.168.77.1 worker_ip=192.168.77.2
key=$work/id_ed25519

step() { printf '\n=== %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  sudo jokku nodes:list >&2 || true
  sudo journalctl -u jokku -n 40 --no-pager >&2 || true
  wssh "sudo journalctl -u jokku -n 40 --no-pager" >&2 || true
  tail -n 30 "$work/console.log" >&2 2>/dev/null || true
  exit 1
}
wssh() { ssh -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR "tester@$worker_ip" "$@"; }
eventually() { # eventually <seconds> <description> <command...>
  local deadline=$((SECONDS + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "timed out: $what"
    sleep 2
  done
}

step "install the control node"
sudo JOKKU_DOWNLOAD_URL="file://$dist" JOKKU_IMPORT_KEYS=0 sh ./install.sh
for svc in jokku jokku-proxy jokku-buildkitd; do systemctl is-active "$svc"; done

step "a network for the worker VM"
sudo ip link add "$bridge" type bridge
sudo ip addr add "$host_ip/24" dev "$bridge"
sudo ip link set "$bridge" up
sudo ip tuntap add "$tap" mode tap
sudo ip link set "$tap" master "$bridge" up
sudo iptables -t nat -A POSTROUTING -s 192.168.77.0/24 ! -d 192.168.77.0/24 -j MASQUERADE
sudo iptables -I FORWARD 1 -i "$bridge" -j ACCEPT
sudo iptables -I FORWARD 1 -o "$bridge" -j ACCEPT

step "boot an Ubuntu VM"
curl -fsSL -o "$work/base.img" https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img
qemu-img create -q -f qcow2 -b "$work/base.img" -F qcow2 "$work/worker.qcow2" 12G
ssh-keygen -q -t ed25519 -N "" -f "$key"
cat >"$work/user-data" <<EOF
#cloud-config
users:
  - name: tester
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: ["$(cat "$key.pub")"]
EOF
printf 'instance-id: worker1\nlocal-hostname: worker1\n' >"$work/meta-data"
cat >"$work/network-config" <<EOF
version: 2
ethernets:
  nic:
    match: {driver: virtio_net}
    addresses: [$worker_ip/24]
    routes: [{to: default, via: $host_ip}]
    nameservers: {addresses: [8.8.8.8, 1.1.1.1]}
EOF
cloud-localds --network-config="$work/network-config" "$work/seed.iso" "$work/user-data" "$work/meta-data"
sudo qemu-system-x86_64 -enable-kvm -cpu host -smp 2 -m 3072 -display none -daemonize \
  -pidfile "$work/qemu.pid" -serial "file:$work/console.log" \
  -drive "file=$work/worker.qcow2,if=virtio" -drive "file=$work/seed.iso,if=virtio,format=raw" \
  -netdev "tap,id=n0,ifname=$tap,script=no,downscript=no" -device virtio-net-pci,netdev=n0
eventually 300 "the VM boots and accepts ssh" wssh true
wssh "test -e /dev/kvm && echo 'worker has KVM (nested)' || echo 'worker has no KVM: it will route traffic but not run apps'"

step "serve jokku to the VM"
cp ./install.sh "$dist/install.sh"
(cd "$dist" && python3 -m http.server 8000 --bind "$host_ip" >/dev/null 2>&1) &
server=$!
trap 'kill $server 2>/dev/null; sudo kill "$(sudo cat "$work/qemu.pid")" 2>/dev/null || true' EXIT
eventually 30 "the file server answers" curl -fsS "http://$host_ip:8000/checksums.txt"

step "join the worker with install.sh --join"
cmd=$(sudo jokku cluster:join-command | tail -n 1)
printf '%s\n' "$cmd"
addr=$(sed -n 's/.*--join \([^ ]*\) .*/\1/p' <<<"$cmd")
token=$(sed -n 's/.*--token \([^ ]*\).*/\1/p' <<<"$cmd")
[ -n "$addr" ] && [ -n "$token" ] || fail "could not read the join command"
wssh "curl -fsSL http://$host_ip:8000/install.sh | sudo JOKKU_DOWNLOAD_URL=http://$host_ip:8000 sh -s -- --join $addr --token $token --name worker1"
eventually 60 "worker1 reports in" bash -c "sudo jokku nodes:list | grep -E '^worker1 +worker +ready'"
sudo jokku nodes:list

step "the WireGuard mesh is up"
eventually 60 "a WireGuard handshake with worker1" bash -c "sudo wg show jokku-wg latest-handshakes | awk '{exit !(\$2 > 0)}'"
eventually 30 "worker1's mesh address answers" ping -c 1 -W 2 10.210.2.1
wssh "ping -c 1 -W 2 10.210.1.1" || fail "the worker cannot reach the control node over the mesh"

step "deploy an app on the control node"
mkdir -p "$work/app" && cd "$work/app"
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q -b main
printf 'FROM public.ecr.aws/docker/library/busybox:1.36\nRUN mkdir /www\n' >Dockerfile
printf 'web: echo "served by $(hostname)" > /www/index.html && echo "listening on $PORT" && exec httpd -f -p "$PORT" -h /www\n' >Procfile
git add -A && git commit -qm init
sudo jokku apps:create hello
sudo git -c safe.directory='*' archive --format=tar HEAD >"$work/app.tar"
cd "$OLDPWD"
# Deploy through the API like git push would (no ssh keys needed here).
sudo curl -fsS --unix-socket /run/jokku/jokku.sock -H "Content-Type: application/x-tar" \
  --data-binary "@$work/app.tar" "http://jokku/v1/apps/hello/deploys?source=archive" | tee "$work/deploy.log" | tail -n 3
grep -q '"status":"succeeded"' "$work/deploy.log" || fail "deploy failed"
domain=$(sudo jokku domains:report hello --domains-app-vhosts)

step "the worker's proxy serves the app across the mesh"
sudo jokku ps:report hello
eventually 30 "worker1 routes to the app" curl -fsS --max-time 5 -H "Host: $domain" "http://$worker_ip/"
curl -fsS -H "Host: $domain" "http://$worker_ip/" | grep -q "served by hello-web-1" || fail "unexpected answer from the worker's proxy"

step "requests through the worker show up as router lines on the control node"
eventually 20 "a router line via worker1" bash -c "sudo jokku logs hello -p router -n 50 | grep -q 'via=worker1'"

step "logs and events cover the cluster"
eventually 20 "app output in jokku logs" bash -c "sudo jokku logs hello -n 50 | grep -q 'app\[web.1\]: listening on'"
sudo jokku events | grep -q "worker1 joined" || fail "no join event"

worker_runs_vms=false
if sudo jokku nodes:report worker1 --node-runs-microvms | grep -q '^yes'; then worker_runs_vms=true; fi
echo "worker runs microVMs: $worker_runs_vms"

if $worker_runs_vms; then
  step "scale out: one instance on each node"
  sudo jokku ps:scale hello web=2
  sudo jokku ps:report hello
  sudo jokku ps:report hello | grep -qE "Status web.[12]: +healthy \(v[0-9]+, 10\.210\.2\." || fail "no instance on worker1"
  wssh "ls /var/lib/jokku/artifacts/*.ext4" >/dev/null || fail "worker1 did not download the root filesystem"
  # The control node's proxy reaches the instance on worker1 over the mesh.
  seen=""
  for _ in $(seq 1 12); do seen="$seen $(curl -fsS -H "Host: $domain" http://127.0.0.1/)"; done
  grep -q hello-web-1 <<<"$seen" && grep -q hello-web-2 <<<"$seen" || fail "requests did not reach both nodes: $seen"
  # Logs come from both nodes.
  eventually 20 "logs from both nodes" bash -c "sudo jokku logs hello -n 50 | grep -q 'app\[web.2\]: listening on'"
fi

step "internal DNS names reach every node"
wssh "systemctl is-active jokku-dns" || fail "jokku-dns is not running on the worker"
eventually 30 "the worker knows hello's names" wssh "sudo grep -q web.hello.internal /var/lib/jokku/dns/zone.json"

step "the control node notices the worker going down and coming back"
wssh "sudo systemctl stop jokku"
eventually 60 "worker1 marked down" bash -c "sudo jokku nodes:list | grep -E '^worker1 +worker +down'"
if $worker_runs_vms; then
  wssh "systemctl list-units --plain --no-legend 'jokku-vm-*' | grep -q running" || fail "the worker's VM stopped with its jokku service"
fi
wssh "sudo systemctl start jokku"
eventually 60 "worker1 back" bash -c "sudo jokku nodes:list | grep -E '^worker1 +worker +ready'"
if $worker_runs_vms; then
  eventually 30 "the worker's instance is adopted, not restarted" bash -c \
    "sudo jokku ps:report hello | grep -E 'Status web.[12]: +healthy \(v[0-9]+, 10\.210\.2\..*, 0 restarts\)'"

  step "a volume moves between nodes with its data"
  ctl=$(sudo jokku nodes:list | awk '$2 == "control" {print $1}')
  mkdir -p "$work/keep" && cd "$work/keep"
  git init -q -b main
  printf 'FROM public.ecr.aws/docker/library/busybox:1.36\nRUN mkdir /data\n' >Dockerfile
  printf 'web: date >>/data/boots && exec httpd -f -p "$PORT" -h /data\n' >Procfile
  git add -A && git commit -qm init
  sudo git -c safe.directory='*' archive --format=tar HEAD >"$work/keep.tar"
  cd "$OLDPWD"
  sudo jokku apps:create keep
  sudo jokku storage:create keep data --size 1g
  sudo jokku storage:move keep data worker1 # never used yet: its disk is made on worker1
  sudo jokku storage:mount keep data:/data --no-restart
  sudo curl -fsS --unix-socket /run/jokku/jokku.sock -H "Content-Type: application/x-tar" \
    --data-binary "@$work/keep.tar" "http://jokku/v1/apps/keep/deploys?source=archive" | tee "$work/keep.log" | tail -n 3
  grep -q '"status":"succeeded"' "$work/keep.log" || fail "deploying the app with a volume failed"
  sudo jokku ps:report keep | grep -qE "Status web.1: +healthy \(v[0-9]+, 10\.210\.2\." || fail "the volume's instance is not on worker1"
  kdomain=$(sudo jokku domains:report keep --domains-app-vhosts)
  kboots() { curl -fsS --max-time 5 -H "Host: $kdomain" http://127.0.0.1/boots | wc -l; }
  eventually 30 "keep serves its volume" curl -fsS --max-time 5 -H "Host: $kdomain" http://127.0.0.1/boots
  [ "$(kboots)" -eq 1 ] || fail "expected one boot on worker1"
  sudo jokku storage:move keep data "$ctl" || fail "storage:move to the control node"
  eventually 60 "keep healthy on the control node" bash -c \
    "sudo jokku ps:report keep | grep -qE 'Status web.1: +healthy \(v[0-9]+, 10\.210\.1\.'"
  eventually 30 "keep serves after the move" curl -fsS --max-time 5 -H "Host: $kdomain" http://127.0.0.1/boots
  [ "$(kboots)" -eq 2 ] || fail "the volume's data did not come along: $(curl -fsS -H "Host: $kdomain" http://127.0.0.1/boots)"
  eventually 30 "worker1 deletes its old copy" wssh "! sudo sh -c 'ls /var/lib/jokku/volumes/*.ext4*'"
  sudo jokku events keep | tail -n 5
  sudo jokku apps:destroy keep --force

  step "a dead worker's instance moves to the control node"
  rm -f "$work/failures"
  (while :; do curl -fs -o /dev/null --max-time 10 -H "Host: $domain" http://127.0.0.1/ || echo x >>"$work/failures"; sleep 0.5; done) &
  watcher=$!
  sudo kill "$(sudo cat "$work/qemu.pid")" # the worker machine dies
  eventually 150 "instance rescheduled to the control node" bash -c \
    "sudo jokku ps:report hello | grep -cE 'Status web.[12]: +healthy \(v[0-9]+, 10\.210\.1\.' | grep -qx 2"
  kill $watcher 2>/dev/null || true
  # A request already sent to the worker when it dies can't complete (the
  # host vanished without closing its connections); every new request must
  # be retried on the surviving instance.
  failures=0
  [ ! -f "$work/failures" ] || failures=$(wc -l <"$work/failures")
  echo "failed requests while the worker died: $failures"
  [ "$failures" -le 2 ] || fail "$failures requests failed while the worker died and its instance moved"
  sudo jokku events | tail -n 5
  curl -fsS -H "Host: $domain" http://127.0.0.1/ | grep -q "served by" || fail "the app is not serving after the failover"
fi

step "removing the worker tears down its mesh peer"
sudo jokku nodes:remove worker1 --force
eventually 30 "worker1 gone" bash -c "! sudo jokku nodes:list | grep -q worker1"
eventually 30 "no mesh left on the control node" bash -c "! ip link show jokku-wg"

printf '\nAll cluster checks passed.\n'
