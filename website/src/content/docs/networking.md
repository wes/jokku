---
title: Private networking
description: Apps reach each other by name, on any server in the cluster, over an encrypted network.
---

## Apps talk to each other by name

Inside your apps, `<app>.internal` reaches another app, and `<process>.<app>.internal` one of its process types, on any server in the cluster.

| Name | Reaches |
| --- | --- |
| `cache.internal` | The `cache` app's `web` instances (or its only process type) |
| `worker.shop.internal` | The `shop` app's `worker` instances |
| `cache` | From any app, the `cache` app |
| `worker` | From `shop`, shop's own worker processes |

Connect on the port the app listens on:

```sh
jokku config:set myapp REDIS_URL=redis://cache.internal:6379
```

Names resolve to the addresses of the instances that should be running, preferring healthy ones, so a name follows a deploy as the new instances pass their checks. Answers live for five seconds, since instances come and go with deploys and moves.

> [!TIP]
> For an app that only other apps should reach, such as a database, turn its public proxy off with `jokku proxy:disable <app>`.

## Under the hood

- Each server runs a small DNS server for its microVMs, `jokku-dns`, separate from the main daemon, so updating Jokku never interrupts lookups. Every other name goes to the server's own resolvers.
- Servers are joined by a WireGuard mesh on UDP port 51820. Traffic between instances on different servers is encrypted and routed straight to the server that hosts them.
- The cluster network is `10.210.0.0/16`. Server *N* owns `10.210.N.0/24`, and its microVMs get addresses from it.
- Instances reach the internet through their server.
