# Jokku

Jokku is a self-hosted platform for deploying your apps, modeled on
[Dokku](https://dokku.com). You `git push` an app that has a Dockerfile, and
Jokku builds it and runs it behind HTTPS.

What's different: every app instance runs in its own lightweight
[Firecracker](https://firecracker-microvm.github.io) virtual machine instead
of a container, and you can add servers with one command, so apps scale
across a cluster. The commands are Dokku's, so if you know Dokku, you
already know Jokku.

> **Status: early development.** On a single server, `git push` builds your
> Dockerfile and runs it in Firecracker microVMs behind the proxy, with
> zero-downtime deploys, scaling, logs and updates. Multi-server clusters
> are next. See the [roadmap](docs/architecture.md#milestones).

## Install

On a fresh Ubuntu or Debian server with KVM (bare metal, or a virtual
machine with nested virtualization turned on) and ports 80 and 443 free:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
```

That's it. The SSH keys you used to log in to the server can now deploy apps
and run commands.

Run commands from your laptop over SSH, or on the server itself with
`sudo jokku <command>`. The examples below assume this alias on your laptop:

```sh
alias jokku='ssh jokku@your-server'
jokku apps:list
```

To let someone else deploy, add their public key:

```sh
cat alice.pub | jokku ssh-keys:add alice
```

### Adding more servers

*Coming in milestone 2.*

One server is a complete Jokku. To add another, ask the first server for a
join command:

```sh
jokku cluster:join-command
```

It prints a one-liner. Run it on the new server:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh -s -- --join 203.0.113.10 --token jk1_...
```

The servers connect over an encrypted private network (WireGuard). Apps are
spread across all of them, and every server can receive web traffic, so point
your DNS at as many as you like.

```sh
jokku nodes:list
NAME      ROLE     STATUS  ADDRESS        CPUS  MEMORY
server-1  control  ready   203.0.113.10   8     16g
server-2  worker   ready   203.0.113.11   8     16g
```

## Deploy an app

Your app needs a `Dockerfile`, and it should listen on the port in `$PORT`
(the port your Dockerfile `EXPOSE`s, or 5000).

**1. Create the app and set its config**

```sh
jokku apps:create myapp
jokku config:set myapp DATABASE_URL=postgres://...
```

**2. Push it**

```sh
cd myapp
git remote add jokku jokku@your-server:myapp
git push jokku main
```

Jokku builds the Dockerfile, boots the app, waits for it to accept
connections and prints its URL, for example `https://myapp.203.0.113.10.sslip.io`
(plain `http://` when the server's address is private, such as on a home
network). Pushing to an app that doesn't exist yet creates it, so step 1 is
optional. If the new version fails to start, the push is rejected and the
previous version keeps serving.

**3. Add your domain**

```sh
jokku domains:add myapp myapp.com
```

Point `myapp.com` at your server and Jokku gets an HTTPS certificate for it
automatically.

### Watch it

```sh
jokku logs myapp -t        # follow output from every instance
jokku ps:report myapp      # what's running, where, and how big
jokku ps:restart myapp     # also: ps:stop, ps:start, ps:rebuild
```

### Scale it

```sh
jokku ps:scale myapp web=3                                          # more instances
jokku resource:limit myapp --cpu 2 --memory 1g --process-type web   # bigger instances
```

On a cluster, instances are spread across your servers. To run more than one
kind of process, add a `Procfile` to your repo:

```
web: bin/server
worker: bin/jobs
```

Then scale each one: `jokku ps:scale myapp web=3 worker=1`.

### Deploy from GitHub Actions

Create a key for GitHub and give it access:

```sh
ssh-keygen -t ed25519 -N "" -f deploy_key
cat deploy_key.pub | jokku ssh-keys:add github-actions
```

Save the contents of `deploy_key` as a repository secret named
`JOKKU_DEPLOY_KEY`, then add `.github/workflows/deploy.yml`:

```yaml
name: deploy
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
      - run: |
          mkdir -p ~/.ssh
          echo "$JOKKU_DEPLOY_KEY" > ~/.ssh/id_ed25519 && chmod 600 ~/.ssh/id_ed25519
          ssh-keyscan your-server >> ~/.ssh/known_hosts
          git push jokku@your-server:myapp HEAD:refs/heads/main
        env:
          JOKKU_DEPLOY_KEY: ${{ secrets.JOKKU_DEPLOY_KEY }}
```

## Update

On the server:

```sh
sudo jokku update
```

It shows the new version and asks before changing anything. It backs up
Jokku's database first, and if the new version fails to start, it puts the
previous version and data back automatically. Your apps keep running and
serving traffic throughout. Use `--yes` to skip the question or
`--version v0.0.3` to pick a release.

Servers on v0.0.2 or older don't have `jokku update` yet. Update them once
with:

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/update.sh | sudo sh
```

With more than one server, update the first (control) server, then the rest.

## Learn more

- [Commands](docs/commands.md): every command and what works today
- [Architecture](docs/architecture.md): how Jokku works under the hood
