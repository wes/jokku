---
title: Config vars
description: Set environment variables for an app, or for every app at once.
---

Config vars become your app's environment. Changing them rolls out a new release from the same build, so there's nothing to rebuild.

## Set and read

```sh
jokku config:set myapp DATABASE_URL=postgres://... SECRET_KEY=abc123
jokku config:show myapp            # all of them (also: jokku config myapp)
jokku config:get myapp SECRET_KEY  # one value
jokku config:keys myapp            # just the names
jokku config:unset myapp SECRET_KEY
jokku config:clear myapp           # remove every one
```

A change to a deployed app is applied right away. To store it for the next deploy instead, add `--no-restart`:

```sh
jokku config:set --no-restart myapp FEATURE_FLAG=on
```

Values with spaces, newlines or quotes are easier to pass base64-encoded, with `--encoded`:

```sh
jokku config:set --encoded myapp GREETING=aGVsbG8sIHdvcmxk   # "hello, world"
```

## Global config

`--global` sets a var for every app. App values win over global ones, and `--merged` shows both together:

```sh
jokku config:set --global TZ=UTC
jokku config:show --merged myapp
```

A global change reaches each app the next time it restarts or deploys.

## Export

```sh
jokku config:export myapp                  # export KEY='value' lines
jokku config:export --format envfile myapp
jokku config:export --format json myapp
```

Formats are `exports` (the default), `envfile`, `docker-args`, `shell`, `json`, `json-list` and `pretty`.

## The port

`PORT` is special: it sets the port your app listens on and Jokku routes to. See [the port](/docs/dockerfile#the-port).

## Compose apps

In a [compose app](/docs/compose), config vars fill in `${VAR}` in the compose file rather than going to every service, and only the services whose environment changes restart.
