---
title: Registry images
description: Skip the build and run an image straight from a registry, public or private.
---

## Deploy an image

```sh
jokku builder:image myapp ghcr.io/you/myapp:v2
jokku builder:image cache redis:7
```

This creates the app if needed and deploys the image. From then on, the app runs that image:

- `ps:rebuild` pulls the tag again and deploys it.
- `builder:image` with another tag deploys that one.
- `git push` to the app is refused, so a push can't replace the image by accident.

To build the app from git again:

```sh
jokku builder:dockerfile myapp
```

## Private registries

Log in once. Logins apply to every app, for `builder:image`, for compose `image:` services, and for a Dockerfile's `FROM`:

```sh
echo $TOKEN | jokku registry:login ghcr.io you
jokku registry:report
jokku registry:logout ghcr.io
```

`registry:login` reads the password from stdin, as above, or takes it as the last argument.

## Example: a Redis for your app

```sh
jokku builder:image cache redis:7
jokku proxy:disable cache                                   # keep it off the public proxy
jokku config:set myapp REDIS_URL=redis://cache.internal:6379
```

Your app reaches it by name on any server; see [Private networking](/docs/networking). If it should keep its data across deploys, give it a [volume](/docs/storage):

```sh
jokku storage:mount cache data:/data
```
