---
title: Introduction
description: Jokku is a self-hosted platform for deploying your apps. You git push, it builds your Dockerfile and runs it in Firecracker microVMs on your own servers.
---

Jokku is modeled on [Dokku](https://dokku.com). You `git push` an app that has a Dockerfile, and Jokku builds it, runs it and routes your domains to it, with Let's Encrypt certificates for the apps you want on HTTPS.

Two things set it apart:

- **Every instance is a microVM.** Each instance of your app runs in its own lightweight [Firecracker](https://firecracker-microvm.github.io) virtual machine instead of a container.
- **Servers join with one command.** One server is a complete Jokku. Add more and your apps spread across them, with nothing else to change.

The commands are Dokku's, so if you know Dokku, you already know Jokku.

## The 60-second tour

On a fresh Ubuntu or Debian server with KVM:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
```

On your laptop, in a repo with a `Dockerfile` (you only need git and ssh):

```sh
git remote add jokku jokku@your-server:myapp
git push jokku main
# => http://myapp.203.0.113.10.sslip.io
```

Every other command runs over SSH, from anywhere:

```sh
ssh jokku@your-server config:set myapp DATABASE_URL=postgres://...
ssh jokku@your-server domains:add myapp myapp.com
ssh jokku@your-server ps:scale myapp web=4 worker=2
ssh jokku@your-server resource:limit myapp --cpu 2 --memory 1g --process-type web
```

## What you get

- **`git push` deploys** with zero downtime. If the new version fails to start, the push is rejected and the previous version keeps serving.
- **Dockerfiles, compose files and registry images.** Build from a Dockerfile, deploy a whole `compose.yaml`, or run an image from a registry.
- **Automatic HTTPS** from Let's Encrypt, served by Caddy, which is built into Jokku.
- **Scaling** across processes, instances and servers with `ps:scale` and `resource:limit`.
- **Volumes** for data that must last, which move with their app when it changes servers, and **backups** to any S3-compatible bucket that bring them back on another server if theirs dies.
- **Databases.** Postgres, MySQL and Redis, linked to your apps with a connection URL.
- **Edges** for home labs: serve your domains from a small public server that your servers dial, with nothing open at home, including services on your network that Jokku doesn't run.
- **Logins** in front of any app: a shared password, or your users with authenticator codes.
- **Private networking.** Apps reach each other by name, such as `cache.internal`, on any server in the cluster.
- **`jokku top`**, a live view of your servers, apps and every request.

## Status

> [!NOTE]
> Jokku is in early development. `git push` builds your Dockerfile and runs it in Firecracker microVMs behind the proxy, with zero-downtime deploys, scaling, logs, volumes, backups, databases and updates, on one server or a cluster, even at home behind an edge. [What's new](/docs/changelog) lists every release, and the [command reference](/docs/commands) shows what works today and what's planned.

## Where to next

```cards
Install Jokku | /docs/installation | Set up your first server with one command.
Deploy an app | /docs/quickstart | Push a Dockerfile and get a URL back.
Add servers | /docs/cluster | Grow from one server to a cluster.
Coming from Dokku | /docs/dokku | What stays the same and what's different.
```
