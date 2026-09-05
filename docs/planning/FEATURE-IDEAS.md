# New Feature Ideas — Quick Decision Guide

Short version of every idea discussed, with simple frontend/backend flow, so you can pick what to
build next without reading a long report.

## Compare at a glance

| # | Feature | Pain point it solves | Effort | Priority |
|---|---|---|---|---|
| 1 | AI Template Generator | No developer needed to build a template | Medium | **Build first** |
| 2 | Async Jobs + Webhooks | Bulk PDF runs shouldn't block on one HTTP call | Medium | **Build first** |
| 3 | Failed-Job Dashboard | "Why did my PDF fail?" support tickets | Small | **Build first** |
| 4 | Batch Endpoint | 500 invoices = 500 separate requests today | Small | Nice to have |
| 5 | AI Fix-It Assistant | Template errors are confusing to fix | Small | Nice to have |
| 6 | AI Visual QA | Layout bugs reach customers silently | Medium | Nice to have |
| 7 | Font/Locale Pre-flight Check | CJK/RTL/emoji text renders broken silently | Small | Nice to have |
| 8 | E-Sign Hook | Invoices/contracts need signing after generation | Medium | Later |
| 9 | Healthcare Compliance Mode | Audit trail / PHI redaction | Medium | Confirm need first |

**Note on effort**: "Small" = days, "Medium" = 1-2 weeks, for one developer familiar with this codebase.

---

## 1. AI Template Generator

**Pain point**: Every new invoice/report layout today needs a developer to hand-write HTML/CSS.
This is the #1 reason integrations stall.

**Frontend flow**
1. User types a description ("blue-header invoice with logo and line-item table") or uploads a
   sample PDF/image.
2. Clicks **Generate Template**.
3. Preview PDF appears in ~5-10 seconds.
4. User tweaks text/colors if needed, clicks **Save Template**.

**Backend flow**
1. `POST /v1/templates/generate` — receives the description or uploaded image.
2. Calls an LLM (Claude) to produce HTML + CSS + a list of merge-field names.
3. Sends that HTML straight through the **existing** `/v1/pdf/html` renderer to build a live
   preview — no new render engine needed.
4. Returns `{template, mergeFields, previewPdfUrl}`.

**Key decision**: this adds your first external AI-API dependency. Keep it strictly at
template-creation time — never inside the actual render request path, so render speed/reliability
stays untouched.

---

## 2. Async Jobs + Webhooks

**Pain point**: Generating 200 invoices for a billing run means holding 200 HTTP connections open,
or writing your own retry/polling logic today.

**Frontend flow**
1. User (or their backend) submits a job with `"async": true` and a `webhook_url`.
2. Gets back `{job_id, status_url}` immediately.
3. Either polls `status_url`, or just waits — a `POST` lands on their `webhook_url` when done, with
   the PDF URL or bytes.

**Backend flow**
1. `POST /v1/pdf/html` with `async:true` → enqueue into the existing `orchestration.Pool`, return
   `job_id` right away instead of blocking.
2. A worker renders it in the background (same render path as today, no new engine).
3. On completion, store the result and `POST` it to `webhook_url` (with retry on failure).
4. `GET /v1/jobs/{id}` returns status for anyone polling instead.

**Key decision**: needs somewhere to store job status + output (`storage-and-delivery`, already
scoped in your roadmap, not yet built) — this is the one prerequisite piece.

---

## 3. Failed-Job Dashboard

**Pain point**: Right now, if a PDF fails, the only way to find out why is to check server logs
yourself or get an email from a confused customer.

**Frontend flow**
1. A simple web page: list of recent requests, status (success/failed), and — for failures — the
   actual error code/message.
2. Click a failed one → see the exact request that was sent and the exact error.
3. Optional **Retry** button.

**Backend flow**
1. Every request already gets logged with a request ID and structured error (this exists today —
   see `internal/observability`).
2. Add a small `GET /v1/requests/{id}` and `GET /v1/requests?status=failed` endpoint that reads from
   that same log/store.
3. Frontend is just a thin read-only view on top of data you already collect.

**Why build this early**: cheapest item on this whole list — no new backend logic, just exposing
data that already exists.

---

## 4. Batch Endpoint

**Pain point**: A month-end run of 500 invoices means 500 separate API calls today.

**Frontend flow**
1. User submits one request with a list of `{content, payload}` pairs.
2. Gets back one `batch_id`.
3. Checks batch status, or gets one webhook when the whole batch finishes.

**Backend flow**
1. `POST /v1/pdf/batch` — accepts an array, fans each item out into the existing worker pool
   (same underlying renderer, just called many times).
2. Tracks per-item success/failure under one `batch_id`.
3. Returns a zip of PDFs, or individual URLs, once complete.

Builds naturally on top of #2 (Async Jobs) — do that one first.

---

## 5. AI Fix-It Assistant

**Pain point**: When a template has a missing field or a header/footer too tall for its margin
(already detected today!), the customer just gets an error string and has to guess the fix.

**Frontend flow**
1. Error appears as normal.
2. A **Suggest a fix** button appears next to it.
3. Click it → an AI-suggested corrected template/CSS snippet appears, which the user can accept or
   ignore.

**Backend flow**
1. On a `TEMPLATE_ERROR` or header/footer height-mismatch error, send the failing
   template + the exact error to an LLM.
2. LLM returns a suggested corrected snippet.
3. Return that suggestion alongside the normal error response — doesn't change existing error
   behavior, purely additive.

Smallest AI idea on this list — it attaches to error handling you've already built and tested.

---

## 6. AI Visual QA

**Pain point**: A template can render "successfully" (no error) but still look broken —
overlapping text, an image that didn't load, awkward page breaks. Nobody catches this until a
customer does.

**Frontend flow**
1. User clicks **Check my template** before going live.
2. Sees a short list: "Looks fine" or "Possible issues: text overflow on page 2, image missing on
   line 14."

**Backend flow**
1. Render the template against a few sample payloads (already possible with the sandbox path).
2. Send the rendered PDF pages (as images) to a vision-capable LLM, asking it to flag anything that
   looks visually wrong.
3. Return the flagged issues as a simple list.

This also fills a gap your own test suite still has open (no automated visual-regression testing
yet) — this feature and that internal testing gap can share the same underlying check.

---

## 7. Font/Locale Pre-flight Check

**Pain point**: Chinese/Japanese/Korean/Arabic text silently renders as empty boxes if the right
font isn't available — no error, just a broken-looking PDF the customer discovers on their own.

**Frontend flow**
1. Nothing extra to build — this runs automatically before rendering.
2. If a problem is detected, the API response includes a warning instead of just failing silently.

**Backend flow**
1. Scan the request content for non-Latin character ranges (CJK, Arabic, Hebrew).
2. Check whether the configured font stack actually covers those characters.
3. If not, add a `"warnings"` field to the response (not a hard failure) — pure Go string/rune
   checking, no AI needed for this one.

Cheapest, most surgical fix for the exact bug class your own research already flagged as the
#1 most-cited PDF-generation failure across every competitor.

---

## 8. E-Sign Hook

**Pain point**: Once an invoice/contract PDF exists, the next thing most businesses need is
getting it signed — today that's a manual, separate step.

**Frontend flow**
1. After generating a PDF, user clicks **Send for signature**.
2. Enters signer's email.
3. Gets notified when signed; signed copy available for download.

**Backend flow**
1. `POST /v1/pdf/{id}/send-for-signature` — hands the generated PDF off to a third-party e-sign
   API (DocuSign, HelloSign, etc.) rather than building signing yourself.
2. Store the signing status, forward the provider's completion webhook to the user's own
   `webhook_url`.

This is an integration, not a new core capability — lowest technical risk, but depends on picking
and paying for a third-party e-sign provider.

---

## 9. Healthcare Compliance Mode (confirm this is actually your vertical first)

**Pain point**: If your customers are healthcare businesses, an audit trail and PHI-safe handling
is often the actual reason they buy — more than PDF rendering quality itself.

**Frontend flow**
1. Admin turns on **Compliance Mode** for their account.
2. Every generated PDF's dashboard entry shows: who requested it, when, from what payload hash.
3. Optional redaction toggle: mask configured "sensitive" fields in logs/previews.

**Backend flow**
1. Log a hash of the payload (not the raw payload itself, if it contains PHI) alongside every
   request — ties into the dashboard in #3.
2. Never log raw sensitive field values, only redacted previews.
3. Add a documented data-retention/deletion policy for stored PDFs.

Don't build this speculatively — confirm with your actual customers first that this is the real
buying driver before investing here.

---

## Recommendation

If you can only pick three to start:

1. **Async Jobs + Webhooks** — closes an acknowledged gap, no new dependency, unblocks everything
   batch-related.
2. **AI Template Generator** — the single most differentiated feature here; directly kills the
   "need a developer" pain point that likely costs you the most customers today.
3. **Failed-Job Dashboard** — cheapest to build (data already exists), reduces your own support
   load immediately.

Everything else can wait until you see real usage patterns from these three.
