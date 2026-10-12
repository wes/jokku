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
sudo jokku update --version v0.8.11    # pick a release
```

## What happens

1. Jokku downloads the new release and checks it against the release's checksums.
2. It backs up its database and the current binary. The newest five backups are kept, in `/var/lib/jokku/backups/`.
3. It installs the new binary and runs `jokku setup`, which brings the server to the state the new release needs, then waits until the new version is answering. If this server hasn't been asked yet, setup also asks whether to show [Jokku's splash screen](/docs/installation#the-splash-screen) at login.
4. If any of that fails, it puts the previous version and data back automatically.

**Your apps keep running and serving traffic throughout.** Each microVM runs in its own system service rather than inside the Jokku daemon, and the proxy and internal DNS are separate services too, restarted only when their own code changed. The control commands are unavailable for a few seconds while Jokku restarts.

## Clusters

Run `sudo jokku update` on the control server. Once it has updated itself, it brings the cluster's other servers to the same version, one at a time, and shows how each one goes:

```console
-----> Updating the cluster's other nodes, one at a time
       Ctrl-C stops watching, not the update; jokku nodes:list shows how it goes
       server-2: updating from v0.8.5 to v0.8.6
       server-2: updated to v0.8.6 (38s)
       server-3: updating from v0.8.5 to v0.8.6
       server-3: updated to v0.8.6 (41s)

=====> All 3 nodes run v0.8.6
```

Each server runs the same update as above, by itself, so its apps keep serving. The control server waits for each one to come back on the new version before starting the next, and while a server restarts, its instances stay put rather than being moved elsewhere.

- **If a server's update fails**, that server goes back to the version it had, and the update stops there, so a release that breaks one server isn't rolled onto the rest. The failed update's output is in `journalctl -u jokku-update` on that server. Once it's fixed, run `sudo jokku update` on the control server again to carry on.
- **Servers that are down** update when they're back.
- **`jokku nodes:list`** shows each server's version, and where it is in an update.
- **To update only the control server**, add `--this-node`. Running `sudo jokku update` there again later updates the rest.

Servers update themselves this way from v0.8.5. Run `sudo jokku update` once on each server that's on an older version. The control server accepts workers one release behind, so they keep working in the meantime.

## Servers on v0.0.2 or older

These don't have `jokku update` yet. Update them once with:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
```
