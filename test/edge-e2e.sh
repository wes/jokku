#!/bin/bash
# An edge in front of a "home" cluster, on a GitHub Actions runner with KVM:
# the runner is the home control node, and a QEMU VM running Ubuntu is the
# public edge. The runner drops every connection the VM opens to it, as a
# home router would, so everything works only because home dials out. Checks
# jokku edge:add over ssh, apps served through the edge, what the edge may
# reach at home, an external app on a "LAN" behind the control node, logins
# (password, users with TOTP, share links), the page shown when home is
# unreachable, and removing the edge.
#
#   test/edge-e2e.sh <dist-dir>   (jokku-linux-amd64 + checksums.txt)
set -euo pipefail
dist=$(cd "$1" && pwd)
work=$(mktemp -d)
bridge=jke-br tap=jke-tap
host_ip=192.168.78.1 edge_ip=192.168.78.2
key=$work/id_ed25519

step() { printf '\n=== %s\n' "$*"; }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  sudo jokku nodes:list >&2 || true
  sudo wg show >&2 || true
  sudo journalctl -u jokku -n 40 --no-pager >&2 || true
  essh "sudo journalctl -u jokku -n 40 --no-pager; sudo journalctl -u jokku-proxy -n 20 --no-pager" >&2 || true
  tail -n 30 "$work/console.log" >&2 2>/dev/null || true
  exit 1
}
essh() { ssh -i "$key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR "tester@$edge_ip" "$@"; }
eventually() { # eventually <seconds> <description> <command...>
  local deadline=$((SECONDS + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "timed out: $what"
    sleep 2
  done
}
# via <host> <path> [curl args...]: a request through the edge, as the
# internet would send it.
via() {
  local host=$1 path=$2
  shift 2
  curl -sS --max-time 15 -H "Host: $host" "$@" "http://$edge_ip$path"
}

step "install the home control node"
sudo JOKKU_DOWNLOAD_URL="file://$dist" JOKKU_IMPORT_KEYS=0 sh ./install.sh
sudo jokku domains:set-global home.test

step "a network for the edge VM"
sudo ip link add "$bridge" type bridge
sudo ip addr add "$host_ip/24" dev "$bridge"
sudo ip link set "$bridge" up
sudo ip tuntap add "$tap" mode tap
sudo ip link set "$tap" master "$bridge" up
sudo iptables -t nat -A POSTROUTING -s 192.168.78.0/24 ! -d 192.168.78.0/24 -j MASQUERADE
sudo iptables -I FORWARD 1 -i "$bridge" -j ACCEPT
sudo iptables -I FORWARD 1 -o "$bridge" -j ACCEPT

step "boot an Ubuntu VM as the edge"
curl -fsSL -o "$work/base.img" https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img
qemu-img create -q -f qcow2 -b "$work/base.img" -F qcow2 "$work/edge.qcow2" 12G
ssh-keygen -q -t ed25519 -N "" -f "$key"
cat >"$work/user-data" <<EOF
#cloud-config
users:
  - name: tester
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: ["$(cat "$key.pub")"]
EOF
printf 'instance-id: edge1\nlocal-hostname: edge1\n' >"$work/meta-data"
cat >"$work/network-config" <<EOF
version: 2
ethernets:
  nic:
    match: {driver: virtio_net}
    addresses: [$edge_ip/24]
    routes: [{to: default, via: $host_ip}]
    nameservers: {addresses: [8.8.8.8, 1.1.1.1]}
EOF
cloud-localds --network-config="$work/network-config" "$work/seed.iso" "$work/user-data" "$work/meta-data"
sudo qemu-system-x86_64 -enable-kvm -cpu host -smp 2 -m 2048 -display none -daemonize \
  -pidfile "$work/qemu.pid" -serial "file:$work/console.log" \
  -drive "file=$work/edge.qcow2,if=virtio" -drive "file=$work/seed.iso,if=virtio,format=raw" \
  -netdev "tap,id=n0,ifname=$tap,script=no,downscript=no" -device virtio-net-pci,netdev=n0
eventually 300 "the VM boots and accepts ssh" essh true

step "serve jokku to the edge"
cp ./install.sh "$dist/install.sh"
(cd "$dist" && python3 -m http.server 8000 --bind "$host_ip" >/dev/null 2>&1) &
server=$!
trap 'kill $server 2>/dev/null; sudo kill "$(sudo cat "$work/qemu.pid")" 2>/dev/null; sudo ip netns del lan 2>/dev/null || true' EXIT
eventually 30 "the file server answers" curl -fsS "http://$host_ip:8000/checksums.txt"

step "home is behind NAT: the edge can't open connections to it"
# Only the file server stands in for the internet; everything else the edge
# starts (WireGuard handshakes included) is dropped. Replies to what home
# starts still flow, as through a home router.
sudo iptables -I INPUT 1 -s "$edge_ip" -m conntrack --ctstate NEW -j DROP
sudo iptables -I INPUT 1 -s "$edge_ip" -p tcp --dport 8000 -j ACCEPT
if essh "curl -s --max-time 3 -k https://$host_ip:7443/v1/version"; then fail "the edge reached the control node's API directly"; fi

step "jokku edge:add installs the edge over ssh"
sudo mkdir -p /root/.ssh
sudo cp "$key" /root/.ssh/edge_key
printf 'Host %s\n  IdentityFile /root/.ssh/edge_key\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n' "$edge_ip" | sudo tee -a /root/.ssh/config >/dev/null
sudo JOKKU_INSTALL_URL="http://$host_ip:8000/install.sh" JOKKU_DOWNLOAD_URL="http://$host_ip:8000" \
  jokku edge:add "tester@$edge_ip" --name edge1 </dev/null | tee "$work/edge-add.log"
grep -q "routes the cluster's domains" "$work/edge-add.log" || fail "the edge didn't report being connected"
eventually 60 "edge1 connected" bash -c "sudo jokku edge:list | grep -E '^edge1 +connected'"
sudo jokku edge:list
sudo jokku nodes:list | grep -E '^edge1 +edge +ready' || fail "nodes:list doesn't show the edge"
essh "systemctl is-active jokku jokku-proxy" || fail "the edge's services aren't running"
essh "! systemctl is-active jokku-dns" || fail "the edge runs jokku-dns, which only VMs need"
eventually 60 "a WireGuard handshake with the edge" bash -c "sudo wg show jokku-wg latest-handshakes | awk '{exit !(\$2 > 0)}'"
essh "ping -c 1 -W 3 10.210.1.1" || fail "the edge can't reach the control node over the mesh"

step "deploy an app at home, and serve it through the edge"
mkdir -p "$work/app" && cd "$work/app"
export GIT_AUTHOR_NAME=e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=e2e GIT_COMMITTER_EMAIL=e2e@example.com
git init -q -b main
printf 'FROM public.ecr.aws/docker/library/busybox:1.36\nRUN mkdir /www\n' >Dockerfile
printf 'web: echo "served by $(hostname)" > /www/index.html && exec httpd -f -p "$PORT" -h /www\n' >Procfile
git add -A && git commit -qm init
sudo jokku apps:create hello
sudo git -c safe.directory='*' archive --format=tar HEAD >"$work/app.tar"
cd "$OLDPWD"
sudo curl -fsS --unix-socket /run/jokku/jokku.sock -H "Content-Type: application/x-tar" \
  --data-binary "@$work/app.tar" "http://jokku/v1/apps/hello/deploys?source=archive" | tee "$work/deploy.log" | tail -n 3
grep -q '"status":"succeeded"' "$work/deploy.log" || fail "deploy failed"
eventually 60 "the edge serves hello" bash -c "curl -fsS --max-time 5 -H 'Host: hello.home.test' http://$edge_ip/ | grep -q 'served by hello-web-1'"
eventually 20 "a router line via edge1" bash -c "sudo jokku logs hello -p router -n 50 | grep -q 'via=edge1'"

step "the edge reaches only what it routes"
upstream=$(sudo jokku ps:report hello | sed -n 's/.*healthy (v[0-9]*, \(10\.210\.1\.[0-9]*:[0-9]*\),.*/\1/p' | head -n 1)
[ -n "$upstream" ] || fail "no healthy instance in: $(sudo jokku ps:report hello)"
essh "curl -fsS --max-time 5 http://$upstream/" | grep -q "served by" || fail "the edge can't reach the app's web port"
essh "curl -sk --max-time 5 https://10.210.1.1:7443/v1/version" | grep -q version || fail "the edge can't reach the control API over the mesh"
if essh "timeout 5 bash -c 'echo > /dev/tcp/10.210.1.1/22'"; then fail "the edge reached ssh on the control node"; fi
if essh "timeout 5 bash -c 'echo > /dev/tcp/10.210.1.1/7444'"; then fail "the edge reached the control node's agent API"; fi
if ! essh "sudo iptables -C INPUT -i jokku-wg -j JOKKU-MESH"; then fail "the edge doesn't firewall the mesh"; fi

step "an external app on the home LAN, through the edge"
# The "LAN": a network namespace behind the control node, with Home
# Assistant on 8123 and something private on 9000.
sudo ip netns add lan
sudo ip link add jke-lan type veth peer name eth0 netns lan
sudo ip addr add 10.99.0.1/24 dev jke-lan
sudo ip link set jke-lan up
sudo ip netns exec lan ip addr add 10.99.0.10/24 dev eth0
sudo ip netns exec lan ip link set eth0 up
sudo ip netns exec lan ip link set lo up
sudo ip netns exec lan ip route add default via 10.99.0.1
mkdir -p "$work/ha" "$work/private"
echo "home assistant" >"$work/ha/index.html"
echo "private" >"$work/private/index.html"
(cd "$work/ha" && sudo ip netns exec lan python3 -m http.server 8123 --bind 10.99.0.10 >/dev/null 2>&1) &
(cd "$work/private" && sudo ip netns exec lan python3 -m http.server 9000 --bind 10.99.0.10 >/dev/null 2>&1) &
eventually 20 "the LAN service answers at home" curl -fsS --max-time 3 http://10.99.0.10:8123/
sudo jokku external:create ha http://10.99.0.10:8123
sudo jokku external:list | grep -E '^ha +http://10.99.0.10:8123' || fail "external:list"
eventually 60 "the edge serves the LAN service" bash -c "curl -fsS --max-time 5 -H 'Host: ha.home.test' http://$edge_ip/ | grep -q 'home assistant'"
essh "ip route get 10.99.0.10" | grep -q jokku-wg || fail "the edge doesn't route the target through the mesh"
if essh "curl -s --max-time 5 http://10.99.0.10:9000/"; then fail "the edge reached a LAN port it doesn't route"; fi
if sudo jokku ps:scale ha web=1 2>/dev/null; then fail "an external app was scaled"; fi

step "a password login, checked on the edge"
echo 123456 | sudo jokku http-auth:enable ha --password
eventually 30 "the edge asks for a login" bash -c "curl -s -o /dev/null -w '%{http_code}' -H 'Accept: text/html' -H 'Host: ha.home.test' http://$edge_ip/ | grep -qx 302"
via ha.home.test / -H 'Accept: application/json' -o /dev/null -w '%{http_code}' | grep -qx 401 || fail "an API client should get 401"
jar=$work/jar
via ha.home.test /.jokku/login -c "$jar" -o /dev/null --data-urlencode password=654321 -w '%{http_code}' | grep -qx 401 || fail "a wrong password got in"
via ha.home.test /.jokku/login -c "$jar" -o /dev/null --data-urlencode password=123456 --data-urlencode rd=/ -w '%{http_code}' | grep -qx 303 ||
  fail "the right password was refused"
via ha.home.test / -b "$jar" | grep -q "home assistant" || fail "logged in, but no app"

step "a share link"
url=$(sudo jokku http-auth:share ha --expires 1h --note e2e | tail -n 1)
token=${url##*/}
[ -n "$token" ] || fail "no share link: $url"
# The link reaches the edge with its next state, a moment after it's made.
eventually 30 "the share link works on the edge" bash -c \
  "curl -s -o /dev/null -w '%{http_code}' -c '$work/share-jar' -H 'Host: ha.home.test' http://$edge_ip/.jokku/share/$token | grep -qx 303"
via ha.home.test / -b "$work/share-jar" | grep -q "home assistant" || fail "in with the link, but no app"
id=$(sudo jokku http-auth:shares ha | awk 'NR == 2 {print $1}')
sudo jokku http-auth:unshare ha "$id"
eventually 30 "a revoked link stops working" bash -c \
  "curl -s -o /dev/null -w '%{http_code}' -b '$work/share-jar' -H 'Accept: text/html' -H 'Host: ha.home.test' http://$edge_ip/ | grep -qx 302"

step "a user with an authenticator code"
printf 'correct horse\n' | sudo jokku http-auth:users:add wes --totp | tee "$work/user.log"
secret=$(sed -n 's/.*Key: \([A-Z2-7]*\).*/\1/p' "$work/user.log")
[ -n "$secret" ] || fail "no TOTP key shown"
sudo jokku http-auth:enable hello wes
eventually 30 "hello asks for a login" bash -c "curl -s -o /dev/null -w '%{http_code}' -H 'Accept: text/html' -H 'Host: hello.home.test' http://$edge_ip/ | grep -qx 302"
page=$(via hello.home.test /.jokku/login -c "$work/u-jar" --data-urlencode user=wes --data-urlencode 'password=correct horse' --data-urlencode rd=/)
pending=$(sed -n 's/.*name="pending" value="\([^"]*\)".*/\1/p' <<<"$page" | sed 's/&#43;/+/g; s/&#61;/=/g')
[ -n "$pending" ] || fail "no code step after the password: $page"
code=$(python3 -c '
import base64, hmac, struct, sys, time
key = base64.b32decode(sys.argv[1] + "=" * (-len(sys.argv[1]) % 8))
mac = hmac.new(key, struct.pack(">Q", int(time.time()) // 30), "sha1").digest()
o = mac[-1] & 15
print("%06d" % ((struct.unpack(">I", mac[o:o + 4])[0] & 0x7fffffff) % 1000000))' "$secret")
via hello.home.test /.jokku/login -b "$work/u-jar" -c "$work/u-jar" -o /dev/null -w '%{http_code}' \
  --data-urlencode "pending=$pending" --data-urlencode "code=$code" --data-urlencode rd=/ | grep -qx 303 || fail "the TOTP code was refused"
via hello.home.test / -b "$work/u-jar" | grep -q "served by" || fail "logged in as wes, but no app"
sudo jokku http-auth:disable hello

step "home unreachable: the edge says so"
sudo ip link set jokku-wg down
eventually 60 "the edge shows its page" bash -c "curl -s --max-time 15 -H 'Host: hello.home.test' http://$edge_ip/ | grep -q 'be reached right now'"
sudo systemctl restart jokku
eventually 90 "home is back" bash -c "curl -fsS --max-time 5 -H 'Host: hello.home.test' http://$edge_ip/ | grep -q 'served by'"

step "removing the edge"
sudo jokku edge:remove edge1 --force
sudo jokku edge:list | grep -q edge1 && fail "edge:list still shows the removed edge"
# It stays a peer a moment, so it hears it was removed and stops routing;
# then it is gone from the mesh.
eventually 60 "the edge stops routing" bash -c "curl -s --max-time 5 -H 'Host: hello.home.test' http://$edge_ip/ | grep -q 'No app is configured'"
eventually 180 "no mesh left at home" bash -c "! ip link show jokku-wg"

printf '\nAll edge checks passed.\n'
