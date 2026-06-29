# Sub2API Server Agent Rules

Before making changes in this repository, agents must read this file and
`BRANCH_DEPLOYMENT.md`. Do not start implementation, verification, deployment,
or GitHub submission work until those project rules are understood.

Before editing source behavior, inspect the relevant implementation and tests in
the repository. Do not implement from assumptions, external rebuild attempts, or
dependency-driven exploration.

This repository is deployed on the server from GitHub-built container images.
Do not install dependencies, run package managers, compile, or build Docker
images on the server.

Do not run local test, build, package-manager, module-download, or compile
commands on the server. Verification must use GitHub Actions or already
published container metadata; do not bypass this with local dependency downloads
or local compilation.

The fork `main` branch is a clean mirror of `upstream/main`. Do not put
deployment-only files such as `AGENTS.md`, `BRANCH_DEPLOYMENT.md`, or branch
image workflows on `main`.

Production uses the `mine` branch and the image `ghcr.io/wesperez/sub2api:mine`.
It is deployed from `/root/sub2api-prod-deploy` and is the instance exposed by
the public Nginx domain `wooai.cc.cd`.

Debugging uses the `debug` branch and the image
`ghcr.io/wesperez/sub2api:debug`. It is deployed from
`/root/sub2api-debug-deploy` on `127.0.0.1:13082` and must stay isolated from
production.

The debug environment is enabled only during active debugging or testing. When
there is no active test, stop the debug compose project so it does not occupy
memory. Do not keep debug containers running as a second long-lived production
instance.

Temporary debugging commits may be pushed to GitHub only while testing the
debug image. After the test is finished, remove those temporary commits from the
remote branch history and keep only the final reviewed code.

Outside an active debug test, `debug` and `mine` must not differ in source code.
The preferred steady state is that both branches carry the same documentation
and point at the same commit. Short-lived code differences are allowed only on
`debug` during active testing; promote the final change back to `mine` and clean
up the temporary debug history afterward.

When debugging server issues:

1. Switch temporary code changes to the `debug` branch.
2. Let GitHub Actions build and publish `ghcr.io/wesperez/sub2api:debug`.
3. Start or update only the debug environment with:

   ```bash
   cd /root/sub2api-debug-deploy
   docker compose pull
   docker compose up -d
   ```

4. After testing, stop the debug environment unless another test is active:

   ```bash
   cd /root/sub2api-debug-deploy
   docker compose stop
   ```

Production updates must come from the `mine` branch image only:

```bash
cd /root/sub2api-prod-deploy
docker compose pull
docker compose up -d
```

Watchtower runs on the server with label-based updates enabled. Only the
Sub2API application containers are labeled for automatic image updates; the
PostgreSQL and Redis containers are intentionally not labeled.
