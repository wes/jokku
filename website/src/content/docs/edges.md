---
title: Edges & home labs
description: Run Jokku at home with no port open, and serve your domains from a small public server that the cluster dials.
---

## Serve a home lab from a public server

Jokku can run on servers that the internet can't reach: a home lab behind a router, say. An **edge** is a small public server, such as a $5 VPS, that receives the traffic for your domains and sends it to your apps over Jokku's encrypted network. Your servers at home connect out to the edge, so nothing at home needs an open port.

```
 browser ──https──▶ edge (public)                home (no open ports)
                    certificates, routes,  ◀═══  your Jokku servers dial it
                    logins                       over WireGuard
```

Add one from your control server, with the edge's address. Jokku logs in over ssh, installs itself there and connects:

```sh
jokku edge:add root@203.0.113.7
```

On the edge, open UDP port 51820 (the encrypted network) and TCP ports 80 and 443 in its firewall. Then point your domains' DNS at it. That's all: every app's domains, certificates and logins are served from the edge, and you keep managing everything at home.

| Command | What it does |
| --- | --- |
| `edge:add [<user>@]<address>` | Add an edge and install it over ssh (as root unless you give a user with sudo). |
| `edge:add <address> --print` | Print the command to run on the edge yourself instead. |
| `edge:list` | Show the edges and whether they're connected. |
| `edge:remove <name>` | Remove an edge: it stops routing, and your servers stop dialing it. |

An edge needs no KVM and runs no apps, so the smallest cloud server will do. Add a second one in another region and point DNS at both if you like.

> [!TIP]
> Your servers at home can keep serving too. With the same domains resolving to a home server on your own network, traffic from home skips the edge, with the same certificates. External apps are served by the edges and by the server they go through.

## What an edge can reach

An edge faces the internet, so Jokku gives it only what its routes need. Over the encrypted network it can reach the web ports of your apps' instances, the [external apps](#services-jokku-doesnt-run) it routes to, and the control server's API, which it uses to fetch its routes. It can't reach anything else at home: not ssh, not other ports of your apps or databases, not other machines on your network. It never receives an app's code or your backups.

Like any HTTPS proxy, an edge holds your certificates and sees the requests it forwards. To check [logins](/docs/logins) on its own, it also holds the users' password hashes and authenticator secrets and the key that signs sessions; removing an edge replaces that key, which logs everyone out.

When your servers at home can't be reached, the edge answers with a page saying so rather than an error.

## Services Jokku doesn't run

Route a domain to anything on your network, such as Home Assistant, a NAS or Proxmox, as an **external app**:

```sh
jokku external:create ha http://192.168.1.50:8123
jokku domains:add ha ha.example.com
jokku letsencrypt:enable ha
```

An external app is like any other for routing: `domains:*`, `letsencrypt:*`, `proxy:*`, [logins](/docs/logins) and `jokku logs ha` all work. Edges reach it through the control server, which forwards to it; give `--via <node>` to go through another server on its network. Those are the servers that serve it: the edges, and the one it goes through.

| Command | What it does |
| --- | --- |
| `external:create <name> <url>` | Route to `http://` or `https://` an IP address and port. |
| `external:create ... --insecure` | For an `https://` service with a self-signed certificate. |
| `external:list`, `external:info <name>` | Show them. |
| `external:set <name> url\|via\|insecure <value>` | Change one. |
| `external:destroy <name>` | Stop routing to it. |

The URL takes a private IP address (`10.x`, `172.16-31.x`, `192.168.x` or `100.64-127.x`) rather than a name: the edge reaches it through your server, and can't look up names on your network.
