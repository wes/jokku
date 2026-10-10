---
title: Backups
description: Back volumes, and the cluster itself, up to any S3-compatible bucket, encrypted, incremental and on a schedule, and get them back automatically when a server dies.
---

Volumes back up to any S3-compatible bucket: Tigris, AWS S3, Cloudflare R2, Backblaze B2 and the like. Add the bucket once, as a destination, then say which volumes go there. From then on they're backed up every 15 minutes, and if their server dies, they come back on another one by themselves.

### 1. Add a destination

```sh
jokku backups:destination-add tigris --endpoint https://fly.storage.tigris.dev \
  --bucket my-backups --access-key-id tid_xxx      # asks for the secret key
```

The secret key is asked for, or read from stdin. Add `--region` if your provider needs one.

Your first destination also starts backing up [the cluster itself](#the-cluster-itself) every hour.

### 2. Save the encryption key

Backups are encrypted with a key Jokku makes the first time you add a destination. Jokku shows it once and asks you to confirm you saved it, by typing its last six characters.

> [!WARNING]
> **Save the key somewhere safe, away from your servers.** A backup can't be restored without it, by Jokku or anyone else. `jokku backups:key` shows it again.

To store a destination's backups unencrypted instead, pass `--no-encrypt` to `backups:destination-add`.

### 3. Choose what to back up

```sh
jokku backups:set myapp data tigris     # under jokku/myapp/data, every 15 minutes
jokku backups:list myapp data
jokku backups:run myapp data            # back it up now, too
```

`backups:set` takes an optional path as a last argument, for a different place in the bucket. Or open [`jokku top`](/docs/top#backups), press `7`, pick a volume and press `space`.

## Schedules and retention

Every volume is backed up every 15 minutes, keeping every backup from the last 24 hours and the last one of each day for 30 days. Change that with `backups:set`:

```sh
jokku backups:set myapp data tigris --every 1h            # or 6h, 1d, or off for only when you run backups:run
jokku backups:set myapp data tigris --keep-recent 48h --keep-daily 14
```

Running `backups:set` again without them keeps the schedule you have.

After each backup, the backups the schedule no longer keeps are deleted, and every few hours so are the blocks no remaining backup uses. A failed backup is tried again after five minutes. Scheduled backups don't fill `jokku events`: you'll see one when backups start failing, and another when they work again.

## How backups work

- **Complete, but incremental.** Each backup is complete on its own, but only uploads what changed since the previous one, so a backup every 15 minutes costs little. The disk is read in blocks, and a block already in the bucket is never uploaded again.
- **The app keeps running.** Its writes to the volume pause for a moment at the end, usually a few milliseconds, so the backup is the disk as it was at one instant, the way a power cut would leave it. Databases recover from that the same way they do after a crash.
- **Straight from the disk.** The server holding the volume uploads to the bucket itself, a couple of volumes at a time.
- **Encrypted before it leaves.** Blocks are compressed and encrypted on your server, and named so that their names give nothing away. A changed or damaged block is caught when it's restored.

## When a server dies

Once a server has been gone for five minutes, each backed-up volume on it is restored onto another server from its latest backup, and its app starts there. Anything written after that backup is lost, unless the server comes back.

If it does come back, it keeps its copy of the disk aside, never using it again, so you can still get at those last writes. `storage:report` shows where it is, and you delete it with:

```sh
jokku storage:discard-old-copy myapp data
```

A volume whose disk goes missing on a server that's up is restored in place, the same way.

To have a volume wait for its server instead, as it does without backups:

```sh
jokku backups:set myapp data tigris --auto-restore off
```

> [!NOTE]
> A server that's only cut off from the others, not dead, would otherwise keep running its copy of the app while the restored copy runs too. So a server that can't reach the control server for two minutes stops its apps with backed-up volumes, unless it's clearly the control server that's down: the servers it can still reach don't hear from it either, and together they're most of the cluster. In a two-server cluster, that means the worker stops those apps whenever it loses the control server for two minutes.

## Restore

```sh
jokku backups:restore myapp data                          # the latest backup
jokku backups:restore myapp data 2026-10-09T14-15-00Z     # a particular one
jokku backups:restore myapp data --node server-2          # its server is down: restore onto another
```

The current data is backed up first, so a restore can be undone; `--skip-backup` skips that. The backup downloads while the app runs, then the app restarts with the restored disk.

## The cluster itself

Your first destination also backs up the control server every hour: its database (apps, config vars, domains, settings and the backup key), its identity, and the images of each app's three most recent releases.

```sh
jokku backups:cluster tigris --every 6h     # where and how often
jokku backups:cluster-run                   # back it up now
jokku backups:cluster-list
jokku backups:cluster-unset
```

To rebuild a lost control server, install Jokku on a new server with the same name and, so the other servers reconnect, the same address. Then:

```sh
sudo jokku restore-cluster --endpoint https://fly.storage.tigris.dev \
  --bucket my-backups --access-key-id tid_xxx       # asks for the secret key and the backup key
```

It checks the backup is this server's, downloads the release images, puts the cluster's state in place (keeping the server's own aside) and restarts Jokku. Apps start again from their releases, without a new deploy, and volumes come back from their own backups. Remove servers that are gone for good with `jokku nodes:remove <name> --force`, so their volumes come back elsewhere.

## See it all

In [`jokku top`](/docs/top#backups), press `7` for a view of everything that is, and isn't, backed up: every volume and the cluster, when each was last backed up and is next, a chart of recent backups, and what they keep. From there, `space` turns a volume's backups on or off, `b` backs it up now, `s` changes how often and `a` turns automatic restore on or off.

On the command line:

```sh
jokku backups:report              # schedule, last and next backup, failures, and what's stored, for everything
jokku backups:report myapp
jokku backups:destinations        # the buckets you've added
jokku backups:unset myapp data    # stop backing it up; its backups stay in the bucket
jokku backups:destination-remove tigris
```

Removing a destination, or unsetting a volume, leaves its backups in the bucket.
