# What is this project? (Simple explanation)

This document explains the whole project in simple words. No heavy technical language. Just
straight talk on what we built, why we built it, and what it can do for you today.

## In one line

This is **your own PDF-making service**. You send it some HTML or Markdown text, and it sends you
back a proper PDF file. Think of it as your own private version of tools like cloudlayer.io,
DocRaptor, or PDFShift — but running on your own server, under your own control, with no monthly
subscription to anyone else.

## Why we built this

Basically, three things matter the most for any PDF-generation tool:

1. **Speed** — it should not take forever to make one PDF.
2. **Accuracy** — the PDF should look exactly like the HTML/CSS you designed, not a broken version
   of it.
3. **Customisation** — you should be able to generate PDF "your way" — your own data, your own
   templates, your own layout.

This project is built keeping exactly these three things in mind, in that order of priority.

## How it actually works (in simple words)

Imagine you give this service a piece of HTML, like:

```html
<html><body><h1>Hello, this is my invoice</h1></body></html>
```

Internally, this project opens up a real Chromium browser (the same engine behind Google Chrome),
loads your HTML inside it, and asks the browser to "print this page to PDF." That's it. Since it's
a real browser doing the printing, your CSS, fonts, images, tables — everything renders exactly
like it would on a real webpage. No guessing, no half-supported CSS.

There is also a second, lighter way to make a PDF (explained below) for cases where you don't need
a full browser.

## The two ways to make a PDF

| Way | Uses | Best for | Trade-off |
|---|---|---|---|
| **Full way (Chromium)** | A real Chromium browser | Anything with JavaScript, modern CSS, complex layouts | Slightly heavier on the server |
| **Light way (WeasyPrint)** | A pure Python PDF engine, no browser | Very simple, static HTML with basic CSS | No JavaScript support at all; some advanced CSS may render slightly differently |

You choose which one you want by calling a different address (endpoint) — nothing is decided
automatically behind your back. This is on purpose: automatically switching engines can silently
change how your document looks, and we never want that surprise.

## Making the same template work for many people (Templates + Data)

Let's say you want to send 1,000 people the same invoice design, just with different names and
amounts. You don't have to build 1,000 different HTML strings yourself. Instead:

1. You write **one HTML template**, with placeholders like `{{.customerName}}` and `{{.amount}}`.
2. You send that template **plus** a small piece of data (like `{"customerName": "Ravi", "amount":
   "500"}`).
3. This project fills in the blanks for you, and then turns the final result into a PDF.

This is exactly how real invoice/certificate/report generation tools work — one design, many
outputs.

**Important safety note**: any data value you send gets automatically "cleaned" before it's put
into the HTML. So even if someone tries to sneak in a `<script>` tag through the data (say, a
customer's name field on a form), it will show up as plain, harmless text in your PDF — it will
never run as real code. This is built in by default, you don't have to do anything extra for it.

## Handling many requests at the same time, safely

Imagine 50 people request a PDF at the exact same second. A browser (Chromium) can only handle so
many pages at once before it starts slowing down or running out of memory. So this project has a
built-in "queue manager" (we call it job-orchestration) that works like a queue at a bank counter:

- Only a certain number of requests are worked on at the same time.
- Extra requests wait in a line, up to a limit.
- If the line gets too long, new requests are told clearly "please try again in a moment" (instead
  of just hanging forever with no answer).

This keeps the system stable even under heavy load, instead of crashing or slowing to a crawl.

## Checking headers and footers before printing

If you use a header or footer on your PDF (like a company logo on top of every page, or page
numbers at the bottom), this project actually **measures** how tall your header/footer really is
before printing, and tells you clearly if it won't fit in the space you gave it. This avoids a very
common, very annoying bug seen in most PDF tools — where your header text silently overlaps with
your main content because nobody checked the size beforehand.

## One simple, consistent way to talk to it

No matter which feature you use (plain HTML, Markdown, or the light WeasyPrint path), you always
talk to this project the exact same way:

- You send a JSON request with your content (and optionally, your data for template-filling).
- You get back either:
  - A PDF file, if everything worked, or
  - A clear JSON error message, telling you exactly what went wrong (missing field, bad data, too
    much load right now, etc.)

One shape, one mental model — you don't need to remember five different ways of talking to five
different features.

## What this project can do today (already built and tested)

- Convert HTML to PDF (using real Chromium)
- Convert Markdown to PDF (using the same Chromium engine)
- Convert simple static HTML to PDF using the lighter WeasyPrint engine (no browser needed)
- Fill templates with your own data before generating the PDF (for both HTML and Markdown)
- Check header/footer size before printing so it never silently overlaps
- Handle many requests at once safely, with a proper queue and clear "please wait/retry" signals
  when the system is genuinely busy
- One consistent way (API) to use all of the above

## What this project does NOT do yet (honestly, not hidden)

We believe in being upfront about what's missing, not pretending everything is done:

- **No login/API keys yet** — right now, anyone who can reach the server can use it. You must put
  your own security/login layer in front of it if you expose it outside your private network.
- **No usage tracking/billing** — there's no built-in counting of "who used how much."
- **No background/async jobs with webhooks** — every request is "wait for the answer right now."
  There's no "start a job, I'll notify you later" mode yet.
- **No file storage** — the PDF is handed back to you immediately; this project does not save a
  copy anywhere for you.
- **No watermark, no custom font embedding, no PDF/A yet** — these are on the future list.
- **No visual template-builder screen** — you write your HTML/Markdown template yourself (in code),
  there's no drag-and-drop design tool built into this yet.

None of these are forgotten — they are all planned, just not built yet. Think of this project as a
strong, working "engine room" that already does the hardest and most important part (fast, accurate,
customisable PDF generation) really well. The business-facing layer around it (login, billing,
storage, webhooks) is the next phase of work.

## Who is this for?

If you (or your team) need to generate PDFs from HTML/Markdown from inside your own application —
invoices, reports, certificates, tickets, anything — and you want full control over the exact
output, without depending on a third-party paid service, this project gives you that engine. See the
next two documents for how to set it up, and how to actually connect your own project (for example, a
Node.js app) to it.
