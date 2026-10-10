---
title: Databases
description: Run Postgres, MySQL and Redis for your apps, each on a volume of its own, linked with a connection URL.
---

## Create and link a database

Jokku runs Postgres, MySQL and Redis for your apps, each from its official image:

```sh
jokku db:postgres:create shopdb
jokku db:postgres:link shopdb myapp
```

Linking sets `DATABASE_URL` on the app (`REDIS_URL` for Redis) and restarts it, so it connects right away. The database is reachable only from your apps, at `postgres-shopdb.internal`, on any server in the cluster.

| Engine | Commands | Default |
| --- | --- | --- |
| Postgres | `db:postgres:*` | Postgres 17, 512 MB of memory |
| MySQL | `db:mysql:*` | MySQL 8.4, 1 GB |
| Redis | `db:redis:*` | Redis 7 with append-only persistence, 256 MB |

Each gets a 10 GB [volume](/docs/storage). Choose otherwise when you create it:

```sh
jokku db:postgres:create shopdb --image-version 16 --size 50g --memory 1g
```

## Work with it

```sh
jokku db:postgres:connect shopdb       # a psql shell (mysql or redis-cli for the others)
jokku db:postgres:info shopdb          # status, address, links and backups
jokku db:postgres:info shopdb --dsn    # just its URL
jokku db:list                          # every database
```

| Command | What it does |
| --- | --- |
| `db:<engine>:link <name> <app> [--alias NAME]` | Set the URL on an app (`<NAME>_URL` with `--alias`) and restart it; `--no-restart` to wait. |
| `db:<engine>:unlink <name> <app>` | Remove it, and restart the app. |
| `db:<engine>:logs <name> [-t]` | Its log output. |
| `db:<engine>:restart`, `stop`, `start` | As for an app. |
| `db:<engine>:destroy <name>` | Delete it and its data. Refused while it's linked; its backups stay in their bucket. |

## Export and import

```sh
jokku db:postgres:export shopdb > shopdb.dump    # pg_dump; mysqldump for MySQL, an RDB snapshot for Redis
jokku db:postgres:import shopdb < shopdb.dump    # Postgres and MySQL
```

Over SSH, leave out `-t` so the dump passes through untouched: `ssh jokku@your-server db:postgres:export shopdb > shopdb.dump`.

## Backups

With a [backup destination](/docs/backups), a database is backed up every 15 minutes from the moment it's created, and if its server dies it comes back on another one from its latest backup.

## Under the hood

A database is an app named after its engine, such as `postgres-shopdb`, deployed from a compose file with one service and a `data` volume. So `jokku top`, `storage:report` and `resource:limit` work on it too. It isn't listed by `apps:list`, and pushes to it are refused.

> [!NOTE]
> SQLite, a file on an app's own volume, comes next.
