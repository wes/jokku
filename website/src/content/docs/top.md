---
title: jokku top
description: A live view of your servers, apps, instances and every request, in your terminal.
---

```sh
jokku top
```

A live view of the machines, apps, instances and recent events, with CPU and memory for each, refreshed every two seconds. Over SSH it needs a terminal, so use `ssh -t jokku@your-server top`, or put `-t` in your alias.

## Views

Switch with the number keys, or with tab:

| Key | View | Shows |
| --- | --- | --- |
| `1` | Overview | Servers, apps, databases and recent events at a glance |
| `2` | Nodes | Each server: status, CPU, memory, and memory promised to microVMs |
| `3` | Apps | Each app: healthy instances, release, last deploy, domains, and the databases it uses |
| `4` | Instances | Each microVM: state, server, CPU, memory, uptime, restarts |
| `5` | Events | Deploys, crashes, servers coming and going |
| `6` | Traffic | Every request, live |
| `7` | Backups | What is and isn't backed up, and the switches to change it |

On a server or app, press `enter` to see its instances, and `l` for an app's logs. `esc` goes back, `?` shows help and `q` quits.

## Apps and databases

The overview and **Apps** list your [databases](/docs/databases) apart from your apps, below them. On **Apps**, each app has the databases it's linked to under it, with the config var that holds each one's URL, and each database shows the apps that use it:

```console
  APP                    STATUS     INSTANCES      REL    LAST DEPLOY                DOMAINS
  ● blog                 running    ●              v3     ok 3d ago                  blog.example.com
    └ shopdb             running    ●              postgres                          DATABASE_URL
  ● shop                 running    ●●●            v12    ok 1h ago                  shop.example.com
    └ shopdb             running    ●              postgres                          DATABASE_URL
    └ cache              asleep     ○              redis                             REDIS_URL

  DATABASE               STATUS     INSTANCES      ENGINE    LAST DEPLOY             USED BY
  ● scratch              running    ●              mysql     ok 2d ago               not linked
  ● shopdb               running    ●              postgres  ok 2h ago               blog, shop
  ● cache                asleep     ○              redis     ok 5d ago               shop
```

## Traffic

Press `6` for **Traffic**. Every request flies across its app's lane as a dot and lands on the instance and server that answered:

```console
  42.6 req/s  ·  p50 3.1ms  p95 18ms  ·  0.4% errors  ·  12804 total
  ▅▆▅▇▆▅▆█▇▆▅▄▅▆▇▆▅▆▇█▆▅▄▅▆▇▆▅ req/s   ▁▁▂▁▃▂▁▁▂▁▁▃▂▁▁▂▁▁▂▁▆▂▁▁▂▁▃▂ p95

  api            2 ●●   28.1/s ┄┄━━●┄╌──━●┄┄┄╌─━━●┄┄┄┄╌──━●┄┄ ▶ web.1@server-1 ▮▮▮▮▮▮ 14.2/s  web.2@server-2 ▮▮▮▮▮ 13.9/s
  shop           3 ●●●  14.2/s ┄┄┄┄╌─━●┄┄┄┄╌─━●┄┄┄┄╌──━━✖┄┄┄┄ ▶ web.1@server-2 ▮▮▮▮▮▮ 14.2/s
    web          1 ●
    worker       2 ●●
  docs           1 ○     0.0/s ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄ ▶ asleep
```

Dots are green for success, cyan for redirects, yellow for 4xx and red for 5xx, and each trails a tail as it goes. A request slower than most of its app's crawls across, trailing amber, and a 5xx bursts where it lands. The instance a request lands on lights up, and busy lanes glow.

After each app's name are its machines: how many, and a dot for each. A dot is green while its machine runs, yellow while it starts, red when it's down, and hollow and dim while the app [sleeps](/docs/processes#sleep-when-idle), when the lane says **asleep**. An app with more than one process group, such as `web` and `worker`, gets a line for each under its lane. An app's name turns orange while one of its requests is on the way.

The headline shows requests per second, latency and error rate over the last ten seconds, and under it the last minute, a second at a time. Below the lanes, you'll see which servers' proxies received the traffic and a list of recent requests.

Press `p` to pause, `c` to clear and `f` to turn the effects off. With them on, the view redraws about 30 times a second: over `ssh -t` on a slow connection, turning them off keeps it responsive. Run on your own computer, `jokku top` draws there and fetches only the requests from your server.

## Backups

Press `7` for **Backups**: every volume and the cluster itself, backed up or not, so you can tell at a glance that everything that matters is safe.

```console
  ⚠ 1 volume not backed up
  Volumes      ██████████████████████████░░░░░░░░░░░░░░  2 of 3 volumes
  Cluster      ● every 1h · last 12m ago · next in 48m · 41 kept, 210 MB
  Key          ● saved · ID 3f9a1b2c4d5e6f70
  Destinations tigris (my-backups, encrypted)

    WHAT                         NODE      USED           LAST BACKUP     NEXT     EVERY   KEPT   STORED   AUTO-RESTORE
  ● cluster (control node)                                ✓ 12m ago       in 48m   1h      41     210 MB
  ● shop / pgdata                server-2  1.3G / 10.0G   ✓ 4m ago        in 11m   15m     98     1.4 GB   on
  ● status / kuma-data           server-2  120M / 1.0G    ✓ 2m ago        in 13m   15m     96     88 MB    on
  ○ blog / uploads               server-1  3.0G / 10.0G   not backed up (space)

  shop / pgdata → tigris jokku/shop/pgdata
  every 15m · if its node dies, restored elsewhere after 5m
  keeping every backup from the last 24h and the last of each day for 30 days
  ▁▂▃▁▁▂▅▁▁▁▂▃▂▁▁▄▁▁▂▁▁▃▁▂ recent backups, by what each added
```

Dots are green when backups are up to date, yellow when one is overdue or hasn't happened yet, red when the last one failed, and grey when there are none. When nothing needs attention, the top line says **Everything is backed up**. The selected row shows where its backups go, what's kept, and a chart of recent backups by how much each added, with failures in red.

| Key | Does |
| --- | --- |
| `space` | Turn backups on for the selected volume or the cluster, or off (press twice). With more than one destination, it asks which. |
| `b` | Back it up now |
| `s` | Change how often: 15m, 1h, 6h, 1d, or off |
| `a` | Turn automatic restore on or off |

Restoring replaces data, so it stays on the command line: the selected row shows the `jokku backups:restore` command for it. See [Backups](/docs/backups).

## Events without the dashboard

```sh
jokku events
jokku events myapp -n 50
```
