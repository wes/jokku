# Jokku

Jokku is a self-hosted platform for deploying your apps, modeled on
[Dokku](https://dokku.com). You `git push` an app that has a Dockerfile, and
Jokku builds it, runs it and routes your domains to it, with Let's Encrypt
certificates for the apps you want on HTTPS.

What's different: every app instance runs in its own lightweight
[Firecracker](https://firecracker-microvm.github.io) virtual machine instead
of a container, and you can add servers with one command, so apps scale
across a cluster. The commands are Dokku's, so if you know Dokku, you
already know Jokku.

> **Status: early development.** `git push` builds your Dockerfile and runs
> it in Firecracker microVMs behind the proxy, with zero-downtime deploys,
> scaling, logs and updates, on one server or a cluster. See the
> [roadmap](docs/architecture.md#milestones).

## Install

On a fresh Ubuntu or Debian server with ports 80 and 443 free and KVM: bare
metal, or a virtual machine with nested virtualization turned on (Proxmox: CPU
type `host`). Firecracker needs a CPU from 2011 or newer (Intel Sandy Bridge
or AMD Bulldozer; it is tested on Intel Skylake and newer).

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
```

That's it. The SSH keys you used to log in to the server can now deploy apps
and run commands.

Run commands from your laptop over SSH, or on the server itself with
`sudo jokku <command>`. The examples below assume this alias on your laptop:

```sh
alias jokku='ssh -t jokku@your-server'
jokku apps:list
```

To let someone else deploy, add their public key:

```sh
cat alice.pub | jokku ssh-keys:add alice
```

### Adding more servers

One server is a complete Jokku. To add another, ask the first server for a
join command:

```sh
jokku cluster:join-command
```

It prints a one-liner. Run it on the new server:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo JOKKU_VERSION=v0.1.0 sh -s -- --join 203.0.113.10:7443 --token JOKKU1...
```

The servers connect over an encrypted private network (WireGuard on UDP
51820; the first server also needs TCP 7443 open for joining). Apps are spread
across all of them, and every server can receive web traffic, so point your DNS
at as many as you like.

```sh
jokku nodes:list
NAME      ROLE     STATUS  ADDRESS        MESH IP     CPUS  MEMORY  INSTANCES  VERSION
server-1  control  ready   203.0.113.10   10.210.1.1  8     16g     3          v0.1.0
server-2  worker   ready   203.0.113.11   10.210.2.1  8     16g     2          v0.1.0
```

If a server dies, its apps are started on the others after a minute. To take
one out for maintenance, `jokku nodes:drain server-2` moves its apps away
first; `jokku nodes:remove server-2` takes it out of the cluster. The first
server holds the cluster's state: if it goes down, the others keep running and
serving what they have, and you can't deploy until it's back.

### Serving from home, with no open port

Jokku can run where the internet can't reach it, such as a home lab behind
a router. Add an **edge**, a small public server that receives your domains'
traffic and sends it home over the encrypted network; your servers dial it,
so nothing at home is exposed:

```sh
jokku edge:add root@203.0.113.7                    # installs it over ssh
jokku external:create ha http://192.168.1.50:8123  # route a domain to something on your LAN
jokku http-auth:enable ha --password               # with a login page in front
```

An edge needs only TCP 80 and 443 and UDP 51820 open. See
[Edges](docs/architecture.md#edges) for how it works, and what an edge can
and can't reach.

### Watch everything

```sh
jokku top
```

A live view of the machines, apps, instances and recent events, with CPU and
memory for each. Press `6` for **Traffic**: every request flies across its
app's lane as a dot (green, yellow for 4xx, red for 5xx) and lands on the
instance and server that answered, with requests per second, latency and error
rate updating live. Press `7` for **Backups**: every volume and the cluster
itself, backed up or not, with their last and next backups, a chart of
recent ones, and what they keep; `space` turns a volume's backups on or off,
`b` backs it up now, `s` changes how often and `a` turns automatic restore
on or off. Over SSH it needs a terminal, so use `ssh -t jokku@your-server
top` (or put `-t` in your alias).

## Deploy an app

Your app needs a `Dockerfile`, and it should listen on the port in `$PORT`:
the port your Dockerfile `EXPOSE`s, or 5000. To pick it yourself, set it with
`jokku config:set myapp PORT=3000`.

**1. Create the app and set its config**

```sh
jokku apps:create myapp
jokku config:set myapp DATABASE_URL=postgres://...
```

**2. Push it**

```sh
cd myapp
git remote add jokku jokku@your-server:myapp
git push jokku main
```

Jokku builds the Dockerfile, boots the app, waits for it to accept
connections and prints its URL, for example `http://myapp.203.0.113.10.sslip.io`.
Pushing to an app that doesn't exist yet creates it, so step 1 is
optional. If the new version fails to start, the push is rejected and the
previous version keeps serving.

Jokku builds the `Dockerfile` at the root of the repo. To use another one, or
build from a subdirectory:

```sh
jokku builder:dockerfile myapp docker/prod.Dockerfile   # relative to the build dir
jokku builder:set myapp build-dir api                   # build from api/ in the repo
jokku builder:report myapp                              # how the app is built
```

An app can also deploy a [compose file](#deploy-a-compose-file) or a
[registry image](#deploy-an-image) instead.

**3. Add your domain**

```sh
jokku domains:add myapp myapp.com
```

Point `myapp.com` at your server. New apps are served over plain HTTP on
port 80. For HTTPS, turn on Let's Encrypt for the app:

```sh
jokku letsencrypt:set --global email you@example.com   # once, for expiry notices
jokku letsencrypt:enable myapp
```

Jokku then gets a certificate for each of the app's public domains, renews
it, and redirects HTTP to HTTPS. `letsencrypt:disable myapp` goes back to
plain HTTP.

### Watch it

```sh
jokku logs myapp -t        # follow output from every instance, plus a router line per request
jokku logs myapp -p router # just the requests
jokku ps:report myapp      # what's running, where, and how big
jokku ps:restart myapp     # also: ps:stop, ps:start, ps:rebuild
```

To look around inside a running instance, open a shell in it, or run a
single command:

```sh
jokku enter myapp                      # a shell in the app's web (or only) process
jokku enter myapp worker.2             # in a particular instance
jokku enter myapp web ls -la /app      # one command; it exits with the command's status
jokku enter --root myapp web           # as root rather than the image's user
```

Over ssh, use `ssh -t jokku@your-server enter myapp` for an interactive
shell.

### Scale it

```sh
jokku ps:scale myapp web=3                                          # more instances
jokku resource:limit myapp --cpu 2 --memory 1g --process-type web   # bigger instances
```

On a cluster, instances are spread across your servers. To run more than one
kind of process, add a `Procfile` to your repo:

```
web: bin/server
worker: bin/jobs
```

Only `web` runs at first; scale the others to start them, for example
`jokku ps:scale myapp web=3 worker=1`. For a Procfile somewhere else:
`jokku builder:set myapp procfile Procfile.prod`.

### Deploy a compose file

An app can also be a whole compose file: each service runs as one of the
app's process types, with its own image. You deploy it the same way, with
`git push`.

**1. Put the compose file in your repo**

Jokku reads the first of `compose.yaml`, `compose.yml`,
`docker-compose.yaml` or `docker-compose.yml` at the root of the repo. Commit
it along with whatever its `build:` services need, such as their Dockerfiles.
For example:

```yaml
services:
  web:
    build: .
    ports: ["8080:3000"]     # this service gets the app's domains; it listens on 3000
    environment:
      DATABASE_URL: postgres://shop:${DB_PASSWORD}@db/shop
    depends_on: [db]
  db:
    image: postgres:17
    environment:
      POSTGRES_USER: shop
      POSTGRES_DB: shop
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes: ["pgdata:/var/lib/postgresql/data"]
volumes:
  pgdata:
```

**2. Switch the app to compose**

```sh
jokku builder:compose shop                           # the first of compose.yaml ... docker-compose.yml
jokku builder:compose shop docker-compose.prod.yml   # or a file you name
```

This creates the app if it doesn't exist yet. Naming a file lets you deploy,
say, a production file that sits next to the `docker-compose.yml` you use
locally. Only that one file is read: a `docker-compose.override.yml` is not
merged in, so local development overrides stay out of production.

Jokku never switches to compose by itself, since many repos keep a compose
file around just for local development. A push with a compose file and no
Dockerfile tells you to run `builder:compose`. `jokku builder:dockerfile
shop` switches back.

**3. Set the values your file uses**

`${VAR}` in the compose file is filled in from the app's config vars, which
act like docker compose's `.env` file (a committed `.env` works too, and
config vars win over it):

```sh
jokku config:set --no-restart shop DB_PASSWORD=s3cret
```

Services only get the environment the file gives them; a config var reaches
a service through `${VAR}`.

**4. Push it**

```sh
git remote add jokku jokku@your-server:shop
git push jokku main
```

Jokku builds each `build:` service, pulls each `image:` service, creates
the named volumes and starts the services in `depends_on` order. The service
named `web`, or the only one with `ports:`, gets the app's domains and
HTTPS, on the port its `ports:` entry points to (3000 above).

Once deployed:
- Services reach each other by name (`db`).
- Each service is a process type: `ps:scale shop web=3`, `logs shop -p db`
  and `storage:list shop` work per service.
- Deploys and `config:set` only restart the services they change, so the
  database keeps running when the web service changes.

Jokku tells you what it can't run (privileged containers, host paths,
secrets) instead of guessing; see
[Compose apps](docs/architecture.md#compose-apps).

### Deploy an image

Skip the build and run an image from a registry:

```sh
jokku builder:image myapp ghcr.io/you/myapp:v2
jokku builder:image cache redis:7
```

This creates the app if needed and deploys the image. From then on, the app
runs that image. `ps:rebuild` pulls the tag again, and `builder:image` with
another tag deploys that one. `git push` to the app is refused, so a push
can't replace the image by accident; `jokku builder:dockerfile myapp` makes
pushes build it again.

For a private registry, log in first:
`echo $TOKEN | jokku registry:login ghcr.io you`.

### Apps talk to each other by name

Inside your apps, `<app>.internal` reaches another app, and
`<process>.<app>.internal` one of its process types, on any server in the
cluster. Short names work too: from `shop`, `cache` reaches the `cache` app
and `worker` reaches shop's own worker processes. Connect on the port the app
listens on, for example `redis://cache.internal:6379`.

### Keep data

An instance's own files are reset on every deploy, as in a container. Put
data that must last, such as a database, uploads or a SQLite file, on a
volume:

```sh
jokku storage:mount myapp data:/app/data                # creates the volume "data" (10g) if needed
jokku storage:mount mydb pg:/var/lib/postgresql/data --size 50g
jokku storage:list myapp
```

A volume is a disk on the server its instance runs on. It works the way a
Docker volume does. On first use it gets whatever the image has at that path,
and it is writable by the image's user. The disk only uses space for what is
written.

- **One instance per volume.** A process with a volume runs a single
  instance, so `ps:scale myapp web=2` is refused. Deploys stop the old
  instance before starting the new one, so expect a few seconds of downtime
  instead of a zero-downtime switch.
- **Moving servers.** `jokku nodes:drain` brings volumes along: the disk is
  copied while the app keeps running, then the app stops briefly for a final
  copy and starts on the new server. Move one yourself with
  `jokku storage:move myapp data server-2`.
- **If a server dies,** apps with volumes on it wait for it to come back
  instead of starting elsewhere, because their data is there, unless the
  volumes are [backed up](#back-up-volumes): then, after five minutes, they
  are restored onto another server from their latest backup.

To see a volume's files, `jokku enter myapp web ls /app/data`. To copy them
out or back in, as a backup or to move data between servers:

```sh
jokku storage:export myapp data > data.tar.gz             # the app pauses for a moment
jokku storage:import myapp data --clear < data.tar.gz     # then the app restarts
```

Both go through the running instance that mounts the volume, so the app must
be running. The export pauses the app's processes while it copies, so a
database file is captured at one point in time; `--live` skips the pause.
The import replaces files with the archive's (`--clear` empties the volume
first), then restarts the app so it reads them. Imported files belong to
whoever owns the volume's mount point, so an archive made on your laptop
doesn't leave them owned by a user the app isn't; `--keep-owners` keeps the
archive's numeric owners instead. Over ssh, leave out `-t`, so
the archive passes through untouched: `ssh jokku@your-server storage:export
myapp data > data.tar.gz`. `storage:report` shows where each volume's disk
image is.

`storage:unmount` detaches a volume and keeps its data. `storage:resize` grows
it, and `storage:destroy` deletes it.

### Back up volumes

Volumes back up to any S3-compatible bucket: Tigris, AWS S3, Cloudflare R2,
Backblaze B2 and the like. Add the bucket once, as a destination, then say
which volumes go there:

```sh
jokku backups:destination-add tigris --endpoint https://fly.storage.tigris.dev \
  --bucket my-backups --access-key-id tid_xxx      # asks for the secret key
jokku backups:set myapp data tigris                # under jokku/myapp/data, every 15 minutes
jokku backups:list myapp data
jokku backups:run myapp data                       # back it up now, too
```

Each volume is backed up every 15 minutes, keeping every backup from the
last 24 hours and the last one of each day for 30 days. Change that with
`backups:set` flags: `--every 1h` (or `off`, for only when you run
`backups:run`), `--keep-recent 48h`, `--keep-daily 14`. Older backups are
deleted after each backup, and then the blocks no remaining backup uses.

Backups are encrypted with a key Jokku makes the first time you add a
destination. **Save the key somewhere safe, away from your servers:** a
backup can't be restored without it, by Jokku or anyone else. Jokku shows it
once and asks you to confirm you saved it; `jokku backups:key` shows it
again. Pass `--no-encrypt` to `destination-add` for a destination that
stores backups unencrypted.

Each backup is complete on its own, but only uploads what changed since the
previous one, so a backup every 15 minutes costs little. The disk is read in
blocks, and a block already in the bucket is never uploaded again. While a backup runs, the app keeps running. Its writes
to the volume pause for a moment at the end, so the backup is the disk as it
was at one instant, the way a power cut would leave it; databases recover
from that the same way they do after a crash.

To restore:

```sh
jokku backups:restore myapp data                   # the latest backup
jokku backups:restore myapp data 2026-10-09T14-15-00Z
jokku backups:restore myapp data --node server-2   # its server is down: restore onto another
```

The current data is backed up first, so a restore can be undone. The backup
downloads while the app runs; then the app restarts with the restored disk.

`backups:report` shows each volume's schedule, its last and next backups,
any failure, and what its backups take up in the bucket; `jokku top`
(press `7`) shows it all at a glance and lets you turn backups on and off.

**When a server dies,** each backed-up volume on it is restored onto
another server from its latest backup once the server has been gone for
five minutes, and its app starts there. Anything written after that backup
is lost, unless the server comes back: then it keeps its copy of the disk
aside, never used again. `storage:report` shows where, and
`jokku storage:discard-old-copy myapp data` deletes it once you have what
you need from it. `backups:set ... --auto-restore off` makes a volume wait
for its server instead.

A server that is only cut off from the others, not dead, would otherwise
keep running its copy of the app while the restored one runs too. So a
server that loses touch with the control node for two minutes stops its
apps with backed-up volumes, unless it is clearly the control node that is
down (the servers it can reach don't hear from it either, and they are most
of the cluster). In a two-server cluster that means a worker stops those
apps whenever it can't reach the control node for two minutes.

**The cluster itself** is backed up too: adding your first destination also
backs up the control node every hour. That covers its database (apps, config
vars, domains, settings, the backup key) and identity, and the images of
recent releases. To rebuild a lost control node, install Jokku on a new
server with the same name (and, so the other servers reconnect, the same
address), then:

```sh
sudo jokku restore-cluster --endpoint https://fly.storage.tigris.dev \
  --bucket my-backups --access-key-id tid_xxx     # asks for the secret key and the backup key
```

Apps start again from their releases, and volumes come back from their own
backups. Remove servers that are gone for good with `jokku nodes:remove
<name> --force`, so their volumes come back elsewhere. `jokku
backups:cluster <destination> --every 6h` changes where and how often, and
`backups:cluster-list` lists them.

### Databases

Jokku runs Postgres, MySQL and Redis for your apps, each from its official
image, on a volume of its own:

```sh
jokku db:postgres:create shopdb        # Postgres 17; also db:mysql:create and db:redis:create
jokku db:postgres:link shopdb myapp    # sets DATABASE_URL on myapp (REDIS_URL for Redis) and restarts it
jokku db:postgres:connect shopdb       # a psql shell (mysql or redis-cli for the others)
jokku db:postgres:info shopdb          # status, address, links and backups; --dsn prints its URL
jokku db:list
```

A database is reachable only from your apps, at `postgres-shopdb.internal`.
If you have a [backup destination](#back-up-volumes), it's backed up every
15 minutes from the moment it's created, and restored onto another server if
its own dies.

```sh
jokku db:postgres:export shopdb > shopdb.dump     # pg_dump; mysqldump for MySQL, an RDB snapshot for Redis
jokku db:postgres:import shopdb < shopdb.dump     # Postgres and MySQL
jokku db:postgres:create shopdb --image-version 16 --size 50g --memory 1g
jokku db:postgres:link shopdb myapp --alias orders    # sets ORDERS_URL instead
jokku db:postgres:logs shopdb -t                  # also restart, stop and start
jokku db:postgres:destroy shopdb                  # refused while linked; its backups stay in their bucket
```

Postgres 17 gets 512 MB of memory, MySQL 8.4 gets 1 GB and Redis 7 (with
append-only persistence) gets 256 MB, each with a 10 GB volume. Under the
hood a database is an app named after its engine, like `postgres-shopdb`, so
`jokku top`, `storage:report` and `resource:limit` work on it too. It isn't
listed by `apps:list`, and pushes to it are refused. SQLite comes next.

### Deploy from GitHub Actions

Create a key for GitHub and give it access:

```sh
ssh-keygen -t ed25519 -N "" -f deploy_key
cat deploy_key.pub | jokku ssh-keys:add github-actions
```

Save the contents of `deploy_key` as a repository secret named
`JOKKU_DEPLOY_KEY`, then add `.github/workflows/deploy.yml`:

```yaml
name: deploy
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
      - run: |
          mkdir -p ~/.ssh
          echo "$JOKKU_DEPLOY_KEY" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
          ssh-keyscan your-server >> ~/.ssh/known_hosts
          git push jokku@your-server:myapp HEAD:refs/heads/main
        env:
          JOKKU_DEPLOY_KEY: ${{ secrets.JOKKU_DEPLOY_KEY }}
```

## Update

On the server:

```sh
sudo jokku update
```

It shows the new version and asks before changing anything. It backs up
Jokku's database first, and if the new version fails to start, it puts the
previous version and data back automatically. Your apps keep running and
serving traffic throughout. Use `--yes` to skip the question or
`--version v0.0.3` to pick a release.

Servers on v0.0.2 or older don't have `jokku update` yet. Update them once
with:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
```

With more than one server, update the first (control) server, then run
`sudo jokku update` on each of the others.

## Learn more

- [Commands](docs/commands.md): every command and what works today
- [Architecture](docs/architecture.md): how Jokku works under the hood
