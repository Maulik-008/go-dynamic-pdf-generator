# Spec: auth-and-tenancy (v1 — shared static API keys)

Module spec under `CAPABILITY-MAP.md`. Full responsibility: *"API keys, per-key rate limiting, usage
metering (billing-ready)."* This document covers only the **v1 slice actually built**: shared
static API-key authentication as HTTP middleware. Per-key rate limiting and usage metering are
deferred, with reasoning below — not silently dropped.

## Why this slice, now

Every deployment guide in this repo carries the same warning in bold: *"this service has no
login/security check built in — do not expose it to the public internet."* That warning exists
because there was literally no auth. A single shared key, checked at the edge, removes the
"anyone who can reach the port can render" problem — which is the whole reason the service had to
sit on a private network with nothing in front of it. It does not need the rest of
`auth-and-tenancy` to be useful.

## What it is

`internal/auth` — one exported type, `Authenticator`, and its `Middleware`.

- **Keys**: `API_KEYS` env, comma-separated. One or more, so a key can be rotated without
  downtime (add new, migrate callers, drop old). Blank entries are ignored.
- **Presentation**: `X-API-Key: <key>` or `Authorization: Bearer <key>` — both accepted, because
  that is exactly what `pdf-service-saas` and its callers already send, so a consumer repointing
  at this service changes a URL and nothing else.
- **Comparison**: `crypto/subtle.ConstantTimeCompare`, every configured key checked with no early
  return, so neither "which key matched" nor "how close a near-miss was" is observable by timing.
- **Exempt paths**: `/livez`, `/readyz`, `/healthz` — a load balancer must be able to probe
  without a key.
- **Failure**: `401` with the same JSON error envelope every other endpoint uses,
  `{"error":{"code":"UNAUTHORIZED","message":"..."}}`, plus `WWW-Authenticate`. One message for
  "missing" and "wrong" alike — distinguishing them only helps an attacker.
- **Fail closed**: `cmd/api` refuses to start if `API_KEYS` is empty unless `AUTH_DISABLED=true`
  is set explicitly. A deployment cannot be left open by forgetting an env var.
- **Placement**: inside the request-logging middleware, so a rejected request is still logged
  with its correlation id.

## Error code table addition

| Code | Status | When |
|---|---|---|
| `UNAUTHORIZED` | 401 | no key, or a key that matches none of `API_KEYS`, on a non-exempt path |

## Deferred, with reasoning

- **Per-key rate limiting** — needs per-key identity to be a real thing first (a key store, or at
  least named keys), and a limiter store. `job-orchestration` already provides *global*
  backpressure (`QUEUE_FULL` + `Retry-After`), so the service is not unprotected against overload
  in the meantime; what is missing is *fairness between callers*, which only matters once there is
  more than one caller. Synectus Medico is a single caller.
- **Usage metering** — `CAPABILITY-MAP.md`'s Round 1 review already says "record per-key usage
  counters from day one even though nothing is billed." That is cheap, but it needs somewhere to
  put the counters (`storage-and-delivery`, not built) to be more than an in-memory number lost on
  restart. Recorded as the next slice.
- **Scoped / per-tenant keys** — same dependency (a key store).

## Testing

`internal/auth/apikey_test.go`: disabled-when-no-keys passthrough; both header forms accepted;
missing / wrong / empty / wrong-bearer all 401 with the JSON envelope and `WWW-Authenticate`;
multiple keys each accepted (rotation); exempt paths skip the check while protected paths still
require it. Middleware wiring is covered end-to-end by a live-curl check in
`docs/guides/`.
