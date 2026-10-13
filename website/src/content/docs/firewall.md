---
title: Firewall
description: Jokku bans the addresses that scan your sites for weaknesses, and slows floods of requests, without being set up.
---

Every site on the internet gets scanned. Bots ask for `/.env`, `/.git/config`, `/wp-login.php` and hundreds of other files that might give something away, and on a small site they can make most of the requests. Jokku's firewall is on for every app from the start. It bans an address that asks for too many pages that don't exist, and slows one that sends too many requests a second.

## What it does

| Rule | Default | Stops |
| --- | --- | --- |
| Ban | 20 pages not found within 10 minutes bans an address for an hour | Vulnerability scanners |
| Rate limit | Over 50 requests a second, in bursts of up to 300, get `429 Too Many Requests` | One address flooding a site |

A banned address gets a short `403 Forbidden` from every site, saying why and for how long, until the ban ends.

- **Where:** in the proxy a visitor reaches first. Behind an [edge](/docs/edges) that's the edge, so blocked requests never travel up your home connection. Each proxy keeps its own counts, in memory.
- **Never banned or slowed:** private addresses (your home network, the cluster's own, Tailscale's `100.64.0.0/10`), and addresses you allow.
- **A site's broken links don't count:** a page not found that one of the site's own pages asked for, like a missing image, is the site's mistake, not the visitor's.
- **IPv6:** a ban covers the address's /64, which belongs to one customer, so a scanner can't step to the next address.
- **The visitor's own address:** the one their connection came from, whatever `X-Forwarded-For` they send. Apps get it in `X-Forwarded-For`, and the router logs show it as `client=`.

## See what it's doing

```console
$ jokku firewall:report
=====> Firewall
       Status:      on
       Ban:         20 pages not found within 10m bans an address for 1h
       Rate limit:  50 requests a second, bursts of 300
       Allowed:     private addresses
       Last hour:   1234 of 2045 requests blocked (60%), 0 slowed, 3 bans
       Last day:    13102 of 23697 requests blocked (55%), 0 slowed, 34 bans
=====> Banned (1)
ADDRESS         SITE        BANNED   LEFT  BLOCKED  ON           PAGES NOT FOUND
185.177.72.100  joedesigns  12m ago  48m   2919     jokku-edge1  /.env /.git/config /wp-login.php /phpinfo.php /.aws/credentials
=====> Close to a ban
ADDRESS         SITE        NOT FOUND  LAST
34.39.249.92    wunl        12 of 20   1m ago
```

The [Firewall view](/docs/top#firewall) of `jokku top` (key `9`) shows the same, live, site by site. Each ban is an [event](/docs/logs#events), and a request the firewall answered is marked in the app's router logs:

```console
$ jokku logs joedesigns -p router
... app[router]: method=GET path="/" host=joedesigns.com status=403 duration=0ms bytes=91 via=jokku-edge1 client=185.177.72.100 firewall=banned
```

Those answers take no time, so `jokku top` leaves them out of response times.

## Change it

| Command | What it does |
| --- | --- |
| `firewall:unban <ip>` | Let a banned address in again, on every proxy. |
| `firewall:add-allowed-ip <ip\|cidr>` | Never ban or slow an address, such as your office's or a monitoring service's. |
| `firewall:remove-allowed-ip <ip\|cidr>` | Treat it like any other again. |
| `firewall:disable <app>` | Turn it off for one app: one that answers "not found" a lot on purpose, like an API clients look things up in. `firewall:enable <app>` turns it back on. |
| `firewall:disable --global` | Turn it off everywhere; `firewall:enable --global` turns it back on. |
| `firewall:set <rule> [<value>]` | Change a rule: `not-found`, `window`, `ban`, `rate` (0 for no limit) or `burst`. Without a value, back to the default. |

```sh
jokku firewall:set not-found 10   # stricter
jokku firewall:set ban 24h        # longer bans
```

## What it can't stop

A flood from thousands of addresses at once, or one big enough to fill the edge's connection, can't be stopped by the server it reaches. That takes a network in front of your sites, such as Cloudflare. The firewall handles what small sites actually get: scanners, and one address sending far too much.

Behind a proxy of your own, such as Cloudflare's, rather than a Jokku [edge](/docs/edges), every visitor arrives from that proxy's addresses, and the firewall can't tell them apart: allow its addresses with `firewall:add-allowed-ip`, or turn the firewall off.

Bans live in each proxy's memory, so a proxy that restarts starts over. [Databases reached from outside](/docs/databases#reach-it-from-outside) aren't behind the firewall: limit who can connect with `--allow`.
