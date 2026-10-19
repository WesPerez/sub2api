# OpenAI stream recovery review (2026-10-26)

## Failure and evidence

A production Responses stream returned HTTP 200, a `response.created` event
containing about 37 KiB of metadata and no output, then this error:

```json
{"type":"error","sequence_number":1,"code":"rate_limit_exceeded","message":"Your requests to gpt-6-astra for gpt-6-astra in eastus2 have exceeded token rate limit.","param":null}
```

The recovery classifier read `response.error.code` and `error.code`, but not
`code`. The message's `rate limit` also did not match the `rate_limit` fallback.
The stream was therefore forwarded instead of entering the existing recovery
policy. Ops subsequently recorded 429, which did not change the HTTP 200
already sent on the wire. Router preserved the specific message in a failed
Responses terminal event.

Commit `85a27fae39f9ba0a2b35c791e998cf873c901eb1` already supported recovery
from nested SSE rate-limit errors. This is a missing supported event shape,
not evidence that an upstream recently changed its protocol or that a recent
Sub2 release removed all rate-limit handling. HTTP 429, an HTTP 200 SSE error,
and a post-response Ops classification must be distinguished.

## Changes and boundaries

The October 26 incident exposed a second event shape: a nested `error` with
`type=too_many_requests`, `code=no_capacity`, and the provider's peak-load
request-size message, after `response.created`. The observed Sub2 upstream was
AgentRouter; these fields do not establish which infrastructure its upstream
uses. The gateway now recognizes the exact machine code as request-scoped
capacity shedding, including a bare error without a following failed terminal.
It preserves the provider message, uses the existing recovery budget before
semantic output, and does not place the account or pool into cooldown for this
request-specific rejection. Arbitrary text mentioning `no_capacity` is not
sufficient to classify an error as capacity shedding.

The classifier falls back to top-level `code` only for `type: error`, retaining
nested-code precedence and existing account retry configuration. The fallback
that synthesizes `response.failed` also preserves the top-level error code;
the first CI run exposed this second omission after semantic output. The capacity
error sanitizer handles the same top-level shape so its existing client code
policy remains consistent. It does not rewrite a real rate-limit code into a
capacity code.

Before semantic output, recovery can reuse the configured same-account retry
or account failover. Metadata and keepalive bytes alone are not model output.
After text or tool output, the current stream fails without changing accounts
and mixing responses. Exhausted recovery still returns an error. This change
does not add quota, alter model selection, or change retry budgets.

An independent cancellation review found compatibility retries inside
the native and passthrough Responses HTTP loops could create a new detached upstream attempt after
the client had canceled. Both loops now check the original context before each
attempt. It preserves draining an already-started response for terminal usage.
The observed header-timeout incident showed an attempt started before Router
timed out and finished later; it did not prove this compatibility-retry bug
occurred in that incident. No timeout, billing, account credential or resource
configuration changes are included.

## Verification contract

GitHub-hosted backend unit and integration suites must pass for the final SHA.
The added tests cover the exact event above, nested precedence, unrelated
events, nonretryable errors, default/custom/empty pool retry policy, metadata
larger than the incident preamble, keepalive, text/tool output boundaries,
Ops classification, capacity normalization, successful handler recovery and
exhaustion. Cancellation tests exercise both routes and cover zero attempts for an already-canceled
request, no second attempt after a rejected-field response cancels the client,
closing that response, no generic error write, and terminal usage from a
started response. Existing compact/identity and disconnect-drain tests remain
part of the full suites.

The capacity regression tests cover nested, top-level, and failed-terminal
shapes, metadata larger than the incident preamble, both Responses stream
handlers, preservation of the original error after visible output, and the
absence of account cooldown for request-scoped capacity failures.

Deploy only after exact-SHA image verification, isolated Debug checks and
sealed evidence. Promote the same digest and verify production health,
network identity and the official-client canary. Runtime limits remain:
genuine upstream quota failures can persist, and draining an in-flight request
may outlast the client's header budget. Test results and production identities
belong in the corresponding release evidence, not inferred from this document.

The second CI run exposed two older OAuth tests that canceled before calling
Forward while expecting a new request to start. Their detach assertion now
cancels inside the upstream recorder after dispatch, and additionally checks
terminal usage. The new pre-canceled tests separately require zero attempts.
This distinguishes preserving an in-flight request from starting new work for
a client that has already left.

## Configurable no-output recovery (0.2.19)

HTTP Responses on OpenAI and multi-protocol API-key providers shares an absolute retry budget
across account selection, queue waiting, same-account delays and upstream
attempts. `gateway.openai_first_output_timeout_seconds` and the optional
high-effort override limit one attempt; `gateway.openai_responses_first_output_budget_seconds`
limits cumulative waiting before output. Deployment uses 60 / 60 / 120 seconds;
all values are configurable and the shared budget defaults to disabled.

The attempt clock includes response headers and slow HTTP error bodies.
Metadata/heartbeats do not reset it. A complete semantic event disarms the
clock, so normal streams may run longer than 120 seconds. Before output, client
cancellation closes the reader promptly; after output, existing usage draining
continues. A timed-out attempt is closed before account failover and the
existing one-timeout-switch limit remains. Fast errors retain existing retry
counts and error classification. Shared-budget exhaustion returns the local
504 first_output_timeout; it is not rewritten by upstream passthrough rules.
No session, cache, turn or installation IDs are rotated.

Native, passthrough, Chat and Anthropic conversion attempts consume the same
budget, including non-streaming responses. Each attempt attaches cancellation
to the actual transport and response body; carrying a context value alone is
insufficient. Compact, WebSocket and the separate Grok media lifecycle retain
their existing timers.
Cancellation does not prove that the external provider stopped computation or
waived usage: replay before visible output can still incur upstream charges.

## Budget and idle cleanup audit (2026-10-29)

Production observations found 33 shared-budget exhaustions with access latency
up to 169.624 seconds. The corresponding Router request waited 171.979 seconds.
Its timeout detail was emitted after cleanup; it does not establish when the
timer fired or prove that cleanup alone caused the entire overrun.

The absolute pre-output budget now uses the access middleware's ingress clock,
before authentication (handler entry is the fallback for direct invocation), so time already
spent reading/parsing the request reduces the remaining allowance. Ownership,
security, mapping, billing, reservation, user/account/image admission and
identity/token preparation use temporary deadline contexts. Acquired leases
and reservations remain attached to the original request lifecycle. The HTTP
Responses ingress wrapper also bounds slow request upload and authentication
from ingress; it releases the read deadline before streaming begins.
Terminal JSON errors are self-delimited and flushed before deferred accounting
cleanup, so clients need not wait for Redis cleanup or handler EOF. Actual
host scheduling stalls can still postpone timer callbacks and network delivery;
120 seconds is an application deadline, not a real-time operating-system guarantee.

An upstream timeout no longer synchronously drains scanner events. The scanner
owns its buffer and is signaled to exit before transport close. Transport body
close cancels first, serializes decompressor reads/close, and waits at most
500 ms from its first invocation. An unfinished close logs a warning and keeps
transport in-flight accounting until physical cleanup completes; it cannot be
reused as an idle client. A permanently uncooperative transport can still retain
its cleanup worker and accounting. New telemetry separates observed timeout
time from close duration. Account timeout maintenance has a one-second bound.

Stream idle checks now wake at the last complete upstream line plus the
configured idle interval, instead of periodic polling that could nearly double
the wait. Passthrough and Chat conversion consume that same configured idle
limit through the existing line pump. Anthropic conversion stops at message_stop
instead of awaiting transport EOF. Empty completed responses cannot disarm the
budget or be reported as success. Existing complete-line activity semantics, terminal usage, post-output
disconnect draining and the prohibition on replay after output are preserved.

## Deadline handoff (2026-10-30)

Local attempt timeouts are recorded once without synchronously reading upstream
error policies. Shared-budget terminals are flushed before the warning log.
Confirmed timeout failover releases the old account slot concurrently with the
next account attempt, then joins that release before the handler returns. This
applies only to request-scoped timeouts that cannot retry the same account;
ordinary retries and billing reservation ownership are unchanged. A pending
release continues to count against capacity, and is never treated as released
before Redis confirms it. Both successful buffered responses and coded JSON
errors carry Content-Length so cleanup cannot delay body completion.
Ingress authentication/upload timeouts use the same self-delimiting response
and flush before their warning log; a real HTTP regression blocks both that
logger and the middleware return path while the client reads the complete 504.

Timeout Ops details record the deadline, first cancellation observation,
cancellation return and Forward return relative to the attempt start. These
in-memory timestamps distinguish a late callback from slow cancellation or
handoff without adding log I/O inside the timer. Kernel packet timestamps remain
the transport observation; client-process read timestamps are a separate metric.
