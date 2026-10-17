---
title: Processes & scaling
description: Run more instances, bigger instances, and more kinds of process, with zero-downtime rollouts.
---

## Scale out and up

```sh
jokku ps:scale myapp web=3                                          # more instances
jokku resource:limit myapp --cpu 2 --memory 1g --process-type web   # bigger instances
```

Each instance is its own Firecracker microVM. On a [cluster](/docs/cluster), instances are spread across your servers. `ps:scale` with no counts shows the current scale.

## Process types

To run more than one kind of process, add a `Procfile` to your repo:

```procfile
web: bin/server
worker: bin/jobs
```

Only `web` runs at first. Scale the others to start them:

```sh
jokku ps:scale myapp web=3 worker=1
```

Only `web` gets HTTP traffic. For a Procfile somewhere else: `jokku builder:set myapp procfile Procfile.prod`.

## Instance sizes

Each instance starts with **1 vCPU and 256 MiB** of memory. Set sizes per process type:

```sh
jokku resource:limit myapp --cpu 2 --memory 1g --process-type web
jokku resource:limit myapp --memory 512m --process-type worker
jokku resource:report myapp
jokku resource:limit-clear myapp --process-type worker
```

`resource:report` shows the size each process type runs at, a compose file's `cpus` and `mem_limit` included, and `jokku top` shows it on its Apps view. Memory takes `512`, `512m` or `1g`; plain numbers are megabytes. In a microVM, the limit is also the reservation: memory is never overcommitted, so an instance only starts on a server with that much free. vCPUs may be shared.

> [!NOTE]
> Firecracker can't add vCPUs to a running VM, so a size change is applied with a rolling restart into new microVMs, the same way Dokku applies resource changes.

## Start, stop and restart

```sh
jokku ps:report myapp      # what's running, where, and how big
jokku ps:restart myapp     # restart every instance
jokku ps:stop myapp
jokku ps:start myapp
jokku ps:rebuild myapp     # build the last pushed source again and deploy it
```

Crashed instances restart on their own. The policy is `on-failure:10` by default:

```sh
jokku ps:set myapp restart-policy always    # always | no | on-failure[:N]
```

## Sleep when idle

An app few people visit doesn't need its microVMs running all day. Give it a `sleep-after`, and once it goes that long without a request, its instances stop:

```sh
jokku ps:set myapp sleep-after 15m   # or 30s, 2h; at least 30s
jokku ps:set myapp sleep-after       # never sleep (the default)
```

The next request wakes it. The proxy holds the request while the instances boot where they were, then passes it on, so the visitor just waits a moment: about as long as the app takes to start listening, a second or two for a small one. Requests that arrive meanwhile wait with it, for up to a minute.

- The whole app sleeps and wakes together, every process type. Its volumes stay where they are.
- Its memory still counts against its servers, so it always has room to wake.
- Only requests through the proxy keep it awake, and only they wake it. Anything that requests it counts, a monitoring check included. Apps calling it at `myapp.internal` don't count and can't wake it, so leave apps that others call awake.
- A request that stays open, such as a websocket, keeps it awake.
- With a [login](/docs/logins) in front, only requests that get past the login wake it: the login page is served without it.
- `ps:report` says whether it's asleep, and `ps:start` wakes it. Deploys, restarts and config changes wake it too.
- Databases don't sleep.

## Zero-downtime deploys

Every deploy, scale or settings change rolls out the same way:

1. Jokku boots the new release's instances to the target scale.
2. It waits for them to pass checks: the process stays up and, for `web`, accepts TCP connections on `$PORT`.
3. It switches traffic to the new instances.
4. After the `wait-to-retire` period, 60 seconds by default, it stops the old ones.

If checks fail, the deploy fails, the new instances are torn down, the old release keeps serving, and you see the failing instance's output.

```sh
jokku checks:set myapp wait-to-retire 30   # seconds to keep old instances
jokku checks:set myapp timeout 300         # seconds new instances get to pass checks (default 120)
jokku checks:report myapp
```

**Only what changes restarts.** A process type the new release would run exactly as before (same image, command, environment, port, size and volumes) is left running. Scaling `worker` leaves `web` alone, and the deploy says so:

```console
remote: -----> Unchanged, left running: web
```

> [!WARNING]
> A process type with a [volume](/docs/storage) runs one instance, and its deploys stop the old instance before starting the new one, so expect a few seconds of downtime there.

## Releases

Each deploy and config change creates a numbered, immutable release: the build, process types, config vars and sizes.

```sh
jokku releases myapp
```

`releases:rollback` is planned.

## App housekeeping

```sh
jokku apps:list
jokku apps:rename myapp shop
jokku apps:clone myapp myapp-staging   # copies config, scale and settings
jokku apps:lock myapp                  # refuse deploys until apps:unlock
jokku apps:destroy myapp
```
