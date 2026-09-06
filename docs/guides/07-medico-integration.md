# Running Locally & Wiring Synectus Medico's Backend

This guide covers two things: getting the service running on a dev machine (macOS or Linux), and
repointing the `saas-backend` (`../saas-backend`) PDF pipeline at it in place of
`pdf-service-saas`.

---

## 1. Run it locally

### Prerequisites

- Go 1.26+ (`go version`).
- A headless Chromium / `chrome-headless-shell` binary. On macOS the easiest source is a
  Playwright or Puppeteer cache copy already on disk:

  ```bash
  find ~ -name 'chrome-headless-shell' -type f 2>/dev/null
  # e.g. ~/Library/Caches/ms-playwright/chromium_headless_shell-XXXX/chrome-headless-shell-mac-arm64/chrome-headless-shell
  ```

  If you have none: `npx playwright install chromium` or `npx puppeteer browsers install chrome-headless-shell`.

### Configure and start

```bash
cp .env.local.example .env.local
# edit .env.local: set CHROMIUM_PATH, keep API_KEYS as-is for local dev
./scripts/dev.sh
```

`scripts/dev.sh` sources `.env.local` and runs `go run ./cmd/api`. The binary itself only reads
plain env vars (no dotenv dependency), so it behaves identically under systemd/Docker.

### Smoke test

```bash
KEY=local-dev-test-key-not-secret
BASE=http://127.0.0.1:8080

# health (no key needed)
curl -s $BASE/livez | jq .

# missing key -> 401
curl -s -o /dev/null -w '%{http_code}\n' -XPOST $BASE/v1/pdf/html \
  -H 'Content-Type: application/json' -d '{"content":"<h1>hi</h1>"}'

# HTML -> PDF, Letter (deployment default), with a key
curl -s -XPOST $BASE/v1/pdf/html -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"<html><body><h1>Hello</h1></body></html>"}' --output hello.pdf
file hello.pdf   # => PDF document, 1 page

# options envelope: A4 landscape, 20mm margins
curl -s -XPOST $BASE/v1/pdf/html -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"<h1>opts</h1>","options":{"paperSize":"A4","landscape":true,"margin":{"top":"20mm","bottom":"20mm","left":"20mm","right":"20mm"}}}' \
  --output opts.pdf

# fitToPage: response carries X-Fit-Scale / X-Fit-Overflow headers
curl -s -D - -o fit.pdf -XPOST $BASE/v1/pdf/html -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"<html><body>...tall content...</body></html>","options":{"fitToPage":true}}' | grep -i x-fit
```

### Full check

```bash
./scripts/test.sh              # build + vet + gofmt + go test ./...
./scripts/test.sh -race        # pass extra flags through to go test
```

> **macOS note:** ten `internal/renderengines` tests assert process counts by reading `/proc`
> and therefore only pass on Linux (`TestPool_ReusesProcessesAcrossRenders`,
> `TestPool_*Recover*`, etc.). This is pre-existing — the repo was developed in a Linux sandbox —
> not a regression. Every render/measure/fit test and every other package pass on macOS.

---

## 2. Repoint `saas-backend` at this service

### What moves and what does NOT

| Concern | Before (`pdf-service-saas`) | After |
|---|---|---|
| HTML → PDF render | Node + `puppeteer-cluster`, `maxConcurrency=1` | this service (Go, real pool + crash recovery + backpressure) |
| Page-number footer | hard-coded in the service | `saas-backend` sends it as `options.footerTemplate` + `displayHeaderFooter` |
| Disclaimer footer overlay (last page) | `pdfGenerator.addFooter` (pdf-lib) in the service | **capability now lives here** as `options.overlay` (pdfcpu stamp — see `SPEC-conversion-api.md`). `saas-backend` still applies its own local pdf-lib overlay (`utils/helpers/pdf-lib/addDisclaimerFooter.js`) for now; switching it to send `options.overlay` and deleting that helper is a follow-up, not done in this pass. |
| Embedded uploaded-PDF merge, multi-chunk report assembly | already in `saas-backend` (`generatePatientReportPdf`, `mergePDFs`) | unchanged |
| `fitToPage` (ONE_PAGE reports) | service measures + scales, returns `X-Fit-*` headers | identical — this service does the same |
| Auth | `X-API-Key` vs `PDF_SERVICE_API_KEY` | identical header, checked with constant-time compare; `Authorization: Bearer` also accepted |

Net effect on `saas-backend`: **only `services/pdfServiceClient.js` changes** (endpoint path,
request body shape, and it now applies the disclaimer overlay locally). `generatePatientReportPdf`,
`buildIntakeFormPdf`, `mergePDFs`, and every controller call site are untouched.

### Env

```
PDF_SERVICE_URL=http://localhost:8080          # this service
PDF_SERVICE_API_KEY=local-dev-test-key-not-secret
```

### Request mapping

`pdfServiceClient.generatePDF(htmlContent, landscape, footerContent, { fitToPage })` becomes:

```
POST {PDF_SERVICE_URL}/v1/pdf/html
X-API-Key: {PDF_SERVICE_API_KEY}
{
  "content": htmlContent,
  "options": {
    "paperSize": "Letter",
    "landscape": landscape,
    "margin": { "top": "0", "right": "0", "bottom": "10mm", "left": "0" },
    "printBackground": true,
    "displayHeaderFooter": true,
    "footerTemplate": "<the page-number template>",
    "fitToPage": fitToPage === true
  }
}
```

- Success → raw PDF bytes. If `fitToPage`, read `X-Fit-Scale` / `X-Fit-Overflow` response headers.
- If `footerContent` was passed, `pdfServiceClient` still applies the disclaimer overlay to the
  returned buffer locally (pdf-lib), exactly as `pdf-service-saas` did — see
  `utils/helpers/pdf-lib/addDisclaimerFooter.js`. The service can now do this itself via
  `options.overlay` (below); moving `saas-backend` onto it is a planned follow-up.
- Errors → JSON `{"error":{"code","message"}}`; `QUEUE_FULL`/`ENGINE_UNAVAILABLE` carry
  `Retry-After`.

Once `saas-backend` switches over, the disclaimer render+overlay becomes part of the same request
instead of a second local pass:

```jsonc
{
  "content": htmlContent,
  "options": {
    "paperSize": "Letter", "landscape": landscape,
    "margin": { "top": "0", "right": "0", "bottom": "10mm", "left": "0" },
    "printBackground": true, "displayHeaderFooter": true,
    "footerTemplate": "<the page-number template>",
    "fitToPage": fitToPage === true,
    // only when footerContent is set — the box CSS + {{totalPages}} layout
    // currently built in addDisclaimerFooter.js, sent as-is:
    "overlay": { "pages": "last", "html": "<style>…</style><div class=\"disclaimer-footer\">…{{totalPages}} / {{totalPages}}…</div>" }
  }
}
```

Setting the same values as `PDF_DEFAULT_*` on the service (see `deploy/systemd/great-pdf-generator.env`)
is belt-and-suspenders: the client sends them explicitly so output does not depend on the
deployment being configured, and the deployment defaults document the intent and cover any caller
that sends no options.
