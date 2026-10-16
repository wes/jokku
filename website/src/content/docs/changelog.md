---
title: What's new
description: Every Jokku release, newest first, and what it brought.
---

Update with `sudo jokku update`. From v0.8.5, running it on a cluster's control server updates the other servers too. Each release's binaries and full notes are on [GitHub](https://github.com/wes/jokku/releases).

## v0.8.19: Test your network

October 16, 2026

- **`jokku top` has a Network view** (key `0`). Press `t` twice and it tests every link between your servers, one at a time: each server sends to each other one as fast as it can for three seconds and pings it 100 times, over the encrypted network your apps use. It shows throughput and round trips for every pair, names a server whose links are mostly slow or losing pings (a bad cable or switch port, say), and compares each server with the test before.
- **[`jokku network:test` and `network:report`](/docs/networking#testing-the-network)** do the same from the command line. The control server keeps the last ten tests, so you can test before you change your network and compare after.
- On a terminal too narrow for every tab, `jokku top`'s tabs shrink to fit, and keep the name of the view you're on.

## v0.8.18: Servers that start before their network

October 16, 2026

- **A control server that starts before its network still gives the others its address.** If the network took longer to come up than the system waits for it, the control server recorded no address at all. Any server that restarted after that couldn't set up its encrypted link to the others, so its apps, and the databases other servers' apps use, were out of reach while every server still read "ready". The control server now uses its last known address until it finds its own, and keeps following it if it changes.
- **One unusable address no longer takes a server's whole network down.** A server that can't dial one of the others now connects to the rest. It waits for that one to connect instead, and names it in its log.
- **Restarting Jokku doesn't stop databases anymore.** Restarting it on a server, to update it for example, stopped the apps with [auto-restored volumes](/docs/backups#when-a-server-dies) for up to half a minute while the server checked that it wasn't cut off. Running apps are now left alone until the control server answers, which takes a moment. The control server never stops its own apps.

## v0.8.17: Disk in jokku top

October 13, 2026

- **`jokku top` shows each server's disk.** The Nodes view has a DISK USED column, a bar with used and total, and the overview gives each server a disk bar.
- **The Nodes view's columns line up.** A memory figure wider than its column pushed the rest of its row out of line, and the CPU figure sat against the memory bar. Every column is now as wide as its widest figure, with three spaces between columns. On a terminal too narrow for all of them, the addresses move to the line under the table.

## v0.8.16: Asleep isn't unhealthy

October 13, 2026

- **`jokku top` lists external apps apart.** The Overview has an External section, and the Apps view puts them under their own heading. Each one shows where it forwards to and through which server, in place of its instances.
- **An asleep app no longer counts against "instances healthy".** Its instances, and those of a stopped app, are counted on their own: `21/21 instances healthy · 1 asleep`. `cluster:report` shows the same counts.

## v0.8.15: A round sun

October 13, 2026

- **The login splash's sun is round.** Its yellow dome was narrower than the orange band under it, which stuck out a column on each side, and its stripes narrowed faster than its top. Every row now follows one circle.
- **`firewall:unban` always lifts the ban**, as does `u` in `jokku top`'s Firewall view. An unban in the same second as the ban, or from a control server whose clock was a little behind the edge's, could leave it in place.

## v0.8.14: A firewall

October 13, 2026

- **[A firewall](/docs/firewall), on for every app without being set up.** An address that asks for 20 pages that don't exist within 10 minutes, the way vulnerability scanners hunt for `/.env`, `/.git/config` and `/wp-login.php`, is banned for an hour from every site. One that sends more than 50 requests a second gets `429 Too Many Requests`. Private addresses are never banned, and a page not found that one of the site's own pages asked for, like a missing image, doesn't count.
- It runs in the proxy a visitor reaches first, so **behind an [edge](/docs/edges), scanners are stopped at the edge** and their requests never travel up your home connection.
- **`jokku top` has a Firewall view** (key `9`): the banned addresses and why, those close to a ban, and what was blocked, site by site, over the last minute, hour and day. `u` lifts a ban. `jokku firewall:report` shows the same, and every ban is an event.
- A request the firewall answered is marked in the router logs (`firewall=banned`), which name the visitor's own address, and it's left out of response times.
- `firewall:unban`, `firewall:add-allowed-ip`, `firewall:disable <app>` (or `--global`) and `firewall:set` (`not-found`, `window`, `ban`, `rate`, `burst`) change it.

## v0.8.13: MySQL's slowest queries

October 12, 2026

- **MySQL databases show their slowest queries too**, in the [Databases view](/docs/top#databases) of `jokku top`, as Postgres databases do: the five that took the most time in all, with how long each takes on average and how many times it ran. They come from the statement digests MySQL keeps, where values are `?`, so the same query with different values is one line. Nothing needs turning on or a restart.

## v0.8.12: Inside your databases

October 12, 2026

- **[`jokku top`](/docs/top#databases) has a Databases view** (key `8`). For each database it shows connections against its limit, how many are running a query, queries a second, how long they take and how often reads come from memory, over the last minute, hour and day (`w` switches between them). Under it are the selected database's clients (which app's instance, or clients outside through which proxy), its users and its slowest queries.
- **MySQL and Redis show p50, p95 and p99**, from the histograms they keep. **Postgres shows the average per query and its slowest queries**, from `pg_stat_statements`. `jokku db:postgres:info shopdb` shows the same numbers. See [See inside it](/docs/databases#see-inside-it).
- New Postgres databases load `pg_stat_statements` from the start. On one created before v0.8.12, Jokku turns it on the first time it looks, and it starts at the database's next restart: `jokku db:postgres:restart shopdb`. Until then, the average is per transaction.
- **[Users](/docs/databases#users) for Postgres and MySQL databases**: `jokku db:postgres:users:add shopdb reporting --read-only` adds a login with a password Jokku makes, and prints its URL. A user reads and writes the data, or only reads, but can't change the schema. `users:list`, `users:passwd` and `users:remove` do the rest, and removing a user closes its connections.
- Every ten seconds, your control server runs each database's own client in its microVM to look inside. Those runs don't show up as events, and their queries aren't counted.

## v0.8.11: Databases kept on your network

October 12, 2026

- **Reach a database from your own network but not from the internet** with `jokku db:postgres:expose shopdb shopdb.192.168.1.10.sslip.io --lan`. Your servers take the connections, and your [edges](/docs/edges) don't: they don't listen on the port, and they can't reach the database. See [Only on your network](/docs/databases#only-on-your-network).
- Unless you give an `--allow` list of your own, only private addresses are let in: your home network's, Tailscale's, and IPv6's private ones.
- The certificate is one the proxy makes itself, since Let's Encrypt can't reach your network. `sslmode=require` accepts it without checking.
- `jokku db:postgres:info shopdb` has a "Through" row, which says whether the edges carry a database or only your servers do.

## v0.8.10: Databases you can reach from outside

October 11, 2026

- **[Expose a database](/docs/databases#reach-it-from-outside) at a domain** with `jokku db:postgres:expose shopdb db.example.com`, and connect to it from your laptop, a BI tool or another host. It works through an [edge](/docs/edges) too, so a database at home is reachable with no port open there.
- **Postgres and Redis go over TLS**, which the proxy insists on, and share one port each (5432 and 6379), told apart by the domain the client asks for. Postgres's usual `sslmode=require` works, and so does Postgres 17's direct TLS. A public domain gets a Let's Encrypt certificate that clients can check. **MySQL** gets a port of its own, since it can't be told apart by domain.
- **Let in only your addresses** with `--allow 203.0.113.7,198.51.100.0/24`. Without it, `expose` warns that any address can connect. `jokku db:postgres:info shopdb --public-dsn` prints the URL to connect with, and `db:postgres:unexpose shopdb` closes it again. Open connections stay open when you deploy other apps.
- `domains:add` refuses a database's app now. Domains route web traffic, which a database doesn't speak, so a domain added to one never did anything.
- Update every server, and every edge, to v0.8.10 before you expose a database. A server or edge that's still on v0.8.9 keeps serving the web, but not exposed databases.

## v0.8.9: The last hour and day in jokku top

October 11, 2026

- **The Traffic view in [`jokku top`](/docs/top#traffic) shows p50 and p95 over the last hour and the last 24 hours**, under the last ten seconds' numbers. They count every request your servers' proxies handled. Each server keeps a compact record of how long its requests took, rebuilt from its proxy's log when it restarts, and the control server adds them up, so the percentiles are the whole cluster's, to within 5%.
- A server that's down or doesn't answer is named on that line, so you know its requests aren't counted.

## v0.8.8: The sun lines up

October 11, 2026

- The [splash screen](/docs/installation#the-splash-screen)'s sun is drawn with half blocks (▀ ▄ █), from the same set of characters as the logo, instead of quarter blocks. Many fonts don't have quarter blocks, so terminals borrowed them from another font, which put the edges of the sun and its stripes out of line.

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
