# Resin Response Recovery

Resin cannot inspect HTTPS application payloads. An established SOCKS tunnel
may look healthy while Responses requests receive an empty stream or an HTML
challenge. This optional integration sends application verdicts back to Resin.

## Configuration

Deploy a Resin version exposing recovery API version 1 before enabling:

```yaml
gateway:
  resin_recovery:
    enabled: true
    proxy_endpoints:
      - proxy.internal:10834
      - 172.17.0.1:10834
    failure_threshold: 3
    window_seconds: 60
```

Equivalent environment keys are `GATEWAY_RESIN_RECOVERY_ENABLED`,
`GATEWAY_RESIN_RECOVERY_PROXY_ENDPOINTS` (comma-separated host:port values),
`GATEWAY_RESIN_RECOVERY_FAILURE_THRESHOLD`, and
`GATEWAY_RESIN_RECOVERY_WINDOW_SECONDS`. Defaults are disabled, three failures,
and a 60-second window. Zero threshold/window values use those defaults.

Only the configured SOCKS endpoints, expanded `Platform.Account` identities,
OpenAI HTTP transport profile, and POST paths ending in `/responses` participate.
This includes normal forwarding, passthrough, buffered Responses, and the
Responses account connection test. Chat Completions, WebSocket, and
`/responses/compact` are not managed by this version. Ordinary proxies and
strict guarded browser identities retain their previous behavior.

## Recovery Contract

Before each managed request, acquire snapshots the account lease. Node hash,
precise generation, and exit IP are added to its connection-pool scope.
Observations are isolated by proxy identity, expanded account, target origin,
and lease generation. A healthy different target cannot clear this target's
failures. Resin's actual lease remains account-global.

Empty/preamble-only streams, streams ending before a terminal event, transport
errors, and recognized HTML error pages contribute failures. Requests already
in flight when a failure is observed count as one incident, while successive
client/test requests count separately. Valid terminal events and verified JSON
4xx error bodies clear earlier failures. Completed 402/429 replies are business
errors even without JSON Content-Type; they are not failed nodes. JSON 5xx alone
is not sufficient evidence to clear a failure streak. Client cancellation,
unread bodies, and local stream size limits do not count as node failures.

At threshold, a bounded asynchronous report uses node plus generation CAS.
Resin excludes the failed node and exit IP for ten minutes for the same
account/target. It returns `rotated`, `stale_lease`, or `no_alternative`.
No alternative preserves the original lease and established tunnels, but keeps
the cooldown for later recovery. Reports back off for 30 seconds; unavailable
acquire metadata also backs off without disabling the original proxy path.
State is in memory, bounded to 4096 scopes and eight simultaneous reports.

Retired transports close idle connections only. Existing streams retain their
references; new requests use the replacement generation. Managed stream failures
do not quarantine every account sharing the same proxy ID. Recovery never
replays a business POST: existing gateway account failover semantics are unchanged.

Control requests use the existing proxy token in an Authorization header, never
in a URL or diagnostic cache key. HTTP control traffic, like SOCKS authentication,
requires a trusted local network. This is not an Internet-facing TLS control API.

## Verification And Rollout

CI covers threshold/reset, concurrency collapse, late results, A-B-A generations,
target/account isolation, empty/partial/HTML streams, business errors, normal and
TLS pools, connection tests, and buffered responses. Race tests run in Actions.
Use synthetic isolated accounts for injected failures; do not rotate production
account leases as a smoke test. Build/test in Actions, validate the debug image,
then follow `BRANCH_DEPLOYMENT.md` for exact-image promotion. Deploy Resin before
enabling this Sub2API setting; disable the setting before rolling Resin back to
a version without the recovery contract.
