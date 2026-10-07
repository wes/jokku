# Jokku

Dokku's workflow on Firecracker microVMs, across a cluster.

```sh
git push jokku main            # builds your Dockerfile, boots microVMs, routes HTTPS
jokku ps:scale myapp web=4     # spread across every node in the cluster
jokku resource:limit myapp --cpu 2 --memory 1g
jokku nodes:add root@203.0.113.11   # one command to grow the cluster
```

- **Dokku's commands, unchanged.** `apps:create`, `config:set`,
  `domains:add`, `ps:scale`, `ssh-keys:add`, `git push`. Output looks the
  same too.
- **MicroVMs, not containers.** Each instance is a Firecracker VM with its own
  kernel, sized per process type.
- **A cluster from day one.** One server is a complete Jokku. Nodes join over
  SSH and talk over a WireGuard mesh. The scheduler spreads instances, and
  embedded Caddy routes traffic with automatic TLS from any node.
- **API first.** The CLI is a client of an HTTP API, so a GUI or other tools
  can be built on the same calls.
- **One binary.** Go, SQLite, embedded Caddy. No Kubernetes, no etcd.

> **Status: milestone 0.** The control plane, API, CLI, SSH access and
> `git push` plumbing work. Building and booting microVMs is milestone 1. See
> [docs/architecture.md](docs/architecture.md) for the design and
> [docs/commands.md](docs/commands.md) for which commands work today.

## How it fits together

```
laptop ──ssh──▶ control node ──▶ API (/v1) ──▶ SQLite
  git push        sshd forced command           scheduler ──▶ agents on every node
  jokku CLI       (jokku ssh-command)           builder         └─ Firecracker VMs
                                                                └─ Caddy (HTTPS)
                       nodes connected by a WireGuard mesh
```

Every way in ends at the same API: the CLI on the server uses a unix socket,
the CLI on your laptop tunnels HTTP over SSH (`ssh jokku@host jokku
api:dial-stdio`), `git push` goes through a pre-receive hook, and CI or a GUI
can use HTTPS with a token (milestone 3).

## Install

The laptop CLI works today (Go required):

```sh
go install github.com/wes/jokku/cmd/jokku@latest
```

The server installer arrives in milestone 3. The target experience:

```sh
# server: Ubuntu/Debian with KVM
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
cat ~/.ssh/id_ed25519.pub | ssh root@server jokku ssh-keys:add admin

# laptop, in a repo with a Dockerfile
git remote add jokku jokku@server:myapp
git push jokku main
```

The `jokku` CLI on your laptop finds the server and the app from the `jokku`
git remote, like the Dokku client. Without a local install, `ssh
jokku@server config:set myapp KEY=value` works too.

### Deploying from GitHub Actions

Add a deploy key with `jokku ssh-keys:add github-actions key.pub` and push.
The protocol is the same as Dokku's, so `dokku/github-action` should work
as-is (not yet tested against a live server):

```yaml
- uses: actions/checkout@v4
  with: { fetch-depth: 0 }
- uses: dokku/github-action@master
  with:
    git_remote_url: ssh://jokku@203.0.113.10:22/myapp
    ssh_private_key: ${{ secrets.JOKKU_SSH_KEY }}
```

## Development

Requires Go (the toolchain in `go.mod` is fetched automatically). The control
plane runs on macOS or Linux; microVMs need Linux with KVM.

```sh
git clone https://github.com/wes/jokku && cd jokku
make test
make dev                                   # daemon with state in .dev/
export JOKKU_SOCKET=$PWD/.dev/jokku.sock   # in another shell
bin/jokku apps:create myapp
bin/jokku config:set myapp KEY=value
make linux                                 # static binaries for servers
```

Layout:

| Path | What |
| --- | --- |
| `cmd/jokku` | the single binary |
| `internal/cli` | Dokku-style commands; API clients only |
| `internal/client` | Go API client (unix socket, SSH) |
| `internal/api` | HTTP API handlers |
| `internal/store` | SQLite state and migrations |
| `internal/props` | the `<plugin>:set` properties registry |
| `internal/deploy` | source inspection now; build and rollout in M1 |
| `internal/daemon` | wires a node together |
| `internal/sshkeys`, `internal/gitrepo` | `authorized_keys` and push repos |
