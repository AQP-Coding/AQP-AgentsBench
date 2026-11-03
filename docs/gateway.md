# Account for every forwarded model attempt

`aqp_bench.gateway.Gateway` forwards Chat Completions to one fixed OpenRouter
upstream and records observed token counts and billed cost. It uses Python's
standard library; tests use only loopback HTTP providers.

## Quick path

```python
from aqp_bench.gateway import Gateway

# Resolve the key explicitly in the runner; never persist it in run artifacts.
with Gateway(explicit_key, cost_limit=1.0) as gateway:
    base_url = gateway.url  # ends in /v1
    local_capability = gateway.token
    # Configure every agent/subagent with this base URL and local capability.
    # Run the agent here, then stop it before leaving the gateway context.
    records = gateway.attempts
```

Use `Authorization: Bearer <local capability>` on every request, including model
discovery. `POST /v1/chat/completions` and `GET /v1/models` are the only routes.
Queries, absolute request URLs, arbitrary paths, redirects, inherited proxy
settings, and client authorization forwarding are not supported. The actual key
is sent only to the configured upstream. No access logs or request prompts are
stored. Response errors are generic; secrets are redacted from forwarded content
and recorded identifiers. Only aggregate numeric usage and short identifiers are
retained. Do not use secrets as agent IDs, parent IDs, or model names.

For Docker connectivity only, the runner can bind `host="0.0.0.0"` and replace the
URL host with `host.docker.internal` in container configuration. The capability is
still mandatory. Restrict network access using host firewall rules; this local
benchmark proxy is not an Internet-facing service. The dashboard stays loopback.
Upstream HTTPS is required except literal loopback HTTP addresses used by tests.

## Accounting and failures

Each submitted provider completion attempt gets a unique `attempt_id`, HTTP
`status`, duration, requested/observed model, and any provider request ID. Optional
`X-AQP-Agent-ID` and `X-AQP-Parent-ID` headers preserve known attribution; absent
agent attribution is `unattributed`. Do not assume a missing child ID belongs to
its parent. Model discovery and locally rejected requests are not provider model
attempts and are not billed attempts.

Nonstream JSON and live SSE responses are supported. SSE lines are flushed
immediately, including keepalive comments. A final usage object replaces previous
usage values instead of being added repeatedly. `usage.cost` supplies
`cost_usd`; `prompt_tokens` and `completion_tokens` supply input/output counts.
Cache and reasoning token details are retained when reported. Parent/child
rollups are not added. Missing, negative, nonfinite, or malformed values remain
`null`, never zero. `usage_complete` requires both token counts and billed cost.
There is no follow-up generation lookup or automatic request retry.

In-flight provider attempts are visible immediately with `pending: true` and
unknown billing. Completion replaces that pending record under the same attempt
ID instead of adding a second request. If execution stops before final usage
arrives, retain the pending record and incomplete-accounting status rather than
silently dropping an already-submitted attempt.

A socket/protocol failure or incomplete stream sets `infrastructure_failed`;
provider HTTP 429/5xx or equivalent SSE errors also set it. Ordinary provider 4xx
is not classified as an infrastructure fault. Provider errors retain any supplied
billing even when generation fails. Detected client write failures are labeled
`client_disconnect`; a disconnected stream may have unknown final billing.

When known accumulated billing reaches `cost_limit`, `limit_reached` is set and
new completion requests receive HTTP 402 without contacting the provider.
In-flight concurrent requests can overshoot the limit. Unknown billing prevents
any claim of a hard spend cap. Zero limit blocks all completion requests.

## Resource and lifecycle boundaries

Requests are limited to 8 MiB, nonstream responses to 16 MiB, SSE frames to 1 MiB,
and active client connections to 32. Network I/O has a 30-second idle timeout.
Streams have no fixed total output limit, so long active generations remain
responsive. Chunked client request bodies are rejected; clients must send a
valid Content-Length. Over-capacity connections close without upstream work.

`attempts` returns independent lock-protected JSON-safe snapshots. `on_attempt`
notifications run on one observer thread with a bounded queue (128); exceptions
are isolated and notifications may be dropped if the observer cannot keep up.
Snapshots, not notifications, are the authoritative accounting source. Stop
agents before closing the context; active request workers are daemon threads and
context exit does not wait for a stalled upstream or callback.

## Protocol evidence and verification

OpenRouter documents `usage.cost` as the total charge and detailed token counts
in its [usage accounting reference](https://github.com/OpenRouterTeam/docs/blob/main/cookbook/administration/usage-accounting.mdx).
Its [streaming reference](https://github.com/OpenRouterTeam/docs/blob/main/api_reference/streaming.mdx)
describes the final usage frame before `[DONE]` and errors within SSE events.
Current usage parameters are automatic; the gateway does not add deprecated
usage flags or rewrite agent messages.

Run `PYTHONPATH=. pytest -q -p no:cacheprovider tests/test_gateway.py`.
No paid calls or real OpenRouter integration were used for these checks. Normal
agent network egress means gateway configuration is not a security boundary
against an actively malicious agent bypassing it; use trusted adapters and
explicit benchmark configuration.
