---
title: Dockerfile apps
description: How Jokku builds your Dockerfile, picks the port, and finds your process types.
---

Building from a Dockerfile is the default. On each push, Jokku builds the `Dockerfile` at the root of the repo with BuildKit, turns the image into a root filesystem for its microVMs, and rolls the new version out.

## The port

Your app should listen on `$PORT`. Jokku sets it to the first of these that applies:

1. The `PORT` config var, if you set one: `jokku config:set myapp PORT=3000`.
2. The port the image `EXPOSE`s. An image inherits its base image's ports (nginx's 80, say), so when there are several, the ones from the newest `EXPOSE` step win.
3. The image's own `PORT` environment variable.
4. Otherwise, `5000`.

Each deploy tells you which it used:

```console
remote: -----> $PORT is 3000, from EXPOSE 3000/tcp
```

## Another Dockerfile or directory

To build a different Dockerfile, or build from a subdirectory of the repo:

```sh
jokku builder:dockerfile myapp docker/prod.Dockerfile   # relative to the build dir
jokku builder:set myapp build-dir api                   # build from api/ in the repo
jokku builder:report myapp                              # how the app is built
```

`builder:dockerfile` is also how you switch an app back to building from git after it ran a [compose file](/docs/compose) or a [registry image](/docs/images).

## Process types

Without a `Procfile`, the app has a single `web` process that runs the image's `ENTRYPOINT` and `CMD`. To run more than one kind of process, add a `Procfile` to your repo:

```procfile
web: bin/server
worker: bin/jobs
```

Only `web` runs at first; scale the others to start them. See [Processes & scaling](/docs/processes).

For a Procfile somewhere else, relative to the build dir:

```sh
jokku builder:set myapp procfile Procfile.prod
```

## Private base images

If your Dockerfile starts `FROM` an image in a private registry, log in once and every build can pull it:

```sh
echo $TOKEN | jokku registry:login ghcr.io you
```

## Rebuild without a push

`ps:rebuild` builds the last pushed source again and deploys it, for example to pick up a newer base image:

```sh
jokku ps:rebuild myapp
```

## Git settings

| Setting | Default | What it does |
| --- | --- | --- |
| `deploy-branch` | `main` | The branch that deploys when pushed. Pushes to other branches are stored but not deployed. |
| `keep-git-dir` | `false` | Keep the `.git` directory in the build context. |

```sh
jokku git:set myapp deploy-branch production
jokku git:report myapp
```

## Not supported yet

- **Build args.** `docker-options:add ... build` is planned.
- **Buildpacks.** Herokuish, Cloud Native Buildpacks, nixpacks and railpack aren't supported for now: Jokku builds Dockerfiles, compose files and registry images.
