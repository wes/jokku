---
title: Deploy your first app
description: Push a repo with a Dockerfile, get a URL back, then put it on your own domain with HTTPS.
---

Your app needs a `Dockerfile`, and it should listen on the port in `$PORT`. That's the port your Dockerfile `EXPOSE`s, or 5000 if it exposes none. To pick it yourself, set it with `jokku config:set myapp PORT=3000`.

### 1. Create the app

```sh
jokku apps:create myapp
jokku config:set myapp DATABASE_URL=postgres://...
```

Pushing to an app that doesn't exist yet creates it, so this step is optional. It's handy when the app needs config before its first boot.

### 2. Push it

```sh
cd myapp
git remote add jokku jokku@your-server:myapp
git push jokku main
```

Jokku builds the Dockerfile, boots the app, waits for it to accept connections and prints its URL:

```console
$ git push jokku main
remote: -----> Received git source for myapp (18.4 KiB)
remote: -----> Building myapp from Dockerfile
remote:        #8 [3/4] RUN npm ci --omit=dev
remote:        #9 [4/4] COPY . .
remote: -----> Creating the microVM root filesystem
remote: -----> $PORT is 3000, from EXPOSE 3000/tcp
remote: -----> Starting web.1 on server-1 (1 vCPU, 256 MiB)
remote:        web.1 is up
remote: =====> Application deployed:
remote:        http://myapp.203.0.113.10.sslip.io
remote:        For HTTPS: jokku letsencrypt:enable myapp
```

Every app gets a working address on [sslip.io](https://sslip.io) right away, so you can try it before touching DNS.

> [!TIP]
> If the new version fails to start, the push is rejected, the last lines of its output are printed, and the previous version keeps serving.

### 3. Add your domain

```sh
jokku domains:add myapp myapp.com
```

Point `myapp.com` at your server with an A record. New apps are served over plain HTTP on port 80.

### 4. Turn on HTTPS

```sh
jokku letsencrypt:set --global email you@example.com   # once, for expiry notices
jokku letsencrypt:enable myapp
```

Jokku then gets a certificate for each of the app's public domains, renews it, and redirects HTTP to HTTPS. `jokku letsencrypt:disable myapp` goes back to plain HTTP.

## Deploy again

Commit and push. Each deploy boots the new version next to the old one, switches traffic once it's healthy, and retires the old instances a minute later.

```sh
git commit -am "Make it faster"
git push jokku main
```

Pushes to `main` deploy. To deploy from another branch, run `jokku git:set myapp deploy-branch production`, or push a branch onto main with `git push jokku my-branch:main`.

## Next steps

```cards
Scale it | /docs/processes | Run more instances, bigger instances, and workers.
Watch it | /docs/logs | Follow logs and open a shell inside an instance.
Keep data | /docs/storage | Give a database or uploads a volume.
Deploy from CI | /docs/github-actions | Push from GitHub Actions on every merge.
```
