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
jokku db:postgres:info shopdb          # status, address, connections, query times, links and backups
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

### Only on your network

To reach a database from your own network but not from the internet, add `--lan`:

```sh
jokku db:postgres:expose shopdb shopdb.192.168.1.10.sslip.io --lan
```

Your servers take the connections, and your [edges](/docs/edges) don't: they don't listen on the port, and they can't reach the database. Unless you give an `--allow` list of your own, only private addresses are let in (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, Tailscale's `100.64.0.0/10`, and IPv6's `fc00::/7`). Point the name at one of your servers, as the sslip.io name above does, or use a name from your own DNS. The certificate is one the proxy makes itself, since Let's Encrypt can't reach your network: `sslmode=require` accepts it without checking.

| Command | What it does |
| --- | --- |
| `db:<engine>:expose <name> <domain> [--port N] [--allow IPS] [--lan]` | Let clients outside connect, or change how. `--lan` keeps it on your network. |
| `db:<engine>:unexpose <name>` | Stop them: only your apps reach it again. |
| `db:<engine>:info <name> --public-dsn` | The URL to connect from outside, with the password. |

## See inside it

The **Databases** view of [`jokku top`](/docs/top#databases) (key `8`) shows each database's connections against its limit, its queries a second, how long they take, and how often reads come from memory, over the last minute, hour and day, with who's connected, its users and its slowest queries. `db:<engine>:info` shows the same:

```console
$ jokku db:postgres:info shopdb
       ...
       Connections:  12 of 97 (2 running a query)
       Queries:      21.0/s over the last minute, 16.7/s over the last hour, 99.1% cache hits
       Latency:      2.4 ms per query on average over the last hour
       Users:        postgres (all), reporting (read-only)
```

Every ten seconds, your control server runs the database's own client (`psql`, `mysql` or `redis-cli`) in its microVM and leaves its own queries out of what it counts. It keeps the history in memory, so the hour and day start over when it restarts. The times are how long queries took inside the database, without the trip from your app.

| Engine | Query times |
| --- | --- |
| MySQL | p50, p95 and p99, from the histogram MySQL keeps of its statements, and the slowest statements, from its statement digests. |
| Redis | p50, p95 and p99, from Redis's histogram, whose buckets double in size, so they're right to within a factor of two. |
| Postgres | The average per query, and the slowest queries, from `pg_stat_statements`. Postgres keeps no histogram, so there are no percentiles. |

`pg_stat_statements` has to be loaded when Postgres starts. A new database loads it. On a database created before v0.8.12, Jokku turns it on the first time it looks, and it starts at the database's next restart: `jokku db:postgres:restart shopdb`. Until then, the average is per transaction.

## Users

Your apps connect as the database's own user. Add others for a reporting tool, a teammate or a second app:

```sh
jokku db:postgres:users:add shopdb reporting --read-only
jokku db:postgres:users:list shopdb
jokku db:postgres:users:remove shopdb reporting
```

`users:add` makes a password and prints the user's URL, and its URL from outside when the database is [exposed](#reach-it-from-outside). A user reads and writes the data, or only reads with `--read-only`, but can't change the schema, so migrations stay with the database's own user.

| Command | What it does |
| --- | --- |
| `db:<engine>:users:list <name> [--dsn]` | Every user and what it may do: `all` (the database's own), `read-write`, `read-only`, or `other` for privileges you set yourself. `--dsn` shows the URLs of the users Jokku added, with their passwords. |
| `db:<engine>:users:add <name> <user> [--read-only]` | Add a user. |
| `db:<engine>:users:passwd <name> <user>` | Give it a new password. The old one stops working. |
| `db:<engine>:users:remove <name> <user>` | Remove it and close its open connections. In Postgres, what it owned goes to the database's own user. |

Postgres (14 and newer) and MySQL have users. Redis has one password, the one in its URL.

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
