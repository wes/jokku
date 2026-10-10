---
title: Installation
description: Install Jokku on a fresh server with one command, then run commands from your laptop over SSH.
---

## Requirements

- A fresh **Ubuntu or Debian** server.
- Ports **80 and 443** free.
- **KVM.** Bare metal works, or a virtual machine with nested virtualization turned on (on Proxmox, set the CPU type to `host`).
- A CPU from **2011 or newer**, which Firecracker needs: Intel Sandy Bridge or AMD Bulldozer and later. Jokku is tested on Intel Skylake and newer.

> [!TIP]
> Setup warns you if the CPU can't run Firecracker, and a deploy on such a server stops right away with the reason, rather than after a full build.

> [!NOTE]
> At home, behind a router? Ports 80 and 443 don't need to be open to the internet: install Jokku as usual, then add an [edge](/docs/edges), a small public server that receives your domains' traffic and sends it home.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo sh
```

That's it. The SSH keys you used to log in to the server can now deploy apps and run commands.

The installer downloads the `jokku` binary, checks it against the release's checksums, then runs `jokku setup`. Setup creates the `jokku` user, its directories and services, and installs the pinned Firecracker and BuildKit that Jokku runs with. Each step checks before it changes anything, so running it again is safe.

You can set a few options in the environment:

| Variable | What it does |
| --- | --- |
| `JOKKU_VERSION=v0.8.2` | Install a specific release instead of the latest. |
| `JOKKU_IMPORT_KEYS=0` | Don't give the SSH keys you logged in with access to Jokku. |

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo JOKKU_VERSION=v0.8.2 sh
```

## Run commands

Run commands from your laptop over SSH, with nothing installed locally, or on the server itself with `sudo jokku <command>`:

```sh
ssh jokku@your-server apps:list    # from your laptop
sudo jokku apps:list               # on the server
```

The rest of these docs assume this alias on your laptop:

```sh
alias jokku='ssh -t jokku@your-server'
jokku apps:list
```

> [!NOTE]
> The `-t` gives commands a terminal, which interactive ones like `jokku top` and `jokku enter` need. Leave it out when you pipe data through, as with `storage:export`.

## Give others access

To let someone else deploy, add their public key:

```sh
cat alice.pub | jokku ssh-keys:add alice
jokku ssh-keys:list
jokku ssh-keys:remove alice
```

Every key is an admin, as in Dokku without its ACL plugin. The key's name is recorded on each deploy, so you can see who shipped what.

## Ports

| Port | Where | Used for |
| --- | --- | --- |
| 22/tcp | first server | SSH: `git push` and commands |
| 80, 443/tcp | every server that takes web traffic | HTTP and HTTPS |
| 7443/tcp | first server | Servers joining the cluster |
| 51820/udp | every server | The encrypted network between servers |

The last two only matter once you [add more servers](/docs/cluster). An [edge](/docs/edges) needs only 80, 443 and 51820 open, and the servers behind it none.

## Repair a server

`jokku setup` brings the server to the state the installed release needs. Run it again if something on the server was changed or removed:

```sh
sudo jokku setup
```
