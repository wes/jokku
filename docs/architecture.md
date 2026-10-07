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
   SSH forced-command, the git hook and the HTTP proxy (Caddy is embedded as
   a library). The only other binaries on a node are `firecracker`,
   `jailer`, `buildkitd` (control node) and the tiny in-VM `jokku-init`.
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
# on a fresh Ubuntu/Debian server with KVM
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
cat ~/.ssh/id_ed25519.pub | ssh root@server jokku ssh-keys:add admin

# on your laptop, in your app's repo (it has a Dockerfile)
go install github.com/wes/jokku/cmd/jokku@latest
git remote add jokku jokku@server:myapp
git push jokku main
# => https://myapp.203.0.113.10.sslip.io

jokku config:set myapp DATABASE_URL=postgres://...
jokku domains:add myapp myapp.com
jokku ps:scale myapp web=4 worker=2
jokku resource:limit myapp --cpu 2 --memory 1g --process-type web

# grow the cluster: run from your laptop, uses your SSH access to the box
jokku nodes:add root@203.0.113.11
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
 laptop CLI ---ssh jokku@control "jokku api:dial-stdio"--+
 server CLI (root) ----------------------------------------+--> /run/jokku/jokku.sock --> API
 git push --> sshd --> jokku ssh-command --> git hook ------+
 GUI / CI / curl ----- https://control:7443 + bearer token --> API
 agents -------------- WireGuard + node token -------------> API (/v1/agent/*)
```

- **Unix socket** `/run/jokku/jokku.sock`, mode `0660`, group `jokku`.
  Anyone who can open the socket is an admin, the same model as Docker.
- **SSH.** The laptop CLI runs `ssh jokku@host jokku api:dial-stdio` and
  speaks HTTP over the SSH session's stdio, the way `docker -H ssh://` does.
  SSH is only a transport and authenticator; the CLI is still an API
  client. The connection is reused for 60s via `ControlPersist`, so
  repeated commands are fast.
- **Plain `ssh jokku@host apps:list`** also works with no local install,
  exactly like Dokku: the forced command runs the CLI on the server, which
  calls the API over the socket.
- **HTTPS + token** (milestone 3) for CI systems and a future GUI:
  `jokku tokens:create ci` issues a bearer token. The control node
  generates a self-signed CA at init; clients pin it by fingerprint.

The laptop CLI finds its server the same way the Dokku client does: from a
git remote named `jokku` (`jokku@host:app`), which also supplies the default
`--app`. `JOKKU_HOST=jokku@host` overrides it, `JOKKU_SOCKET` forces a
local socket.

## Authentication

| Who | How | Command |
| --- | --- | --- |
| People and CI pushing code or running commands | SSH public keys, stored in SQLite, rendered into `~jokku/.ssh/authorized_keys` with a forced command | `ssh-keys:add <name> [file]` |
| Jokku pulling a private repo (`git:sync`) | A deploy key Jokku generates; you add its public half to the GitHub repo | `git:generate-deploy-key`, `git:public-key`, `git:allow-host github.com` |
| Jokku pulling over HTTPS | Stored credentials per host | `git:auth github.com <user> <token>` |
| HTTPS API clients | Bearer tokens (hashed at rest) | `tokens:create <name>` |
| Nodes | Join token once, then a per-node token over WireGuard | `cluster:join-command`, `nodes:add` |

Each `authorized_keys` line looks like:

```
restrict,pty,command="/usr/local/bin/jokku ssh-command --key-name admin" ssh-ed25519 AAAA...
```

`jokku ssh-command` reads `SSH_ORIGINAL_COMMAND` and dispatches:

| Original command | Action |
| --- | --- |
| `git-receive-pack 'app'` | Ensure the app and its bare repo exist, then run `git-receive-pack` |
| `git-upload-pack 'app'` | Let people clone what was deployed |
| `jokku api:dial-stdio` | Pipe stdio to the API socket (remote CLI) |
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

Each instance (`web.1`, `worker.2`, ...) is one Firecracker microVM
launched through the `jailer` (chroot, cgroup v2 limits, seccomp,
unprivileged uid).

| Piece | What it is |
| --- | --- |
| Kernel | One `vmlinux` per Jokku release, built with virtio, ext4, overlayfs, vsock and kernel IP autoconfig. |
| initramfs | Contains `jokku-init`, a small static Go binary. Shipping it outside the app image means a Jokku upgrade never requires rebuilding apps. |
| `vda` | The release's `rootfs.ext4`, attached read-only and shared by every instance of the release on that node. |
| `vdb` | A tiny config drive: JSON with the command, env, user, workdir, hostname and volume mounts. |
| `vdc` | A per-instance sparse scratch ext4 used as the overlay upper layer. Writable root, gone when the instance stops (12-factor, like Dokku's containers). |
| `vdd`+ | Persistent volumes from `storage:mount`. |
| `eth0` | A TAP device on the node's `jokku0` bridge, addressed via the kernel `ip=` boot argument. |

`jokku-init` boot sequence: mount `/proc`, `/sys` and `/dev`, read the
config drive, mount `vda` read-only and `vdc` as an overlay, mount volumes,
write `/etc/hosts` and `/etc/resolv.conf`, `switch_root`, then start the
process as the image's `USER`. It stays as PID 1 to reap zombies, forward
signals and run a small vsock guest agent that provides:

- the log stream (stdout/stderr, read by the node agent),
- `exec`, for `jokku enter app web.1` and `jokku run app <cmd>`,
- graceful stop: `SIGTERM`, then a grace period, then power off.

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

Caddy runs **embedded in the jokku daemon** on every ingress node and is
configured through its JSON API in-process. Embedding gives Jokku:

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

When an instance needs a home, the scheduler filters nodes that are online,
schedulable, have the release's architecture and have the memory free (no
memory overcommit; vCPUs may be overcommitted, configurable per node). It
then prefers nodes running the fewest instances of the same app and process
type (spread), then the least-loaded node. Volumes pin an instance to the
volume's node.

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

```
laptop$ jokku nodes:add root@203.0.113.11 [--name worker-1]
```

1. The CLI asks the API for a short-lived join token. The token embeds the
   control CA fingerprint, which the joining node uses to pin TLS.
2. Using **your** SSH access to the new machine, the CLI uploads and runs
   the installer: `jokku cluster:join 203.0.113.10 --token jk1_...`.
3. The new node generates a WireGuard key and calls
   `POST https://control:7443/v1/cluster/join` with the token, its public
   key, endpoint, CPUs, memory and architecture.
4. Control assigns the next free `/24` and a node token, and adds the peer
   to every other node's desired state.
5. The node brings up `wg0` and its agent starts long-polling over the mesh.
   Done.

If you cannot SSH to the machine from where you are, `jokku
cluster:join-command` prints the one-liner to paste there (for example in
cloud-init user-data for autoscaling groups).

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
  `ssh-command`, git receive and hook. *(this commit)*
- **M1 – single-node deploys.** BuildKit build, OCI to ext4, `jokku-init`,
  Firecracker driver, bridge/TAP/NAT, agent reconcile loop, embedded
  Caddy, rollouts, checks, logs, `ps:*`. At this point it is a working
  Dokku replacement on one box.
- **M2 – cluster.** WireGuard mesh, join, `nodes:*`, scheduler, artifact
  distribution, cluster cert storage, failover.
- **M3 – remote API.** HTTPS listener, tokens, `git:sync`, deploy keys,
  `git:from-image`, `git:from-archive`, and the installer (`install.sh` in
  this repo, fetching binaries from GitHub releases).
- **M4 – depth.** `run` and `enter` via vsock, volumes, `releases:rollback`,
  app.json health checks, log drains, services.
