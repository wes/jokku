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
| `git push jokku main` (auto-creates the app) | ✅ received, inspected, recorded; build and boot 🔜 M1 |
| `git:set`, `git:report` (`deploy-branch`, `keep-git-dir`) | ✅ |
| `builder:set` (`selected`, `build-dir`), `builder-dockerfile:set` (`dockerfile-path`) | ✅ settings; used by the M1 builder |
| `git:sync [--build]`, `git:from-image`, `git:from-archive` | 🔜 M3 |
| `git:generate-deploy-key`, `git:public-key`, `git:allow-host`, `git:auth` | 🔜 M3 |
| `ps:rebuild`, `docker-options:add ... build` (build args) | 🔜 M1 |
| `releases`, `releases:rollback` | ➕ 🔜 M4 |
| herokuish / CNB / nixpacks / railpack builders | ✖ for now (Dockerfile and images only) |

## Processes and resources

| Command | Status |
| --- | --- |
| `ps:scale <app> [type=n ...]`, `ps:report`, `ps:set` (`restart-policy`, `procfile-path`) | ✅ records; rollout 🔜 M1 |
| `ps:start`, `ps:stop`, `ps:restart`, `ps:inspect` | 🔜 M1 |
| `resource:limit [--process-type] [--cpu] [--memory]`, `resource:limit-clear`, `resource:report` | ✅ records; applied 🔜 M1 |
| `resource:reserve` | ✖ in a microVM the limit *is* the reservation |
| `checks:set` (`wait-to-retire`), `checks:report` | ✅ |
| `checks:enable`, `checks:disable`, `checks:skip` | 🔜 M1 |
| `logs [-t] [-n N] [-p type]` | 🔜 M1 |
| `run`, `enter` | 🔜 M4 (vsock guest agent) |

## Routing

| Command | Status |
| --- | --- |
| `domains:add`, `domains:remove`, `domains:set`, `domains:clear`, `domains:report`, and the `-global` variants | ✅ |
| `proxy:enable`, `proxy:disable`, `proxy:report` | ✅ settings; Caddy 🔜 M1 |
| `letsencrypt:enable`, `letsencrypt:disable`, `letsencrypt:set` (`email`), `letsencrypt:report` | ✅ settings; certificates 🔜 M1 |
| `ports:list`, `ports:add`, `ports:set`, `ports:remove`, `ports:clear` | 🔜 M1 |
| `certs:add`, `certs:remove`, `certs:report` | 🔜 M3 |
| `nginx:*` | ✖ Caddy replaces nginx |

## Access

| Command | Status |
| --- | --- |
| `ssh-keys:add <name> [file]` (or stdin), `ssh-keys:remove [--fingerprint]`, `ssh-keys:list [--format json]` | ✅ |
| `ssh jokku@host <command>` with no local install | ✅ |
| Laptop CLI via the `jokku` git remote or `JOKKU_HOST` | ✅ |
| `tokens:create`, `tokens:list`, `tokens:remove` (HTTPS API) | ➕ 🔜 M3 |

## Cluster

| Command | Status |
| --- | --- |
| `nodes:list`, `nodes:report`, `nodes:set` (`schedulable`, `ingress`), `nodes:drain`, `nodes:undrain` | ➕ ✅ (control node only until M2) |
| `nodes:add <ssh-target>`, `nodes:remove`, `cluster:join`, `cluster:join-command` | ➕ 🔜 M2 |

## Storage and services

| Command | Status |
| --- | --- |
| `storage:ensure-directory`, `storage:mount`, `storage:unmount`, `storage:list` | 🔜 M4 (ext4 volumes; pins the instance to a node) |
| `postgres:*`, `redis:*`, ... | 🔜 later, as apps plus volumes plus `*:link` |
| `plugin:*` | ✖ |
