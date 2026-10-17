---
title: Commands
description: Every command, and what works today. Jokku keeps Dokku's command names, argument order and output, except where noted.
---

Every row has a status: ✅ ➕ ✏️ 🔜 ✖. A command marked *Replaces Dokku’s* takes over from a Dokku command, named beside it.

Run any of them over SSH as `ssh jokku@your-server <command>`, or on the server as `sudo jokku <command>`.

## Apps and config

| Command | Status |
| --- | --- |
| `apps:list`, `apps:create`, `apps:destroy [--force]`, `apps:rename`, `apps:clone`, `apps:lock`, `apps:unlock`, `apps:exists`, `apps:report` | ✅ |
| `config:show`, `config:get [--quoted]`, `config:set [--no-restart] [--encoded]`, `config:unset`, `config:keys`, `config:export [--format]`, `config:clear`, all with `--global` and `--merged` | ✅ |
| `config <app>` (alias of `config:show`) | ✅ |

## Deploys

How an app is built is the one area that departs from Dokku. Dokku has no compose builder, and choosing a builder and its file takes commands in several namespaces. In Jokku it is one command: `builder:dockerfile`, `builder:compose` or `builder:image`.

| Command | Status |
| --- | --- |
| `git push jokku main` (creates the app if needed; builds and deploys with zero downtime) | ✅ |
| `git:set`, `git:report` (`deploy-branch`, `keep-git-dir`) | ✅ |
| `builder:dockerfile <app> [<path>]`, the default builder (replaces `builder:set selected` and `builder-dockerfile:set dockerfile-path`) | ✏️ ✅ |
| `builder:compose <app> [<path>]`: deploy a compose file, a process type per service | ➕ ✅ |
| `builder:image <app> <image>` (replaces `git:from-image`; pulls the image and creates the app if needed) | ✏️ ✅ |
| `builder:set` (`build-dir`, `procfile`), `builder:report` | ✅ |
| `registry:login [--password-stdin] <server> <username> [<password>]`, `registry:logout`, `registry:report` | ✅ logins apply to every app |
| `ps:rebuild` | ✅ |
| `releases` | ➕ ✅ |
| `git:sync [--build]`, `git:from-archive` | 🔜 |
| `git:generate-deploy-key`, `git:public-key`, `git:allow-host`, `git:auth` | 🔜 |
| `docker-options:add ... build` (build args) | 🔜 |
| `releases:rollback` | ➕ 🔜 |
| herokuish, CNB, nixpacks and railpack builders | ✖ for now: Dockerfiles, compose files and images |

## Processes and resources

| Command | Status |
| --- | --- |
| `ps:scale <app> [type=n ...] [--skip-deploy]`, `ps:report`, `ps:set` (`restart-policy`) | ✅ |
| `ps:set <app> sleep-after <duration>` | ➕ ✅ the app's instances stop after that long without requests, and the next request starts them again ([how](/docs/processes#sleep-when-idle)) |
| `ps:start`, `ps:stop`, `ps:restart` | ✅ `ps:start` also wakes a sleeping app |
| `resource:limit [--process-type] [--cpu] [--memory]`, `resource:limit-clear`, `resource:report` | ✅ applied with a rolling restart |
| `checks:set` (`wait-to-retire`, `timeout`), `checks:report` | ✅ |
| `logs [-t] [-n N] [-p type] [-q]`, with `app[router]` lines per request (`-p router` for only those) | ✅ |
| `enter <app> [<process>] [<command>...]` (a shell, or one command, in a running instance; `--root`) | ✅ |
| `checks:enable`, `checks:disable`, `checks:skip` | 🔜 |
| `run` (a one-off instance) | 🔜 |
| `ps:inspect` | ✖ use `ps:report` |
| `resource:reserve` | ✖ in a microVM, the limit is the reservation |

## Routing

| Command | Status |
| --- | --- |
| `domains:add`, `domains:remove`, `domains:set`, `domains:clear`, `domains:report`, and the `-global` variants | ✅ |
| `proxy:enable`, `proxy:disable`, `proxy:report` | ✅ |
| `proxy:set` (`compress`: compression, on by default) | ➕ ✅ Dokku's `proxy:set` picks a proxy type instead |
| `letsencrypt:enable`, `letsencrypt:disable`, `letsencrypt:set` (`email`), `letsencrypt:report` | ✅ off for new apps, as in Dokku |
| `certs:add`, `certs:remove`, `certs:report` | 🔜 |
| `ports:list`, `ports:add`, `ports:set`, `ports:remove`, `ports:clear` | 🔜 apps listen on `$PORT` for now |
| `external:create <name> <url> [--via NODE] [--insecure]`, `external:list`, `external:info`, `external:set` (`url`, `via`, `insecure`), `external:destroy` (route domains to services Jokku doesn't run, on your network) | ➕ ✅ |
| `http-auth:enable <app> [<user>...] [--password]`, `http-auth:disable`, `http-auth:set-password`, `http-auth:report` | ✅ a login page instead of basic auth; users can have authenticator codes |
| `http-auth:add-allowed-ip`, `http-auth:remove-allowed-ip`, `http-auth:add-bypass-path`, `http-auth:remove-bypass-path` | ✅ |
| `http-auth:share <app> [--expires 24h] [--note]`, `http-auth:shares`, `http-auth:unshare` (links that let their holder in until they expire) | ➕ ✅ |
| `http-auth:users:add <name> [--totp]`, `http-auth:users:list`, `http-auth:users:passwd`, `http-auth:users:totp [--off]`, `http-auth:users:remove`, `http-auth:set --global` (`login-domain`, `session-days`) | ➕ ✅ |
| `firewall:report`, `firewall:unban <ip>`, `firewall:add-allowed-ip`, `firewall:remove-allowed-ip`, `firewall:enable`/`firewall:disable` (`<app>` or `--global`), `firewall:set` (`not-found`, `window`, `ban`, `rate`, `burst`): bans addresses that scan for weaknesses and slows floods, on by default | ➕ ✅ |
| `nginx:*` | ✖ Caddy replaces nginx |

## Storage and services

| Command | Status |
| --- | --- |
| `storage:mount <app> <name>:<path> [--process-type] [--size] [--no-restart]`, `storage:unmount`, `storage:list`, `storage:report` | ✅ named volumes instead of host directories |
| `storage:create [--size] [--type]`, `storage:resize`, `storage:move <app> <name> <node>`, `storage:destroy [--force]` | ➕ ✅ |
| `storage:export <app> <name> [--live]`, `storage:import <app> <name> [--clear] [--keep-owners]` | ➕ ✅ |
| `storage:discard-old-copy <app> <name>` (the disk a server kept after its volume was restored elsewhere) | ➕ ✅ |
| Object-storage volumes (`--type s3`), shared by many instances | 🔜 |
| `db:postgres:*`, `db:mysql:*`, `db:redis:*`: `create [--image] [--image-version] [--size] [--memory]`, `link [--alias] [--no-restart]`, `unlink`, `connect`, `export`, `import` (Postgres, MySQL), `info [--dsn] [--public-dsn]`, `list`, `logs`, `restart`, `stop`, `start`, `destroy`; and `db:list` | ✏️ ✅ replaces Dokku's `postgres:*`, `mysql:*` and `redis:*` plugins |
| `db:<engine>:expose <name> <domain> [--port] [--allow <ips>] [--lan]`, `db:<engine>:unexpose` (clients outside reach a database through the proxies, edges included unless `--lan` keeps it on your network: Postgres and Redis over TLS, sharing 5432 and 6379 and told apart by domain; MySQL on a port of its own) | ➕ ✅ |
| `db:<engine>:users:list <name> [--dsn]`, `users:add <name> <user> [--read-only]`, `users:passwd`, `users:remove` (a Postgres or MySQL database's logins: Jokku makes the password, and a user reads and writes data but not the schema, or only reads) | ➕ ✅ |
| `db:sqlite:*` (a SQLite file on an app's volume) | ➕ 🔜 next |
| `backups:destination-add <name> --endpoint --bucket [--region] --access-key-id [--no-encrypt]`, `backups:destinations`, `backups:destination-remove`, `backups:key` | ➕ ✅ S3-compatible buckets; encrypted with a key you keep |
| `backups:set <app> <volume> <destination> [<path>] [--every] [--keep-recent] [--keep-daily] [--auto-restore on\|off]`, `backups:unset`, `backups:run`, `backups:list`, `backups:restore <app> <volume> [<backup>] [--node] [--skip-backup]`, `backups:report` | ➕ ✅ incremental, block by block; every 15 minutes by default; restored onto another server when its own has been down five minutes |
| `backups:cluster <destination> [<path>] [--every] [--keep-recent] [--keep-daily]`, `backups:cluster-run`, `backups:cluster-list`, `backups:cluster-unset` | ➕ ✅ the control server's database, identity and recent releases, hourly once a destination exists |
| `storage:ensure-directory` | ✖ volumes are disks, created by `storage:mount` |
| `plugin:*` | ✖ |

## Cluster

| Command | Status |
| --- | --- |
| `cluster:join-command [--ttl 1h] [--reusable]` (prints an `install.sh --join` one-liner) | ➕ ✅ |
| `cluster:report` | ➕ ✅ |
| `nodes:list`, `nodes:report`, `nodes:set` (`schedulable`, `ingress`) | ➕ ✅ |
| `nodes:drain`, `nodes:undrain` | ➕ ✅ |
| `nodes:rebalance [--dry-run] [--volumes]` | ➕ ✅ |
| `nodes:remove [--force]` | ➕ ✅ |
| `network:test [--seconds N]`, `network:report` (tests every link between the nodes over the mesh, one at a time: throughput, round trips and lost pings, and how each node compares with the test before) | ➕ ✅ |
| `edge:add [<user>@]<address> [--name] [--print]` (a public server routing your domains to nodes behind NAT; installs it over ssh), `edge:list`, `edge:remove [--force]` | ➕ ✅ |
| `events [<app>] [-n N]` | ➕ ✅ |
| `top`, a live dashboard of servers, apps, instances, events, traffic and backups | ➕ ✅ |
| Updating every server from the control server | 🔜 |
| A replicated control server | 🔜 |

## Access and server

| Command | Status |
| --- | --- |
| `ssh-keys:add <name> [file]` (or stdin), `ssh-keys:remove [--fingerprint]`, `ssh-keys:list [--format json]` | ✅ |
| `ssh jokku@host <command>`, with nothing installed locally | ✅ |
| `jokku update [--yes] [--version vX] [--this-node]` (asks first; backs up and rolls back; on the control server, then updates the other servers one at a time) | ➕ ✅ |
| `jokku setup` (repair or reapply server setup) | ➕ ✅ |
| `jokku splash`, `splash:enable`, `splash:disable` (the logo and live figures about the server and the cluster at login) | ➕ ✅ |
| `jokku restore-cluster --endpoint --bucket --access-key-id [--path] [--backup]` (as root, on a new control server, from its backups) | ➕ ✅ |
| `tokens:create`, `tokens:list`, `tokens:remove` (an HTTPS API for CI and tools) | ➕ 🔜 |
