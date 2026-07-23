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

### Debug build (`docker-branch.yml`)

Only the `debug` branch builds container images. Trigger: push to `debug`, or
a validated `workflow_dispatch` with selected ref `debug`.

After a validate gate, full CI (`backend-ci.yml`) and a cache-only Docker build
run in parallel. Within full CI, the unchanged unit and integration test
commands run as separate parallel jobs, and both must pass. The build job never
logs in to GHCR and never has `packages: write`. Publish runs only when **both**
verify and build succeed (`needs: [verify, build]`), so a failed CI never
publishes.

Published debug tags (immutable first):

- `ghcr.io/wesperez/sub2api:debug-sha-<40-char-git-sha>` (immutable; never overwritten if provenance differs)
- `ghcr.io/wesperez/sub2api:debug-<12-char-git-sha>` (compat floating short tag; carbon-copy)
- `ghcr.io/wesperez/sub2api:debug` (floating; carbon-copy last, only if `origin/debug` still equals the SHA)

The parallel build reads `docker-branch-debug-trusted` but writes only
`docker-branch-debug-<full-sha>`. The verify-gated publish consumes that
candidate cache; only after the immutable image's SLSA provenance, OCI index,
attestation manifest, in-toto statement blob digest, statement subject, and
predicate all bind the published content does a separate cache-only step update
the trusted cache. This is structural content binding, not an independent
Sigstore signature verification. Reused immutable images do not advance the
trusted cache because the cache-only rebuild is not proven byte-equivalent.
Concurrency group `docker-branch-debug` uses `cancel-in-progress: false`.

Image labels set at debug publish time:

- `org.opencontainers.image.revision=<40-sha>`
- `org.opencontainers.image.ref.name=debug`
- `org.opencontainers.image.source=https://github.com/<owner>/sub2api`

Each successful publish also uploads exactly one `debug-image-metadata` artifact
containing `debug-image.json` (mode 0600). It binds `source_run_id`, revision,
exact digest, immutable tag, and image repository. Promotion rejects a missing,
expired, duplicate, or mismatched metadata artifact.
If `debug-sha-<40>` already exists, publish never overwrites it. Recovery may
reissue metadata only when BuildKit SLSA provenance plus the registry index and
in-toto statement prove the existing digest was built by this repository's
`Docker Branch Images` publish job for the same debug ref and full SHA; labels
alone are insufficient. Metadata records both
the original publisher run and the current successful verification run.

### Production promote (`promote-debug-image.yml`)

`mine` images are **not** built by CI. They are created only by the
`Promote Debug Image` workflow (`workflow_dispatch` on selected ref
`mine`) via `docker buildx imagetools` carbon-copy of an **exact index/manifest
digest**. No rebuild, no label rewrite, and no claim of
`org.opencontainers.image.ref.name=mine` (source labels stay as debug-produced).

Required promotion inputs (exact digest ternary provenance):

1. `expected_revision`: 40-char git SHA (`origin/mine` and `origin/debug` must both equal it at the early gate, immediately before immutable writes, and immediately before floating `mine`)
2. `source_digest`: `sha256:...` of `debug-sha-<40>` (must match registry)
3. `source_run_id`: successful `Docker Branch Images` run id (path, head_branch=debug, head_sha exact), and it must equal the R0-1 workflow run bound into sealed local evidence
4. `verification_evidence_sha256`: SHA-256 of the sealed local `release-evidence.json`; promotion records it, while production apply re-hashes and validates the actual file

Promote writes, in order, with per-step digest verification:

1. `ghcr.io/wesperez/sub2api:mine-sha-<40-char-git-sha>` (fail if exists with a different digest)
2. `ghcr.io/wesperez/sub2api:mine-<12-char-git-sha>`
3. `ghcr.io/wesperez/sub2api:mine` (last)

A schema-v1 `promotion.json` artifact (mode 0600) records run id, actor, SHA,
source/target digest, exact tags, evidence hash, and the explicit mode
`recorded-hash-production-apply-verifies-local-file`. The GitHub workflow does
not have the server-local evidence file; production apply must re-hash and
validate it. Concurrency:
`cancel-in-progress: false`.

### Provenance rules

Production deploys must bind the **exact digest ternary**:

- git revision (40-char SHA)
- image digest (`sha256:...`)
- promotion run + verification evidence

**Manual retag is forbidden** (including `docker tag`, `crane copy` outside the
promote workflow, or promoting `:debug` by hand). Do not use
`ghcr.io/wesperez/sub2api:debug` for production.

Workflows publish images only; they do not SSH to this server or restart either
Compose project. Production Watchtower is explicitly disabled. Debug Compose sets
`com.centurylinklabs.watchtower.enable=false`. Documentation-only changes should
be batched with the next normal Sub2API update instead of a standalone image cycle.

## Port Contract And Debug Routing

The host-port ownership is:

| Component | Port | Role |
| --- | --- | --- |
| Sub2API | `127.0.0.1:13080` | production |
| Sub2API | `127.0.0.1:13081` | Debug, on demand |
| Codex Unified Router | `127.0.0.1:13082` | production blue/green slot A |
| Codex Unified Router | `127.0.0.1:13083` | production blue/green slot B |

Router has no fixed Debug port, and no port is reserved exclusively for smoke
tests.

| Change under test | Recommended path |
| --- | --- |
| Router only | CI/mock -> unused production Router candidate slot -> production Sub2API `13080` -> official Codex structured Sol/Grok smoke before cutover |
| Sub2API only, API-level verification | client/probe -> Debug Sub2API `13081` |
| Sub2API only, complete Codex/Router verification | Codex -> temporary isolated Router on an explicitly free non-fixed port -> Debug Sub2API `13081` |
| Router and Sub2API together | Codex -> temporary isolated Router on an explicitly free non-fixed port -> Debug Sub2API `13081` |
| Production smoke | existing production domain -> production Router -> production Sub2API `13080` |

Production Router must never point to Debug Sub2API. Administrative writes,
account imports, configuration changes, and database mutations are not ordinary
smoke tests.

The Router release gate checks candidate health, readiness, and MainPID, then
uses the currently installed official Codex CLI against the loopback candidate
before changing Nginx. It does not use Sub2API
`Test Connection`, raw HTTP clients with copied Codex headers, or a dedicated
smoke port. A failed smoke stops and restores the candidate while the previous
Router continues serving production and draining its existing SSE connections.

Router already uses `13082/13083` as its final production blue/green layout.
Only the Sub2API Debug port remains transitional: it still publishes on `13180`
until a separately controlled migration moves it to `13081`. Before that move,
verify the effective Compose mapping and remove the retired Router legacy
meaning of `13081` from units, defaults, deployment branches, and tests.

## Current Runtime Layout

Production and debug run as independent Docker Compose projects:

| Environment | Branch | Image tag | Compose project | Host port | Data directory |
| --- | --- | --- | --- | --- | --- |
| production | `mine` | `mine` | `sub2api-prod` | `127.0.0.1:13080` | `/root/sub2api-prod-deploy` |
| debug | `debug` | `debug` | `sub2api-debug` | `127.0.0.1:13180` | `/root/sub2api-debug-deploy` |

Each environment has its own Sub2API container, PostgreSQL container, Redis
container, config, logs, and database files. Production is exposed through Nginx
as `wooai.cc.cd` via the Router blue/green slots on `127.0.0.1:13082` and
`127.0.0.1:13083`; it must not receive debug-branch images.

## Debug Runtime Policy

The debug compose project is enabled only during active debugging or testing.
When there is no active test, stop it so the debug Sub2API, PostgreSQL, and
Redis containers do not occupy memory.

Start or update debug only for active testing:

```bash
cd /root/sub2api-debug-deploy
docker compose pull sub2api
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

## Production Image Updates

The server Watchtower currently runs with production explicitly disabled:

```bash
watchtower --label-enable --cleanup --interval 120 --disable-containers sub2api-prod
```

The `sub2api-prod` label alone does not override that command-line exclusion.
Production is updated manually only after the exact candidate revision passes
CI and the isolated debug matrix. PostgreSQL and Redis are not pulled or
recreated during an application update.

Watchtower is disabled for the Debug Compose project. For a new test, explicitly
pull only the debug `sub2api` service and start the isolated compose project with
the commands above.

## Codex API-Key Upstreams

Configure Codex API-key upstreams with the complete Responses endpoint (for
example `https://new.sharedchat.cc/codex/responses`) and explicitly enable
`extra.openai_passthrough`. Configure the required Codex `User-Agent`,
`Originator`, `Version`, `OpenAI-Beta`, and `Accept` values with account-level
header overrides; those overrides are the final outbound header values. Store a
stable UUIDv4 in `extra.openai_device_id`; when the client does not provide one,
the gateway adds it as `client_metadata.x-codex-installation-id`. Existing
client metadata always takes precedence.

For this endpoint, a downstream non-streaming Responses request is sent upstream
as a stream and aggregated back into one JSON response. This body-level bridge
cannot be expressed by header overrides. Streaming requests remain streaming,
and `/compact` remains a unary JSON request. The `new.sharedchat.cc` Codex
endpoint currently has no price route for `gpt-5.6-sol` with
`reasoning.effort=max`; the gateway therefore sends that exact combination as
`xhigh`. Other hosts, models, effort levels, `/compact`, and remote compaction
requests are unchanged. This endpoint also uses HTTP/1.1 upstream because its
SOCKS route accepts HTTP/1.1 but can stall or close HTTP/2 requests before any
response headers; other OpenAI upstreams retain the configured HTTP/2 policy.

Upstreams that block non-mainland egress, including `new.sharedchat.cc`, must use
an account-level proxy assignment. Do not configure a global proxy for this;
other accounts and update traffic must retain their existing routes.

## Production Update

Production updates must come from a **promoted** `mine` image only (never from a
local build or a manual retag of debug). The upgrade script binds the exact
digest, re-verifies the local sealed evidence, and validates the promotion
receipt before touching production:

```bash
cd /root/.codex/skills/sub2api-upgrade
bash scripts/update-sub2api.sh --apply \
  --expected-revision <40-char-git-sha> \
  --expected-digest sha256:<image-index-digest> \
  --promotion-run-id <github-actions-run-id> \
  --verification-evidence <matrix-run-dir/release-evidence.json> \
  --rollback-image-safe
```

Do not use local Docker builds or broad Compose pulls on the server for normal
deploys. Do not retag `debug` to `mine` on the server.
