---
title: Logins
description: Put a login page in front of any app, with a shared password or with users and authenticator codes.
---

## A password in front of an app

Keep an app private without changing it. The simplest login is one shared password (6 characters at least, and longer is better):

```sh
jokku http-auth:enable myapp --password
```

Visitors get a login page before the app. After logging in they stay logged in for 30 days, on that domain. Change the password with `http-auth:set-password myapp`, which logs everyone out.

## Users

For more than one person, add users and let them in. A user can also have a code from an authenticator app (1Password, Google Authenticator, Authy and so on):

```sh
jokku http-auth:users:add wes --totp   # asks for a password, then shows a QR code to scan
jokku http-auth:enable myapp           # any user may log in
jokku http-auth:enable photos wes sam  # only these users
```

The app receives the user's name in the `X-Jokku-User` header, so it can tell who's there. Jokku removes that header, in any spelling, from every incoming request to every app, so visitors can't set it themselves.

| Command | What it does |
| --- | --- |
| `http-auth:users:add <name> [--totp]` | Add a user. |
| `http-auth:users:list` | List them. |
| `http-auth:users:passwd <name>` | Change a password, ending that user's sessions. |
| `http-auth:users:totp <name> [--off]` | Give a user a new authenticator code, or remove it. |
| `http-auth:users:remove <name>` | Remove a user. |

Without a terminal, for scripts, passwords are read from stdin: `echo "$PW" | ssh jokku@server http-auth:users:add wes`.

### One login for every app

By default each domain has its own login page. Set a login domain to log in once for every app that uses users:

```sh
jokku http-auth:set --global login-domain auth.example.com
```

Point `auth.example.com` at your servers (or [edge](/docs/edges)). Apps then send visitors there to log in, and back.

## Exceptions

Some requests shouldn't need a login: your own network, or webhooks a service calls.

```sh
jokku http-auth:add-allowed-ip myapp 192.168.1.0/24
jokku http-auth:add-bypass-path myapp '/api/webhook/*'
```

A path ending in `*` lets in everything below it; otherwise it must match exactly. Remove them with `http-auth:remove-allowed-ip` and `http-auth:remove-bypass-path`.

## Share links

Give someone access for a while, without an account:

```sh
jokku http-auth:share myapp --expires 7d --note "for Sam"
```

It prints a link that lets whoever holds it in until it expires. List an app's links with `http-auth:shares myapp`, and revoke one with `http-auth:unshare myapp <id>`.

## Good to know

- `http-auth:report myapp` shows an app's login, and `http-auth:report --global` the login domain and session length (`http-auth:set --global session-days 7`).
- Logins are checked by Jokku's proxy on every server that serves the app, edges included, and work for [external apps](/docs/edges#services-jokku-doesnt-run) too.
- Wrong guesses are limited per address, per app and per user: after too many in ten minutes, logging in waits. People already logged in aren't affected. Each server keeps its own count.
- `http-auth:disable myapp` removes the login and keeps its settings for next time.
