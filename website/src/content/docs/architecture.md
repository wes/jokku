---
title: How it works
description: One binary, Firecracker microVMs, a WireGuard mesh and boring state. A tour of what happens between git push and a live URL.
---

## Principles

1. **Dokku's experience, kept as-is.** Command names, argument order, output style (`----->`, `=====>`), `git push` deploys. A Dokku user should be at home on day one.
2. **One binary.** `jokku` is the CLI, the API server, the agent on every server, the git hook, the HTTP proxy (Caddy, built in as a library) and even the init process inside every microVM. The only other programs it needs are Firecracker and BuildKit.
3. **The API is the product.** Every command is an HTTP call to the API. Nothing in the CLI touches the database or files directly.
4. **One server is a complete cluster.** A single server running Jokku is exactly Dokku. Adding servers is one command and changes nothing else.
5. **Boring state.** SQLite on the control server is the source of truth. Servers converge on the desired state by reconciling, so restarts and crashes are uneventful.

## Topology

```text
                         DNS: *.example.com -> any server(s)
                                      |
        +-----------------------------+-----------------------------+
        |                             |                             |
  +-----v-------------+        +------v------------+        +------v------------+
  | control server    |        | worker server     |        | worker server     |
  |                   |        |                   |        |                   |
  |  - API + SQLite   |        |  - agent          |        |  - agent          |
  |  - scheduler      |        |  - proxy (Caddy)  |        |  - proxy (Caddy)  |
  |  - builder        |        |                   |        |                   |
  |  - git repos      |        | firecracker VMs   |        | firecracker VMs   |
  |  - agent + proxy  |        |  10.210.2.0/24    |        |  10.210.3.0/24    |
  | firecracker VMs   |        |                   |        |                   |
  |  10.210.1.0/24    |        |                   |        |                   |
  +---------+---------+        +---------+---------+        +---------+---------+
            |                            |                            |
            +============= WireGuard mesh (wg0, udp/51820) ===========+
```

The **control server** runs the API, the database, the scheduler, the builder and your git repositories. It's also a worker unless you mark it unschedulable. **Workers** run the agent, which manages microVMs, and the proxy. They hold no authoritative state.

Losing the control server stops deploys and changes, not running apps: agents and proxies keep serving their last known state.

## From git push to a release

Every deploy, however it starts, becomes the same thing: a source tarball, or an image reference, posted to the API.

```text
source.tar --> BuildKit (Dockerfile) --> OCI image --> flatten layers --> rootfs.ext4
                                              |                               |
                                         image config                content-addressed
                                     (ENTRYPOINT, CMD, ENV,           artifact on the
                                      WORKDIR, USER, EXPOSE)          control server
                                              |                               |
                                              +---------> release <-----------+
                                                 (artifact + Procfile + config vars
                                                  + resources, immutable, versioned)
                                                             |
                                                          rollout
```

1. **Build.** BuildKit builds the Dockerfile to an OCI image.
2. **Convert.** The image's layers are flattened and written as an ext4 disk image. Registry images take the same path, so they and Dockerfile builds share one pipeline.
3. **Release.** An immutable, numbered record of the artifact, process types, config vars and sizes. `config:set` creates a new release from the same artifact, as Heroku does.
4. **Rollout.** New instances boot, pass checks, take traffic, and the old ones retire. See [zero-downtime deploys](/docs/processes#zero-downtime-deploys).

## Inside a microVM

Each instance (`web.1`, `worker.2`, ...) is one Firecracker microVM in its own systemd unit, not a child of the daemon, so restarting or updating Jokku leaves apps running.

| Piece | What it is |
| --- | --- |
| Kernel | Firecracker's guest kernel (6.1), pinned by checksum. |
| Root disk | The release's filesystem, read-only and shared by every instance of the release on that server. |
| `/.jokku/init` | The jokku binary itself, started by the kernel as PID 1. |
| Config drive | A tiny disk with the command, environment, user, working directory and DNS. |
| Scratch disk | A sparse, writable layer on top of the root disk, kept across restarts and discarded when the instance is replaced. |
| Volumes | ext4 disk images for [volumes](/docs/storage), flushed through to the server's disk when the app calls `fsync`. |
| Network | A TAP device on the server's bridge. |

On boot, init reads the config drive, stacks the writable layer over the read-only root, mounts volumes, and starts your process as the image's `USER`. It stays PID 1 to reap zombies. Stopping sends `SIGTERM` to the app, then `SIGKILL` after 10 seconds. The app's output goes to the VM's serial console and into the system journal, which is where `jokku logs` reads it.

Init also serves `jokku enter` and volume backups over vsock, Firecracker's host-to-guest channel, with a token only the server's agent holds.

> [!WARNING]
> Firecracker's `jailer` (chroot, unprivileged user, cgroup limits) isn't used yet. Until it is, treat apps on one server as trusting each other, as with Dokku.

## The control loop

Agents **pull** their state; the control server never needs to dial in to a server to change things.

```text
agent                                         control
  | GET /v1/agent/state?since=<rev>  (long-poll) |
  |--------------------------------------------->|  desired: instances for this server,
  |<---------------------------------------------|  WireGuard peers, proxy routes
  | reconcile: boot/stop VMs, wg peers, Caddy    |
  | POST /v1/agent/status  (every 5s + on change)|
  |--------------------------------------------->|  observed: instance states,
  |                                              |  health, server resources
```

- The long-poll returns as soon as anything changes, so rollouts reach every server in well under a second.
- Reconciling is idempotent: boot what should run and isn't, stop what runs and shouldn't. Rebooting a server needs no special handling.
- Each agent caches its last state on disk, so it keeps serving through a control-server outage and its own restarts. A restarted agent adopts the VMs already running instead of starting them twice.

## Routing

Caddy is built into the jokku binary and runs on every server as its own service, so restarting Jokku never interrupts traffic. The control server computes routes: each app's domains map to every healthy `web` instance of its current release. Caddy load-balances across them and stops sending traffic to instances that fail.

Certificates, ACME accounts and challenge tokens are stored on the control server and shared by the whole cluster.

## Files and ports

```text
/usr/local/bin/jokku              the binary
/var/lib/jokku/jokku.db           state (control)
/var/lib/jokku/git/<app>.git      bare repos (control)
/var/lib/jokku/artifacts/         root filesystems by digest
/var/lib/jokku/instances/<id>/    per-VM scratch disk, config drive, sockets
/var/lib/jokku/volumes/           volume disks, and old copies (.stale) kept after a volume was restored elsewhere
/var/lib/jokku/backups/           database backups taken by updates (backups to S3 go straight to the bucket)
```

| Port | Where | What |
| --- | --- | --- |
| 22/tcp | control | SSH: `git push` and commands |
| 80, 443/tcp | every server that takes traffic | Caddy |
| 7443/tcp | control | HTTPS API (joining servers) |
| 51820/udp | every server | WireGuard |
| 53 | VM bridge only | `jokku-dns`, internal names for microVMs |
| 7444/tcp | mesh only | Agent API: log streams, sessions, volume copies and backups, and whether a server still hears from the control server |

The full design document, with every decision recorded, lives in the repo at [docs/architecture.md](https://github.com/wes/jokku/blob/main/docs/architecture.md).
