---
title: Updating
description: Update Jokku in place. It backs up first, rolls back on failure, and your apps keep serving throughout.
---

## Update a server

On the server:

```sh
sudo jokku update
```

It shows the new version and asks before changing anything.

```sh
sudo jokku update --yes               # don't ask
sudo jokku update --version v0.8.2    # pick a release
```

## What happens

1. Jokku downloads the new release and checks it against the release's checksums.
2. It backs up its database and the current binary. The newest five backups are kept, in `/var/lib/jokku/backups/`.
3. It installs the new binary and runs `jokku setup`, which brings the server to the state the new release needs, then waits until the new version is answering.
4. If any of that fails, it puts the previous version and data back automatically.

**Your apps keep running and serving traffic throughout.** Each microVM runs in its own system service rather than inside the Jokku daemon, and the proxy and internal DNS are separate services too, restarted only when their own code changed. The control commands are unavailable for a few seconds while Jokku restarts.

## Clusters

With more than one server, update the control server first, then run `sudo jokku update` on each of the others. The control server accepts workers one release behind, so you can update them one at a time.

> [!NOTE]
> Updating every server from the control server in one command is planned.

## Servers on v0.0.2 or older

These don't have `jokku update` yet. Update them once with:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
```
