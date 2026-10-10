---
title: What's new
description: Every Jokku release, newest first, and what it brought.
---

Update a server with `sudo jokku update`, and every server in a cluster the same way. Each release's binaries and full notes are on [GitHub](https://github.com/wes/jokku/releases).

## v0.8.2: Serve from home

October 10, 2026

- **[Edges](/docs/edges).** Run Jokku where the internet can't reach it, such as a home lab, and serve your domains from a small public server: `jokku edge:add root@<server>`. Your servers dial the edge, so nothing at home needs an open port, and the edge can reach only what it routes.
- **External apps.** Route a domain to a service Jokku doesn't run, such as Home Assistant or a NAS on your network: `jokku external:create ha http://192.168.1.50:8123`.
- **[Logins](/docs/logins).** A login page in front of any app: one shared password, or your users with authenticator codes. Allowed addresses, paths that skip the login, share links that expire, and one login for every app.
- When an app can't be reached, the proxy shows a page saying so.

## v0.8.1: Traffic you can watch

October 9, 2026

- The Traffic view in [`jokku top`](/docs/top) animates at 30 frames a second. Requests trail, slow ones crawl, failures burst where they land, and numbers roll; `f` turns the effects off for slow links.
- `jokku top` uses the website's colors, with darker versions for light terminals.

## v0.8.0: Databases

October 9, 2026

- **[Databases](/docs/databases).** Postgres, MySQL and Redis for your apps, under `db:`: `jokku db:postgres:create shopdb`, then `db:postgres:link shopdb myapp` sets `DATABASE_URL`. Each runs from its official image on a volume of its own, reachable only from your apps, and is backed up from the start when you have a backup destination.
- An instance's memory in `jokku top` is its own, no longer counting the host's cache of its disks.

## v0.7.0: Backups that restore themselves

October 9, 2026

- **Scheduled [backups](/docs/backups)**, every 15 minutes by default, keeping the last day of them and a daily one for 30 days.
- **Automatic restore.** When a server has been down five minutes, its backed-up volumes come back on another server from their latest backup, and their apps start there. A server cut off from the others stops those apps first, so two copies never run.
- **Cluster backups.** The control server backs itself up every hour, and `jokku restore-cluster` rebuilds it on a new server.
- A Backups view in `jokku top`.

## v0.6.0: Backups

October 9, 2026

- Back volumes up to any S3-compatible bucket: incremental, block by block, and encrypted with a key you keep. Restore any backup onto any server.

## v0.5.0: Builders

October 9, 2026

- One command for each way of building an app: `builder:dockerfile`, `builder:compose` and `builder:image`.

## v0.4.0: A shell inside

October 8, 2026

- `jokku enter` opens a shell, or runs a command, inside a running instance.
- `storage:export` and `storage:import` copy a volume's files out and in.

## v0.3.0: Images, compose and names

October 8, 2026

- Deploy an image from a registry, with logins for private registries.
- Deploy a whole compose file, each service as a process type.
- [Private networking](/docs/networking): apps reach each other by name, such as `cache.internal`.

## v0.2.0 and v0.2.1: Volumes

October 8, 2026

- [Volumes](/docs/storage): disks that move with their app when it changes servers.
- Apps can write to `/dev/stdout` and `/dev/stderr`, as root or not.

## v0.1.0 to v0.1.3: Clusters

October 8, 2026

- **[Clusters](/docs/cluster).** Servers join with one command over a WireGuard mesh; apps spread across them and move when one dies.
- [`jokku top`](/docs/top), a live view of servers, apps and instances, with router logs and a Traffic view.
- `$PORT` comes from the image's own `EXPOSE`, and a `PORT` config var wins.
- New apps start with Let's Encrypt off, as in Dokku.

## v0.0.1 to v0.0.4: The first deploys

October 7, 2026

- The control plane, Dokku's commands over SSH, and the installer.
- `git push` builds a Dockerfile and runs it in Firecracker microVMs behind Caddy, with zero-downtime deploys.
- `jokku update` and `jokku setup`, which back up and roll back.
