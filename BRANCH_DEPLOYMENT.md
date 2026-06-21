# Sub2API Branch Deployment

This repository uses two long-lived deployment branches:

- `main`: production source.
- `debug`: isolated debugging source.

Both branches are built by GitHub Actions. The server should pull the published
GHCR images instead of building locally, because local builds use too much memory
and disk space on the server.

## Images

The workflow `.github/workflows/docker-branch.yml` publishes:

- `ghcr.io/wesperez/sub2api:main`
- `ghcr.io/wesperez/sub2api:main-<short-sha>`
- `ghcr.io/wesperez/sub2api:debug`
- `ghcr.io/wesperez/sub2api:debug-<short-sha>`

## Runtime Layout

Production and debug must run as independent Docker Compose projects:

| Environment | Branch | Image tag | Compose project | Host port | Data directory |
| --- | --- | --- | --- | --- | --- |
| production | `main` | `main` | `sub2api-prod` | `127.0.0.1:13080` | `/root/sub2api-prod-deploy` |
| debug | `debug` | `debug` | `sub2api-debug` | `127.0.0.1:13082` | `/root/sub2api-debug-deploy` |

Each environment has its own Sub2API container, PostgreSQL container, Redis
container, config, logs, and database files. Debug changes must be tested in the
debug environment first, then merged into `main` for production.

## Server Update

Production:

```bash
cd /root/sub2api-prod-deploy
docker compose pull
docker compose up -d
```

Debug:

```bash
cd /root/sub2api-debug-deploy
docker compose pull
docker compose up -d
```

Do not use local Docker builds on the server for normal deploys.
