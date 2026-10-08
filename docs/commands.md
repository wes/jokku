# Command compatibility

Jokku keeps Dokku's command names, argument order and output. This table
tracks what exists today. Milestones are described in
[architecture.md](architecture.md#milestones).

✅ works now · 🔜 planned (milestone) · ➕ Jokku-only · ✖ not planned

## Apps and config

| Command | Status |
| --- | --- |
| `apps:list`, `apps:create`, `apps:destroy [--force]`, `apps:rename`, `apps:clone`, `apps:lock`, `apps:unlock`, `apps:exists`, `apps:report` | ✅ |
| `config:show`, `config:get [--quoted]`, `config:set [--no-restart] [--encoded]`, `config:unset`, `config:keys`, `config:export [--format]`, `config:clear`, all with `--global` and `--merged` | ✅ |
| `config <app>` (alias of `config:show`) | ✅ |

## Deploys

| Command | Status |
| --- | --- |
| `git push jokku main` (auto-creates the app; builds and deploys with zero downtime) | ✅ |
| `git:set`, `git:report` (`deploy-branch`, `keep-git-dir`) | ✅ |
| `builder:set` (`build-dir`), `builder-dockerfile:set` (`dockerfile-path`) | ✅ |
| `git:sync [--build]`, `git:from-image`, `git:from-archive` | 🔜 M3 |
| `git:generate-deploy-key`, `git:public-key`, `git:allow-host`, `git:auth` | 🔜 M3 |
| `ps:rebuild` | ✅ |
| `docker-options:add ... build` (build args) | 🔜 later |
| `releases` | ➕ ✅ |
| `releases:rollback` | ➕ 🔜 M4 |
| herokuish / CNB / nixpacks / railpack builders | ✖ for now (Dockerfile and images only) |

## Processes and resources

| Command | Status |
| --- | --- |
| `ps:scale <app> [type=n ...] [--skip-deploy]`, `ps:report`, `ps:set` (`restart-policy`, `procfile-path`) | ✅ |
| `ps:start`, `ps:stop`, `ps:restart` | ✅ |
| `ps:inspect` | ✖ use `ps:report` |
| `resource:limit [--process-type] [--cpu] [--memory]`, `resource:limit-clear`, `resource:report` | ✅ applied with a rolling restart |
| `resource:reserve` | ✖ in a microVM the limit *is* the reservation |
| `checks:set` (`wait-to-retire`, `timeout`), `checks:report` | ✅ |
| `checks:enable`, `checks:disable`, `checks:skip` | 🔜 later |
| `logs [-t] [-n N] [-p type] [-q]` | ✅ |
| `run`, `enter` | 🔜 M4 (vsock guest agent) |

## Routing

| Command | Status |
| --- | --- |
| `domains:add`, `domains:remove`, `domains:set`, `domains:clear`, `domains:report`, and the `-global` variants | ✅ |
| `proxy:enable`, `proxy:disable`, `proxy:report` | ✅ |
| `letsencrypt:enable`, `letsencrypt:disable`, `letsencrypt:set` (`email`), `letsencrypt:report` | ✅ automatic for public domains |
| `ports:list`, `ports:add`, `ports:set`, `ports:remove`, `ports:clear` | 🔜 later (apps listen on `$PORT`) |
| `certs:add`, `certs:remove`, `certs:report` | 🔜 M3 |
| `nginx:*` | ✖ Caddy replaces nginx |

## Server

| Command | Status |
| --- | --- |
| `jokku update [--yes] [--version vX]` (asks first; backs up and rolls back) | ➕ ✅ |
| `jokku setup` (repair or reapply server setup) | ➕ ✅ |

## Access

| Command | Status |
| --- | --- |
| `ssh-keys:add <name> [file]` (or stdin), `ssh-keys:remove [--fingerprint]`, `ssh-keys:list [--format json]` | ✅ |
| `ssh jokku@host <command>` with no local install | ✅ |
| `jokku` on the server, installed by `install.sh` | ✅ |
| Optional local client via the `jokku` git remote or `JOKKU_HOST` (not required) | ✅ |
| `tokens:create`, `tokens:list`, `tokens:remove` (HTTPS API) | ➕ 🔜 M3 |

## Cluster

| Command | Status |
| --- | --- |
| `cluster:join-command [--ttl 1h] [--reusable]` (prints an `install.sh --join` one-liner) | ➕ ✅ |
| `cluster:report` | ➕ ✅ |
| `nodes:list`, `nodes:report`, `nodes:set` (`schedulable`, `ingress`) | ➕ ✅ |
| `nodes:drain`, `nodes:undrain` (moves instances off with no downtime) | ➕ ✅ |
| `nodes:remove [--force]` | ➕ ✅ |
| `events [<app>] [-n N]` | ➕ ✅ |
| `top` (live terminal dashboard of nodes, apps, instances, events) | ➕ ✅ |
| Rolling update of every node from the control node | 🔜 (for now: `sudo jokku update` on each node) |
| Replicated control node | 🔜 later |

## Storage and services

| Command | Status |
| --- | --- |
| `storage:ensure-directory`, `storage:mount`, `storage:unmount`, `storage:list` | 🔜 M4 (ext4 volumes; pins the instance to a node) |
| `postgres:*`, `redis:*`, ... | 🔜 later, as apps plus volumes plus `*:link` |
| `plugin:*` | ✖ |
