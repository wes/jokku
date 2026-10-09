---
title: Logs & shell access
description: Follow your app's output and its requests, and open a shell inside a running instance.
---

## Logs

```sh
jokku logs myapp -t          # follow output from every instance, plus a line per request
jokku logs myapp -n 500      # the last 500 lines (default 100)
jokku logs myapp -p worker   # one process type...
jokku logs myapp -p web.2    # ...or one instance
jokku logs myapp -p router   # just the requests
jokku logs myapp -q          # messages only, without timestamps and names
```

Output from every instance, on every server, comes back in one stream, with a Heroku-style `app[router]` line for each request:

```console
2026-10-08T14:02:11.204518Z app[web.1]: Listening on :3000
2026-10-08T14:02:15.880921Z app[router]: method=GET path="/" host=myapp.com status=200 duration=4.2ms bytes=5120 instance=web.1@server-1 via=server-2
2026-10-08T14:02:16.017334Z app[worker.1]: Processed job 4812
```

Each router line shows which instance answered, on which server, and which server's proxy took the request.

## Open a shell

To look around inside a running instance, open a shell in it, or run a single command:

```sh
jokku enter myapp                      # a shell in the app's web (or only) process
jokku enter myapp worker.2             # in a particular instance
jokku enter myapp web ls -la /app      # one command; it exits with the command's status
jokku enter --root myapp web           # as root rather than the image's user
```

Over SSH, use `ssh -t jokku@your-server enter myapp` for an interactive shell.

Commands run as the image's `USER` with the app's environment, unless you add `--root`. Every session is recorded in `jokku events` along with who opened it.

> [!NOTE]
> `enter` reaches the microVM through Firecracker's vsock channel, so it needs no network and no SSH server in your image.

## What's running

```sh
jokku ps:report myapp
```

Shows what's running, where, and how big.

## Events

```sh
jokku events          # deploys, crashes and servers coming and going
jokku events myapp    # just one app
jokku events -n 100   # more history (default 30)
```

For a live view of everything, use [`jokku top`](/docs/top).
