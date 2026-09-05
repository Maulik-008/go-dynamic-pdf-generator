# Deep-Dive Research: Reducing CPU and Storage Footprint

Follow-up to `docs/research/load-test-results.md`, which surfaced two real gaps (pool sizing should
track CPU cores; no bounded backpressure yet). This doc researches how far CPU and storage/memory
footprint can genuinely be pushed down — including **correcting my own prior recommendation** from
earlier in this conversation, where I called "merge the pool into one shared browser process" the
"biggest lever" without the tradeoff evidence now in hand.

## Correction up front

I previously recommended collapsing the pool from N separate Chromium processes into one shared
browser process serving many tabs, framing it as an unambiguous win. Real evidence says otherwise:
it's a genuine **tradeoff between resource efficiency and crash blast-radius**, and the two most
relevant real-world projects deliberately chose *opposite* answers for good, stated reasons. Treat
everything below as a decision to make deliberately, not a fix to apply blindly.

## 1. The pool-architecture tradeoff, with real precedent on both sides

**Gotenberg** (our closest architectural relative) does exactly what I suggested: one long-lived,
supervised Chromium process serving up to `chromium-max-concurrency` (default **6**) concurrent
conversions, auto-restarted every `chromium-restart-after` (default **100**) conversions specifically
to bound memory growth — confirmed by reading its actual source
(`pkg/modules/chromium/chromium.go`). But real user reports against that exact model
([#642](https://github.com/gotenberg/gotenberg/issues/642),
[#987](https://github.com/gotenberg/gotenberg/issues/987),
[#1169](https://github.com/gotenberg/gotenberg/issues/1169)) show it **still leaks memory in
production for many workloads even with the restart safety valve** — the shared-process model isn't
free, it trades crash isolation for an ongoing memory-management burden that needs constant tuning.

**Browserless** deliberately chose the opposite: roughly one Chromium process per concurrent session
(~15-25 processes per server, ~10 concurrent requests per GB RAM as their sizing rule), stated in
their own words across multiple posts
([Observations running 2M sessions](https://www.browserless.io/blog/observations-running-headless-browser),
[5M/week follow-up](https://www.browserless.io/blog/observations-running-more-than-5-million-headless-sessions-a-week)):
*"one best-practice is to run only one session through each browser"* specifically because *"if
Chrome crashes for whatever reason, it won't bring down any other sessions."*

A third, independent data point: **Grafana's `grafana-image-renderer`** hit this same tension from
the other direction — its original "new Chrome per request" default caused a real production
incident (community report of
["hundreds/thousands of chrome processes"](https://community.grafana.com/t/grafana-image-rendering-plugin-creating-hundreds-thousands-of-chrome-processes-after-a-while-in-unix-server/79985)
piling up), which led to [issue #66 → PR #84](https://github.com/grafana/grafana-image-renderer/pull/84)
adding **three configurable pool modes** (process-per-request, clustered-many-processes,
clustered-one-shared-browser) — because they concluded there's no single right answer; it's
workload-dependent.

**What actually failed outright**: `--single-process` mode. The only real production precedent is
AWS Lambda (`chrome-aws-lambda`), which doesn't transfer — each Lambda invocation runs exactly one
render in total isolation, so there's no "many renders sharing one process" exposure at all. For a
long-lived multi-tenant server, real crash evidence
([puppeteer#5487](https://github.com/puppeteer/puppeteer/issues/5487),
[#5258](https://github.com/puppeteer/puppeteer/issues/5258)) shows `--single-process` breaks
Chromium's own internal liveness-check IPC, making it plausible for one bad page to abort the entire
process — this is architecturally unsound for our pool, not just risky. **Ruled out, not a
candidate.**

**Recommendation for this platform**: given render-engines' current pool already gives real crash
isolation (each pool slot is its own process) and the load test showed *zero* failures across 1,550
requests, I'd keep the current N-separate-processes model as the default and treat a
Gotenberg-style shared-process mode as an optional, explicitly-opt-in configuration for
memory-constrained deployments — not a wholesale replacement. This is a judgment call with a real
cost either way, worth your explicit sign-off before either direction gets built.

## 2. Low-risk, evidence-backed flags — measured, and mostly reverted

I called `--no-zygote` and `--disable-gpu` "no real downside" based on the research below. **Measured
against our own load test, both showed a real regression and were reverted.** This is exactly why
`performance-optimization`'s discipline exists — secondary-source "no downside" claims don't
substitute for measuring against your own workload.

The research case for each, as originally written:

- **`--no-zygote`**: a Chromium engineer's own comment on the headless-dev mailing list
  ([thread](https://groups.google.com/a/chromium.org/g/headless-dev/c/u1y_0jS19vk)) states the
  zygote process is "mostly useless" under `--no-sandbox`, and removing it "should not have any
  other functional or performance differences." One fewer process per instance, no documented cost.
- **`--disable-gpu`**: we don't need hardware acceleration for headless PDF rendering, and a real bug
  report ([vercel-labs/agent-browser#378](https://github.com/vercel-labs/agent-browser/issues/378))
  showed the GPU process as a genuine runaway-CPU culprit (519% CPU in a stuck session) in a
  chrome-headless-shell process tree.

### Measured outcome (see the ledger below) — both reverted

Methodology: same load test as `docs/research/load-test-results.md` (heavy 3-page document, real
HTTP path, pool config 3×4, 25 requests/worker), re-baselined fresh in the same session immediately
before each change to control for environment drift, each change measured twice to separate signal
from run-to-run noise, correctness suite (`-race`) re-run after each change.

| Idea | Baseline → Result | Verdict | Why |
|---|---|---|---|
| `--no-zygote` | Idle RSS 1050MB → **927MB** (real, repeatable −12%); throughput at c=8: 30.5 → **~24.7/s** (2 runs: 25.65, 23.80); p99 at c=16: 826ms → **~1.01s** (2 runs: 1.021s, 1.004s) | **Reverted** | Real idle-memory win, but a consistent, repeated ~15-20% throughput/latency regression at concurrency≥4 — a genuine cost to our stated top priority (speed), not noise (both runs agree closely) |
| `--disable-gpu` | Idle RSS 1050MB → **~1045MB** (no real change); peak RSS 2286MB → **~2289MB** (no real change); throughput at c=8: 30.5 → **~26.1/s** (2 runs: 26.38, 25.84); p99 at c=16: 826ms → **~907ms** (2 runs: 923ms, 891ms) | **Reverted** | No measured memory benefit at all (contrary to the research's expectation), plus the same throughput/latency regression pattern as `--no-zygote` — likely because this document embeds 3 images, and even software (swiftshader) GPU compositing appears to provide real parallel execution capacity that folding it away removes, not dead weight |

A likely mechanism for both regressions: our heavy-document fixture is image-heavy (3 embedded PNGs)
on a CPU-constrained (4-core) test box. Removing either the zygote or the GPU process reduces
process *count*, but on this workload that process count wasn't idle overhead — the zygote/GPU
processes were doing real, usefully-parallel work (pre-fork page sharing, image compositing) that a
"mostly useless" characterization written for a *general* headless-browsing context doesn't
necessarily hold for a *render-heavy, image-laden PDF* context. This is the concrete lesson from
this experiment: secondary-source "no downside" flag advice is written for browsing/scraping
workloads, not necessarily for CPU-and-image-heavy batch rendering — always re-verify against the
actual workload.

A false-alarm worth recording too: a test failure appeared twice in a row during this experiment
(`TestPool_ClosesWithoutOrphanedProcesses`, "before=38, during=38") that looked at first like a
regression. Investigated properly before concluding anything: reproduced 0/3 times in isolation,
0/3 times on unmodified code run right after it, then 10/10 clean on *both* configurations once the
system settled from the preceding load-test runs — a pre-existing, low-frequency test-timing
sensitivity (this test's before/after process-count snapshots have no settle-time margin) that
exists independent of these flags, not a real regression. Worth hardening later, not urgent now.

**Current state**: neither flag is in the codebase. `chromium.go`'s launch flags are unchanged from
before this experiment (`chromedp.DefaultExecAllocatorOptions` + `ExecPath` + `NoSandbox` only).

- **`--in-process-gpu`** (not tried — the research below already flagged it as regressed on
  specific Chromium versions) and **Gotenberg's per-render user-data-dir cleanup pattern** (not
  directly applicable — our architecture uses long-lived per-slot processes, not per-request
  spawning, so this doesn't transfer the way it does for Gotenberg's model) remain unactioned; see
  their original research below for context if revisited later.

No credible, methodology-disclosed case study with real before/after memory numbers exists publicly
for any of this — several blog posts claim round numbers ("40-60% memory savings") with no shown
benchmark and should be disregarded as unverified content, not cited as evidence. Our own measured
numbers above are the trustworthy ones for this specific workload.

## 3. A Chromium-free path for JS-free documents — real option, but not recommended yet

Investigated whether documents with no `<script>`/JS could skip Chromium entirely via a lighter
engine. Findings, stated plainly rather than oversold:

- **No production-ready pure-Go CSS layout engine exists.** The one candidate found
  ([carlos7ags/folio](https://github.com/carlos7ags/folio)) is ~6 weeks old, two contributors, and
  its own docs admit unsupported CSS is "silently ignored at render time — there is no warning." Not
  safe to depend on for customer-facing output.
- **WeasyPrint** has a real memory advantage (~60MB baseline vs. our ~300-500MB+ per Chromium
  instance) but it isn't bounded the way that implies — memory scales with document complexity (a
  5,000+ row table case hit 1.4GB+), it has its own worker-recycling requirement (same operational
  burden as Chromium, smaller numbers), and it's markedly *slower* per render (~629ms vs. our
  13-58ms warm-instance numbers on comparable documents).
- **The real blocker is accuracy risk, not resource cost.** WeasyPrint has confirmed, open divergences
  from Chromium on exactly the CSS patterns real invoice/report templates use — flexbox items
  failing to break across pages correctly, content silently disappearing off the page bottom
  ([Kozea/WeasyPrint#2076](https://github.com/Kozea/WeasyPrint/issues/2076)), incorrect nested-flex
  height computation ([#2222](https://github.com/Kozea/WeasyPrint/issues/2222)), partial CSS Grid
  support. The failure mode is **silent** — the render "succeeds," a table row just splits at the
  wrong page boundary — which is a customer support ticket, not a caught test failure.
- **No industry precedent exists for this exact pattern.** Every named competitor
  (cloudlayer.io, DocRaptor, PDFShift) uses one engine for everything. Gotenberg/api2pdf expose
  multiple engines, but the *caller* picks by document *type* (HTML vs. Office doc) — never by
  automatic content-complexity detection between two HTML engines behind one endpoint. This would be
  a genuinely novel architecture bet, which raises the bar of proof needed, not lowers it.

**Recommendation: don't build this now.** The honest path to ever doing this safely runs through the
golden-file visual-regression suite already planned for `customization-layer` in
`SPEC-render-engines.md` — only once that exists could a "route by complexity" strategy be validated
per-template before shipping, and even then it should be an explicit allowlist per verified template,
never a blanket heuristic. Revisit if/when there's a concrete cost pressure that justifies that
validation investment.

## 4. Container/storage footprint — mostly already right, a few concrete numbers

- **`chrome-headless-shell` is already the right binary choice** — real numbers: ~86-90MB for the
  shell alone vs. ~170-280MB for full Chromium; a complete runnable reference container
  (`chromedp/headless-shell` on Docker Hub) is **~143MB compressed**. We're already ahead of
  Gotenberg here — Gotenberg's own image is 668MB (480MB in its Chromium-only variant) because it
  `apt-get install`s Debian's full desktop `chromium` package rather than the Chrome-for-Testing
  headless_shell artifact.
- **Locale-pak stripping is safe but small** — locale files hold browser-chrome UI strings, not
  page-rendering data (Unicode shaping comes from `icudtl.dat`, untouched by locale stripping), so
  it's safe for a UI-less renderer. But the realistic win is single-digit MB (Electron precedent:
  70MB→64MB stripping ~150 locale files) — and tellingly, **neither Gotenberg nor the reference
  `chromedp/headless-shell` image bothers doing this**, because it's not where the real size lives.
- **Alpine/musl doesn't work with Google's official prebuilt binaries** (long-documented breakage,
  [puppeteer#1793](https://github.com/puppeteer/puppeteer/issues/1793)) — every real project
  (Gotenberg, chromedp, Playwright's own Docker images) uses glibc-based Debian/Ubuntu slim instead.
  Settled; don't chase Alpine here.
- **Fonts are the real size cost, and our own spec already implies paying it.** Gotenberg's font
  trio (Noto core + CJK + emoji) costs **~144MB installed** — comparable to the entire
  headless-shell binary's own size. `SPEC-render-engines.md`'s Testing Strategy already commits to
  CJK/RTL/emoji as first-class golden-file fixtures, which means we've already implicitly decided
  broad font coverage matters — so this ~144MB is a cost we should knowingly accept, not an
  optimization target to cut, unless that CJK/RTL scope gets deliberately narrowed first.

## Summary: what to actually do

| Item | Verdict | Confidence |
|---|---|---|
| Keep `chrome-headless-shell` (not full Chromium) | Already correct, no change needed | High — real size numbers confirm it |
| Add `--no-zygote`, `--disable-gpu` to `chromium.go`'s launch flags | **Tried, measured, reverted** — both regressed throughput/latency ~15-20% on our image-heavy workload with no offsetting memory win (see §2's measured ledger) | Corrected — "low-risk" was wrong for this workload |
| Adopt Gotenberg's per-render user-data-dir + cleanup pattern | Doesn't transfer — that pattern fits per-request browser spawning, not our long-lived per-slot process model | Not applicable to our architecture |
| Collapse pool to 1 shared browser process (my earlier suggestion) | **Judgment call, not a clean win** — real efficiency-vs-blast-radius tradeoff, both sides have production precedent | Corrected — needs your decision, not auto-applied |
| Strip locale `.pak` files | Skip for now — real but marginal, no reference implementation bothers | Medium |
| Build a Chromium-free path for JS-free documents | Don't build yet — real accuracy risk, no industry precedent, needs golden-file testing infra first | High confidence in the "not yet" |
| Bundle broad (Noto-style) font coverage | Keep — already implied by our own CJK/RTL/emoji testing commitment | High |

Sources are inlined above; the three full background-research reports this synthesizes covered
Chromium process-model minimization, Chromium-free rendering alternatives, and container/storage
footprint — available in this session's history if deeper citation-chasing is needed later.
