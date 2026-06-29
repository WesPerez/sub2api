# Sub2API Branch Deployment

This repository uses two long-lived deployment branches:

- `mine`: production source.
- `debug`: isolated debugging source.

Both branches are built by GitHub Actions. The server should pull the published
GHCR images instead of building locally, because local builds use too much
memory and disk space on the server.

Server-side agents must not install dependencies, download modules, compile, run
package managers, or build Docker images in this repository. Verification must
use GitHub Actions or already published container metadata. If code needs to be
changed for debugging, do it on the `debug` branch and deploy only the debug
environment.

## Fork Main Sync

The fork `main` branch is a clean mirror of `upstream/main` and should not carry
deployment-only files. The workflow `.github/workflows/sync-upstream-main.yml`
runs from the fork default branch and syncs `origin/main` to `upstream/main`
daily, with a manual `workflow_dispatch` fallback.

## Branch Policy

`mine` is the production branch. It should contain only reviewed code that is
ready for the production image `ghcr.io/wesperez/sub2api:mine`.

`debug` exists only to test temporary changes in an isolated environment. During
active investigation it may briefly contain commits that are not on `mine`, and
those commits may be pushed to GitHub so Actions can build
`ghcr.io/wesperez/sub2api:debug`.

After the test is finished, temporary debug commits must be removed from the
remote branch history. Only the final reviewed code should remain. Outside an
active debug test, `debug` and `mine` must not differ in source code. The
preferred steady state is that both branches carry the same documentation and
point at the same commit, so branch history, commit time, and commit hash stay
identical when no debug experiment is in progress.

If a debug change is accepted, promote that final change to `mine` and clean the
debug branch so it matches the final production source. Do not leave old test
commits on GitHub as permanent history.

## Images

The workflow `.github/workflows/docker-branch.yml` publishes:

- `ghcr.io/wesperez/sub2api:mine`
- `ghcr.io/wesperez/sub2api:mine-<short-sha>`
- `ghcr.io/wesperez/sub2api:debug`
- `ghcr.io/wesperez/sub2api:debug-<short-sha>`

## Runtime Layout

Production and debug run as independent Docker Compose projects:

| Environment | Branch | Image tag | Compose project | Host port | Data directory |
| --- | --- | --- | --- | --- | --- |
| production | `mine` | `mine` | `sub2api-prod` | `127.0.0.1:13080` | `/root/sub2api-prod-deploy` |
| debug | `debug` | `debug` | `sub2api-debug` | `127.0.0.1:13082` | `/root/sub2api-debug-deploy` |

Each environment has its own Sub2API container, PostgreSQL container, Redis
container, config, logs, and database files. Production is exposed through Nginx
as `wooai.cc.cd` and must not receive debug-branch images.

## Debug Runtime Policy

The debug compose project is enabled only during active debugging or testing.
When there is no active test, stop it so the debug Sub2API, PostgreSQL, and
Redis containers do not occupy memory.

Start or update debug only for active testing:

```bash
cd /root/sub2api-debug-deploy
docker compose pull
docker compose up -d
```

Stop debug after testing:

```bash
cd /root/sub2api-debug-deploy
docker compose stop
```

Do not run production compose commands from debug-branch work. Do not retag a
debug image as `mine`, and do not use `ghcr.io/wesperez/sub2api:debug` for
production.

## Automatic Image Updates

Watchtower runs on the server with label-based updates:

```bash
watchtower --label-enable --cleanup --interval 120
```

The Sub2API application containers have
`com.centurylinklabs.watchtower.enable=true`. PostgreSQL and Redis do not have
that label, so automatic updates only replace the application image after GitHub
publishes a new branch image.

If the debug compose project is stopped, Watchtower may not update it until it
is started again. That is expected. For a new test, run the debug `docker compose
pull` and `docker compose up -d` commands above.

## Production Update

Production updates must come from `mine` only:

```bash
cd /root/sub2api-prod-deploy
docker compose pull
docker compose up -d
```

Do not use local Docker builds on the server for normal deploys.
