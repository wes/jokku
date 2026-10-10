# Jokku architecture

Jokku is Dokku's workflow on Firecracker microVMs across a cluster of
machines. You `git push`, and Jokku builds the Dockerfile, boots the app as
microVMs, spreads them over your nodes and routes HTTP(S) traffic to them.

This document records the design decisions. When the code and this file
disagree, fix one of them.

## Principles

1. **Dokku's UX, kept as-is.** Command names, argument order, output style
   (`----->`, `=====>`), `git push` deploys, `ssh-keys:add`,
   `config:set`, `ps:scale`, `domains:add`. A Dokku user should be at home
   on day one.
2. **One binary.** `jokku` is the CLI, the API server, the node agent, the
   SSH forced-command, the git hook, the HTTP proxy (Caddy is embedded as a
   library) and even the init process inside every microVM. The only other
   binaries on a node are `firecracker` and `buildkitd`, pinned and installed
   by `jokku setup`.
3. **The API is the product, the CLI is a client.** Every CLI command is
   an HTTP call to `/v1/...`. Nothing in the CLI touches the database or
   the filesystem directly, so a GUI can come later without rework.
4. **One node is a complete cluster.** A single server running Jokku is
   exactly Dokku. Adding nodes is one command and changes nothing else.
5. **Boring state.** SQLite on the control node is the source of truth.
   Nodes converge on desired state by reconciling, so restarts and crashes
   are uneventful.

## The 60-second tour

```sh
# on a fresh Ubuntu/Debian server with KVM: installs jokku, the jokku user and
# the service, and lets the SSH keys you logged in with use Jokku
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh

# on your laptop (only git and ssh needed), in your app's repo with a Dockerfile
git remote add jokku jokku@server:myapp
git push jokku main
# => http://myapp.203.0.113.10.sslip.io

# commands run on the server, or from anywhere as "ssh jokku@server <command>"
ssh jokku@server config:set myapp DATABASE_URL=postgres://...
ssh jokku@server domains:add myapp myapp.com
ssh jokku@server ps:scale myapp web=4 worker=2
ssh jokku@server resource:limit myapp --cpu 2 --memory 1g --process-type web

# grow the cluster: on the control node, print a join command...
jokku cluster:join-command
# ...and run what it prints on the new server
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh -s -- --join 203.0.113.10 --token jk1_...
```

## Topology

```
                         DNS: *.example.com -> any ingress node(s)
                                      |
        +-----------------------------+-----------------------------+
        |                             |                             |
  +-----v-------------+        +------v------------+        +------v------------+
  | control node      |        | worker node       |        | worker node       |
  |                   |        |                   |        |                   |
  | jokku daemon      |        | jokku daemon      |        | jokku daemon      |
  |  - API (/v1)      |        |  - agent          |        |  - agent          |
  |  - SQLite         |        |  - proxy (Caddy)  |        |  - proxy (Caddy)  |
  |  - scheduler      |        |                   |        |                   |
  |  - builder        |        | firecracker VMs   |        | firecracker VMs   |
  |  - git repos      |        |  10.210.2.0/24    |        |  10.210.3.0/24    |
  |  - agent + proxy  |        |                   |        |                   |
  | firecracker VMs   |        |                   |        |                   |
  |  10.210.1.0/24    |        |                   |        |                   |
  +---------+---------+        +---------+---------+        +---------+---------+
            |                            |                            |
            +============= WireGuard mesh (wg0, udp/51820) ===========+
```

### Node roles

- **Control node** (exactly one in v1). Runs the API, SQLite, scheduler,
  builder (BuildKit), git repositories and the artifact store. It is also a
  worker unless you mark it unschedulable. `ssh jokku@control` and
  `git push jokku@control:app` land here.
- **Worker nodes.** Run the agent (manages Firecracker VMs) and the proxy.
  They hold no authoritative state; wipe one and re-join it and it
  converges.

- **Edge nodes.** Public machines that only receive traffic: they run the
  proxy and nothing else, and the other nodes dial them, so a cluster behind
  NAT (a home lab) can serve the internet with no port open. See
  [Edges](#edges).

Every node is an ingress node by default (`nodes:set <node> ingress false`
turns it off). Point DNS at whichever nodes you want to receive traffic.

A highly available control plane (replicated SQLite via Litestream or
similar, plus leader election) is deliberately out of scope for v1. Losing
the control node stops deploys and changes, not running apps: agents and
proxies keep serving their last known state.

## How clients reach the API

```
 jokku on the server ----------------------------------------+
 ssh jokku@server <command> --> sshd --> jokku ssh-command ---+--> /run/jokku/jokku.sock --> API
 git push --> sshd --> jokku ssh-command --> git hook --------+
 GUI / CI / curl ----- https://control:7443 + bearer token --> API
 agents -------------- WireGuard + node token -------------> API (/v1/agent/*)
```

- **Unix socket** `/run/jokku/jokku.sock`, mode `0660`, group `jokku`.
  Anyone who can open the socket is an admin, the same model as Docker.
- **SSH**, exactly like Dokku: `ssh jokku@server apps:list` needs nothing
  installed locally. The forced command runs the CLI on the server, which
  calls the API over the socket.
- **HTTPS + token** (milestone 3) for CI systems and a future GUI:
  `jokku tokens:create ci` issues a bearer token. The control node
  generates a self-signed CA at init; clients pin it by fingerprint.

The `jokku` binary also runs as a client away from the server, though nothing
requires it. It reaches the API by running `ssh jokku@server jokku
api:dial-stdio` and speaking HTTP over the session, like `docker -H ssh://`.
It finds the server and default app from a git remote named `jokku`, or from
`JOKKU_HOST`.

## Authentication

| Who | How | Command |
| --- | --- | --- |
| People and CI pushing code or running commands | SSH public keys, stored in SQLite, rendered into `~jokku/.ssh/authorized_keys` with a forced command | `ssh-keys:add <name> [file]` |
| Jokku pulling a private repo (`git:sync`) | A deploy key Jokku generates; you add its public half to the GitHub repo | `git:generate-deploy-key`, `git:public-key`, `git:allow-host github.com` |
| Jokku pulling over HTTPS | Stored credentials per host | `git:auth github.com <user> <token>` |
| HTTPS API clients | Bearer tokens (hashed at rest) | `tokens:create <name>` |
| Nodes | Join token once, then a per-node token over WireGuard | `cluster:join-command` |

Each `authorized_keys` line looks like:

```
restrict,pty,command="/usr/local/bin/jokku ssh-command --key-name admin" ssh-ed25519 AAAA...
```

`jokku ssh-command` reads `SSH_ORIGINAL_COMMAND` and dispatches:

| Original command | Action |
| --- | --- |
| `git-receive-pack 'app'` | Ensure the app and its bare repo exist, then run `git-receive-pack` |
| `git-upload-pack 'app'` | Let people clone what was deployed |
| `jokku api:dial-stdio` | Pipe stdio to the API socket (optional local client) |
| anything else | Run it as a CLI command on the server |

In v1 every key is an admin, as in Dokku without the ACL plugin. The key
name is passed to the API as the actor and recorded on deploys. Per-app
permissions can be layered on later because every request already carries
an actor.

### GitHub Actions

Pushing from CI is a normal `git push` with a key added via `ssh-keys:add`:

```yaml
- uses: actions/checkout@v4
  with: { fetch-depth: 0 }
- uses: dokku/github-action@master   # works unchanged: same protocol
  with:
    git_remote_url: ssh://jokku@203.0.113.10:22/myapp
    ssh_private_key: ${{ secrets.JOKKU_SSH_KEY }}
```

Or, with no SSH at all, upload a tarball over the HTTPS API:

```sh
git archive HEAD | curl -fsS -H "Authorization: Bearer $JOKKU_TOKEN" \
  -H "Content-Type: application/x-tar" --data-binary @- \
  https://203.0.113.10:7443/v1/apps/myapp/deploys
```

## Deploys

Every deploy, however it starts, becomes the same thing: **a source tarball
(or an image reference) posted to `POST /v1/apps/{app}/deploys`**.

| Source | How it becomes a deploy |
| --- | --- |
| `git push jokku main` | The `pre-receive` hook runs `git archive <rev>` (it can see the quarantined objects) and streams the tarball to the API. Build output streams back as `remote:` lines. A failed deploy rejects the push. |
| `git:sync app <repo> [ref] --build` | Control fetches the repo (deploy key / `git:auth`), archives the ref, deploys. |
| `git:from-archive app <url>` | Control downloads the tarball and deploys it. |
| `builder:image app <image>` | A one-line Dockerfile, `FROM <image>`, goes through the same build: BuildKit pulls the image and it is converted like any other. The app becomes an image app: `ps:rebuild` pulls the tag again, and pushes are refused until `builder:dockerfile` or `builder:compose`. |
| HTTPS API | Upload a tarball, as above. |
| `ps:rebuild app` | Rebuilds the last pushed source, or pulls an image app's image again. |

Long-running calls stream newline-delimited JSON events
(`{"type":"log","message":"..."}` ... `{"type":"done","status":"succeeded"}`).
The CLI and the git hook render them as Dokku-style output; a GUI can render
them however it likes.

### Pipeline

```
source.tar --> BuildKit (Dockerfile) --> OCI image --> flatten layers --> rootfs.ext4
                                              |                               |
                                         image config                content-addressed
                                     (ENTRYPOINT, CMD, ENV,           artifact on the
                                      WORKDIR, USER, EXPOSE)          control node
                                              |                               |
                                              +---------> release <-----------+
                                                 (artifact + Procfile + config vars
                                                  + resources, immutable, versioned)
                                                             |
                                                          rollout
```

1. **Build.** `buildctl` against a local `buildkitd` builds the Dockerfile
   (`builder:dockerfile app Dockerfile.prod`) to an OCI
   image. Build args come from `docker-options:add app build "--build-arg X"`.
   Images from private registries are pulled with the logins saved by
   `registry:login`, handed to BuildKit for that build only.
2. **Convert.** The image's layers are flattened (whiteouts applied) and
   written as an ext4 image; the image config is kept as JSON. This also
   handles `builder:image`, so registry images and Dockerfile builds share
   one path.
3. **Release.** An immutable, numbered record: artifact digest, process
   types, config vars snapshot, resources. `config:set` creates a new release
   from the same artifact, as Heroku does.
4. **Rollout.** See *Zero-downtime rollouts* below.

Process types come from a `Procfile` in the repo root, as in Dokku. Without
one, the app has a single `web` process running the image's
`ENTRYPOINT`/`CMD`. The app listens on `$PORT`: the `PORT` config var if set,
otherwise the image's `EXPOSE`d port. An image inherits its base image's ports
(nginx's 80), so when there are several, the ones from the newest `EXPOSE` step
in the image history win. With no `EXPOSE`, it is the image's `PORT` variable
or `5000`.

## MicroVM runtime

Each instance (`web.1`, `worker.2`, ...) is one Firecracker microVM in its
own transient systemd unit (`jokku-vm-<id>`), not a child of the daemon, so
restarting or updating jokku leaves apps running.

| Piece | What it is |
| --- | --- |
| Kernel | Firecracker's CI guest kernel (6.1), pinned by SHA-256: virtio, ext4, overlayfs, vsock and kernel IP autoconfig built in. |
| `vda` | The release's root filesystem: the image's flattened layers as a read-only ext4, shared by every instance of the release on that node. Built once per image digest. |
| `/.jokku/init` | The jokku binary itself, copied into every rootfs and started by the kernel as PID 1. |
| `vdb` | A tiny config drive: JSON with the command, env, user, workdir, hostname and DNS. |
| `vdc` | A per-instance sparse scratch ext4 used as the overlay upper layer: a writable root that is kept across restarts of the instance and discarded when it is replaced (like Dokku's containers). |
| `vdd`+ | Volumes from `storage:mount`: ext4 disk images on the node, attached with Firecracker's `Writeback` cache so a guest `fsync` reaches the host's disk (the default ignores flushes), and with discard so deleted data frees space in the sparse image. See [Volumes](#volumes). |
| `eth0` | A TAP device on the node's `jokku0` bridge, addressed via the kernel `ip=` boot argument. |

Boot sequence inside the VM: the kernel mounts `vda` read-only and runs
`/.jokku/init`, which reads the config drive, stages an overlay of `vda` and
`vdc` in a tmpfs, `pivot_root`s into it, mounts `/proc`, `/sys`, `/dev`,
`/run` and cgroups, brings up loopback, writes `/etc/hosts` and
`/etc/resolv.conf`, mounts volumes, then starts the process as the image's
`USER`. It stays
PID 1 to reap zombies. A stop request (Firecracker's Ctrl-Alt-Del on x86)
becomes `SIGTERM` to the app's process group, then `SIGKILL` after 10
seconds, then volumes are unmounted and the VM powers off.

The app's stdout and stderr go to the VM's serial console, which is the
unit's output, so they land in the journal tagged with `JOKKU_APP` and
`JOKKU_PROCESS`; `jokku logs` reads them from there.

### Sessions: jokku enter and volume copies

Init doubles as a guest agent on a vsock port. Vsock is Firecracker's
host-to-guest channel, which needs no network and nothing in the image. It
serves three kinds of session:
- **exec:** `jokku enter`, a command or shell with a terminal or plain pipes.
- **export:** a directory as a `.tar.gz`, for `storage:export`.
- **import:** restoring a directory from a tar, for `storage:import`.

A session travels the same frames all the way:

```
CLI --HTTP upgrade--> API --HTTP upgrade, mesh--> node agent --vsock--> guest init
```

The node agent finds the VM through Firecracker's vsock socket in the
instance's directory. It adds the token generated for that VM at boot,
which sits on the config drive that only root in the VM can read. So only
the node's agent can open sessions with a VM, and the app can't use the
guest agent to become root. Each session is recorded in `jokku events` with
who opened it.

- Commands run as the image's `USER` with the app's environment, unless
  `--root` is given.
- Exports pause the app's process group (SIGSTOP/SIGCONT) while they copy,
  unless `--live` is given.
- Imports pause the app, extract the archive, then restart the app inside
  the running VM: init starts it again rather than powering off.

Volumes are read and written through the running instance that mounts them.
The host never mounts a filesystem the app wrote, which would hand the host
kernel data the app controls.

Not yet: running Firecracker under its `jailer` (chroot, unprivileged uid,
cgroup limits), and `jokku run` (a one-off instance). Until the jailer
lands, treat apps on one server as trusting each other, as with Dokku.

Sizing is per process type: `resource:limit app --cpu 2 --memory 1g
--process-type web`. Firecracker cannot hot-add vCPUs, so a size change is a
rolling restart into new VMs, which is also how Dokku applies resource
changes.

## Networking

### Addresses

- Cluster network: `10.210.0.0/16` (set at init).
- Node *N* owns `10.210.N.0/24`. `10.210.N.1` is the node itself: the
  gateway on its `jokku0` bridge and its address on the WireGuard mesh.
  VMs get `.2` through `.254`. That allows up to 254 nodes with 253
  instances each.
- The control node allocates instance IPs when it schedules, so routes are
  known before a VM boots.

### WireGuard mesh

Every node peers with every other node over `wg0` (udp/51820). Peer *N*'s
`AllowedIPs` is `10.210.N.0/24`, so a packet for any VM is routed straight
to the node that hosts it, encrypted. The route `10.210.0.0/16 dev wg0 src
10.210.N.1` makes local traffic to remote VMs leave with a mesh source
address. A node behind NAT works as long as the control node is reachable,
because `PersistentKeepalive` keeps its tunnel open. Outbound internet from
VMs is masqueraded on each node (nftables).

Peers are updated in place, never replaced, and a peer that handshook in
the last three minutes keeps the address it talks from rather than being
sent back to its configured endpoint: a node behind NAT may come from any
port. Edges have no endpoint for their peers at all (see [Edges](#edges)).
The firewall is applied with one `iptables-restore --noflush`, so no packet
meets a half-written chain.

### Internal DNS

Apps reach each other by name. `<process>.<app>.internal` resolves to the
addresses of an app's wanted instances of that process type, wherever they
run, and `<app>.internal` to its `web` instances (or its only process type).
VMs get `search <app>.internal internal`, so `db` finds a process of their own
app and `mydb` finds another app. Healthy instances are preferred, so a name
follows a deploy as the new instances pass checks. Answers carry a 5-second
TTL, since instances come and go with deploys and moves.

Each node runs the server as its own service, `jokku-dns` (`jokku dns`),
on its bridge address (`10.210.N.1:53`), so restarting or updating the daemon
never interrupts lookups. The control node puts the cluster's names in every
node's desired state; the agent writes them to `/var/lib/jokku/dns/zone.json`,
which the service watches. Every other name is forwarded to the host's own
resolvers. A VM is pointed at the service only if it answers when the VM
boots; otherwise it gets the host's resolvers (no internal names, but the
internet works), and picks the service up on its next restart.

### HTTP proxy

Caddy is **embedded in the jokku binary** and runs on every ingress node as
its own service, `jokku proxy`. The agent configures it through Caddy's JSON
admin API on a local unix socket. Keeping it a separate process means
updating or restarting the daemon never interrupts traffic (see
[Updates](#updates)). Embedding gives Jokku:

- One binary, and no Caddyfile templating or reload scripts.
- A custom Caddy storage module backed by the control node, so
  certificates, ACME accounts and HTTP-01 challenge tokens are shared
  cluster-wide. Any ingress node can answer a challenge for a certificate
  another node requested, and the cluster issues each certificate once.

Routes are computed by the control node: for each app with the proxy
enabled, its domains map to the `IP:PORT` of every *healthy* `web` instance
of the current release. Caddy load-balances and does passive health
checks.

Default domains: with `domains:set-global example.com`, apps get
`<app>.example.com`. At install, the global domain defaults to
`<public-ip>.sslip.io`, so the first `git push` already gets a working
URL.

TLS is Let's Encrypt via Caddy and, as in Dokku, it is off for a new app
until `letsencrypt:enable app` (or `letsencrypt:enable --global`, which new
apps then follow). With it on, every public domain of the app gets a
certificate and HTTP redirects to HTTPS. A domain Let's Encrypt can't reach,
such as an IP address or an sslip.io name for a private address, stays on
plain HTTP. Apps created before this default changed keep TLS on. `certs:add`
installs a custom certificate.

`ports:set app http:80:5000 https:443:5000` mirrors Dokku's port mapping.
Raw TCP/UDP ports are a later addition.

**External apps** (`external:create ha http://192.168.1.50:8123`) are apps
of kind `external`: a target address instead of releases, stored as
properties (`external` plugin: `url`, `via`, `insecure`). Their domains
route to the target like any app's, so domains, certificates, logins and
router logs work unchanged; deploys, processes and volumes are refused.
Targets are private IPv4 addresses outside the cluster network, since edges
reach them through a node (`via`, the control node by default) that
forwards and masquerades; only edges and that node route them. An edge
leaves out a target route that would capture its own tunnel's packets.

**Logins** (`http-auth:*`) are a Caddy handler embedded like the storage
module, `jokku_auth`, placed before `reverse_proxy` on routes with a login.
Its config carries everything it checks: the app's mode (one shared
password, or the cluster's users, allowed all or by name), allowed CIDRs,
bypass paths, share links (token hashes and expiries), the users' bcrypt
hashes and TOTP secrets, and the session key. Sessions are HMAC-signed
cookies bound to the host and to a fingerprint of the credential they were
made with, so a changed password, a removed user or a revoked link ends them
at once, and any ingress node, an edge included, checks them on its own.
With a login domain, users log in there once: it posts a one-minute,
host-bound, single-use hand-off to the app domain's `/.jokku/callback`
(over HTTPS for a domain served so, and never to another port), which sets
that domain's cookie. The app gets `X-Jokku-User`, which a small handler on
every route strips from incoming requests in every spelling, and never the
session cookie. Failed logins are limited per client (an IPv6 /64 counting
as one), per app and per user, in each node's memory, and a TOTP code works
once. Removing an edge replaces the session key, since the edge held it.

When an app's instances can't be reached (they're restarting, or the nodes
behind an edge are offline), the proxy answers 502 with a page saying so.

## Control loop

Agents **pull** state; the control node never needs to dial into a node to
change things.

```
agent                                         control
  | GET /v1/agent/state?since=<rev>  (long-poll) |
  |--------------------------------------------->|  desired: instances for this node,
  |<---------------------------------------------|  WireGuard peers, proxy routes
  | reconcile: boot/stop VMs, wg peers, Caddy    |
  | POST /v1/agent/status  (every 5s + on change)|
  |--------------------------------------------->|  observed: instance states,
  |                                              |  health, node resources
```

- The desired state carries a revision number, so the long-poll returns as
  soon as anything changes. Rollouts propagate in well under a second.
- Reconciliation is idempotent: the agent boots what should run and is not
  running, and stops what is running and should not be. Rebooting a node
  needs no special handling.
- The control node is its own agent through the same interface, in-process.
- Interactive streams (`logs -t`, `enter`, `run`) are the exception. The
  control node opens them to the agent's local API on its mesh address
  (`10.210.N.1:7444`), authenticated by WireGuard plus the node token.

### Scheduling

When an instance needs a home, the scheduler filters nodes that are up,
schedulable, not draining, able to run microVMs (each agent reports whether
its CPU and KVM can) and with the memory free: total memory minus a reserve
(a tenth, at least 512 MiB) minus what is already promised to instances.
Memory is never overcommitted; vCPUs may be. It then prefers the node running
the fewest instances of the same app and process type (spread), then the one
with the most free memory. An instance with volumes runs on the node holding
their disks, and only on nodes whose agent reports volume support, so a node
still on an older release never starts one without its disk.

Root filesystems are built on the control node. A node that lacks one
downloads it from the control node and checks its SHA-256 before using it, and
deletes ones it no longer needs.

### Agents

The control node computes each node's desired state from the store on every
poll and tags it with an ETag (a hash). A long-poll returns as soon as the
ETag changes; changes signal waiting polls immediately, and polls also
recompute every two seconds, so nothing that changes the state can be missed.

The agent caches the last state on disk. If the control node is unreachable,
the node keeps running and serving that state, and a restarted agent picks it
back up. Agents keep per-instance state (health, restarts) in memory; after a
restart, VMs that are already running are adopted and checked, never started
twice.

### Zero-downtime rollouts

Rollouts follow Dokku's default zero-downtime behavior:

1. Boot the new release's instances to the target formation (`ps:scale`).
2. Wait for them to pass checks. The default is that the process stays up
   and, for `web`, accepts TCP on `$PORT`. `app.json` `healthchecks`
   (Dokku's format) add HTTP checks.
3. Switch the routes to the new instances.
4. After `checks:set app wait-to-retire` (default 60s), stop the old ones.

If checks fail, the deploy fails, the new instances are torn down, the old
release keeps serving, and the failing instance's logs are printed.
`releases:rollback app v12` (a Jokku addition) rolls out an earlier release.

Only what changes restarts. A deploy, or a settings change (`config:set`,
`ps:scale`, `resource:limit`, `storage:*`), keeps the instances of any
process type the new release would run exactly as before: same image,
command, environment, port, size and volumes. Those instances carry on into
the new release, and the deploy says `Unchanged, left running: db`. So
scaling `worker` leaves `web` alone, and a compose app's database doesn't
restart because its web service changed. `ps:restart` still restarts
everything. Process types start in dependency order (a compose file's
`depends_on`), each group passing its checks before the next starts.

### Scaling and failure

- **Horizontal:** `ps:scale app web=4` rolls out that many instances of the
  current release, spread across nodes.
- **Vertical:** `resource:limit` sets vCPU and memory per process type and
  triggers a rollout.
- **A node dies:** after 30s without a report it is marked `down` and the
  proxies stop sending it new requests. After another 60s (so reboots and
  updates don't shuffle anything) its instances are started on other nodes.
  When it comes back, its desired state no longer includes them and it stops
  them. Instances with volumes on it stay put and wait for it: their data is
  there (an event says so). Volumes that are backed up are restored onto
  another node after five minutes instead (see [Backups](#backups)).
- **Requests while a node dies:** the proxy gives up on an unreachable
  instance after 2s and retries the request on another for up to 5s, and
  passive health checks skip the dead one. Requests already in flight to a
  machine that vanishes can still fail; CI kills a worker outright and allows
  at most two.
- **Draining:** `nodes:drain node2` starts a copy of each instance elsewhere,
  waits for it to pass checks, then stops the original after a 10s grace
  period, so traffic never drops. Instances with volumes move with their
  disks instead, which costs a short stop (see [Volumes](#volumes)).
  `nodes:remove` then takes the node out; it refuses while the node holds
  volumes, and `--force` skips the drain and treats it like a dead node
  (losing the volumes on it).
- **The control node is unreachable:** workers keep running, restarting
  crashed instances and serving traffic with their last known state (cached on
  disk, so it survives their own restarts). Deploys and changes wait until it
  is back.
- **An agent restarts** (a crash, an update): it adopts the VMs that are
  running instead of starting them again, so apps don't notice.

## Volumes

`storage:mount myapp data:/app/data` mounts a named volume, created on first
use, in one process type's instance (`web` unless `--process-type` says
otherwise). Every volume has a **type**. Today there is one, `local`, but the
records, API and agent protocol carry the type so other kinds can be added
alongside it (an object-storage type, shared by many instances, is the
likely next one).

A **local** volume is an ext4 disk image, `/var/lib/jokku/volumes/<id>.ext4`,
on one node. It is sparse, so its size (default 10g, `--size`,
`storage:resize` to grow) is a limit, not an allocation. Because only one VM
can safely mount an ext4 filesystem:

- A process type with a local volume runs one instance (`ps:scale` refuses
  more), and that instance is placed on the node holding the disk. A new
  volume's disk is made, empty, wherever its instance is first placed.
- Deploys stop the old instance before starting the new one, so it has a
  short gap instead of a zero-downtime switch. If the new one fails checks,
  the old one is started again. Independently, an agent never starts a VM
  while another VM on that node has one of its disks attached.

Inside the VM, init mounts each volume before starting the app. A fresh
volume is seeded the way Docker seeds a named volume: with whatever the
image has at that path (owners and modes kept), or, where the image has
nothing, owned by the image's `USER`. mkfs's `lost+found` is removed, because
programs like `initdb` refuse a data directory that isn't empty.

**Moving** a volume (a drain, or `storage:move`) copies the disk node to node
over the mesh, with the receiving node pulling from the owner's agent API
using a token made for that move:

1. The volume is marked as moving, and a replacement instance is placed on
   the new node. It waits there (state `syncing`) for the disk.
2. The new node copies the disk while the app keeps running. It repeats the
   copy until a pass changes little, then reports `synced`.
3. The control node stops the instance. The owner refuses the final pass
   until the VM has let go of the disk. It then sends only the blocks that
   changed, and renames its copy `.moved` so nothing on it can use the disk
   any more.
4. The new node reports `received`, and the move is committed: the new node
   owns the disk and starts the replacement. The old copy is deleted once
   the new node reports the disk ready.

Copies compare 1 MiB blocks by SHA-256, skip holes and zero blocks (sparse
disks stay sparse), and verify every block on arrival. An interrupted pass
resumes from what has already arrived. If the new node goes down before the
commit, the move is called off: the old node takes its disk back and the
instance starts there again. A volume never moves when its node dies, because
the data is only there. Instances wait for that node, which makes backups
the answer to losing a server for good (see [Backups](#backups)).

**Deleting** is explicit. `storage:destroy` (refused while the volume is
mounted or in use) or `apps:destroy` marks a volume as destroying, and its
node deletes the disk and reports it gone. An agent never deletes a disk
just because the control node stopped listing it. Leftovers of moves
(`.incoming`, `.received`, `.moved`) are cleaned up that way.

### Backups

A volume's backups go to a path in an S3-compatible bucket (a
*destination*). The node holding the disk uploads straight to the bucket;
the control node keeps the settings, the encryption key and a record of
each backup. In the bucket:

```
<path>/jokku-backup.json       format, block size, the volume's ID, the key's ID
<path>/backups/<time>.backup   one per backup: the disk's size and its blocks, in order
<path>/blocks/ab/abcd...       1 MiB blocks, named by their contents, shared by every backup that has them
```

**A backup** reads the disk in 1 MiB blocks, skipping holes and zero blocks,
and uploads the blocks the latest backup doesn't have. Then the guest agent
freezes the volume's filesystem (`FIFREEZE`, writes wait, reads go on), the
disk is read again and blocks changed meanwhile are copied to a staging file,
and writes resume; those blocks are uploaded after. The freeze lasts one
read of the disk, and the guest ends it by itself after 30 seconds if the
node goes quiet, failing the backup. A volume no running VM has is read
directly, with its instances held back meanwhile. The backup file is written
last, so a backup cut short is simply not there.

**Encryption** is per destination, on by default. One key per cluster, shown
to the user when made and kept by the control node. Blocks are named by an
HMAC of their contents (SHA-256 without a key), compressed with zstd, and
sealed with AES-256-GCM bound to their name; backup files the same way.
Restores check every block against its name, so a changed bucket is caught.
`jokku-backup.json` records the key's ID, so the wrong key gets a clear
error, and the volume's ID, so two volumes never share a path.

**A restore** makes a new copy of the disk from a backup, as a move does
from the owner, and the current data is backed up first unless
`--skip-backup`:
- On the disk's own node: the backup downloads (`.incoming`, then
  `.received`) while the app runs. Then the instances using the disk are
  replaced, the restored copy takes the disk's place once their VMs let go
  of it, and the replacements start with it.
- Onto another node, when the disk's node is down or gone: the instances
  using the disk are replaced there right away and wait, the download is
  committed like a move, and they start. The old node's disk is kept when it
  comes back, and its instance is stopped.

**Schedules** run on the control node: every few seconds its loop starts
the backups that are due (every 15 minutes by default), at most two at a
time, since each reads a whole disk. A failed one is tried again after five
minutes, or its schedule if sooner; a volume whose node is down, or that is
moving or being restored, is waited for. Scheduled backups make an event
only when they start failing or work again.

**Retention** follows every backup: it deletes the backup files no longer
kept (every backup from the last `--keep-recent`, the last one of each of
the last `--keep-daily` days in UTC, and always the newest). Every six hours,
also after a backup, it reads the kept backup files and deletes the blocks
none of them uses, counting what is left. Both run under the volume's backup
lock: a backup running meanwhile would have new blocks no backup file names
yet.

**Automatic restore.** Once a volume's node has been silent for five
minutes (or was removed), a volume with `--auto-restore on` (the default)
and at least one backup is restored onto another node from its latest
backup, as `backups:restore --node` would, and the instances using it start
there. Writes after that backup are lost, unless the node comes back: it
then renames its disk `<id>.ext4.stale` and keeps it, never used again
(`storage:report` shows it; `storage:discard-old-copy` deletes it).

**Fencing** keeps a node that is only cut off, not dead, from running its
copy of the app alongside the restored one:
- An agent has heard from the control node when it has applied its latest
  state (long polls return at least every 25 seconds). After two minutes
  without, it asks the other nodes' agents (`GET /v1/contact`, on the mesh)
  how long since they heard.
- It keeps running instances with auto-restored volumes only if none of the
  nodes it reaches hears from the control node and, with them, it is a
  strict majority of the cluster: then the control node is down, or cut off
  from most nodes. Otherwise it stops them. Until it has heard or asked
  (just after starting, say), it doesn't start them.
- The control node restores only while it sees at least half of the nodes,
  itself included. So when it is cut off from most of the cluster, it
  restores nothing, and the rest carry on; in a two-node cluster it breaks
  the tie, and the worker stops its apps whenever it can't reach it.
- When a fenced node is back in touch, it applies the latest state before
  starting anything, so it never restarts an app that has moved.

A volume whose disk is **missing** on a node that is up is restored there
from its latest backup the same way, in place.

**The cluster's own backups** run on the control node, hourly by default and
on as soon as a destination exists, under `jokku/cluster` in its bucket:
- `state/`: a consistent snapshot of the database (`VACUUM INTO`) and the
  control node's identity (`tls/control.{key,crt}`, `wireguard.key`, its
  tokens), as one tar, backed up block by block like a disk.
- `artifacts/`: the root filesystem of each app's three latest releases, one
  backup per image named by its digest, sharing blocks. Each is uploaded
  once; images no longer kept are deleted when blocks are collected, so an
  old state backup may name releases whose images are gone (those apps are
  deployed again).

`jokku restore-cluster`, run as root on a new server, downloads a state
backup, checks it is this server's (the control node's name) and that the
server holds no cluster yet, downloads the release images, then stops
jokku, keeps the server's own state aside, puts the backup's in place and
starts jokku again. Workers reconnect if the server has the old address;
volumes on the control node are found missing and restored, and those on
nodes that are gone are restored elsewhere once the nodes are removed.

## Databases

`db:<engine>:create <name>` (Postgres, MySQL, Redis) creates an app named
`<engine>-<name>` of that kind and deploys it from a compose file Jokku
writes (`internal/database`): one service, the engine's official image, a
`data` volume, and memory set in the file. Its config vars hold its image,
its name and generated passwords, and fill in the file. Services get only
the environment the file gives them, so nothing else leaks into the VM. With
no published ports, there is no HTTP route; other apps reach it at
`<engine>-<name>.internal`, where it is the app's only process.

- Postgres keeps its data in `pgdata` inside the volume and MySQL in `data`,
  since both refuse a volume's own root, which holds `lost+found`. Redis
  runs with append-only persistence and a password.
- Linking (`database_links`) sets the engine's variable (`DATABASE_URL`,
  `REDIS_URL`, or `<ALIAS>_URL`) on the app and restarts it; a linked
  database can't be destroyed.
- `connect`, `export` and `import` are `jokku enter` sessions in the
  database's VM: psql, pg_dump and pg_restore; mysql and mysqldump; and
  redis-cli. Passwords go in the session's environment, not its arguments,
  so `jokku events` never shows them.
- With a backup destination, the new volume is backed up at once, to where
  the cluster is backed up, with the defaults. Snapshots taken with writes
  frozen are crash-consistent, which all three recover from.
- A database's app is left out of `apps:list` (`?all=true` lists it), and
  can't be pushed to, renamed or cloned.

## Compose apps

`builder:compose app [<path>]` deploys the app from a compose file instead
of a Dockerfile. Pushing to the app then deploys every service in the file as
one Jokku app, and each service becomes a process type with its own image.
The file is `<path>`, or else `compose.yaml` (or `compose.yml`,
`docker-compose.yaml`, `docker-compose.yml`), in the build dir.

The file is read with compose-go, the loader docker compose itself uses, so
interpolation, `extends` within the file, profiles (`COMPOSE_PROFILES`) and
the short and long syntaxes behave as they do in Docker. The mapping:

| Compose | Jokku |
| --- | --- |
| `build:` (context, dockerfile, dockerfile_inline, args, target) | built with BuildKit; services with the same build share one image |
| `image:` | pulled, with `registry:login` credentials |
| `command`, `entrypoint`, `user`, `working_dir` | the process's command and settings, as `docker run` applies them |
| `environment`, `env_file`, `${VAR}` | the service's environment. Config vars fill in `${VAR}` (they win over the repo's `.env`), but only reach a service through the file, as with docker compose |
| `ports:` | the published service gets the app's domains and HTTPS: the one named `web`, or the only one publishing ports. Its target port is the service's port |
| `expose:` or the image's `EXPOSE` | the service's port: `$PORT`, the TCP health check, and what others connect to |
| service names | DNS: `db` resolves from the app's other services, and `db.<app>.internal` from anywhere |
| named `volumes:` | local volumes, created on deploy and mounted per service (one service and one replica each) |
| `deploy.replicas`, `scale` | the instance count, unless `ps:scale` sets one |
| `deploy.resources.limits`, `cpus`, `mem_limit` | the VM's size, unless `resource:limit --process-type` sets one |
| `restart`, `deploy.restart_policy` | the restart policy |
| `stop_grace_period`, `stop_signal` (and the image's `STOPSIGNAL`) | how the app is stopped |
| `depends_on` | start order |
| bind mounts of repo files, on `image:` services | copied into the image (read-only in effect) |

Refused with a message, rather than half emulated:
- **Host access:** `privileged`, `cap_add`, `devices`, `network_mode`, `pid`, `ipc`, `sysctls`, `security_opt`.
- **Host paths:** bind mounts from outside the repo, such as `/var/run/docker.sock`.
- **Not supported yet:** `secrets`, `configs`, `include`, `extends` from another file, one-off services (`service_completed_successfully`), UDP ports, and two services publishing ports (name one `web`).
- **Volume limits:** a volume shared by two services, or replicas above one with a volume.

Ignored with a note in the deploy log:
- A `healthcheck:` command, which can't run inside the VM yet. Jokku checks the service's port instead.
- Anonymous volumes and `tmpfs`, which become the instance's own disk.

Every path the file names must resolve inside the pushed source, symlinks
included. A deploy runs as root on the control node and must not read
anything else.

A config change re-reads the last deploy's compose file with the new config
vars and reuses every image. Only the services whose environment or settings
change restart. A change that would alter how an image is built (a `${VAR}`
in a build arg, say) asks for `ps:rebuild`.

## Cluster join

New nodes join with the same installer, so there is one install path:

```
control$ jokku cluster:join-command
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh -s -- --join 203.0.113.10 --token jk1_...

new-node$ <paste it>
```

1. `cluster:join-command` asks the API for a short-lived join token. The
   token embeds the control CA fingerprint, which the joining node uses to
   pin TLS.
2. On the new machine, the installer sets up jokku as usual, then runs
   `jokku cluster:join 203.0.113.10 --token jk1_...` instead of initializing
   a control node.
3. The new node generates a WireGuard key and calls
   `POST https://control:7443/v1/cluster/join` with the token, its public
   key, endpoint, CPUs, memory and architecture.
4. Control assigns the next free `/24` and a node token, and adds the peer
   to every other node's desired state.
5. The node brings up `wg0` and its agent starts long-polling over the mesh.
   Done.

The same one-liner works unattended, for example in cloud-init user-data for
autoscaling groups.

### Edges

An edge is a public machine in front of nodes that the internet can't
reach. Joining works the other way round from a worker's, because the edge
can't reach the control node:

```
control$ jokku edge:add root@203.0.113.7
```

1. The control node makes the edge's identity (a `/24` like any node's),
   its credentials and its WireGuard key pair, records the edge with its
   public key and endpoint, and returns a bundle: identity, tokens, the
   private key, and the control node as its first peer (no endpoint).
2. The CLI runs `install.sh --edge <bundle>` on the edge over ssh (or prints
   the command, with `--print`). `jokku setup --edge` writes `node.json`
   (role `edge`, control at its mesh address `10.210.1.1:7443`) and the key,
   and starts only `jokku` and `jokku-proxy`.
3. Every other node now has the edge as a peer, with its endpoint, and dials
   it; `PersistentKeepalive` keeps retrying until the edge is up, and keeps
   NAT open after. The edge's agent starts from the bundle's peer, so its
   first poll goes over the tunnel the control node opened.

An edge's state has the proxy routes of every app, its peers without
endpoints or agent addresses, and through each node the external targets
that node forwards to (extra `AllowedIPs` and `/32` routes). It runs no VMs
(no KVM needed), no DNS service, is never scheduled, gets no root
filesystems (the API refuses), and has no vote in fencing or the restore
quorum.

The nodes behind an edge firewall it (`types.EdgeAccess`): over the mesh it
may reach the control node's API, and on or through each node only the
`IP:port` of that node's web instances and the external targets it
forwards to; everything else from the edge is dropped, ahead of the VM
network's rule that accepts mesh traffic. The edge in turn accepts only
nodes and replies over the mesh.

Removing an edge revokes its token at once but keeps it a peer for two
minutes, so its next poll gets a 401 and it drops its routes; then it is
deleted. An edge that never connected is deleted at once.

## State

SQLite (WAL) at `/var/lib/jokku/jokku.db` on the control node. Main tables:

| Table | Holds |
| --- | --- |
| `apps` | name, locked, current release |
| `config_vars` | per app, plus `app_id = 0` for global |
| `domains` | per app, plus global |
| `properties` | generic `(app, plugin, key) -> value` behind every Dokku-style `*:set` / `*:report` command (`git:set`, `checks:set`, `builder:set`, ...); the `builder` plugin's `type`, `file` and `image` are set only by `builder:dockerfile`, `builder:compose` and `builder:image` |
| `formations` / `resources` | `ps:scale` quantities and `resource:limit` sizes per process type |
| `releases` / `deploys` | immutable releases, deploy history and status |
| `instances` | desired and observed state of every microVM, with the volumes it mounts |
| `volumes` / `volume_mounts` | volumes (type, size, node, move in progress) and where they are mounted |
| `nodes` | mesh address, WireGuard key, capacity, health |
| `ssh_keys` / `api_tokens` | credentials |

Schema changes are numbered, append-only migrations applied at startup.

## Updates

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
```

**The scripts stay dumb; the binary knows what it needs.** `install.sh` and
`update.sh` only download and verify the `jokku` binary, then run `jokku
setup`. Setup brings the server to the state *that* release needs: packages,
the `jokku` user, directories, systemd units, and later Firecracker, the guest
kernel and WireGuard. Each step checks before it changes anything, so install,
update and repair are the same operation. A release that needs new server
state ships the step that creates it, and the curl scripts rarely change.

**Database changes** are numbered, append-only migrations applied when the
daemon starts.

**What `update.sh` does:**

1. Resolve the target: the latest release (via the `/releases/latest`
   redirect) or `JOKKU_VERSION`. Exit early if already on it.
2. Download the binary and verify it against the release's `checksums.txt`.
3. Stop jokku, then copy `jokku.db` (with its WAL) and the current binary to
   `/var/lib/jokku/backups/<time>-<version>/`. The newest five are kept.
4. Install the new binary and run `jokku setup`. Setup restarts jokku and waits
   until the API reports the new version, which proves the new code is
   running.
5. If any of that fails, restore the old binary and database and restart.

**Apps don't go down during updates.** The control API is unavailable for a
few seconds while jokku restarts. Apps and traffic are not affected:

- Each microVM runs in its own systemd unit, not as a child of the daemon.
  Restarting the daemon leaves VMs running, and the agent re-attaches to them
  on startup.
- The proxy is its own unit, `jokku-proxy.service`, running the same binary.
  Each release records a proxy version, and `jokku setup` restarts the proxy
  only when that version changed. The microVMs' DNS service,
  `jokku-dns.service`, works the same way.

**Clusters (milestone 2).** Update the control node first. Control accepts
agents one release behind, so workers can follow one at a time with the same
command. Later, `jokku cluster:update` will roll the whole cluster: workers
fetch the release from the control node, and since VMs survive agent
restarts, nothing needs draining.

A `sudo jokku update` command can wrap the same steps later. The curl script
stays the stable entry point, including for versions that predate the command.

## Filesystem and ports

```
/usr/local/bin/jokku              the binary
/etc/jokku/node.json              node identity: role, control URL, node token
/run/jokku/jokku.sock             API socket (control)
/var/lib/jokku/jokku.db           state (control)
/var/lib/jokku/git/<app>.git      bare repos, owned by the jokku user (control)
/var/lib/jokku/builds/<id>/       build workspace (control)
/var/lib/jokku/artifacts/         rootfs images by digest (cache on workers)
/var/lib/jokku/kernel/            vmlinux + initramfs
/var/lib/jokku/instances/<id>/    per-VM scratch disk, config drive, sockets, logs
/var/lib/jokku/volumes/           volume disks: <id>.ext4 (plus copies while one moves)
/var/lib/jokku/dns/zone.json      internal names the agent hands jokku-dns
/var/lib/jokku/backups/           database backups taken by update.sh
/home/jokku/.ssh/authorized_keys  generated from ssh-keys:*
```

| Port | Where | What |
| --- | --- | --- |
| 22/tcp | control | SSH: `git push`, remote CLI |
| 80, 443/tcp | ingress nodes | Caddy |
| 7443/tcp | control | HTTPS API (join, tokens) |
| 51820/udp | all nodes | WireGuard |
| 53/udp, 53/tcp | VM bridge only | `jokku-dns`: internal names for microVMs |
| 7444/tcp | mesh only | agent API: log streams, sessions with VMs, volume copies between nodes |

An edge needs only 80 and 443/tcp and 51820/udp open; the nodes behind it
need no open port at all.

## Differences from Dokku

| Area | Dokku | Jokku |
| --- | --- | --- |
| Isolation | containers | Firecracker microVMs |
| Machines | one server | cluster, same commands |
| Builders | herokuish, CNB, Dockerfile, nixpacks, ... | Dockerfile, registry images and compose files; buildpacks later |
| Proxy | nginx (pluggable) | embedded Caddy, automatic TLS |
| `storage:mount` | host directory, shared by all containers | named ext4 volume attached to one instance; it runs where the disk is, and the disk moves with it |
| Plugins | bash plugin ecosystem | none in v1; services (postgres, redis) later as apps plus volumes plus `*:link` |
| Releases | no rollback | `releases`, `releases:rollback` |
| Nodes | n/a | `nodes:*`, `cluster:*` |

## Milestones

- **M0 – control plane skeleton.** API, SQLite, CLI over socket and SSH,
  apps, config, domains, properties, ssh-keys, scale and resource records,
  `ssh-command`, git receive and hook, `install.sh`, `update.sh` with backup
  and rollback, `jokku setup`, and release binaries. *(done)*
- **M1 – single-node deploys.** *(done)* BuildKit build, OCI to ext4, guest init,
  Firecracker driver, bridge/TAP/NAT, agent reconcile loop, embedded
  Caddy, rollouts, checks, logs, `ps:*`. At this point it is a working
  Dokku replacement on one box.
- **M2 – cluster.** *(done)* WireGuard mesh, `cluster:join-command` and
  `install.sh --join`, `nodes:*`, scheduler, artifact distribution, cluster
  cert storage, failover, `jokku top`, events.
- **M3 – remote API.** HTTPS listener, tokens, `git:sync`, deploy keys,
  `builder:image` and `registry:login` (*done*), `git:from-archive`.
- **M4 – depth.** The Firecracker `jailer`, `run`, `enter` via vsock (*done*, with
  `storage:export`/`storage:import`), volumes (local disks that move with their instance:
  *done*; object storage next), backups to S3 (*done*: schedules, restores, and automatic restores with
  fencing), `releases:rollback`, app.json health checks,
  log drains, services.
