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
| `1` | Overview | Servers, apps and recent events at a glance |
| `2` | Nodes | Each server: status, CPU, memory, and memory promised to microVMs |
| `3` | Apps | Each app: healthy instances, release, last deploy, domains |
| `4` | Instances | Each microVM: state, server, CPU, memory, uptime, restarts |
| `5` | Events | Deploys, crashes, servers coming and going |
| `6` | Traffic | Every request, live |

On a server or app, press `enter` to see its instances, and `l` for an app's logs. `esc` goes back, `?` shows help and `q` quits.

## Traffic

Press `6` for **Traffic**. Every request flies across its app's lane as a dot and lands on the instance and server that answered:

```console
  42.6 req/s  ·  p50 3.1ms  p95 18ms  ·  0.4% errors  ·  12804 total

  api             28.1/s ┄┄●┄┄┄┄●┄┄┄●┄┄┄┄┄●┄┄●┄┄┄┄┄●┄┄ ▶ web.1@server-1 ▮▮▮▮▮▮ 14.2/s  web.2@server-2 ▮▮▮▮▮ 13.9/s
  shop            14.2/s ┄┄┄┄●┄┄┄┄┄┄┄●┄┄┄┄┄┄✖┄┄┄┄┄┄┄┄┄ ▶ web.1@server-2 ▮▮▮▮▮▮ 14.2/s
  docs             0.3/s ┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄┄●┄┄┄┄┄┄ ▶ web.1@server-1 ▮▮▮▮▮▮ 0.3/s
```

Dots are green for success, yellow for 4xx and red for 5xx, and the headline shows requests per second, latency and error rate, updating live. Below the lanes, you'll see which servers' proxies received the traffic and a list of recent requests.

Press `p` to pause and `c` to clear.

## Events without the dashboard

```sh
jokku events
jokku events myapp -n 50
```
