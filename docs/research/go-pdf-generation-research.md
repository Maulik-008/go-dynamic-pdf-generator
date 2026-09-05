# Building a Go PDF Generation Platform: Research Report

**Scope:** Foundational research for a Go-based document generation service — HTML → PDF, Markdown → PDF, URL → PDF, Image → PDF, and PDF manipulation (merge/split/watermark/encrypt) — positioned as a self-hostable, operationally-sane alternative to SaaS products like [cloudlayer.io](https://cloudlayer.io/) and to fragile Node.js/Puppeteer deployments.

**Date:** 2026-08-30

---

## Executive Summary

1. **PDF is a print/paint format, not a document model.** A PDF page is a flat stream of drawing operators (draw this glyph here, stroke this path there) plus an object graph for resources (fonts, images). There is no reflow, no boxes, no cascade — whatever produces the content stream must have already finished all layout.
2. **There are exactly two architectures for producing that content stream:** (a) **direct construction** — an API call maps straight to drawing operators, and the caller does all layout math (this is what `gofpdf`, `pdfcpu`, `unipdf` do); or (b) **render-then-print** — a full layout engine (a browser, a typesetter, an office suite) does its normal layout pass and then serializes the result to PDF via its own print pipeline (this is what Puppeteer, wkhtmltopdf, WeasyPrint, Prince XML, and Gotenberg's Chromium module do). **Rich HTML/CSS fidelity is only achievable via (b)** — nobody reimplements CSS layout from scratch to feed a direct-construction API.
3. **Choosing (b) means we still depend on Chromium**, and a large share of Chromium's resource cost (multi-process architecture, per-renderer memory, `/dev/shm` requirements, CPU-heavy layout+paint+font-embedding) is **inherent to the engine, not to Node.js**. Go does not make Chromium lighter.
4. **What Go *does* fix is the orchestration layer around Chromium**, which is where most of Puppeteer's well-documented production pain actually lives: zombie child processes that survive a killed parent, hung `browser.close()` calls, Node's single event loop contending across concurrent pages, and a whole second V8/Node runtime's memory sitting on top of Chromium's own. Goroutines + `context.Context` + `os/exec` give deterministic, cancellable subprocess lifecycles that Puppeteer's own GitHub issue tracker shows Node has struggled to guarantee for years.
5. **This is not a hypothesis — it's already been proven.** [Gotenberg](https://gotenberg.dev) is an existing, production-used, MIT-licensed Go service solving this *exact* problem (HTML/URL/Markdown/Office → PDF, self-hosted, Docker-packaged) using `chromedp` to drive Chromium and a pool-of-long-lived-processes-behind-a-bounded-queue architecture instead of spawn-per-request. It is the single most important piece of prior art for this project and should be studied file-by-file, not just read about.
6. **cloudlayer.io itself is a thin(ish) product wrapper around the same idea** — its own marketing describes "a custom-built auto-scaling rendering farm... running headless Chrome." Our differentiation isn't a smarter rendering trick; it's packaging (self-hostable like Gotenberg, but with cloudlayer-grade product polish: templates, async jobs, webhooks, storage) plus the reliability engineering this document lays out.

---

## Part 1 — How a PDF File Actually Works

### 1.1 File structure: header, body, xref, trailer

A PDF is a flat byte sequence with four parts, always in this order ([ISO 32000-2](https://www.iso.org/obp/ui/#iso:std:iso:32000:-2:ed-1:v1:en); [PDF Association errata](https://pdf-issues.pdfa.org/32000-2-2020/clause07.html)):

- **Header** — `%PDF-1.7` (or `%PDF-2.0`), one line.
- **Body** — a sequence of *indirect objects*: `N G obj ... endobj` (object number, generation number). Objects are dictionaries (`<< /Key value >>`), arrays, numbers, strings, names (`/Name`), or **streams** (a dictionary + raw bytes between `stream`/`endstream` — used for page content, fonts, images). Objects reference each other by indirect reference, `12 0 R`, forming an object graph rather than a nested tree.
- **Cross-reference table (xref)** — a byte-offset index: for each object number, where its `obj` starts in the file. PDF 1.5+ may use a compressed cross-reference *stream* instead of the plain-text table.
- **Trailer** — found by seeking to the end and reading backward for `startxref`; it points to the xref's byte offset and holds `/Root` (the Catalog — the object graph's entry point) and `/Size`.

Minimal illustrative example (abbreviated, a 1-page "Hello, world" PDF):

```
%PDF-1.4
1 0 obj << /Type /Catalog /Pages 2 0 R >> endobj
2 0 obj << /Type /Pages /Kids [3 0 R] /Count 1 >> endobj
3 0 obj << /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]
            /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >> endobj
4 0 obj << /Length 44 >>
stream
BT /F1 24 Tf 72 700 Td (Hello, world) Tj ET
endstream
endobj
5 0 obj << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> endobj
xref
0 6
0000000000 65535 f
... (one 20-byte entry per object) ...
trailer << /Size 6 /Root 1 0 R >>
startxref
<byte-offset-of-xref>
%%EOF
```

**Why this shape matters for us:** because objects are addressed by number+offset rather than nested position, a writer can *append* new objects plus a fresh xref+trailer at the end of the file (an "incremental update") without touching existing bytes — trivial to emit sequentially, object by object, streaming to disk. This is exactly why PDF is mechanically easy to *generate* but effectively impossible to hand-edit correctly: every offset must be byte-exact, and shifting any byte silently breaks the xref table.

### 1.2 Content streams: a print/paint language, not a layout language

A page's `/Contents` stream is bytecode in a small postfix operator language — operands first, operator last, **no control flow, no reflow** ([PDF Association operator cheat sheet](https://pdfa.org/wp-content/uploads/2023/08/PDF-Operators-CheatSheet.pdf)):

- **Text**: `BT...ET` brackets a text object; `Tf` sets font/size, `Td`/`Tm` position the cursor, `Tj` shows a string, `TJ` shows a string array with kerning adjustments.
- **Paths/graphics**: `m`/`l`/`c` build path geometry; `f`/`S`/`B` fill/stroke/both; `re` draws a rectangle.
- **Graphics state**: `q`/`Q` push/pop state; `cm` concatenates a transform matrix (the mechanism for every scale/rotate/translate); `gs` applies an ExtGState (alpha, blend mode).
- **Images/reuse**: `Do` paints an **XObject** — an embedded raster **Image XObject**, or a reusable nested **Form XObject** (patterns, transparency groups, repeated content).

There is no "this paragraph flows to the next page." Every glyph and shape is placed at absolute coordinates already computed upstream. **Layout must happen before the content stream is written** — which is the crux of the entire architecture decision in Part 2.

### 1.3 Fonts: the hardest correctness problem in PDF generation

Every glyph drawn needs a font resource, either **non-embedded** (references a standard name like Helvetica — small, but rendering depends on the viewer having a matching font) or **embedded** (the font program bytes live inside the PDF). Producers almost always **subset** embedded fonts to just the glyphs used, to control size.

Simple fonts (Type1, TrueType) map one byte → one glyph (≤256 glyphs). **Composite fonts (Type0)** wrap a CIDFont and map multi-byte codes → CIDs → glyph indices via a CMap — required for CJK, full Unicode, and any subsetted/remapped font. A separate `/ToUnicode` CMap is needed purely so copy-paste/text-extraction works. Getting any piece of this wrong (subsetting, encoding, width tables, ToUnicode) produces boxes, wrong glyphs, or broken text-extraction, often only in specific viewers — which is exactly why "just reimplement PDF font embedding ourselves" is a much bigger undertaking than it looks, and a strong argument for leaning on mature libraries/engines rather than hand-rolling this.

### 1.4 Images and color

Raster images are Image XObjects: a stream dictionary with `/Width`, `/Height`, `/ColorSpace`, `/BitsPerComponent`, and a `/Filter` — **DCTDecode** (embeds JPEG bytes, lossy, best for photos), **FlateDecode** (zlib, lossless, used for raw pixel data and text streams alike), **CCITTFax/JBIG2** (bilevel scans), **JPXDecode** (JPEG2000). Filters can chain. Because images are stored essentially as embedded files, file size scales directly with how well the image was compressed — the practical levers are picking the right filter (JPEG for photos, Flate for line art/screenshots), downsampling to actual display resolution, and not re-encoding already-compressed sources.

### 1.5 Compliance flavors and generation-relevant extras

- **PDF/A** (archival: fonts embedded, device-independent color, no JS/encryption/external refs) vs **PDF/X** (print production: CMYK, mandatory trim/bleed) vs plain PDF (no constraints). Supporting a "compliance mode" is a constraint we put on our own output, not a different engine.
- **Bookmarks/outlines** — a separate `/Outlines` tree, independent of page content.
- **Hyperlinks/annotations, AcroForms** — annotation dictionaries overlaid on a page by rectangle, not part of the content stream.
- **Tagged PDF/accessibility** — a parallel structure tree mapping content back to headings/paragraphs/tables (needed for PDF/UA, screen readers) — one of the largest sources of spec complexity.
- **Headers/footers/page numbers/watermarks** — no special PDF construct; just ordinary text/graphics drawn near the top/bottom of every page by whatever generates the content stream.
- **Merge/split** — copying indirect objects between object graphs and rebuilding xref/trailer; doesn't touch content streams.
- **Encryption** — a trailer-level `/Encrypt` dictionary (RC4/AES) wrapping strings/streams, orthogonal to everything else.

---

## Part 2 — Two Architectures for Producing a PDF

| | **(a) Direct construction** | **(b) Render-then-print** |
|---|---|---|
| How it works | API call → content-stream operators directly | Full layout engine lays out markup, then its own "print" pipeline serializes to PDF |
| Who does layout | The caller (manual coordinates, maybe simple word-wrap) | The engine (real CSS/TeX layout) |
| Examples | `gofpdf`, `pdfcpu`, `unipdf`, `reportlab` | Puppeteer/Playwright + Chromium, wkhtmltopdf (WebKit, unmaintained, unpatched SSRF CVE), WeasyPrint (implements CSS Paged Media itself), Prince XML |
| Strengths | Fast, small, no browser dependency, fully controllable, no external process | Rich HTML/CSS fidelity, handles arbitrary real-world markup, reuses a mature layout engine instead of reimplementing one |
| Weaknesses | No CSS/flexbox/grid — application owns all layout, tables, pagination | Heavyweight dependency (a whole browser or typesetter), harder to sandbox, print-CSS quirks to manage |
| Where it fits us | Image → PDF, PDF manipulation (merge/watermark/encrypt), simple templated reports | HTML → PDF, URL → PDF, Markdown → PDF (via Markdown → HTML first) |

**This is the central fork for our architecture, and the honest takeaway is: we need both.** Nobody ships an HTML→PDF product on pure direct-construction — reimplementing CSS layout is not a reasonable scope for this project (or most projects; it's why WeasyPrint, one of the only teams to attempt it outside a browser, is a large, long-running effort). For the HTML/Markdown/URL surface, we render via headless Chromium, same as everyone else in this space. For image-to-PDF and PDF manipulation, direct construction in pure Go is strictly better — no browser dependency, no process to manage, much lower latency, and this is where a Go-native implementation immediately beats a Puppeteer-only competitor on operational simplicity.

---

## Part 3 — The Market: cloudlayer.io and Competitors

> Sourcing note: direct `WebFetch` of cloudlayer.io and gotenberg.dev was blocked by this session's network egress policy; cloudlayer.io details below are reconstructed from search-indexed snippets of its own site plus third-party aggregators (GitHub SDK repos, G2, SourceForge). Treat exact current pricing/feature wording as *last-indexed, not live-verified* — worth a manual re-check before this shapes final pricing decisions.

### 3.1 cloudlayer.io

**Positioning:** a document-generation *platform*, not just a converter — REST API + visual template designer + SDKs (Node, Python, PHP, Ruby, Go, Java, C#, F# — confirmed via the public `cloudlayerio` GitHub org) + Zapier/no-code integrations.

**Conversions:** HTML→PDF, URL→PDF (auth/cookie support, custom page sizes, batches up to 20 URLs/request), Nunjucks-templated document generation (JSON-driven variables, not a dedicated "Markdown" endpoint per se), HTML/URL→Image (PNG/JPG/WebP, retina/viewport control), `mergePdfs`, `docxToPdf`. Reusable templates with repeating headers/footers, dynamic page numbers/dates, webhooks on completion, a Jobs endpoint for status/cost/processing-time, cloud storage for output. XLSX conversion and watermarking: **not confirmed either way**.

**API shape:** REST/JSON, `X-API-Key` auth. **v1 is synchronous by default; v2 is asynchronous by default** (returns a job ID, retrieve via an Assets endpoint, with an `"async": false` override) — batch endpoints are always async. HTTPS webhooks on success/error.

**Engine:** self-stated in their own marketing as "headless Chrome" running on "a custom-built auto-scaling rendering farm... up to thousands of Chrome instances" — i.e., **Chromium**, confirmed by cloudlayer themselves, at marketing-copy depth (no technical deep-dive blog found).

**Pricing:** per-*document* (not per-page, not per-credit-tier confusion) — Free (30 credits, never expires), Starter $49/mo (5,000 docs), Growth $199/mo (50,000 docs, priority queue), Scale $249/mo (100,000 docs, fastest queue, dedicated AM). One PDF or image = 1 document "regardless of page count or file size" — explicitly marketed against competitors' "confusing credit systems."

**Stated differentiators:** flat per-document billing, 99.8% SLA with downtime credits, 24/7 support, auto-scaling Chrome farm, and a library of `/alternative/*` SEO pages directly knocking competitors (e.g., calling out api2pdf for lacking templates/webhooks/storage).

### 3.2 Direct competitors

| Product | Conversions | Engine | Open-source | Self-hostable | Pricing model |
|---|---|---|---|---|---|
| **cloudlayer.io** | HTML/URL→PDF, →Image, DOCX→PDF, merge, templates | Chromium (self-stated) | No | No | Per-document tiers, $49–$249/mo |
| **DocRaptor** | HTML→PDF, HTML→Excel | **Prince XML** (best-in-class CSS Paged Media, forms, accessibility tags) | No | No | Per-document tiers, $15–$149/mo |
| **PDFShift** | HTML/URL→PDF | Chromium | No | No | Credits by output file size, $9–$39/mo |
| **api2pdf** | HTML/URL/Office/Email/Image→PDF, merge, extract | **Selectable**: Chrome, wkhtmltopdf, or LibreOffice | No | No | Pay-per-conversion (~$0.001–$0.005/doc) |
| **PDFMonkey** | HTML+Liquid templates→PDF | Unconfirmed (likely Chromium) | No | No | Per-document, €0–€300/mo |
| **Docmosis** | Word/ODT templates→DOC/DOCX/HTML/ODT/PDF/RTF | Proprietary Java ("Tornado") — **not** browser-based | No | Yes (Tornado) | Cloud from $49/mo, separate self-host license |
| **Browserless.io** | Screenshots, PDF, general browser automation | Chromium/Firefox/WebKit pool, Puppeteer/Playwright-compatible | Yes (core image) | Yes | Session/unit-based, $0–$350+/mo |
| **Urlbox** | URL/HTML→Image, →PDF | Chromium | No | No | From $19/mo |
| **Gotenberg** | HTML/URL/Markdown→PDF+screenshots, Office→PDF, merge/split/PDF-A/watermark/metadata | Chromium + LibreOffice (UNO) + QPDF/pdfcpu/PDFtk | **Yes (MIT)** | **Yes (Docker)** | Free — self-hosted infra cost only |

### 3.3 What buyers actually care about (recurring marketing signals)

1. **"Stop babysitting headless Chrome yourself"** — the dominant pitch across cloudlayer, Browserless, and PDFShift. Confirms our reliability-engineering focus (Part 4–6) is the actual product, not a nice-to-have.
2. **Reliability/SLA anxiety** — cloudlayer's 99.8% SLA with credits implies customers have been burned by flaky DIY/thin-wrapper solutions.
3. **Pricing-model fragmentation as a pain point** — per-document (cloudlayer) vs per-file-size-credit (PDFShift) vs per-conversion (api2pdf) vs per-session-unit (Browserless): buyers have to reason about four different billing shapes. Flat, predictable pricing is a differentiator, not a given.
4. **"More than a converter"** — cloudlayer explicitly markets templates/webhooks/storage as a moat over bare conversion APIs.
5. **Fidelity claims** ("renders exactly like a real browser") — a direct callback to the wkhtmltopdf era's broken flexbox/grid/font handling; still worth demonstrating concretely, not just asserting.
6. **Self-host vs. SaaS trust trade-off** — Gotenberg's and Browserless's open-source options answer a segment that won't send potentially sensitive documents to third-party SaaS, or that wants to avoid unpredictable usage billing at volume — something pure-SaaS vendors structurally can't offer. **This is our clearest wedge**: cloudlayer-grade product polish, Gotenberg-grade self-hostability.

---

## Part 4 — Why Puppeteer/Node Struggles in Production

### 4.1 Chromium is inherently heavy — this part doesn't go away

Chrome/Chromium runs a coordinated fleet of processes by design (browser process, GPU process, network/utility processes, and a sandboxed renderer per site instance) so a bad tab can't take down the whole browser ([Chromium multi-process architecture](https://www.chromium.org/developers/design-documents/multi-process-architecture/)). A baseline instance runs ~200–300MB; content-heavy pages routinely push a renderer to 500MB–2GB before Chrome kills it. Chromium also relies on `/dev/shm` for IPC — Docker defaults this to a tiny 64MB, so a moderately complex render can OOM instantly unless the container is given real `--shm-size` headroom or launched with `--disable-dev-shm-usage`.

PDF generation specifically is CPU-expensive, not just memory-expensive: `printToPDF` re-runs layout under print media, paginates, resolves/embeds fonts, and serializes vectors/text into the PDF stream — meaningfully more work than a screenshot of the same DOM. A single non-trivial page can pin 70–90% of a CPU core; 10–20 concurrent jobs can saturate a whole host.

**None of this changes because the driver is Go instead of Node** — we are talking to the same Chromium binary either way.

### 4.2 Named production failure modes (from Puppeteer's own issue tracker)

- **Detached DOM/listener leaks across navigations** — a `page.goto()` timeout/error that skips `page.close()` leaves a full DOM+listeners resident; repeated over many failed requests, an unbounded leak.
- **Zombie/orphaned Chrome processes** — long-standing, repeatedly reopened issues ([#1825](https://github.com/puppeteer/puppeteer/issues/1825), [#615](https://github.com/puppeteer/puppeteer/issues/615), [#5279](https://github.com/puppeteer/puppeteer/issues/5279), [#12854](https://github.com/puppeteer/puppeteer/issues/12854)) show killing the Node process doesn't reliably reap Chrome's children — on Lambda, zombies accumulate until the 1024-process cap is hit. `browser.close()` can hang indefinitely if the CDP WebSocket is already lost ([#5331](https://github.com/puppeteer/puppeteer/issues/5331)); one maintainer diagnosis ([#12186](https://github.com/puppeteer/puppeteer/issues/12186)) found the child "wasn't properly detached" from its parent, so even SIGKILL doesn't clean it up — containers need an init reaper (`tini`/`dumb-init`).
- **Event-loop contention** — Node is single-threaded for JS; [#5333](https://github.com/puppeteer/puppeteer/issues/5333) and [#1958](https://github.com/puppeteer/puppeteer/issues/1958) document Puppeteer operations blocking the event loop, and many concurrent pages contend for that one thread.
- **"Page crashed!" errors** — heavy/malformed HTML/CSS or `/dev/shm` exhaustion crash the renderer mid-job ([#890](https://github.com/puppeteer/puppeteer/issues/890), [#1321](https://github.com/puppeteer/puppeteer/issues/1321)), forcing brittle reload-or-relaunch retry logic (this is essentially what `puppeteer-cluster` exists to paper over).
- **CDP WebSocket/FD leaks** — client bugs leave dangling sockets open ([#7407](https://github.com/puppeteer/puppeteer/issues/7407), [#11456](https://github.com/puppeteer/puppeteer/issues/11456)).
- **Version/dependency fragility** — Puppeteer pins an exact Chromium revision; OS/library drift routinely breaks launches. Bundling Chromium yields multi-hundred-MB to ~950MB Docker images. Serverless needed a whole sub-ecosystem (`chrome-aws-lambda`, now `@sparticuz/chromium`) just to fit under Lambda's 250MB package limit.
- **Cold start** — launching a fresh browser per request pays Chromium's ~300–800ms startup cost every time — why pooling/reuse is necessary, not optional.

### 4.3 Industry mitigation pattern (already converged on, worth copying wholesale)

Never launch-per-request. Maintain a pool of N long-lived browser instances; recycle a browser after M requests or T minutes to bound leaked memory; cap concurrency (rough rule of thumb: ~10 concurrent renders per GB); queue overflow rather than accept unbounded work; run pre-request health checks; isolate Chromium in its own container with hard memory/CPU limits and a liveness probe that kills+restarts on threshold breach. This is exactly Gotenberg's `chromium-restart-after` + `chromium-max-concurrency` + `chromium-max-queue-size` model (Part 5.4).

### 4.4 Synthesis: Chromium-inherent vs. Node/Puppeteer-specific

**Persists no matter what drives it (Go included):** multi-process memory footprint, `/dev/shm` requirement, CPU cost of layout/paint/font-embedding, renderer crashes on pathological input, Chromium's binary size/cold-start, CDP-version↔Chromium-build coupling.

**Specifically Node/Puppeteer, and plausibly fixed by a Go driver:**
- A whole second V8/Node runtime's memory and GC overhead sitting *on top of* Chromium's own is pure waste a Go binary avoids.
- Event-loop contention and "too many pages saturate one process" are a Node-specific concurrency ceiling; goroutines let each render job block on its own CDP round-trips without stalling siblings.
- The zombie-process and hung-`browser.close()` bugs are implementation defects in Puppeteer's `child_process`/CDP-teardown handshake, not properties of Chromium; `os/exec` + process-group management + `context.Context` cancellation gives more deterministic, forceful tree-kill semantics.
- CDP WebSocket/FD leaks are client-library bugs — fixable, not eliminated, by careful Go implementation.
- Docker image size shrinks partially (no Node/npm), but Chromium itself still dominates.

**Bottom line:** expect Go to remove orchestration/lifecycle-layer fragility, not to make Chromium itself lighter. The architecture still needs pooling, recycling, resource limits, and health-probed isolation — we're not skipping that engineering, we're doing it with better primitives.

---

## Part 5 — The Go Ecosystem for PDF Generation

### 5.1 Headless-Chromium drivers (render-then-print)

Both talk to Chrome purely over the **Chrome DevTools Protocol (CDP) via WebSocket** — no Node, no Puppeteer, no JS runtime anywhere in the stack.

**[`chromedp`](https://github.com/chromedp/chromedp)** (~13.3k★, CDP bindings generated into sibling `chromedp/cdproto`):
- Lifecycle built entirely on `context.Context`. `chromedp.NewContext(ctx)` creates a browser context; an `Allocator` (e.g. `chromedp.NewExecAllocator`) actually spawns/owns the Chrome process. Child contexts from a parent reuse the same browser process but get their own tab — the standard pooling primitive.
- Cancelling the context cleanly force-kills the Chrome child — the idiomatic per-job timeout/deadline mechanism (`context.WithTimeout`).
- PDF generation is one CDP call:
```go
ctx, cancel := chromedp.NewContext(context.Background())
defer cancel()
var buf []byte
chromedp.Run(ctx, chromedp.Tasks{
    chromedp.Navigate(url),
    chromedp.ActionFunc(func(ctx context.Context) error {
        var err error
        buf, _, err = page.PrintToPDF().WithPrintBackground(true).Do(ctx)
        return err
    }),
})
```
- Very actively maintained; the de facto standard Go CDP driver; **used internally by Gotenberg itself**.

**[`go-rod/rod`](https://github.com/go-rod/rod)** — a higher-level CDP driver over the same protocol: chained-context API, thread-safe ops, auto-download of a compatible browser binary, built-in guards against zombie processes after a crash. PDF export via `page.PDF(&proto.PagePrintToPDF{...})`. Ships `rod.NewBrowserPool(limit)` — a small channel-based pool helper (`pool.Get(create)`/`pool.Put(browser)`). Younger, lower-star-count than chromedp, strong CI/test-coverage discipline, active community (see [go-rod/rod#343](https://github.com/go-rod/rod/discussions/343)).

**Shared limitations either way:** real Chromium memory footprint per instance, need to bundle a Chromium binary, SSRF concerns fetching arbitrary URLs, container flags (`--no-sandbox`/`--disable-dev-shm-usage`) usually needed, print-CSS quirks mirror real browser print behavior.

### 5.2 Pure-Go, no-browser PDF libraries (direct construction)

| Library | Approach | Best for | Key limitation |
|---|---|---|---|
| `github.com/go-pdf/fpdf` (successor to `jung-kurt/gofpdf`) | Imperative canvas API (`Cell`, `MultiCell`, `Image`, `Line`) | Simple templated reports, invoices, certificates | No CSS/flexbox/grid — every coordinate/wrap is manual |
| `github.com/johnfercher/maroto` (v2) | Bootstrap-like row/column grid over gofpdf | Structured reports/invoices where a 12-col grid saves boilerplate | Still no arbitrary CSS — own component set only |
| `github.com/unidoc/unipdf` (v3) | Full PDF object-model SDK: creator, annotator, extractor, forms, signatures, PDF/A | Commercial-grade assembly, redaction, encryption, compliance | Commercial license; no HTML/CSS rendering |
| `github.com/pdfcpu/pdfcpu` | PDF manipulation library + CLI (validate, optimize, merge, split, watermark, encrypt, n-up) | **Post-processing pipeline: merge generated output, watermark, encrypt, PDF/A convert — in pure Go, no subprocess** | Not an authoring engine for rich content from scratch |
| `github.com/signintech/gopdf` | Fluent chained-method canvas API, strong Unicode/CJK TTF support | Documents needing embedded CJK fonts, transparency, PDF import | Same as fpdf — imperative positioning, no CSS engine |

**None of these consume HTML/CSS** — this is exactly why real HTML→PDF services (Gotenberg included) never use them for the HTML path. But `pdfcpu` deserves special note: it means our merge/split/watermark/encrypt/PDF-A operations can run **in-process, in pure Go, with zero external binary** — a concrete advantage over Gotenberg, which shells out to QPDF/PDFtk as a fallback chain for some of these operations.

### 5.3 Markdown pipeline

The standard (and Gotenberg's own) pattern is **Markdown → HTML (pure Go) → PDF (headless Chromium)** — not Markdown → PDF directly.

- **[`yuin/goldmark`](https://pkg.go.dev/github.com/yuin/goldmark)** — full CommonMark v0.31.2 compliance, AST-based, extensible (tables, footnotes, strikethrough), fuzz-tested. The parser Hugo and most modern Go static-site tooling use — the safer default (spec-compliant, predictable output).
- **[`gomarkdown/markdown`](https://pkg.go.dev/github.com/gomarkdown/markdown)** — fast, wider extension set (LaTeX/MathJax passthrough, smart quotes) but still pre-v1.0.

The alternative for typographic quality, Markdown→LaTeX→PDF, is not a pure-Go path — it means shelling out to `pdflatex`/`xelatex`/`tectonic` via `os/exec`, the same "wrap an external process" pattern as the LibreOffice route, just with a different engine. Worth deferring unless a customer specifically needs LaTeX-grade typesetting.

### 5.4 Deep dive: Gotenberg's architecture (our closest prior art)

[Gotenberg](https://github.com/gotenberg/gotenberg) (MIT, "thousands of companies in production") is a **Go HTTP API server with a plugin/module system**, not a monolith:

- **`pkg/modules/chromium`** — owns a `ProcessSupervisor` around headless Chromium, reached via `chromedp`. Key config: `chromium-max-concurrency` (default 6 — the number of tabs Chromium handles well concurrently), `chromium-restart-after` (default 100 conversions — periodically recycles the process to bound leak growth, rather than spawn/kill per request), `chromium-auto-start` vs. lazy start, `chromium-start-timeout`, `chromium-idle-shutdown-timeout`, `chromium-max-queue-size` (requests queue once concurrency saturates, then reject/timeout once the queue itself is full). Health via `supervisor.Healthy()` polling. SSRF protection (allow/deny host lists, private-IP blocking) lives here too, since it fetches arbitrary user URLs.
- **`pkg/modules/libreoffice`** — talks to a LibreOffice instance kept running (unoserver-style) so office-doc conversions don't pay multi-second cold-starts per request.
- **`pkg/modules/pdfengines`** — a `PdfEngineProvider` aggregator with a **failover chain**: for split/flatten/encrypt/PDF-A it tries configured engines in order (pdfcpu, qpdf, PDFtk) since each has different format-support gaps (e.g. qpdf honors per-permission encryption flags individually; pdfcpu treats permissions coarsely).
- **API**: `multipart/form-data` POST routes namespaced by engine+operation — `/forms/chromium/convert/url`, `/forms/chromium/convert/html`, `/forms/chromium/convert/markdown`, `/forms/libreoffice/convert`, `/forms/pdfengines/merge`, `/forms/pdfengines/split`. Synchronous by default (streams the PDF back), with an optional async/webhook mode (204 + callback POST) for long jobs — directly comparable to cloudlayer's v1/v2 sync/async split.
- **Deployment**: one multi-stage Docker image bundling the Go binary + Chromium + LibreOffice + QPDF/PDFtk/pdfcpu/ExifTool + fonts. `docker run -p 3000:3000 gotenberg/gotenberg:8` is the entire install — explicitly "you never manage Chromium/LibreOffice/fonts yourself."
- **Reliability framing**: a small, bounded, health-checked, periodically-recycled pool of long-lived processes behind a queue (not fork-per-request) avoids the process-explosion/memory-fragmentation failure mode of naive Puppeteer setups, while `chromium-restart-after` still guards against slow leaks in long-lived renderers.

### 5.5 Go concurrency/process primitives we'll use directly

- **Worker pool**: goroutines pulling jobs off a buffered channel, each holding a checked-out browser tab/context — mirrors rod's `BrowserPool` or a hand-rolled semaphore-channel around a shared chromedp allocator. This is our reimplementation target for Gotenberg's `chromium-max-concurrency` + queue behavior.
- **`os/exec` + `exec.CommandContext`**: the right primitive for any subprocess (LibreOffice, qpdf, a LaTeX engine later) — cancelling the context tears down the process tree cleanly.
- **`context.Context` propagated end-to-end** (HTTP handler → allocator → CDP call → subprocess) gives one cancellation/timeout mechanism across both halves of the pipeline.
- **Static binary deployment**: `go build` → one self-contained binary, no runtime/`node_modules` tree to patch or audit; only native tools (Chromium, LibreOffice) get vendored into the image, exactly as Gotenberg's Dockerfile does.
- **Memory footprint**: a goroutine costs a few KB of stack vs. a Node worker needing its own V8 heap/event loop — for the HTTP-facing dispatch layer sitting in front of a small pool of heavyweight native processes (the real cost driver either way), Go lets far more concurrent lightweight request handling run per host before Chromium/LibreOffice process count becomes the bottleneck rather than our own runtime overhead.

---

## Part 6 — Proposed Architecture

```
                         ┌─────────────────────────┐
    HTTP/JSON  ────────► │   API layer (net/http)   │
    (multipart or JSON)  │  auth · rate limit ·     │
                         │  request validation      │
                         └────────────┬─────────────┘
                                      │
                         ┌────────────▼─────────────┐
                         │      Job dispatcher       │
                         │  sync (small) vs async     │
                         │  (queue + webhook, large)  │
                         └──┬────────┬────────┬──────┘
                            │        │        │
              ┌─────────────▼┐ ┌─────▼─────┐ ┌▼─────────────────┐
              │ Chromium pool │ │ Direct-PDF │ │ (later) Office   │
              │  (HTML/URL/   │ │  engine    │ │ pool (LibreOffice│
              │  Markdown)    │ │ (image→PDF,│ │ via unoserver)   │
              │  chromedp,    │ │  merge/    │ │                  │
              │  N processes, │ │  split/    │ │                  │
              │  recycle after│ │  watermark/│ │                  │
              │  K conversions│ │  encrypt — │ │                  │
              │               │ │  pdfcpu,   │ │                  │
              │               │ │  pure Go)  │ │                  │
              └───────────────┘ └────────────┘ └──────────────────┘
                            │
                  ┌─────────▼─────────┐
                  │ Object storage      │
                  │ (result PDFs, TTL)  │
                  └────────────────────┘
```

### 6.1 Conversion modules

- **HTML/URL → PDF** — `chromedp`-driven Chromium pool. Pool of long-lived browser processes (not spawn-per-request); each job gets a tab-scoped context with a hard timeout via `context.WithTimeout`; concurrency capped (start at Gotenberg's default of 6 tabs/instance and tune from load testing); process recycled after N conversions or on memory-threshold breach; SSRF guard on user-supplied URLs (deny private/link-local IP ranges, cap redirects, enforce an allowlist mode for enterprise customers).
- **Markdown → PDF** — `goldmark` → HTML (wrapped in our own print-optimized template/CSS) → same Chromium pipeline as above. No separate rendering path to maintain.
- **Image → PDF** — pure Go, **no browser involved**. Place the image on a page via `go-pdf/fpdf` or `signintech/gopdf`, handle page-size-to-image-aspect-ratio logic ourselves. This path should be dramatically faster and cheaper than the Chromium path and is a genuine differentiator worth highlighting — most competitors funnel everything through the same heavyweight browser engine even for a trivial "wrap this JPEG in a PDF" request.
- **PDF manipulation (merge/split/watermark/encrypt/PDF/A convert)** — `pdfcpu` in-process, pure Go. This is a concrete edge over Gotenberg, which falls back to shelling out to QPDF/PDFtk for some of these; we should only add an external-tool fallback chain if/when we hit a real pdfcpu gap.
- **DOCX/XLSX/office → PDF** (later phase, only if demand justifies the extra footprint) — LibreOffice headless via `unoserver`, kept warm exactly like Gotenberg's `libreoffice` module, same pool/recycle pattern as Chromium.

### 6.2 Job model

Mirror the sync/async split both cloudlayer (v1/v2) and Gotenberg converged on independently — it's clearly the right answer, not a coincidence:
- **Sync** (default for small/simple jobs): block the HTTP request, stream the PDF back directly. Bounded by a request timeout.
- **Async** (batch, or any job exceeding a size/time threshold): return a job ID immediately, process off a queue, notify via webhook on completion, expose a status/result endpoint, store output in object storage with a TTL.

### 6.3 Process/pool management

- One pool per engine type (Chromium, LibreOffice-if/when-added); direct-construction paths (image, pdfcpu) don't need pooling — no external process.
- Health checks and liveness probes tied to pool health, not just process-alive — a hung-but-technically-alive Chromium instance should trigger recycling.
- Resource limits enforced at the container/cgroup level (memory + CPU caps per pool worker), not just application-level bookkeeping.
- Bounded queue with backpressure (reject/429 once the queue itself is full), matching Gotenberg's `chromium-max-queue-size` — never accept unbounded work.

### 6.4 Security

- SSRF protections on any URL-fetching path (deny private IP ranges/metadata endpoints, cap redirect depth) — non-negotiable given we're a service that fetches arbitrary user-supplied URLs.
- Request size and time limits enforced before dispatch.
- API key auth; per-key rate limiting.
- Sandboxed Chromium wherever possible; `--no-sandbox` only inside an already-isolated container, never as a substitute for isolation.

### 6.5 Observability

- Metrics: pool utilization, queue depth, per-job render latency (broken out by conversion type), memory per worker, recycle events.
- Structured logging per job (correlation ID end-to-end from HTTP request through subprocess).
- Tracing across the API → dispatcher → engine boundary, since latency debugging in this kind of system is almost always "which stage did the time actually go" not "is the code slow."

### 6.6 Deployment

Single static Go binary; Docker image bundles the binary + Chromium (+ LibreOffice, later) + fonts — same shape as Gotenberg's image, since it's a genuinely good pattern, not something to reinvent differently for its own sake. Stateless API layer scales horizontally by adding replicas; the actual bottleneck at scale will be Chromium/LibreOffice process count per host, not our own Go runtime overhead — size hosts around that.

### 6.7 What we deliberately don't build in v1

- Our own CSS layout/typesetting engine — Chromium's print pipeline is the industry-standard answer and reimplementing it is out of scope.
- LaTeX support — defer until a customer need justifies the extra `os/exec` surface.
- OCR / scanned-document handling — a different problem (image processing/ML), not core to "generate a PDF from HTML/Markdown/URL/Image."

---

## Part 7 — Mapping Puppeteer's Failure Modes to Our Mitigations

| Puppeteer/Node failure mode | Our mitigation |
|---|---|
| Zombie child processes after parent dies | `os/exec` + process groups + `context.Context` cancellation give deterministic tree-kill; container init reaper (`tini`) as defense in depth |
| Hung `browser.close()` | Per-job hard timeout via `context.WithTimeout`; supervisor force-kills on timeout regardless of CDP handshake state |
| Event-loop contention from concurrent pages | No shared single-threaded loop — goroutines block independently on their own CDP round-trips |
| Detached DOM/listener leaks across navigations | Tab-scoped contexts torn down unconditionally on job completion or timeout, not conditionally on a clean code path |
| Slow memory growth in long-lived renderers | `chromium-restart-after`-style recycling: retire a browser process after N conversions or a memory threshold, same as Gotenberg |
| Second V8/Node runtime stacked on Chromium's own | Eliminated entirely — no Node anywhere in the stack |
| Large Docker images from bundling Chromium + Node + npm deps | Smaller: Chromium is still there, but no Node/npm layer on top |
| `/dev/shm` exhaustion, renderer crashes | Container `--shm-size` sizing + `--disable-dev-shm-usage`, health-probed recycling on crash |

---

## Part 8 — Suggested Roadmap

1. **Phase 0 — Direct-construction core.** Image → PDF (fpdf/gopdf) + PDF manipulation (merge/split/watermark/encrypt via pdfcpu). Zero browser dependency, fastest path to a working, genuinely useful product, and validates the API/job-model shape before we take on Chromium's complexity.
2. **Phase 1 — HTML/URL → PDF.** `chromedp`-driven Chromium pool, single-instance-with-recycling to start (mirror Gotenberg's defaults), SSRF guards from day one.
3. **Phase 2 — Markdown → PDF.** `goldmark` → HTML → existing Chromium pipeline.
4. **Phase 3 — Product layer.** Async job model + webhooks + object storage + templating (variables/reusable templates, matching cloudlayer's core value-add).
5. **Phase 4 — Office documents (optional).** LibreOffice/unoserver pool, only if customer demand justifies the added image size/operational surface.
6. **Phase 5 — Hardening.** Load-test-derived pool sizing, PDF/A compliance mode, full observability stack, autoscaling policy tied to queue depth and pool utilization rather than raw CPU.

---

## Appendix — Sources

**PDF format:** [ISO 32000-2:2017](https://www.iso.org/obp/ui/#iso:std:iso:32000:-2:ed-1:v1:en) · [PDF Association errata](https://pdf-issues.pdfa.org/32000-2-2020/clause07.html) · [PDF Association operator cheat sheet](https://pdfa.org/wp-content/uploads/2023/08/PDF-Operators-CheatSheet.pdf) · [pikepdf content-stream docs](https://pikepdf.readthedocs.io/en/stable/topics/content_streams.html) · [loslab PDF structure](https://blog.loslab.com/en-us/pdf-structure/understanding-pdf-file-structure-a-technical-overview.html) · [idrsolutions on CID fonts](https://blog.idrsolutions.com/what-are-cid-fonts/) · [prepressure.com on font embedding](https://www.prepressure.com/pdf/basics/fonts) · [Datalogics PDF/A vs PDF/X](https://www.datalogics.com/pdfa-vs-pdfx-vs-pdf-2-0-which-pdf-standard-do-you-need) · [Overleaf on tagged PDF](https://www.overleaf.com/learn/latex/An_introduction_to_tagged_PDF_files%3A_internals_and_the_challenges_of_accessibility)

**Market/competitors:** [cloudlayer.io](https://cloudlayer.io/) · [cloudlayer pricing](https://cloudlayer.io/pricing/) · [cloudlayer API overview](https://cloudlayer.io/docs/api-overview/) · [docs.cloudlayer.io](https://docs.cloudlayer.io/api) · [cloudlayer GitHub SDKs](https://github.com/cloudlayerio) · [DocRaptor](https://docraptor.com/) · [PDFShift](https://pdfshift.io/) · [api2pdf](https://www.api2pdf.com/) · [PDFMonkey](https://pdfmonkey.io/) · [Docmosis](https://www.docmosis.com/) · [Browserless](https://www.browserless.io/) · [Urlbox](https://urlbox.com/) · [Gotenberg](https://gotenberg.dev/) · [Gotenberg GitHub](https://github.com/gotenberg/gotenberg)

**Puppeteer/Chromium production issues:** [Chromium multi-process architecture](https://www.chromium.org/developers/design-documents/multi-process-architecture/) · [puppeteer/puppeteer#1825](https://github.com/puppeteer/puppeteer/issues/1825) · [#615](https://github.com/puppeteer/puppeteer/issues/615) · [#5279](https://github.com/puppeteer/puppeteer/issues/5279) · [#12854](https://github.com/puppeteer/puppeteer/issues/12854) · [#5331](https://github.com/puppeteer/puppeteer/issues/5331) · [#12186](https://github.com/puppeteer/puppeteer/issues/12186) · [#5333](https://github.com/puppeteer/puppeteer/issues/5333) · [#1958](https://github.com/puppeteer/puppeteer/issues/1958) · [#890](https://github.com/puppeteer/puppeteer/issues/890) · [#1321](https://github.com/puppeteer/puppeteer/issues/1321) · [#7407](https://github.com/puppeteer/puppeteer/issues/7407) · [#11456](https://github.com/puppeteer/puppeteer/issues/11456) · [Browserless.io production observations](https://www.browserless.io/blog/observations-running-headless-browser)

**Go ecosystem:** [chromedp](https://github.com/chromedp/chromedp) · [chromedp examples](https://github.com/chromedp/examples) · [go-rod/rod](https://github.com/go-rod/rod) · [go-pdf/fpdf](https://pkg.go.dev/github.com/go-pdf/fpdf) · [maroto](https://github.com/johnfercher/maroto) · [unipdf](https://pkg.go.dev/github.com/unidoc/unipdf/v3) · [pdfcpu](https://pkg.go.dev/github.com/pdfcpu/pdfcpu) · [gopdf](https://pkg.go.dev/github.com/signintech/gopdf) · [goldmark](https://pkg.go.dev/github.com/yuin/goldmark) · [gomarkdown](https://pkg.go.dev/github.com/gomarkdown/markdown) · [Gotenberg docs](https://gotenberg.dev/docs/configuration) · [Gotenberg merge docs](https://gotenberg.dev/docs/manipulate-pdfs/merge-pdfs) · [Gotenberg webhook docs](https://gotenberg.dev/docs/webhook-download)
