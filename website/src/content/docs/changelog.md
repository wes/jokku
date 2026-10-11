---
title: What's new
description: Every Jokku release, newest first, and what it brought.
---

Update with `sudo jokku update`. From v0.8.5, running it on a cluster's control server updates the other servers too. Each release's binaries and full notes are on [GitHub](https://github.com/wes/jokku/releases).

## v0.8.7: Compressed at home, behind an edge

October 11, 2026

- **Behind an [edge](/docs/edges), the server an app runs on now compresses its responses** before they go up to the edge. Before, the edge compressed them only after the trip from home, which is often the slowest part, so that trip still carried the full size. For a site that serves its files uncompressed, that's about a quarter of the bytes for its pages, styles and scripts.
- The edge sends each request through the web proxy of a server that runs the app, which passes it on to an instance. Apps still see the visitor's address and that they came over HTTPS, logins still happen on the edge, and `jokku logs` and the Traffic view in `jokku top` show each request once, with the instance that answered.
- **Turn compression off for an app** with `jokku proxy:set myapp compress false`. It's on for every app by default.
- An edge on v0.8.6 or older, or a server that is, keeps reaching apps the old way until it's updated.

## v0.8.6: Compressed responses

October 11, 2026

- **[Jokku's proxy compresses](/docs/domains#compression-and-http3) the text your apps send uncompressed,** such as HTML, CSS, JavaScript, JSON and SVG, with zstd or gzip. A site that serves its files raw now sends about a quarter of the bytes for its pages, styles and scripts, so it loads sooner, most of all through an edge or over a slow link. Responses an app compresses itself, images and server-sent events go out as before.
- The docs list UDP port 443 for HTTP/3, on servers that take web traffic and on edges. Without it, browsers fall back to HTTP/2, which takes one more round trip to connect.

## v0.8.5: One update for the whole cluster, and databases in jokku top

October 11, 2026

- **[`sudo jokku update`](/docs/updating#clusters) on the control server updates the whole cluster.** Once it has updated itself, it updates the other servers one at a time, waiting for each to come back on the new version before starting the next, and shows how each one goes. Apps keep serving, and a server's instances stay put while it restarts.
- If a server's update fails, that server goes back to the version it had, and the update stops there, so a release that breaks one server isn't rolled onto the rest. Run `sudo jokku update` again to carry on once it's fixed. `--this-node` updates only the control server, and `jokku nodes:list` shows where each server is in an update.
- Servers update themselves this way from this release on, so update each server by hand one last time to get to v0.8.5.
- **Databases have their own list in [`jokku top`](/docs/top#apps-and-databases)**, below the apps, on the overview and on the Apps tab. On the Apps tab, each app's databases also show under it, with the variable they're linked as.

## v0.8.4: Machines on the Traffic view, and a retro sun

October 10, 2026

- **The Traffic view in [`jokku top`](/docs/top#traffic) shows each app's machines:** how many, and a dot for each, dim while the app sleeps. An app with more than one process group gets a line for each.
- An app's name turns orange while one of its requests is on the way, and the names column widens to fit names of up to 24 characters.
- The selected row in `jokku top` is a subtle shade instead of red. Over SSH, terminals turned the old brown into red.
- The [splash screen](/docs/installation#the-splash-screen) has a striped retro sun in place of the palm tree, in the logo's oranges.

## v0.8.3: Sleep, and a splash screen

October 10, 2026

- **[Sleep when idle](/docs/processes#sleep-when-idle).** `jokku ps:set myapp sleep-after 15m` stops an app's microVMs after 15 minutes without a request, and the next request starts them again where they were. The proxy holds that request until the app answers, so the visitor just waits a moment. Open websockets keep an app awake, and deploys, restarts and `ps:start` wake it.
- **A [splash screen](/docs/installation#the-splash-screen) at login.** It has the Jokku logo, a palm tree, and live figures: the cluster's apps, nodes, instances and last deploy, and the server's address, load, memory, disk and uptime. It also warns about a node that's down, an app short of healthy instances, or system updates waiting.
- **Install and update ask before turning it on**, once per server. Turn it on or off any time with `sudo jokku splash:enable` and `splash:disable`, and see it with `sudo jokku splash`. It takes the place of the system's welcome text, which comes back when you turn it off, as does a banner of your own.

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
