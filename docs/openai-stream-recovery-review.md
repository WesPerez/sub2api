# OpenAI stream recovery review (2026-10-23)

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
