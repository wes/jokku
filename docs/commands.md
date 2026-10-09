# Command compatibility

Jokku keeps Dokku's command names, argument order and output, except where
noted. This table tracks what exists today. Milestones are described in
[architecture.md](architecture.md#milestones).

✅ works now · 🔜 planned (milestone) · ➕ Jokku-only · ✏️ replaces a Dokku command · ✖ not planned

How an app is built is the one area that departs from Dokku: Dokku has no
compose builder, and choosing a builder and its file there takes commands in
several namespaces. In Jokku it is one command, `builder:dockerfile`,
`builder:compose` or `builder:image`.

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
| `builder:dockerfile <app> [<path>]` (the default builder; replaces `builder:set selected` and `builder-dockerfile:set dockerfile-path`) | ✏️ ✅ |
| `builder:compose <app> [<path>]`: deploy a compose file, a process type per service | ➕ ✅ |
| `builder:image <app> <image>` (replaces `git:from-image`; pulls the image; creates the app if needed; `ps:rebuild` pulls again; refuses `git push` until `builder:dockerfile`) | ✏️ ✅ |
| `builder:set` (`build-dir`, `procfile`; `procfile` replaces `ps:set procfile-path`), `builder:report` | ✅ |
| `registry:login [--password-stdin] <server> <username> [<password>]`, `registry:logout`, `registry:report` | ✅ logins apply to every app |
| `git:sync [--build]`, `git:from-archive` | 🔜 M3 |
| `git:generate-deploy-key`, `git:public-key`, `git:allow-host`, `git:auth` | 🔜 M3 |
| `ps:rebuild` | ✅ |
| `docker-options:add ... build` (build args) | 🔜 later |
| `releases` | ➕ ✅ |
| `releases:rollback` | ➕ 🔜 M4 |
| herokuish / CNB / nixpacks / railpack builders | ✖ for now (Dockerfile and images only) |

## Processes and resources

| Command | Status |
| --- | --- |
| `ps:scale <app> [type=n ...] [--skip-deploy]`, `ps:report`, `ps:set` (`restart-policy`; the Procfile path is `builder:set procfile`) | ✅ |
| `ps:start`, `ps:stop`, `ps:restart` | ✅ |
| `ps:inspect` | ✖ use `ps:report` |
| `resource:limit [--process-type] [--cpu] [--memory]`, `resource:limit-clear`, `resource:report` | ✅ applied with a rolling restart |
| `resource:reserve` | ✖ in a microVM the limit *is* the reservation |
| `checks:set` (`wait-to-retire`, `timeout`), `checks:report` | ✅ |
| `checks:enable`, `checks:disable`, `checks:skip` | 🔜 later |
| `logs [-t] [-n N] [-p type] [-q]`, with Heroku-style `app[router]` lines per request (`-p router` for only those) | ✅ |
| `enter <app> [<process>] [<command>...]` (a shell, or one command, in a running instance; `--root`) | ✅ |
| `run` (a one-off instance) | 🔜 M4 |

## Routing

| Command | Status |
| --- | --- |
| `domains:add`, `domains:remove`, `domains:set`, `domains:clear`, `domains:report`, and the `-global` variants | ✅ |
| `proxy:enable`, `proxy:disable`, `proxy:report` | ✅ |
| `letsencrypt:enable`, `letsencrypt:disable`, `letsencrypt:set` (`email`), `letsencrypt:report` | ✅ off for new apps, as in Dokku |
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
| `nodes:drain`, `nodes:undrain` (moves instances off with no downtime; instances with volumes stop briefly while their disks move) | ➕ ✅ |
| `nodes:remove [--force]` | ➕ ✅ |
| `events [<app>] [-n N]` | ➕ ✅ |
| `top` (live terminal dashboard of nodes, apps, instances, events, and a real-time Traffic view of requests) | ➕ ✅ |
| Rolling update of every node from the control node | 🔜 (for now: `sudo jokku update` on each node) |
| Replicated control node | 🔜 later |

## Storage and services

| Command | Status |
| --- | --- |
| `storage:mount <app> <name>:<path> [--process-type] [--size] [--no-restart]` (creates the volume if needed), `storage:unmount`, `storage:list`, `storage:report` | ✅ named volumes instead of host directories; a local volume is attached to one instance |
| `storage:create [--size] [--type]`, `storage:resize`, `storage:move <app> <name> <node>`, `storage:destroy [--force]` | ➕ ✅ |
| `storage:export <app> <name> [--live] > file.tar.gz`, `storage:import <app> <name> [--clear] [--keep-owners] < file.tar.gz` | ➕ ✅ |
| `storage:ensure-directory` | ✖ volumes are disks, created by `storage:mount` |
| Object-storage volumes (`--type s3`), shared by many instances | 🔜 later |
| `postgres:*`, `redis:*`, ... | 🔜 later, as apps plus volumes plus `*:link` |
| `plugin:*` | ✖ |
