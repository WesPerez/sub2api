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
`/root/sub2api-debug-deploy` on `127.0.0.1:13180` and must stay isolated from
production.

The host-port contract is Sub2API production/debug on `127.0.0.1:13080` /
`127.0.0.1:13081`, while Router production uses blue/green slots
`127.0.0.1:13082` / `127.0.0.1:13083`. Router has no fixed Debug port. The
Sub2API target has not yet been fully applied: until the coordinated migration
is complete, the live debug Compose mapping remains `13180`. Always inspect the
effective Compose, Nginx, and listener state before starting a fixed port.

When only Router changes, validate it through CI/mock and the unused production
candidate slot, which continues to use production Sub2API. After candidate
health, readiness, and MainPID checks but before Nginx cutover, the installed
official Codex CLI must directly exercise that candidate with the structured
Sol/Grok semantic smoke gate; failure leaves the active Router and Nginx
unchanged. When only Sub2API changes, test its Debug instance
directly; start a temporary isolated Router on an explicitly free non-fixed
port only when the complete Codex/Router protocol path must be covered.
Production Router must never point to Debug Sub2API.

Push to `debug` (or a validated debug `workflow_dispatch`) runs full CI in
parallel with a cache-only Docker build, then publishes debug tags only after
both succeed (`debug-sha-<40>`, then carbon-copy `debug-<12>` / `debug` when
`origin/debug` still matches). Push to `mine` does **not** build or push images.
Production `mine` / `mine-sha-<40>` tags are created only by the
`promote-debug-image.yml` workflow (exact digest carbon-copy + evidence). A
successful debug publish must upload one unexpired `debug-image-metadata`
artifact whose `debug-image.json` binds the source run, revision, immutable tag,
image, and digest; promotion must verify that binding before any mine tag. GitHub
records the operator-supplied sealed-evidence hash in the promotion receipt;
only the production apply script verifies the actual local evidence file. GitHub
Actions does not restart Compose; production Watchtower is disabled. Debug
Compose disables Watchtower and changes only after an explicit
`docker compose pull/up`. Batch deployment-documentation changes with the next
normal Sub2API update instead of a documentation-only image cycle.

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
2. Let GitHub Actions (`docker-branch.yml`) verify + build, then publish
   `ghcr.io/wesperez/sub2api:debug-sha-<40>` (and floating `debug` when heads match).
3. Start or update only the debug environment with:

   ```bash
   cd /root/sub2api-debug-deploy
   docker compose pull sub2api
   docker compose up -d
   ```

4. After testing, stop the debug environment unless another test is active:

   ```bash
   cd /root/sub2api-debug-deploy
   docker compose stop
   ```

5. To promote a verified revision to production images, ensure `mine` and
   `debug` both point at the same 40-char SHA, then run
   `promote-debug-image.yml` on ref `mine` with `expected_revision`,
   `source_digest`, `source_run_id`, and `verification_evidence_sha256`.
   `source_run_id` must come from the sealed evidence verifier (R0-1), not from
   a separately selected successful run.
   Never manual-retag debug to mine.

Production updates must come from a promoted `mine` image only (exact binding:
revision + digest + sealed local evidence + promotion receipt):

```bash
cd /root/.codex/skills/sub2api-upgrade
bash scripts/update-sub2api.sh --apply \
  --expected-revision <40-char-git-sha> \
  --expected-digest sha256:<image-index-digest> \
  --promotion-run-id <github-actions-run-id> \
  --verification-evidence <matrix-run-dir/release-evidence.json> \
  --rollback-image-safe
```

The running Watchtower explicitly disables `sub2api-prod`; production rollout
is manual after the matching debug image has passed its test matrix and a
successful promotion run. PostgreSQL and Redis are never pulled or recreated as
part of an application upgrade.
