# Sub2API Branch Deployment

This repository uses two long-lived deployment branches:

- `mine`: production source.
- `debug`: isolated debugging source.

Both branches are built by GitHub Actions. The server should pull the published
GHCR images instead of building locally, because local builds use too much memory
and disk space on the server.

Server-side agents must not install dependencies, compile, or build Docker
images in this repository. If code needs to be changed for debugging, do it on
the `debug` branch and deploy only the debug environment.

## Images

The workflow `.github/workflows/docker-branch.yml` publishes:

- `ghcr.io/wesperez/sub2api:mine`
- `ghcr.io/wesperez/sub2api:mine-<short-sha>`
- `ghcr.io/wesperez/sub2api:debug`
- `ghcr.io/wesperez/sub2api:debug-<short-sha>`

## Runtime Layout

Production and debug must run as independent Docker Compose projects:

| Environment | Branch | Image tag | Compose project | Host port | Data directory |
| --- | --- | --- | --- | --- | --- |
| production | `mine` | `mine` | `sub2api-prod` | `127.0.0.1:13080` | `/root/sub2api-prod-deploy` |
| debug | `debug` | `debug` | `sub2api-debug` | `127.0.0.1:13082` | `/root/sub2api-debug-deploy` |

Each environment has its own Sub2API container, PostgreSQL container, Redis
container, config, logs, and database files. Debug changes must be tested in the
debug environment first, then merged into `mine` for production.

Production is exposed through Nginx as `wooai.cc.cd` and must not receive
debug-branch images.

## Automatic Image Updates

Watchtower runs on the server with label-based updates:

```bash
watchtower --label-enable --cleanup --interval 120
```

The Sub2API application containers have
`com.centurylinklabs.watchtower.enable=true`. PostgreSQL and Redis do not have
that label, so automatic updates only replace the application image after GitHub
publishes a new branch image.

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
