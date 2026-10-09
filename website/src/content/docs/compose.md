---
title: Compose apps
description: Deploy a whole compose file with git push. Each service becomes one of the app's process types, with its own image.
---

An app can be a whole compose file. Each service runs as one of the app's process types, with its own image, and you deploy it the same way: with `git push`.

### 1. Put the compose file in your repo

Jokku reads the first of `compose.yaml`, `compose.yml`, `docker-compose.yaml` or `docker-compose.yml` at the root of the repo. Commit it along with whatever its `build:` services need, such as their Dockerfiles. For example:

```yaml title="compose.yaml"
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

### 2. Switch the app to compose

```sh
jokku builder:compose shop                           # the first of compose.yaml ... docker-compose.yml
jokku builder:compose shop docker-compose.prod.yml   # or a file you name
```

This creates the app if it doesn't exist yet. Naming a file lets you deploy, say, a production file that sits next to the `docker-compose.yml` you use locally. Only that one file is read: a `docker-compose.override.yml` is not merged in, so local development overrides stay out of production.

> [!NOTE]
> Jokku never switches to compose by itself, since many repos keep a compose file around just for local development. A push with a compose file and no Dockerfile tells you to run `builder:compose`. `jokku builder:dockerfile shop` switches back.

### 3. Set the values your file uses

`${VAR}` in the compose file is filled in from the app's config vars, which act like docker compose's `.env` file. A committed `.env` works too, and config vars win over it.

```sh
jokku config:set --no-restart shop DB_PASSWORD=s3cret
```

Services only get the environment the file gives them; a config var reaches a service through `${VAR}`.

### 4. Push it

```sh
git remote add jokku jokku@your-server:shop
git push jokku main
```

Jokku builds each `build:` service, pulls each `image:` service, creates the named volumes and starts the services in `depends_on` order. The service named `web`, or the only one with `ports:`, gets the app's domains and HTTPS, on the port its `ports:` entry points to (3000 above).

## Once it's deployed

- Services reach each other by name (`db`), and other apps reach them as `db.shop.internal`.
- Each service is a process type: `ps:scale shop web=3`, `logs shop -p db` and `storage:list shop` work per service.
- Deploys and `config:set` only restart the services they change, so the database keeps running when the web service changes.

## How compose maps to Jokku

The file is read with compose-go, the loader docker compose itself uses, so interpolation, `extends` within the file, profiles (`COMPOSE_PROFILES`) and the short and long syntaxes behave as they do in Docker.

| Compose | In Jokku |
| --- | --- |
| `build:` | Built with BuildKit (context, dockerfile, dockerfile_inline, args, target). Services with the same build share one image. |
| `image:` | Pulled, with your `registry:login` credentials. |
| `command`, `entrypoint`, `user`, `working_dir` | The process's command and settings, as `docker run` applies them. |
| `environment`, `env_file`, `${VAR}` | The service's environment. |
| `ports:` | The published service gets the app's domains and HTTPS. |
| `expose:` or the image's `EXPOSE` | The service's port: `$PORT`, its health check, and what others connect to. |
| Named `volumes:` | [Volumes](/docs/storage), created on deploy and mounted per service. |
| `deploy.replicas`, `scale` | The instance count, unless `ps:scale` sets one. |
| `deploy.resources.limits`, `cpus`, `mem_limit` | The microVM's size, unless `resource:limit` sets one. |
| `restart`, `deploy.restart_policy` | The restart policy. |
| `stop_grace_period`, `stop_signal` | How the service is stopped. |
| `depends_on` | Start order. |
| Bind mounts of repo files on `image:` services | Copied into the image (read-only in effect). |

## What Jokku won't run

Jokku tells you what it can't run instead of guessing. These are refused with a message:

- **Host access:** `privileged`, `cap_add`, `devices`, `network_mode`, `pid`, `ipc`, `sysctls`, `security_opt`.
- **Host paths:** bind mounts from outside the repo, such as `/var/run/docker.sock`.
- **Not supported yet:** `secrets`, `configs`, `include`, `extends` from another file, one-off services (`service_completed_successfully`), UDP ports, and two services publishing ports (name one `web`).
- **Volume limits:** a volume shared by two services, or more than one replica of a service with a volume.

These are skipped, with a note in the deploy log:

- A `healthcheck:` command, which can't run inside the microVM yet. Jokku checks the service's port instead.
- Anonymous volumes and `tmpfs`, which become the instance's own disk.
