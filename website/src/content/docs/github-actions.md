---
title: GitHub Actions
description: Deploy on every push to main. It's an ordinary git push with a key of its own.
---

### 1. Create a key for GitHub

```sh
ssh-keygen -t ed25519 -N "" -f deploy_key
cat deploy_key.pub | jokku ssh-keys:add github-actions
```

### 2. Save it as a secret

Save the contents of `deploy_key` as a repository secret named `JOKKU_DEPLOY_KEY`, then delete the file from your laptop.

### 3. Add the workflow

```yaml title=".github/workflows/deploy.yml"
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

The build output streams into the Actions log, and a failed deploy fails the job, while the previous version keeps serving.

> [!TIP]
> Jokku speaks the same protocol as Dokku, so `dokku/github-action` works unchanged too. Set its `git_remote_url` to `ssh://jokku@your-server:22/myapp`.

## Why `fetch-depth: 0`

A shallow clone can't be pushed to a new remote, so the checkout needs the full history.
