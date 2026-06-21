# Sub2API Server Agent Rules

This repository is deployed on the server from GitHub-built container images.
Do not install dependencies, run package managers, compile, or build Docker
images on the server.

Production uses the `mine` branch and the image `ghcr.io/wesperez/sub2api:mine`.
It is deployed from `/root/sub2api-prod-deploy` and is the instance exposed by
the public Nginx domain `wooai.cc.cd`.

Debugging uses the `debug` branch and the image
`ghcr.io/wesperez/sub2api:debug`. It is deployed from
`/root/sub2api-debug-deploy` and must stay isolated from production.

When debugging server issues:

1. Switch code changes to the `debug` branch.
2. Let GitHub Actions build and publish `ghcr.io/wesperez/sub2api:debug`.
3. Deploy only the debug environment with:

   ```bash
   cd /root/sub2api-debug-deploy
   docker compose pull
   docker compose up -d
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
