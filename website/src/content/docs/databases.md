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

Linking sets `DATABASE_URL` on the app (`REDIS_URL` for Redis) and restarts it, so it connects right away. The database is reachable only from your apps, at `postgres-shopdb.internal`, on any server in the cluster, until you [expose it](#reach-it-from-outside).

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

## Reach it from outside

To connect from your laptop, a BI tool or another host, expose the database at a domain:

```sh
jokku db:postgres:expose shopdb db.example.com --allow 203.0.113.7
jokku db:postgres:info shopdb --public-dsn
# postgres://postgres:…@db.example.com:5432/shopdb?sslmode=require
```

Point the domain at your servers, or at your [edges](/docs/edges), and open the port in their firewalls. Jokku's proxy takes the connections, on every server that takes web traffic and on every edge, and sends them to the database over the cluster's encrypted network.

| Engine | Port | How it connects |
| --- | --- | --- |
| Postgres | 5432, shared by every Postgres you expose | TLS, which the proxy insists on. `sslmode=require` works, and so does Postgres 17's `sslnegotiation=direct`. The proxy tells databases apart by the domain the client asks for. |
| Redis | 6379, shared the same way | TLS: `rediss://`. |
| MySQL | 3306 for the first, `--port` for others | Passed through as it is, encrypted with MySQL's own certificate. MySQL starts TLS only after the server speaks, so the proxy can't read a domain first, and each MySQL database needs a port of its own. |

A domain Let's Encrypt can reach gets a certificate from it, so clients can check it (`sslmode=verify-full sslrootcert=system`). Any other name gets one the proxy makes itself, which `sslmode=require` accepts without checking.

`--allow` takes addresses and ranges, comma-separated (`--allow 203.0.113.7,198.51.100.0/24`), and the proxy turns away everyone else. Without it, any address can connect, and only the database's password and TLS protect it: Jokku generates long random passwords, but an allow list is better. Run `expose` again to change the domain, port or list. `--port` picks another port for Postgres or Redis too.

| Command | What it does |
| --- | --- |
| `db:<engine>:expose <name> <domain> [--port N] [--allow IPS]` | Let clients outside connect, or change how. |
| `db:<engine>:unexpose <name>` | Stop them: only your apps reach it again. |
| `db:<engine>:info <name> --public-dsn` | The URL to connect from outside, with the password. |

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
