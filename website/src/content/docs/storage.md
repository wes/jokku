---
title: Volumes
description: Keep a database, uploads or a SQLite file across deploys on a volume that moves with its app.
---

An instance's own files are reset on every deploy, as in a container. Put data that must last, such as a database, uploads or a SQLite file, on a volume:

```sh
jokku storage:mount myapp data:/app/data                # creates the volume "data" (10g) if needed
jokku storage:mount mydb pg:/var/lib/postgresql/data --size 50g
jokku storage:list myapp
```

A volume is a disk on the server its instance runs on. It works the way a Docker volume does: on first use it gets whatever the image has at that path, and it is writable by the image's user. The disk only uses space for what is written, so its size is a limit, not an allocation.

Volumes mount in the `web` process unless you pass `--process-type worker`.

## How volumes behave

- **One instance per volume.** A process with a volume runs a single instance, so `ps:scale myapp web=2` is refused. Deploys stop the old instance before starting the new one, so expect a few seconds of downtime instead of a zero-downtime switch.
- **Moving servers.** `jokku nodes:drain` brings volumes along: the disk is copied while the app keeps running, then the app stops briefly for a final copy and starts on the new server.
- **If a server dies,** apps with volumes on it wait for it to come back instead of starting elsewhere, because their data is there. Volumes that are [backed up](/docs/backups) don't wait: after five minutes they're restored onto another server from their latest backup, and their apps start there.

> [!WARNING]
> A volume lives on one server. [Back up](/docs/backups) anything that matters: backups go to an S3-compatible bucket every 15 minutes, and bring the volume back on another server if its own dies.

## Move a volume

```sh
jokku storage:move myapp data server-2
```

The copy compares 1 MiB blocks by checksum, skips empty space, and repeats until little changes. Then the app stops, the last changed blocks are sent, and it starts on the new server. If the new server goes down before the move completes, the move is called off and the app starts again where it was.

## Copy files in and out

To see a volume's files, `jokku enter myapp web ls /app/data`. To copy them out or back in, as an archive on your laptop or to move data between servers:

```sh
jokku storage:export myapp data > data.tar.gz             # the app pauses for a moment
jokku storage:import myapp data --clear < data.tar.gz     # then the app restarts
```

Both go through the running instance that mounts the volume, so the app must be running.

- **Export** pauses the app's processes while it copies, so a database file is captured at one point in time. `--live` skips the pause.
- **Import** replaces files with the archive's (`--clear` empties the volume first), then restarts the app so it reads them. Imported files belong to whoever owns the volume's mount point, so an archive made on your laptop doesn't leave them owned by a user the app isn't. `--keep-owners` keeps the archive's numeric owners instead.

> [!TIP]
> Over SSH, leave out `-t` so the archive passes through untouched: `ssh jokku@your-server storage:export myapp data > data.tar.gz`.

## Manage volumes

```sh
jokku storage:create myapp uploads --size 20g   # create without mounting
jokku storage:resize myapp data 20g             # grow it
jokku storage:unmount myapp data                # detach it; the data stays
jokku storage:destroy myapp data                # delete it and its data
jokku storage:report myapp                      # where each volume's disk is, and any old copy kept aside
jokku storage:discard-old-copy myapp data       # delete the copy a server kept after its volume was restored elsewhere
```

## Coming later

Object-storage volumes (`--type s3`) that many instances can share, and services like `postgres:*` and `redis:*` built from apps plus volumes.
