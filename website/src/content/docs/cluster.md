---
title: Adding servers
description: One server is a complete Jokku. Add another with one command and apps spread across both.
---

## Add a server

Ask the first server for a join command:

```sh
jokku cluster:join-command
```

It prints a one-liner. Run it on the new server, which needs the same [requirements](/docs/installation#requirements) as the first:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo JOKKU_VERSION=v0.5.0 sh -s -- --join 203.0.113.10:7443 --token JOKKU1...
```

The servers connect over an encrypted private network (WireGuard on UDP 51820; the first server also needs TCP 7443 open for joining). Apps are spread across all of them, and every server can receive web traffic, so point your DNS at as many as you like.

```console
$ jokku nodes:list
NAME      ROLE     STATUS  ADDRESS        MESH IP     CPUS  MEMORY  INSTANCES  VERSION
server-1  control  ready   203.0.113.10   10.210.1.1  8     16g     3          v0.5.0
server-2  worker   ready   203.0.113.11   10.210.2.1  8     16g     2          v0.5.0
```

A join command works for an hour and adds one server. For autoscaling, make one that adds any number of servers until it expires, and put it in your cloud-init user-data:

```sh
jokku cluster:join-command --ttl 24h --reusable
```

## The control server

The first server is the **control** server. It holds the cluster's state, builds your apps and receives your `git push` and SSH commands. It also runs apps, like every other server.

The others are **workers**. They run apps and serve traffic, and hold no state of their own: wipe one and join it again, and it picks up where it left off.

> [!NOTE]
> If the control server goes down, the others keep running and serving what they have, and restart crashed instances. You can't deploy or change anything until it's back.

## Where apps run

When an instance needs a home, Jokku picks a server that is up, has the memory free, and runs the fewest instances of that same process. So `ps:scale myapp web=3` on three servers puts one on each.

```sh
jokku nodes:set server-1 schedulable false   # run no apps here (say, keep the control server for builds)
jokku nodes:set server-3 ingress false       # take no web traffic here
jokku nodes:report server-2
jokku cluster:report
```

## When a server dies

1. After 30 seconds without a report, it's marked `down`, and the proxies stop sending it requests.
2. After another minute, so that reboots and updates don't shuffle anything, its instances are started on the other servers.
3. When it comes back, it stops the instances that have moved.

The proxy gives up on an unreachable instance after two seconds and retries the request on another, for up to five seconds. Apps with [volumes](/docs/storage) on a dead server wait for it to come back, because their data is there. To bring them up elsewhere, [restore their latest backup](/docs/backups#restore) onto another server.

## Maintenance

To take a server out for maintenance, drain it first:

```sh
jokku nodes:drain server-2     # move its apps away with no downtime
jokku nodes:undrain server-2   # let it take apps again
```

Draining starts a copy of each instance elsewhere, waits for it to pass checks, then stops the original, so traffic never drops. Instances with volumes move with their disks, which costs a short stop.

To take a server out of the cluster for good:

```sh
jokku nodes:remove server-2
```

`nodes:remove` refuses while the server holds volumes. `--force` skips the drain and treats the server as dead, losing any volumes on it.
