---
title: Coming from Dokku
description: Same commands, same git push. What changes is where your apps run and how many servers they can use.
---

Jokku keeps Dokku's command names, argument order and output. `apps:create`, `config:set`, `ps:scale`, `domains:add`, `ssh-keys:add` and `git push` all work the way you expect, and tools that speak Dokku's protocol, like `dokku/github-action`, work unchanged.

## What's different

| Area | Dokku | Jokku |
| --- | --- | --- |
| Isolation | Containers | Firecracker microVMs |
| Servers | One | A cluster, with the same commands |
| Builders | herokuish, CNB, Dockerfile, nixpacks, ... | Dockerfiles, compose files and registry images; buildpacks later |
| Proxy | nginx (pluggable) | Caddy, built in, with certificates shared across servers |
| `storage:mount` | A host directory, shared by all containers | A named volume attached to one instance; it runs where the disk is, and the disk moves with it |
| Plugins | The bash plugin ecosystem | None; what the common plugins do is built in |
| Databases | `postgres:*`, `mysql:*`, `redis:*` plugins | Built in, under one namespace: `db:postgres:*`, `db:mysql:*`, `db:redis:*`. See [Databases](/docs/databases) |
| Basic auth | The `http-auth` plugin | `http-auth:*`, with a login page, users with authenticator codes and share links. See [Logins](/docs/logins) |
| Backups | Per plugin, such as `postgres:backup` | Every volume, to S3-compatible buckets, restored onto another server if its own dies. See [Backups](/docs/backups) |
| Behind NAT | Needs open ports | [Edges](/docs/edges): a public server your servers dial |
| Releases | No rollback | `releases`, and `releases:rollback` planned |
| Servers | n/a | `nodes:*` and `cluster:*` |

## Choosing a builder

This is the one area that departs from Dokku. Rather than `builder:set selected` and per-builder settings, Jokku has one command per way of building:

```sh
jokku builder:dockerfile myapp                     # build the Dockerfile (the default)
jokku builder:dockerfile myapp docker/prod.Dockerfile
jokku builder:compose myapp                        # deploy a compose file
jokku builder:image myapp ghcr.io/you/myapp:v2     # run a registry image (replaces git:from-image)
```

`builder:set` still handles `build-dir` and `procfile`; the Procfile path moved there from `ps:set procfile-path`.

## Things to know

- **Volumes are disks, not directories.** A process with a volume runs one instance, and its deploys stop the old instance first. See [Volumes](/docs/storage).
- **Databases are `db:<engine>:*`**, not `postgres:*`: `db:postgres:create`, `db:postgres:link` and so on, with the same idea as Dokku's plugins. There are no aliases for the plugin names.
- **HTTPS is off for new apps**, as in Dokku. Turn it on with `letsencrypt:enable`.
- **`resource:reserve` isn't needed.** In a microVM, the memory limit is the reservation.
- **Apps listen on `$PORT`.** `ports:*` mapping is planned.
- **Not planned:** `nginx:*` (Caddy replaces nginx), `plugin:*` and `ps:inspect` (use `ps:report`).

See the [command reference](/docs/commands) for every command and its status.
