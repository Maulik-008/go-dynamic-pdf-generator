# How to Connect Your Project (Node.js example) and Generate a PDF

This guide answers a very common, practical question: *"I have a Node.js project. My frontend
builds an HTML page (or a Markdown document). Now I want a PDF out of it. How do I connect to this
service and make that happen?"*

We'll explain it step by step, with real, working code. And at the end, we'll also explain how
**any** other project (Python, PHP, Java, whatever) can connect too — because this service just
talks plain HTTP + JSON, so it doesn't care what language is calling it.

## First, understand the recommended architecture

A very important point before we start: **this PDF service has no login/security check built in
yet** (see the other two guides for details). So please don't let your frontend (the browser) talk
to it directly over the public internet. The correct, safe way is:

```
Your Frontend (browser)
        │
        ▼  (your own app's normal API call, with your own login/auth)
Your Node.js Backend
        │
        ▼  (internal network call, not exposed publicly)
This PDF Service
        │
        ▼
   PDF bytes come back
        │
        ▼
Your Node.js Backend sends the PDF back to the browser
```

In short: **your frontend talks to your own backend as usual. Your backend talks to this PDF
service. The PDF service should only be reachable from your backend, not from the public
internet.**

## The API contract (what you send, what you get back)

Every conversion endpoint on this service works the exact same simple way:

**Authentication.** Every conversion endpoint requires an API key (the health probes do not).
Send it as either `X-API-Key: <key>` or `Authorization: Bearer <key>`. The server is configured
with one or more keys via `API_KEYS` (comma-separated, for rotation). A missing or wrong key is
`401` with `{"error":{"code":"UNAUTHORIZED", ...}}`.

**You send a JSON request:**

```json
{
  "content": "<html><body><h1>Hello {{.name}}</h1></body></html>",
  "payload": { "name": "Ravi" },
  "options": { "paperSize": "Letter", "landscape": false }
}
```

- `content` — **required.** Your HTML (or Markdown, depending on which endpoint you call). This
  can either be your final, ready HTML, or a *template* with placeholders like `{{.name}}`.
- `payload` — **optional.** If you include it, `content` is treated as a template, and the values
  in `payload` are filled into the placeholders before making the PDF. If you leave it out, your
  `content` is used exactly as-is.
- `options` — **optional.** Page setup and wait strategy. Every field is optional and falls back
  to the server's deployment default. See the table below.

**Options** (all optional; `/v1/pdf/html` supports all, `/v1/pdf/markdown` supports all except
`fitToPage`/`embedImages`/`overlay`, `/v1/pdf/html-lite` supports only `timeoutMs`):

| Field | Type | Notes |
|---|---|---|
| `paperSize` | string | `Letter` `Legal` `Tabloid` `A3` `A4` `A5`. Or set `width`+`height` instead. |
| `width`, `height` | string/number | A number is inches; a string may carry a unit (`"210mm"`, `"8.5in"`, `"72pt"`, `"96px"`, `"1cm"`). Must be set together. |
| `landscape` | bool | |
| `margin` | `{top,right,bottom,left}` | Each a length like `width`/`height`. |
| `scale` | number | `0.1`–`2.0`. |
| `printBackground` | bool | Default true. |
| `displayHeaderFooter` | bool | Enables `headerTemplate`/`footerTemplate`. |
| `headerTemplate`, `footerTemplate` | string | Chromium's own margin-box HTML (supports `.pageNumber`, `.totalPages`, …). |
| `waitForFonts`, `waitForImages` | bool | Default true. |
| `timeoutMs` | int | Whole-render budget, clamped to `[1000, 120000]`. |
| `fitToPage` | bool | **`/v1/pdf/html` only.** Shrink content to a single page (min scale 0.6). The scale applied comes back in the `X-Fit-Scale` response header; `X-Fit-Overflow: true` means even 0.6 wasn't enough. |
| `embedImages` | bool | **`/v1/pdf/html` only, and only if the server enabled it.** Fetch remote `<img src>` and inline them before rendering. |
| `overlay` | `{html, pages?}` | **`/v1/pdf/html` only.** Render `html` on a transparent, full-size page and stamp it onto selected pages of the finished PDF — for a box that must land on *some* pages, not every page like `footerTemplate`. `pages`: `"last"` (default), `"first"`, `"all"`, or a list like `"3"`, `"3-5"`, `"2,4"`. The token `{{totalPages}}` in `html` becomes the final page count. The fragment places itself with its own CSS (e.g. `position:fixed; bottom:0`); unpainted areas stay transparent. |

**You get back one of two things:**

- **Success** → the raw PDF file itself (`Content-Type: application/pdf`). You just save these
  bytes as a `.pdf` file, or stream them straight to your user.
- **Failure** → a JSON error, always in the same shape:

  ```json
  {"error": {"code": "TEMPLATE_ERROR", "message": "a clear description of what went wrong"}}
  ```

Here are all the error codes you might see:

| Code | HTTP status | What it means | What you should do |
|---|---|---|---|
| `UNAUTHORIZED` | 401 | No API key, or a key that isn't configured on the server | Send `X-API-Key` / `Authorization: Bearer` with a valid key |
| `INVALID_REQUEST` | 400 | Empty body, broken JSON, missing `content`, or an unusable `options` object (unknown `paperSize`, `scale` out of range, a length that doesn't parse, `fitToPage`/`embedImages`/`overlay` on the wrong route, `overlay` with no `html` or a bad `pages` selector) | Fix your request — this is a bug on your side |
| `REQUEST_TOO_LARGE` | 413 | Your HTML/Markdown was too big | Reduce the size, or split into smaller documents |
| `TEMPLATE_ERROR` | 422 | Something's wrong with your template/data combination (e.g. you used `{{.name}}` but didn't send `name` in `payload`) | Check your template placeholders match your data fields |
| `RENDER_ERROR` | 422 | The actual PDF rendering failed | Check your HTML/CSS for problems |
| `QUEUE_FULL` | 503 | The server is genuinely too busy right now | Wait a moment and try again — a `Retry-After` header tells you how many seconds to wait |
| `NOT_CONFIGURED` | 503 | You called the WeasyPrint "lite" endpoint, but that path isn't turned on for this server | Use the normal HTML/Markdown endpoint instead, or ask whoever runs the server to enable it |

## The three endpoints available

| Endpoint | What it's for |
|---|---|
| `POST /v1/pdf/html` | Turn HTML into a PDF (full browser accuracy — supports JavaScript, modern CSS) |
| `POST /v1/pdf/markdown` | Turn Markdown text into a PDF |
| `POST /v1/pdf/html-lite` | Turn simple, static HTML into a PDF using a lighter engine (no JavaScript support at all) — only available if the server admin has turned it on |

## Quick test with curl (before writing any code)

```bash
curl -X POST http://localhost:8080/v1/pdf/html \
  -H "Content-Type: application/json" \
  -d '{"content":"<html><body><h1>Hello World</h1></body></html>"}' \
  --output hello.pdf
```

If this downloads a working `hello.pdf`, the service is reachable and working — now let's connect
it to your Node.js project.

## Step-by-step: Node.js integration

### Step 1: A small, reusable helper to call the PDF service

Create a file, say `pdfClient.js`:

```js
// pdfClient.js
const PDF_SERVICE_URL = process.env.PDF_SERVICE_URL || "http://localhost:8080";
const PDF_SERVICE_API_KEY = process.env.PDF_SERVICE_API_KEY;

/**
 * Calls the PDF service and returns the generated PDF as a Node.js Buffer.
 * Throws a clear Error if anything goes wrong.
 */
async function generatePdf(endpoint, content, payload, options) {
  const body = { content };
  if (payload) body.payload = payload;
  if (options) body.options = options;

  const response = await fetch(`${PDF_SERVICE_URL}${endpoint}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-API-Key": PDF_SERVICE_API_KEY,
    },
    body: JSON.stringify(body),
  });

  if (!response.ok) {
    const body = await response.json().catch(() => null);
    const code = body?.error?.code || "UNKNOWN_ERROR";
    const message = body?.error?.message || `HTTP ${response.status}`;

    // Special handling: the server told us it's busy, and for how long to wait.
    if (code === "QUEUE_FULL") {
      const retryAfter = response.headers.get("Retry-After") || "1";
      const err = new Error(`PDF service is busy, retry after ${retryAfter}s`);
      err.code = code;
      err.retryAfter = Number(retryAfter);
      throw err;
    }

    const err = new Error(`PDF generation failed [${code}]: ${message}`);
    err.code = code;
    throw err;
  }

  const arrayBuffer = await response.arrayBuffer();
  return Buffer.from(arrayBuffer);
}

function generatePdfFromHtml(html, payload) {
  return generatePdf("/v1/pdf/html", html, payload);
}

function generatePdfFromMarkdown(markdown, payload) {
  return generatePdf("/v1/pdf/markdown", markdown, payload);
}

module.exports = { generatePdfFromHtml, generatePdfFromMarkdown };
```

*(Node.js 18+ has `fetch` built in already. If you're on an older Node.js version, install
`node-fetch` and import it at the top instead.)*

### Step 2: Case A — your frontend already builds the full, ready HTML

This is the simplest case. Your frontend generates the complete HTML string (maybe using a React
component rendered to a string, or any template library on the frontend) and sends it to your own
backend.

```js
// server.js (Express example)
const express = require("express");
const { generatePdfFromHtml } = require("./pdfClient");

const app = express();
app.use(express.json({ limit: "10mb" })); // HTML documents can be large, raise the limit if needed

app.post("/api/generate-pdf", async (req, res) => {
  try {
    const { html } = req.body; // the frontend already sent complete, ready HTML

    const pdfBuffer = await generatePdfFromHtml(html);

    res.set("Content-Type", "application/pdf");
    res.set("Content-Disposition", 'attachment; filename="document.pdf"');
    res.send(pdfBuffer);
  } catch (err) {
    console.error(err);
    res.status(502).json({ error: err.message });
  }
});

app.listen(3000, () => console.log("Listening on port 3000"));
```

Your frontend would simply call `POST /api/generate-pdf` on **your own Node.js server** (not the
PDF service directly), like any other normal API call in your app.

### Step 3: Case B — you keep the template on your backend, frontend only sends the data

This is the recommended approach for anything repeated, like invoices or certificates — you don't
want to send a huge HTML string over the network every single time, and you don't want your
frontend deciding what the invoice layout looks like.

```js
// invoiceTemplate.js
// Note the {{.field}} style — this project's template engine uses Go's
// template syntax, so placeholders always start with a dot.
const INVOICE_TEMPLATE = `
<html>
<body>
  <h1>Invoice for {{.customerName}}</h1>
  <table border="1" cellpadding="6">
    <tr><th>Item</th><th>Amount</th></tr>
    {{range .items}}
    <tr><td>{{.description}}</td><td>Rs. {{.amount}}</td></tr>
    {{end}}
  </table>
  <p><b>Total: Rs. {{.total}}</b></p>
</body>
</html>
`;

module.exports = { INVOICE_TEMPLATE };
```

```js
// server.js (added route)
const { INVOICE_TEMPLATE } = require("./invoiceTemplate");

app.post("/api/invoice/pdf", async (req, res) => {
  try {
    // This is the actual data your frontend form/page sent — just plain data,
    // no HTML at all.
    const { customerName, items, total } = req.body;

    const pdfBuffer = await generatePdfFromHtml(INVOICE_TEMPLATE, {
      customerName,
      items,
      total,
    });

    res.set("Content-Type", "application/pdf");
    res.set("Content-Disposition", 'attachment; filename="invoice.pdf"');
    res.send(pdfBuffer);
  } catch (err) {
    console.error(err);
    res.status(502).json({ error: err.message });
  }
});
```

Your frontend now only ever sends plain data (`{ customerName, items, total }`), never HTML — much
cleaner, and your invoice design lives in one place (your backend), easy to update later.

### Step 4: Handling the "server is busy" case properly

If you expect heavy traffic, add a small retry when you get `QUEUE_FULL`:

```js
async function generatePdfWithRetry(html, payload, maxRetries = 2) {
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    try {
      return await generatePdfFromHtml(html, payload);
    } catch (err) {
      if (err.code === "QUEUE_FULL" && attempt < maxRetries) {
        await new Promise((r) => setTimeout(r, (err.retryAfter || 1) * 1000));
        continue;
      }
      throw err;
    }
  }
}
```

### Step 5: What about Markdown instead of HTML?

Exactly the same pattern, just call `generatePdfFromMarkdown` instead:

```js
const markdown = "# Hello " + userName + "\n\nThis is your report.";
const pdfBuffer = await generatePdfFromMarkdown(markdown);
```

## How ANY other project (not just Node.js) can connect

Since this is just a plain HTTP API that speaks JSON in and PDF bytes out, **literally any**
programming language that can make an HTTP request can use it. There is nothing Node.js-specific
about the service itself — the example above is just one way of calling it.

**Python example:**

```python
import requests

response = requests.post(
    "http://localhost:8080/v1/pdf/html",
    json={"content": "<html><body><h1>Hello from Python</h1></body></html>"},
)
if response.ok:
    with open("output.pdf", "wb") as f:
        f.write(response.content)
else:
    print("Error:", response.json())
```

**PHP example (using curl):**

```php
<?php
$ch = curl_init("http://localhost:8080/v1/pdf/html");
curl_setopt($ch, CURLOPT_POST, true);
curl_setopt($ch, CURLOPT_HTTPHEADER, ["Content-Type: application/json"]);
curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode([
    "content" => "<html><body><h1>Hello from PHP</h1></body></html>",
]));
curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
$pdf = curl_exec($ch);
file_put_contents("output.pdf", $pdf);
```

The idea is always the same, no matter the language:
1. Send a `POST` request with a JSON body containing `content` (and optionally `payload`).
2. If you get a successful response, the response body itself *is* your PDF file — just save those
   bytes.
3. If you get an error response, it's JSON, in the `{"error": {"code", "message"}}` shape described
   above.

## Quick checklist before going live

- [ ] Your Node.js backend (not your frontend) is the only thing that talks to the PDF service.
- [ ] The PDF service is only reachable from your own backend/internal network, not the public
      internet.
- [ ] You handle the `QUEUE_FULL` case gracefully (retry, or tell the user "please try again in a
      moment").
- [ ] You've tested what happens if your HTML/template has a mistake — you should see a clear
      `TEMPLATE_ERROR` or `RENDER_ERROR`, not a silent failure.
- [ ] If you send user-provided data through `payload`, remember it's automatically safe (HTML
      special characters get escaped) — you don't need to sanitize it yourself before sending it.
