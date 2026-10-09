---
title: Domains & HTTPS
description: Route your domains to an app and get Let's Encrypt certificates, renewed for you.
---

## Default addresses

Every app gets an address as soon as it deploys. At install, the global domain is your server's public IP on [sslip.io](https://sslip.io), so `myapp` answers at `http://myapp.203.0.113.10.sslip.io` with no DNS setup at all.

To give apps addresses under your own domain instead, set a global domain and point a wildcard record, `*.example.com`, at your server:

```sh
jokku domains:set-global example.com
```

Apps deployed without a domain of their own then get `<app>.example.com`.

## Add a domain

```sh
jokku domains:add myapp myapp.com www.myapp.com
jokku domains:report myapp
```

Point each domain at your server with an A record. With [more than one server](/docs/cluster), every server can take web traffic, so point DNS at as many of them as you like.

| Command | What it does |
| --- | --- |
| `domains:add <app> <domain>...` | Add domains. |
| `domains:remove <app> <domain>...` | Remove domains. |
| `domains:set <app> <domain>...` | Replace the app's domains. |
| `domains:clear <app>` | Remove them all. |
| `domains:report <app>` | Show them. |

Each has a `-global` variant, such as `domains:add-global`, for the global domains.

## HTTPS

HTTPS is off for new apps, as in Dokku. Turn it on per app:

```sh
jokku letsencrypt:set --global email you@example.com   # once, for expiry notices
jokku letsencrypt:enable myapp
```

Jokku then gets a certificate for each of the app's public domains, renews it, and redirects HTTP to HTTPS. A domain Let's Encrypt can't reach, such as an IP address or an sslip.io name for a private address, stays on plain HTTP.

```sh
jokku letsencrypt:report myapp
jokku letsencrypt:disable myapp      # back to plain HTTP
jokku letsencrypt:enable --global    # new apps start with HTTPS on
```

> [!TIP]
> On a cluster, certificates are stored centrally. Any server can answer the Let's Encrypt challenge for a certificate another one requested, and each certificate is issued once for the whole cluster.

## Apps without web traffic

Turn the proxy off for an app that shouldn't be reachable from the internet, such as a database or a queue. Other apps can still reach it over [private networking](/docs/networking).

```sh
jokku proxy:disable cache
jokku proxy:report cache
```

## Coming later

- `certs:add` to install your own certificate.
- `ports:*` to map ports the way Dokku does. For now, apps listen on `$PORT`.
