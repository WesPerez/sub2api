# Sub2API Server Agent Rules

## 生产服务器发布与日志约定

- 本服务器只编辑、审查和推送源码，拉取 GitHub CI 已验证的镜像或发布包并运行。禁止在任意工作树、临时目录、本机容器或 self-hosted runner 下载/安装项目依赖、编译、构建、打包或构造镜像；已有缓存也不构成例外。
- 依赖安装、类型检查、编译型测试和镜像/发布包构建全部交给 GitHub-hosted Actions。禁止在本机运行 npm/pnpm/yarn install、pip install、cargo fetch/build/test/check、go mod download/build/test、docker build/buildx build、compose build/up --build 或等价脚本。无需下载和编译的静态检查与现有工具的运行验收可以执行。
- 发布顺序是审查源码、提交推送、远程 CI 通过、核对完整 Git SHA 与镜像 digest/产物校验和，再由项目既有发布入口更新。保持现有端口、网络、数据挂载、认证和健康门禁。
- 普通运行日志默认保留最近 3 天，并设置合理的文件/行数/容量上限；已有原生有界策略优先复用。清理只淘汰最旧且已结束的记录，保护正在写入的文件、进行中的请求和活跃会话。禁止直接删除运行数据库、WAL/SHM 或 Docker 活动日志文件；容量不足时不得绕过保护删除最新记录。
- 消费、账单、防重、账户凭证、业务状态与恢复数据按各自职责保留，不能以“日志”名义删除。Router、Sub2 和 Metapi 的专门保留合同优先。
- 同一恢复链最多保留一组最新且已验证完整的恢复点；若它引用独立依赖、密钥或数据库组件，这些共同构成一组。活动发布、尚未验收的恢复点和唯一凭证副本继续保护；旧副本须确认替代关系和无引用后退役。
- 临时开发工作树在提交已由主线或持久远端分支保全、未提交改动已妥善保存、测试退出且没有任务/服务引用后，用 git worktree remove 收尾；不得用 --force 丢弃独有改动。

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

### Production container network contract

Production `sub2api-prod` has an out-of-band attachment to Docker's built-in
`bridge` network. The application container must use `172.17.0.2/16` on that
network and reach the host proxy at `172.17.0.1:10834`. This attachment is
intentionally not declared in the production Compose network list; recreating
the container removes it.

The production `/app/network-entrypoint.sh` waits for `172.17.0.2` and exits
with code `78` when the address is absent. With `restart: unless-stopped`, a
replacement started without the live bridge attachment enters a restart loop.
Therefore agents must not run generic `docker compose up`, `restart`,
`create`, `rm`, or `down` commands against the production `sub2api` service.
Read-only `config`, `ps`, `inspect`, and log commands are allowed.

When a configuration change requires replacing the production container, use
the guarded production update script or an equivalent controlled sequence:
create the replacement with `up --no-start --no-deps --force-recreate`, start
it under the entrypoint gate, immediately run
`docker network connect --gw-priority -1 bridge <container>`, and independently
verify the exact `172.17.0.2` address, host-proxy reachability, `healthy` status,
and a stable restart count. Do not add the host-proxy bridge to Compose as a
substitute for this live attachment, and do not treat Compose's `Started`
message as a successful production rollout.

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
semantic smoke gate for active production providers; failure leaves the active
Router and Nginx unchanged. When only Sub2API changes, test its Debug instance
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
artifact whose `debug-image.json` binds the source run, revision, SHA tag,
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
  --verification-evidence <matrix-run-dir/release-evidence.json>
```

The running Watchtower explicitly disables `sub2api-prod`; production rollout
is manual after the matching debug image has passed its test matrix and a
successful promotion run. PostgreSQL and Redis are never pulled or recreated as
part of an application upgrade.

## 当前个性化职责

上游同步后按五项维护：发布/远程 CI、Resin 账号身份水合、账号管理与连接测试、应用内部运维任务、Responses 异常后的 Resin 租约恢复。运维任务已从宿主机调度仓迁入本项目，不能因旧版职责清单只有三项而遗漏它们。整理前的提交通过 origin/archive/pre-governance-20260922 保留，生产更新仍必须经过 Debug 验证与同 digest 晋级。
