# 0.2.24 source transition — 2026-11-22

This source candidate starts at upstream
`328db26f450edbdf9f8d4344a3ea903ac6396842` (0.2.24). The previous production
source is `7b4adfa0999d84ffe8daaaef063e795c761e485a`, based on
`a3298b46ebe239fb0720b068a1eb3383955bdac2` (0.2.19). This document records
source adaptation and pending gates; it is not a release receipt.

| Responsibility | Previous source | Adaptation on new upstream |
| --- | --- | --- |
| D1 deployment/CI | `cf3c857a1` | Keep exact-SHA debug build, sealed evidence and same-digest promotion. Retain upstream Go 1.27.4, Node 24, new migration test schema and isolated billing-memory fixture. |
| D2 Resin identity/recovery | `d8db00b00` | Bind in shared `decodeCachedAccountPayload` before shared snapshot publication; retain upstream configuration-aware SSE bounds while preserving response observation. |
| D3 account tools/Admin API | `483058ace` | Keep public routing/model search and usage filters. Restricted lists use upstream name/authorized-email search and group filtering; extended aggregate search is denied only for restricted keys. Full-access keys retain upstream full-access behavior. Keep upstream separate account/user cost fields. |
| D6 gateway compatibility | `7b4adfa09` | Preserve reasoning, cancellation, first-output budget and session exclusions. Keep upstream body memory guards, per-read/per-write timeouts, image-size spillover, Codex untrusted-context normalization and Anthropic terminal/error validation. Per-read socket deadlines are capped by the Responses ingress deadline. |

No historical archive H1–H3, retired D4/D5 or global account-creation defaults
are replayed. Existing upstream migration bytes remain unchanged.

## Production migration gate

The six new SQL files include the concurrent
`idx_usage_logs_api_key_created_cost` index and nonblocking group-rollup state.
The old image is unsafe as an automatic schema rollback after these changes.
Use the existing forward-only dump, exact-image evidence and promotion contract.

Read-only preflight found PostgreSQL 18.6, an ordinary unpartitioned usage table
with approximately 810,216 rows, 478,674,944 heap bytes and 888,315,904 index bytes.
All 299 existing migration checksums matched their deployed source. The new cost
index and rollup columns were absent. These are preflight observations, not proof
of migration duration or production readiness.

The installed updater waits 180 seconds for application health, while migrations
have a ten-minute context and HTTP starts only after migration completion.
Before production apply, hosted CI must exercise a real old-schema upgrade,
including concurrent index construction and existing rows; the production guard
must distinguish bounded migration progress from application failure and cover
the migration deadline. Do not pre-run SQL on production or assume the index fits
within the old health timeout. PostgreSQL/Redis remain running during app upgrade.

## Required final-SHA acceptance

- Existing full hosted backend unit/integration, frontend and release-contract CI.
- New body-read deadline composition test plus existing HTTP1/HTTP2 drain and
  response-lifetime tests; first-output, cancellation and Resin race tests.
- Shared scheduler payload identity, upstream image-size spillover/sticky tests,
  scoped name/authorized-email restrictions and full-access search behavior.
- Upstream fresh-schema/idempotence tests plus real continuous 0.2.19→0.2.24
  migration of populated synthetic data and interrupted concurrent-index retry.
- Core planner-selected Debug matrix, sealed exact-SHA/digest evidence, and
  carbon-copy promotion before guarded production apply and confirmation.

Local verification is limited to syntax, diff and source-contract inspection.
Runtime baseline and production evidence change only after actual confirmation.

The new integration regression uses a manifest of the original 299 SQL files
and byte hashes from `7b4adfa0999d84ffe8daaaef063e795c761e485a`, reusing the
unchanged embedded SQL. It creates a separate synthetic database on the hosted
PostgreSQL 18.6 container, populates 4,096 usage rows, and compares every old
usage field, costs, identities and rollup state. It cancels the real migration
only after PostgreSQL reports the cost index waiting on its uncommitted writer,
verifies the invalid index and absent migration record, then verifies recovery
replaces that index and completes all 305 migrations without data changes. A
second run checks idempotence. These tests still need the final-SHA hosted run;
they do not establish production migration duration or remove the health gate.

The health gate itself remains a separate unresolved deployment contract. The
application gives migration startup up to ten minutes, starts HTTP only after
all migrations finish, and currently reports Docker `unhealthy` before that
deadline can expire. Extending the updater's 180-second wait alone is unsafe:
it rejects `unhealthy` immediately and the debug adapter also requires immediate
healthy state. Before considering a production rollout, expose bounded migration
phase/deadline progress to the release verifier. It may tolerate missing HTTP
or `starting`/`unhealthy` only while that exact migration run is demonstrably
progressing within its deadline; process exit, fatal errors, or restart growth
must fail immediately. Hosted schema-recovery success does not establish that
production completes its migration within ten minutes.
