---
title: Backups
description: Back volumes up to any S3-compatible bucket, encrypted and incremental, and restore them in place or onto another server.
---

Volumes back up to any S3-compatible bucket: Tigris, AWS S3, Cloudflare R2, Backblaze B2 and the like. Add the bucket once, as a destination, then say which volumes go there.

### 1. Add a destination

```sh
jokku backups:destination-add tigris --endpoint https://fly.storage.tigris.dev \
  --bucket my-backups --access-key-id tid_xxx      # asks for the secret key
```

The secret key is asked for, or read from stdin. Add `--region` if your provider needs one.

### 2. Save the encryption key

Backups are encrypted with a key Jokku makes the first time you add a destination. Jokku shows it once and asks you to confirm you saved it.

> [!WARNING]
> **Save the key somewhere safe, away from your servers.** A backup can't be restored without it, by Jokku or anyone else. `jokku backups:key` shows it again.

To store a destination's backups unencrypted instead, pass `--no-encrypt` to `backups:destination-add`.

### 3. Choose what to back up

```sh
jokku backups:set myapp data tigris     # under jokku/myapp/data in the bucket
jokku backups:run myapp data            # back it up now
jokku backups:list myapp data
```

`backups:set` takes an optional path as a last argument, for a different place in the bucket.

## How backups work

- **Complete, but incremental.** Each backup is complete on its own, but only uploads what changed since the last one. The disk is read in blocks, and a block already in the bucket is never uploaded again.
- **The app keeps running.** Its writes to the volume pause for a moment at the end, so the backup is the disk as it was at one instant, the way a power cut would leave it. Databases recover from that the same way they do after a crash.
- **Straight from the disk.** The server holding the volume uploads to the bucket itself.

## Restore

```sh
jokku backups:restore myapp data                          # the latest backup
jokku backups:restore myapp data 2026-10-09T14-15-00Z     # a particular one
jokku backups:restore myapp data --node server-2          # its server is down: restore onto another
```

The current data is backed up first, so a restore can be undone; `--skip-backup` skips that. The backup downloads while the app runs, then the app restarts with the restored disk.

> [!TIP]
> If a server dies for good, its volumes' apps wait for it. Restoring the latest backup with `--node` brings them back on another server.

## Manage backups

```sh
jokku backups:report              # where each volume backs up, and when it last did
jokku backups:report myapp
jokku backups:destinations        # the buckets you've added
jokku backups:unset myapp data    # stop backing it up; its backups stay in the bucket
jokku backups:destination-remove tigris
```

Removing a destination, or unsetting a volume, leaves its backups in the bucket.

## Coming next

For now, backups run when you ask. Schedules, and restoring automatically onto another server when one dies, come next.
