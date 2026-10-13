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
| `JOKKU_VERSION=v0.8.13` | Install a specific release instead of the latest. |
| `JOKKU_IMPORT_KEYS=0` | Don't give the SSH keys you logged in with access to Jokku. |
| `JOKKU_SPLASH=1` or `0` | Answer the splash screen question in advance: show it at login, or don't. |

```sh
curl -fsSL https://raw.githubusercontent.com/wes/jokku/main/install.sh | sudo JOKKU_VERSION=v0.8.13 sh
```

## The splash screen

Setup asks whether to greet you with Jokku's splash screen when you log in to the server. It shows the logo and live figures:

- The cluster: apps, nodes, instances and the last deploy.
- The server itself: address, load, memory, disk and uptime.
- Anything that needs a look: a node that's down, an app short of healthy instances, or system updates waiting.

```
  jokku1  control node  ● ready  jokku v0.8.13

  apps       15 · 1 asleep                    ip      192.168.86.113
  databases  2                                load    0.52 0.26 0.15 · 6 cpus
  nodes      5 of 5 ready                     memory  3.6G of 29G
  instances  19 · 7 on this node              disk    11% of 387G
  mesh       10.210.1.1                       uptime  2 days, 17 hours
  deployed   blog, 4 hours ago

  jokku help · jokku top · jokku apps:list · docs at https://jokku.dev
```

It takes the place of the system's own welcome text. Warnings, such as a restart needed after updates, still show. You can turn it on or off at any time:

```sh
sudo jokku splash            # show it now
sudo jokku splash:enable     # show it at every login
sudo jokku splash:disable    # stop, and bring back the system's welcome text
```

If you already had your own `/etc/update-motd.d/00-jokku`, Jokku keeps it, and `splash:disable` puts it back. Setup asks once per server: during the install, or during the next update if nobody was at a terminal to answer.

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
| 443/udp | every server that takes web traffic | HTTP/3, a faster HTTPS (optional: browsers fall back to HTTP/2) |
| 7443/tcp | first server | Servers joining the cluster |
| 51820/udp | every server | The encrypted network between servers |

The last two only matter once you [add more servers](/docs/cluster). An [edge](/docs/edges) needs only these open: TCP 80 and 443, UDP 443 and UDP 51820. The servers behind it need none.

## Repair a server

`jokku setup` brings the server to the state the installed release needs. Run it again if something on the server was changed or removed:

```sh
sudo jokku setup
```
