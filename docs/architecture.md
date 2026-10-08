# Jokku architecture

Jokku is Dokku's workflow on Firecracker microVMs across a cluster of
machines. You `git push`, and Jokku builds the Dockerfile, boots the app as
microVMs, spreads them over your nodes and routes HTTPS traffic to them.

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
# => https://myapp.203.0.113.10.sslip.io

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
| `git:from-image app <image>` | Skips the build and converts the registry image. |
| HTTPS API | Upload a tarball, as above. |
| `ps:rebuild app` | Rebuilds the last deployed source. |

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
   (`builder-dockerfile:set app dockerfile-path Dockerfile.prod`) to an OCI
   image. Build args come from `docker-options:add app build "--build-arg X"`.
2. **Convert.** The image's layers are flattened (whiteouts applied) and
   written as an ext4 image; the image config is kept as JSON. This also
   handles `git:from-image`, so registry images and Dockerfile builds share
   one path.
3. **Release.** An immutable, numbered record: artifact digest, process
   types, config vars snapshot, resources. `config:set` creates a new release
   from the same artifact, as Heroku does.
4. **Rollout.** See *Zero-downtime rollouts* below.

Process types come from a `Procfile` in the repo root, as in Dokku. Without
one, the app has a single `web` process running the image's
`ENTRYPOINT`/`CMD`. The app listens on `$PORT`, which is the image's single
`EXPOSE`d port if it has one and `5000` otherwise.

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
| `vdd`+ | Persistent volumes from `storage:mount` (milestone 4). |
| `eth0` | A TAP device on the node's `jokku0` bridge, addressed via the kernel `ip=` boot argument. |

Boot sequence inside the VM: the kernel mounts `vda` read-only and runs
`/.jokku/init`, which reads the config drive, stages an overlay of `vda` and
`vdc` in a tmpfs, `pivot_root`s into it, mounts `/proc`, `/sys`, `/dev`,
`/run` and cgroups, brings up loopback, writes `/etc/hosts` and
`/etc/resolv.conf`, then starts the process as the image's `USER`. It stays
PID 1 to reap zombies. A stop request (Firecracker's Ctrl-Alt-Del on x86)
becomes `SIGTERM` to the app's process group, then `SIGKILL` after 10
seconds, then power off.

The app's stdout and stderr go to the VM's serial console, which is the
unit's output, so they land in the journal tagged with `JOKKU_APP` and
`JOKKU_PROCESS`; `jokku logs` reads them from there.

Not yet: running Firecracker under its `jailer` (chroot, unprivileged uid,
cgroup limits), and a vsock guest agent for `jokku run` and `jokku enter`.
Both are planned; until the jailer lands, treat apps on one server as
trusting each other, as with Dokku.

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
HTTPS URL.

TLS is automatic (Let's Encrypt via Caddy) for every domain that is not an
IP address. The `letsencrypt:*` commands exist for Dokku muscle memory
(`letsencrypt:set --global email you@x.com`, `letsencrypt:disable app`),
and `certs:add` installs a custom certificate.

`ports:set app http:80:5000 https:443:5000` mirrors Dokku's port mapping.
Raw TCP/UDP ports are a later addition.

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
with the most free memory. Volumes will pin an instance to the volume's node.

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

### Scaling and failure

- **Horizontal:** `ps:scale app web=4` adds or removes instances of the
  current release across nodes.
- **Vertical:** `resource:limit` sets vCPU and memory per process type and
  triggers a rollout.
- **Node failure:** a node silent for 30s is marked `down`. Its instances
  are rescheduled elsewhere (except volume-pinned ones). When it comes
  back, it receives a desired state without them and stops them.
- **Draining:** `nodes:drain node2` moves everything off before
  maintenance.

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

## State

SQLite (WAL) at `/var/lib/jokku/jokku.db` on the control node. Main tables:

| Table | Holds |
| --- | --- |
| `apps` | name, locked, current release |
| `config_vars` | per app, plus `app_id = 0` for global |
| `domains` | per app, plus global |
| `properties` | generic `(app, plugin, key) -> value` behind every Dokku-style `*:set` / `*:report` command (`git:set`, `checks:set`, `builder-dockerfile:set`, ...) |
| `formations` / `resources` | `ps:scale` quantities and `resource:limit` sizes per process type |
| `releases` / `deploys` | immutable releases, deploy history and status |
| `instances` | desired and observed state of every microVM |
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
  only when that version changed.

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
/var/lib/jokku/volumes/           persistent volumes
/var/lib/jokku/backups/           database backups taken by update.sh
/home/jokku/.ssh/authorized_keys  generated from ssh-keys:*
```

| Port | Where | What |
| --- | --- | --- |
| 22/tcp | control | SSH: `git push`, remote CLI |
| 80, 443/tcp | ingress nodes | Caddy |
| 7443/tcp | control | HTTPS API (join, tokens) |
| 51820/udp | all nodes | WireGuard |
| 7444/tcp | mesh only | agent streams (logs, exec) |

## Differences from Dokku

| Area | Dokku | Jokku |
| --- | --- | --- |
| Isolation | containers | Firecracker microVMs |
| Machines | one server | cluster, same commands |
| Builders | herokuish, CNB, Dockerfile, nixpacks, ... | Dockerfile and images first; buildpacks later |
| Proxy | nginx (pluggable) | embedded Caddy, automatic TLS |
| `storage:mount` | host directory, shared by all containers | ext4 volume attached to one VM; pins it to a node |
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
  `git:from-image`, `git:from-archive`.
- **M4 – depth.** The Firecracker `jailer`, `run` and `enter` via vsock, volumes, `releases:rollback`,
  app.json health checks, log drains, services.
