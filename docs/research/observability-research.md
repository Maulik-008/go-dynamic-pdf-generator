# Deep-Dive Research: Production Observability for the Conversion API

Research input for the `observability` module in `docs/planning/CAPABILITY-MAP.md` ("Metrics (RED:
rate/errors/duration), structured logging, tracing, health/readiness probes"). Scope is deliberately
narrower than that capability line: **logging, health probes, RED metrics, request-logging
middleware, and correlation IDs.** Distributed tracing is explicitly out of scope here — it is the
one part of the capability that genuinely cannot be done stdlib-only, and it should get its own
decision once there is a second service to correlate with.

This document contains **no implementation code by design** — it is research and a recommendation.

---

## 0. Method, and an honest note about source access

Every claim below that is quoted was read directly from a primary source during this session.
A number of the usual high-trust sources were **unreachable from this sandbox** (the egress proxy
blocked `go.dev`, `kubernetes.io`, `prometheus.io`, `sre.google`, `aws.amazon.com`,
`opentelemetry.io`, `grafana.com`, `w3.org`). Where that happened I read the *same content from its
source repository* on `raw.githubusercontent.com` — e.g. the Kubernetes docs from
`kubernetes/website`, the Prometheus docs from `prometheus/docs`, the Go blog from
`golang/website`, and Go itself from `golang/go`. Those are the canonical sources for the rendered
sites, so the citations are equivalent; I've given the canonical URL plus the repo path.

Three well-known secondary sources on the readiness debate (Colin Breck's "How to Avoid Shooting
Yourself in the Foot", the AWS Builders' Library "Implementing health checks", the Google SRE Book
chapter on cascading failures) could **not** be fetched. I have deliberately **not** quoted them,
and the argument in §2 stands on sources I did read: the Kubernetes docs, Envoy's own
documentation, and an open Kubernetes issue. Treat any half-memory of those three as unverified.

---

## 1. `log/slog` — structured logging in the stdlib

### 1.1 What the stdlib actually gives you

`log/slog` landed in Go 1.21 and is a complete production logging solution: levels, structured
key/value attributes, a JSON handler, context plumbing, and a documented `Handler` extension point.
For this codebase (`go 1.26`) there is **no gap that justifies zerolog/zap**. Verdict up front:
**stdlib is genuinely sufficient here, with no asterisk.**

The recommended production shape is small:

- `slog.NewJSONHandler(os.Stdout, opts)` — line-delimited JSON, one object per record. The docs
  describe `NewJSONHandler` as writing "line-delimited output", which is what every log shipper
  (Fluent Bit, Vector, CloudWatch, Loki) expects.
- `HandlerOptions.Level` takes a `Leveler`. Use a **`slog.LevelVar`**, not a bare `slog.Level`:
  `LevelVar` is "a thread-safe Level holder" whose `Set` changes the minimum level "dynamically
  across all goroutines using that handler instance." That is the mechanism for a `LOG_LEVEL` env
  var, and later for a debug-toggle endpoint, without a restart.
- `HandlerOptions.AddSource` adds file/line. It costs a `runtime.CallersFrames` lookup per record;
  enable it for `Error` in production, or globally in dev.
- `HandlerOptions.ReplaceAttr` rewrites or drops attributes before output — the redaction hook.
- `slog.SetDefault(logger)` once in `main`.

### 1.2 `SetDefault` is the single biggest lever for *this* repo

From `log/slog`'s own source:

> "SetDefault makes l the default Logger, which is used by the top-level functions Info, Debug and
> so on. After this call, output from the log package's default Logger (as with log.Print, etc.)
> will be logged using l's Handler, at a level controlled by SetLogLoggerLevel."

`cmd/api/main.go` currently has **eleven** `log.Printf` / `log.Print` / `log.Fatalf` calls. One
`slog.SetDefault` call converts every one of them into a structured JSON record with **zero edits to
those lines** — the message becomes the `msg` field. That means the migration can be staged: set the
default handler first, get JSON everywhere immediately, then convert individual call sites to
attributes as they're touched. Nothing has to be rewritten in a big bang.

Two caveats on that:

- **Before** `SetDefault` is called, it goes the other way: "slog top-level logging functions call
  the default `log.Logger`", and the minimum level in that mode is Info, so `slog.Debug` before
  `SetDefault` silently disappears unless `SetLogLoggerLevel(slog.LevelDebug)` is called first.
  Practical consequence: **call `SetDefault` as the very first statement in `main`**, before the
  `CHROMIUM_PATH` check — otherwise the `log.Fatal` on line 25 escapes as unstructured text.
- `log.Fatalf` still calls `os.Exit(1)`. It will now be structured, but it is still an
  un-deferrable exit. That's fine for the startup path in `main` (it already is), but the new
  logging middleware must never use it.

### 1.3 Request-scoped attributes: the two real patterns

There are exactly two idiomatic approaches and they are not equivalent.

**(a) Logger-in-context.** Middleware builds `logger := slog.Default().With("request_id", id,
"method", r.Method)` and stores the `*slog.Logger` in the request context under an unexported key;
handlers pull it out. This is the simplest correct thing, and it is fast: the docs note that
`WithAttrs`/`WithGroup` "let the handler format attributes added by `Logger.With` once, rather than
at each logging call, which can provide a significant speedup." Downside: every function that wants
to log needs the logger threaded to it or needs a `LoggerFrom(ctx)` helper.

**(b) Attrs-in-context + a wrapping `Handler`.** Middleware puts *attributes* in the context; a
custom `Handler` implementation reads them out in `Handle(ctx, r)` and appends them. The `Handler`
docs say the context is "present solely to provide Handlers access to the context's values" — this
is literally what it is for. Downside: you must implement `Handler` correctly.

**The gotcha that bites everyone with (b):** the non-`Context` methods do not pass your context.
`Logger.Info(...)` uses `context.Background()` internally; only `InfoContext`, `WarnContext`,
`ErrorContext`, `DebugContext`, `Log`, and `LogAttrs` carry a real ctx. If you build a
context-reading handler and then call `slog.Info` anywhere in a request path, your request_id
silently vanishes from that line. There is no compile error and no vet check for it.

The second gotcha with (b): a wrapping handler must return **its own wrapper** from `WithAttrs` and
`WithGroup`. Returning `h.Handler.WithAttrs(attrs)` unwraps you, and every subsequent call on that
derived logger loses the context behaviour. The `Handler` doc contract also requires: ignore zero
`r.Time` and zero `r.PC`, resolve `Attr` values, ignore an `Attr` where key and value are both zero,
inline groups with empty keys, and ignore empty groups. `testing/slogtest` exists specifically to
verify a custom handler against that contract — use it if you write one.

**Recommendation for this repo: start with (a).** The handler set is four routes and the call depth
is shallow (`handleConversion` → `submit` → `renderFn`). Pattern (b)'s payoff is in deep call trees
where threading a logger is painful; that isn't the situation yet. Revisit if `internal/renderengines`
starts wanting to log per-render.

### 1.4 The other real gotchas

- **`"!BADKEY"`.** With the alternating key/value form, "an odd count causes the unpaired value to
  receive the key `!BADKEY`". The Go blog defends the alternating syntax and says the team relies on
  "a vet check to catch common mistakes with omitted keys or values" — so `go vet` (already in CI
  presumably) covers the common case, but not dynamic slices of args. Using `slog.String(...)`,
  `slog.Int(...)` typed attrs sidesteps it entirely.
- **Errors in JSON: handled, but only one level deep.** This is the gotcha most people get wrong by
  assuming `encoding/json` semantics. `JSONHandler` special-cases errors:

  ```go
  case KindAny:
      a := v.Any()
      _, jm := a.(json.Marshaler)
      if err, ok := a.(error); ok && !jm {
          s.appendString(err.Error())
      } else {
          return appendJSONMarshal(s.buf, a)
      }
  ```

  with the documented rule: *"an Attr whose Value is of type error is formatted as a string, by
  calling its Error method. Only errors in Attrs receive this special treatment, not errors embedded
  in structs, slices, maps or other data structures that are processed by the encoding/json
  package."* So `slog.Any("error", err)` is correct and gives the message; `slog.Any("result",
  struct{ Err error }{err})` gives `{"Err":{}}`. Relevant here because `writeRenderError` handles
  wrapped errors (`errors.Is(err, orchestration.ErrQueueFull)`) — log the error as its own attr.
- **Handler errors are swallowed.** "`Logger` discards any errors from `Handle`." If stdout is a
  full pipe, records are lost silently. Also, "an encoding failure does not cause `Handle` to return
  an error. Instead, the error message is formatted as a string." Acceptable, but don't build
  anything that assumes a log line is a durable side effect (e.g. don't use logs as the billing
  record for `auth-and-tenancy` metering).
- **Concurrency.** "Handlers must manage their own concurrency; methods may be called concurrently
  with each other. Built-in handlers lock before calling `io.Writer.Write` to ensure atomic record
  output." The built-ins are safe. A hand-rolled handler writing to a shared `io.Writer` without a
  mutex will interleave records under this service's concurrent render load. Also: "`Record` copies
  share state. Do not modify a `Record` after sharing it" — use `Record.Clone()`.
- **`WithGroup("")` is a no-op** and returns the receiver, so a computed group name that comes out
  empty silently flattens your namespace rather than erroring.

**Sources (§1):**
[Structured Logging with slog, Go Blog](https://go.dev/blog/slog) (read via
`golang/website@master:_content/blog/slog.md`) ·
[pkg.go.dev/log/slog](https://pkg.go.dev/log/slog) ·
[`golang/go@master:src/log/slog/logger.go`](https://github.com/golang/go/blob/master/src/log/slog/logger.go) ·
[`golang/go@master:src/log/slog/handler.go`](https://github.com/golang/go/blob/master/src/log/slog/handler.go) ·
[`golang/go@master:src/log/slog/json_handler.go`](https://github.com/golang/go/blob/master/src/log/slog/json_handler.go)

---

## 2. Liveness vs readiness vs startup — and the capacity question

### 2.1 The actual semantics, per Kubernetes

Straight from the Kubernetes docs (`kubernetes/website@main:content/en/docs/concepts/workloads/pods/probes.md`):

| Probe | Definition (verbatim) | Failure consequence |
|---|---|---|
| **liveness** | "Liveness probes determine when to restart a container. For example, liveness probes could catch a deadlock, where an application is running, but unable to make progress." | kubelet **kills and restarts** the container |
| **readiness** | "Readiness probes determine when a container is ready to accept traffic. This is useful when waiting for an application to perform time-consuming initial tasks, such as establishing network connections, loading files, and warming caches." | Pod's EndpointSlice entry goes `ready: false`; **traffic is withdrawn**, container keeps running |
| **startup** | "Startup probes verify whether the application within a container is started. If a startup probe is configured, Kubernetes does not execute liveness or readiness probes until the startup probe succeeds." | container restarted, but **suppresses liveness/readiness entirely until it passes** |

Two structural facts that people get wrong:

1. **They are independent.** "The readiness and liveness probes do not depend on each other to
   succeed. If you want to wait before executing a readiness probe, you should use
   `initialDelaySeconds` or a `startupProbe`."
2. **Readiness runs forever.** "Readiness probes run on the container during its whole lifecycle" —
   it is not a startup-only gate. This is exactly why the capacity question in §2.3 is live.

The docs carry an explicit caution on liveness:

> "Liveness probes can be a powerful way to recover from application failures, but they should be
> used with caution. Liveness probes must be configured carefully to ensure that they truly indicate
> unrecoverable application failure, for example a deadlock. **Incorrect implementation of liveness
> probes can lead to cascading failures.**"

And a practical one for the `httpGet` mechanism: "the kubelet stops reading the response body after
10KiB. The probe's success is determined solely by the response status code." So a health endpoint's
body is for humans only; never encode status in it.

Guidance on whether you need a liveness probe at all is more permissive than folklore suggests: if
the process crashes on its own when it's broken, `restartPolicy` already handles it and a liveness
probe adds only risk. The docs' recommended liveness pattern is to reuse "the identical low-cost HTTP
endpoint used for readiness checks with an elevated `failureThreshold`."

### 2.2 The uncontroversial correct behaviours

- **Liveness must not check dependencies.** A liveness probe that checks Chromium, WeasyPrint, or a
  future Redis turns a dependency blip into a cluster-wide restart storm — the documented cascading
  failure. Liveness answers exactly one question: *is this process still able to run its own event
  loop?* A handler that returns 200 unconditionally answers that, because if the goroutine scheduler
  or the accept loop were wedged, the probe would time out.
- **Readiness must fail during shutdown.** This is the highest-value readiness use and it is
  currently missing. On `SIGTERM` the endpoint controller and `httpServer.Shutdown` race: the docs
  note terminating Pods get `ready: false` so "load balancers will not use it for regular traffic",
  but propagation to every kube-proxy/ingress is asynchronous. Flipping readiness to fail *first*,
  sleeping briefly, then calling `Shutdown` closes that window. There is a nuance worth knowing: the
  docs say "If you want to be able to drain requests when the Pod is deleted, you do not necessarily
  need a readiness probe; when the Pod is deleted, the corresponding endpoint in the EndpointSlice
  will update its conditions." So this is belt-and-braces against slow propagation, not strictly
  mandatory — but it costs a boolean.
- **Startup probes are for slow starts.** `renderengines.NewPool` starts every Chromium instance
  *before* `httpServer.ListenAndServe` runs (`main.go` lines 31–39 precede line 97). During warmup
  the port isn't even open, so **every probe fails with connection-refused**, not with a bad status.
  That is precisely the case a startup probe exists for: set
  `failureThreshold × periodSeconds` above worst-case pool warmup, and liveness/readiness stay
  suppressed until then. Without one, a `POOL_SIZE` large enough to make warmup exceed the liveness
  budget produces an infinite restart loop that looks like a crash.

### 2.3 The contested one: should readiness fail when the service is merely at capacity?

This is the question that matters most for this codebase, because `orchestration.Pool` already knows
exactly when it is at capacity and already exposes `QueueDepth()`/`QueueCapacity()`. It is genuinely
argued both ways.

**The case FOR (readiness as backpressure).** Kubernetes' own definition — "ready to accept traffic"
— is a statement about *now*, not about *startup*, and the docs confirm readiness runs for the whole
lifecycle. A pod whose queue is full genuinely cannot accept traffic. Failing readiness pushes the
decision up to the load balancer, which can route to a pod that *can* serve, instead of every
overloaded pod burning CPU generating 503s. For queue-consuming workers with no HTTP client to
receive a 503, this is essentially the only backpressure channel available. And the platform spec
already commits to KEDA queue-depth autoscaling — capacity-aware readiness feels like the same idea
one layer down.

**The case AGAINST (readiness-on-load is a documented cascading-failure generator).** The mechanism
is mechanical and unavoidable: N pods, all near capacity. One tips over, fails readiness, and is
removed from the Service. Its share of traffic redistributes to the remaining N−1, pushing them
further over. The next one falls. This is a positive feedback loop that converts a *degradation*
into a *total outage*, and it is worse than the overload it was trying to fix, because a 503 with
`Retry-After` at least serves some traffic while an empty EndpointSlice serves none.

The strongest evidence that this is a real, recognised failure mode is that **Envoy built a specific
feature to defeat it**, and Kubernetes has not. From Envoy's own docs
(`envoyproxy/envoy@main:docs/root/intro/arch_overview/upstream/load_balancing/panic_threshold.rst`):

> "During load balancing, Envoy will generally only consider available (healthy or degraded) hosts
> in an upstream cluster. However, if the percentage of available hosts in the cluster becomes too
> low, Envoy will disregard health status and balance either amongst all hosts or no hosts."

> "**The panic threshold is used to avoid a situation in which host failures cascade throughout the
> cluster as load increases.**"

Envoy's default panic threshold is **50%**. Below that fraction of healthy hosts, Envoy deliberately
sends traffic to hosts marked unhealthy, on the reasoning that a degraded backend beats no backend.

**Kubernetes has no equivalent.** [kubernetes/kubernetes#115917, "Panic Threshold for Readiness
Probe Failures"](https://github.com/kubernetes/kubernetes/issues/115917) is an open request for
exactly this, explicitly citing Envoy as prior art and describing the cascade: traffic spike →
readiness failures → pods removed → remaining pods overloaded → total outage. The requester
acknowledges "the feature has a lot of moving parts though i.e it shouldn't trigger during normal
scenarios like deployments, rotating the pods of a service, interruptions in the underlying ec2."
It has not been adopted.

**So the asymmetry is decisive:** in Envoy you can make capacity-based health signals safe. In plain
Kubernetes Services you cannot — kube-proxy will happily drain a Service to zero ready endpoints and
the result is connection failures at the ingress, not graceful degradation.

**Conclusion.** Readiness should **not** fail on queue depth / capacity in this service. And there
is a much better reason than the general argument: *this service already does the right thing.*
`orchestration.Submit` returns `ErrQueueFull` without blocking, and `writeRenderError` turns it into
`503 QUEUE_FULL` with a `Retry-After` header. **That is load shedding done correctly** — per-request,
immediate, with a client-actionable signal, and without withdrawing the pod. Layering readiness
failure on top would replace a good mechanism with a worse one.

Narrow conditions under which I'd change this answer, stated so the decision is falsifiable:
1. The service moves behind Envoy/Istio with a panic threshold configured, **or**
2. It grows an async/queue-consumer mode (`storage-and-delivery` in the capability map) where there
   is no synchronous client to receive a 503, **and**
3. Even then: with hysteresis (unready above X% for N consecutive probes, ready again below a
   *lower* Y%) and a floor on minimum ready replicas.

The autoscaling signal the platform spec wants belongs in **metrics**, not in the readiness probe.
Those are different channels with different failure modes, and conflating them is the root of the
anti-pattern.

**Sources (§2):**
[Liveness, Readiness, and Startup Probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/)
(read via `kubernetes/website@main:content/en/docs/concepts/workloads/pods/probes.md`) ·
[Configure Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
(same repo, `content/en/docs/tasks/configure-pod-container/…`) ·
[Pod Lifecycle: Termination of Pods](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
(same repo) ·
[Envoy: Panic threshold](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/panic_threshold)
(read via `envoyproxy/envoy@main:docs/root/…/panic_threshold.rst`) ·
[kubernetes/kubernetes#115917](https://github.com/kubernetes/kubernetes/issues/115917)

---

## 3. RED metrics: canonical definition, and how much dependency they actually cost

### 3.1 The definition

**RED = Rate, Errors, Duration**, coined by Tom Wilkie (Weaveworks, later Grafana Labs), as the
request-centric counterpart to Brendan Gregg's resource-centric USE method and a narrowing of
Google's four golden signals (latency, traffic, errors, saturation) to the three that apply to every
request-serving service:

- **Rate** — requests per second the service is handling
- **Errors** — the rate of *failed* requests
- **Duration** — the distribution of time those requests take

*Caveat on sourcing:* `grafana.com` was unreachable from this sandbox, so the canonical RED blog
post is cited but not quoted. The definition above is corroborated by the Prometheus project's own
instrumentation guidance, which was read directly and says the same thing in different words:

> "The key metrics in such a system are the number of performed queries, errors, and latency. The
> number of in-progress requests can also be useful."

with an important consistency rule:

> "Be consistent in whether you count queries when they start or when they end. When they end is
> suggested, as it will line up with the error and latency stats, and tends to be easier to code."

The OpenTelemetry semantic conventions give the modern canonical shape for the Duration part:
`http.server.request.duration`, a **histogram in seconds**, with required attributes
`http.request.method` and `url.scheme`, conditionally-required `http.response.status_code`,
`error.type`, and `http.route`. Recommended bucket boundaries are
`[0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10]` seconds.

**Those default buckets are wrong for this service** and that is worth calling out. Load testing
(`docs/research/load-test-results.md`) puts Chromium renders in the seconds range with a 30s default
budget and a 40s `WriteTimeout`. With OTel's defaults, essentially every render lands in the
`+Inf` bucket and p95 is unresolvable. Buckets must be chosen against the actual latency profile —
something like `[0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 13, 21, 34]` — and, because the two engines have very
different profiles, the `engine` dimension matters more than the method dimension.

### 3.2 What "Errors" means here, specifically

A naive `errors = non-2xx` is actively misleading for this API. The existing error taxonomy in
`internal/api/handlers.go` splits cleanly into two populations:

| Code | Status | Whose fault | Belongs in the RED error rate? |
|---|---|---|---|
| `INVALID_REQUEST` | 400 | client | no (client bug) |
| `REQUEST_TOO_LARGE` | 413 | client | no |
| `TEMPLATE_ERROR` | 422 | client's template | no |
| `RENDER_ERROR` | 422 | client's *document* — usually | mostly no, but this is where engine faults hide |
| `QUEUE_FULL` | 503 | **us — saturation** | yes, and it is the saturation signal |
| `NOT_CONFIGURED` | 503 | deployment misconfig | yes, but it's constant-on, so alert once |

`RENDER_ERROR` is the ambiguous one: `writeRenderError` collapses *"your HTML is broken"* and
*"Chromium died"* into the same 422. That is an existing gap this research surfaces — the RED error
rate can't distinguish a customer sending bad input from the render pool degrading. **Recommendation:
label the error counter by `code`, and treat separating engine faults from document faults inside
`RENDER_ERROR` as a follow-up in `render-engines`, not something metrics can paper over.**

Also note `Submit` returns `ctx.Err()` on client disconnect, which currently falls through to
`RENDER_ERROR` 422 — writing to a closed connection. Client cancellations should be their own
outcome label, not counted as errors.

### 3.3 Option A — structured access logs, zero dependencies

The RED numbers are all present in a single well-formed access-log line: a record per request with
`route`, `status`, `code`, `engine`, `duration_ms`. Rate is a count over time, errors are a filtered
count, duration percentiles come from the aggregator.

**This costs nothing beyond the middleware you need anyway** and works with every log backend
(CloudWatch Logs Insights, Loki+LogQL, Datadog, Elastic). Its real limits: percentile queries over
raw logs get expensive at volume; retention is shorter than metrics retention; and it is a poor fit
for a fast autoscaling control loop. For a v1 single-node deployment it is, honestly, enough.

### 3.4 Option B — `expvar`: what it really gives you, and what it doesn't

`expvar` is stdlib and is often reached for as "the stdlib metrics package." That framing oversells
it. What it actually is: **a JSON dump of named variables at `/debug/vars`.**

What you get: `Int`, `Float`, `String`, `Map`, `Func` types, with atomic mutation ("operations for
modifying public variables are atomic"), published via `Publish(name, v)`, served by `Handler()`.

The real limitations, in order of how much they matter here:

1. **No histogram, and no percentiles.** There is no bucketed type. Duration — the D in RED — has to
   be hand-rolled on top of `expvar.Map` with one `Int` per bucket, at which point you have written a
   histogram anyway and are just serialising it as JSON instead of as Prometheus text.
2. **No dimension/label model.** `expvar.Map` nests, so `renders{engine,code}` becomes
   `renders → chromium → RENDER_ERROR → 3`. Workable, but every consumer needs bespoke parsing.
3. **Nothing scrapes JSON natively.** Prometheus cannot ingest `/debug/vars`. The bridge is
   `prometheus/client_golang`'s `collectors.NewExpvarCollector` — which means if the destination is
   Prometheus, expvar buys you nothing and you take the dependency anyway. (The older
   `prometheus.NewExpvarCollector` is deprecated, and its own godoc example is candidly labelled
   "Not a good role-model, actually…".)
4. **Side-effect registration is a footgun.** Importing `expvar` runs an `init` that does
   `http.HandleFunc("GET /debug/vars", expvarHandler)` on `http.DefaultServeMux`, and publishes
   `cmdline` (i.e. `os.Args`) and `memstats`. This repo is safe from accidental exposure — `Routes()`
   builds its own `http.NewServeMux()` and `DefaultServeMux` is never served — but it means expvar's
   endpoint must be *deliberately* mounted, and `cmdline` should be considered before doing so:
   `CHROMIUM_PATH` and `WEASYPRINT_PATH` are harmless, but the pattern leaks argv, and
   `auth-and-tenancy` may later put secrets there.

**But there is one genuinely strong use for expvar in this specific project**, and it changes the
calculus. The platform spec commits to "Kubernetes + KEDA (queue-depth-based autoscaling)". KEDA has
two relevant scalers:

- The **`prometheus` scaler** does *not* scrape your app — it needs `serverAddress` plus a PromQL
  `query`, i.e. a full Prometheus server in the cluster.
- The **`metrics-api` scaler** hits your app's HTTP endpoint directly and supports
  `format: json | xml | yaml | prometheus`, with `valueLocation` using **GJSON path notation** for
  JSON.

So a bare `expvar.Publish("queue_depth", expvar.Func(...))` reading `pool.QueueDepth()`, served on an
internal port, is **directly consumable by KEDA with zero dependencies and no Prometheus server.**
For the one metric the spec says must drive scaling, stdlib is not a compromise — it is sufficient.

### 3.5 Option C — hand-write the Prometheus text exposition format

Worth taking seriously rather than dismissing. The format is simple and the Prometheus docs
themselves describe it as "easy to assemble, especially for minimalistic cases (no nesting
required)". The full contract you must honour:

- `Content-Type: text/plain; version=0.0.4`
- lines separated by `\n`, **final line must have a trailing `\n`**
- optional `# HELP <name> <docstring>` and `# TYPE <name> counter|gauge|histogram|summary|untyped`
- "All lines for a given metric must be provided as one single group, with the optional HELP and
  TYPE lines first"
- in label values and docstrings, `\`, `"`, and `\n` must be escaped as `\\`, `\"`, `\n`
- plus naming rules: `_total` suffix for counters, **base units — seconds, not milliseconds**, single
  application prefix
- plus, for a histogram, `_bucket{le="…"}` with a mandatory `le="+Inf"` bucket, `_sum`, and `_count`

That is real but bounded: a counter vec, a gauge, and a fixed-bucket histogram over `atomic.Int64`
is a few hundred lines including tests, and the metric surface here is small and *fixed* (see §6).

What you give up: exemplars, native/sparse histograms, OpenMetrics, the Go runtime collector (though
`runtime/metrics` is stdlib and a handful of series can be mapped by hand), and — most importantly —
the library's guardrails. `client_golang` panics on duplicate registration, enforces label
cardinality consistency, and gets the escaping right. Home-grown code gets those wrong quietly.

### 3.6 Option D — `prometheus/client_golang`: the actual cost

Concretely, from `prometheus/client_golang@main:go.mod`, it pulls **12 direct** and **12 indirect**
modules, including `google.golang.org/protobuf`, `golang.org/x/oauth2`, `golang.org/x/net`,
`klauspost/compress`, `json-iterator/go`, `modern-go/reflect2`, `prometheus/common`,
`prometheus/procfs`, and `golang-jwt/jwt/v5`.

For a repo whose entire dependency tree today is chromedp + goldmark + six indirects, that is
roughly a 4× increase in module count and drags in a JWT library and an OAuth2 library that this
service has no use for. Under this project's stated "**Ask first:** adding a new external
dependency" rule, that is a decision to be made explicitly, not absorbed.

To be fair to it: what you buy is not the exposition format (that's trivial) — it's **correctness
guardrails and ecosystem fit**. That is worth a lot once more than one person is adding metrics, and
worth relatively little while the metric surface is six series families defined in one file.

**Sources (§3):**
[The RED Method (Grafana Labs)](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/) *(cited, not fetched — domain blocked)* ·
[Prometheus: Instrumentation best practices](https://prometheus.io/docs/practices/instrumentation/) (read via `prometheus/docs@main:docs/practices/instrumentation.md`) ·
[Prometheus: Metric and label naming](https://prometheus.io/docs/practices/naming/) (same repo) ·
[Prometheus: Histograms and summaries](https://prometheus.io/docs/practices/histograms/) (same repo) ·
[Prometheus: Exposition formats](https://prometheus.io/docs/instrumenting/exposition_formats/) (same repo) ·
[OTel semantic conventions: HTTP metrics](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/) (read via `open-telemetry/semantic-conventions@main:docs/http/http-metrics.md`) ·
[pkg.go.dev/expvar](https://pkg.go.dev/expvar) and [`golang/go@master:src/expvar/expvar.go`](https://github.com/golang/go/blob/master/src/expvar/expvar.go) ·
[KEDA metrics-api scaler](https://keda.sh/docs/2.17/scalers/metrics-api/) and [KEDA Prometheus scaler](https://keda.sh/docs/2.17/scalers/prometheus/) (read via `kedacore/keda-docs@main`) ·
[`prometheus/client_golang@main:go.mod`](https://github.com/prometheus/client_golang/blob/main/go.mod)

### 3.7 A codebase-specific finding: `QueueDepth()` alone is not a saturation signal

`Pool.QueueDepth()` returns `len(p.jobs)` — jobs that were admitted but not yet picked up by a
worker. That is a correct *queue* depth. But it reads **0** in two completely different states:

- genuinely idle, and
- every worker busy, queue momentarily empty — i.e. 100% utilised and one arrival away from rejecting.

The saturation number is `len(p.admission)` (running **plus** queued) against
`cap(p.admission)` = `Workers + QueueCapacity`. That is not currently exposed. Adding an
`InFlight()` accessor is a small additive change to `internal/orchestration` and gives a utilisation
ratio that is a far better autoscaling and alerting signal than queue depth alone — and it would
also make the KEDA target in §3.4 meaningful, since scaling on a number that is usually 0 is a
scaling policy that acts only after the queue is already backing up.

`QueueCapacity()` is already exposed and is a static config value — publish it once as an `_info`-ish
gauge, not per-scrape.

---

## 4. Request-logging middleware: wrapping `http.ResponseWriter`

### 4.1 The idiomatic pattern

Capture status and duration by wrapping the `ResponseWriter`: record `time.Now()`, call
`next.ServeHTTP(wrapped, r)`, then log status, byte count, and `time.Since(start)`.

Two details from the stdlib docs that the wrapper must get right:

- **Default to 200.** "If WriteHeader isn't explicitly called, the first Write implicitly calls it
  with status 200." A wrapper initialised to `status: 0` will log `0` for every successful response
  where the handler only called `Write`. Initialise to `http.StatusOK`.
- **Record only the first `WriteHeader`.** "WriteHeader sets the HTTP status code and headers, but
  may only be called once. Subsequent calls are ignored." The stdlib also emits a `superfluous
  response.WriteHeader call` warning to the server's `ErrorLog`. A wrapper that overwrites its
  recorded status on every call will report the *last* status while the client received the *first*.

For this codebase specifically: `writePDF` and `writeJSONError` both call `WriteHeader` exactly once,
so double-write isn't a live bug — but `writeJSONError` calls `json.NewEncoder(w).Encode(...)` after
`WriteHeader`, and an encode failure there cannot change the already-sent status. Its return value is
currently discarded; the middleware is the place to notice that the response was truncated (bytes
written < expected), not the error path.

### 4.2 The classic pitfall: swallowing optional interfaces

`http.ResponseWriter` is a small interface, but the concrete value the stdlib passes in also
implements several optional ones. From the `httpsnoop` README (the package that exists solely because
of this problem):

> "a naive approach of just wrapping `http.ResponseWriter` in your own struct that also implements
> the `http.ResponseWriter` interface will hide the additional interfaces mentioned above" —
> `http.Flusher`, `http.CloseNotifier`, `http.Hijacker`, `http.Pusher`, `io.ReaderFrom`.

It also warns that faking them is worse than hiding them, since "applications may operate differently
simply by detecting the presence of these interfaces." The classical fix was the combinatorial
horror of generating one wrapper type per subset of implemented interfaces.

**As of Go 1.20 you should not do any of that.** The Go proposal that introduced
`http.ResponseController` ([golang/go#54136](https://github.com/golang/go/issues/54136)) states the
problem directly — optional methods on `ResponseWriter` are "undiscoverable magic methods" that
"scale poorly and do not interact well with middleware which wraps the `ResponseWriter` type" — and
its answer is a convention:

> "Middleware wrapping `ResponseWriter` should implement an `Unwrap()` method returning the wrapped
> writer. This allows ResponseController to navigate through wrapper layers and locate the actual
> implementation supporting specific operations."

So the correct modern wrapper is: embed/hold the original, record status and bytes, and add
`func (w *wrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }`. Handlers then reach
flush/hijack/deadlines through `http.NewResponseController(w)`, which walks the `Unwrap` chain and
returns `http.ErrNotSupported` if nothing in the chain provides the capability.

**This service doesn't flush or hijack today** — `writePDF` sets `Content-Length` and does one
`w.Write(pdf)`. But the platform spec's own Development Rules say "**Always:** … stream PDF responses
rather than buffer in memory", and streaming is exactly what needs `Flusher` and per-request
`SetWriteDeadline` (the 40s `WriteTimeout` in `main.go` is currently global). Adding `Unwrap()` is
three lines and is the difference between the streaming work being straightforward later and being a
debugging session.

### 4.3 Getting a low-cardinality `route` label from the stdlib

Verified in the Go source (`golang/go@master:src/net/http/request.go`):

```go
// Pattern is the [ServeMux] pattern that matched the request.
// It is empty if the request was not matched against a pattern.
Pattern string
```

and in `server.go`, `ServeMux.ServeHTTP` does `h, r.Pattern, r.pat, r.matches = mux.findHandler(r)` —
assigning **to the original `*Request`**, not to a clone, before calling `h.ServeHTTP(w, r)`.

Two consequences that matter:

- Middleware wrapping the mux from outside **can** read `r.Pattern`, but **only after**
  `next.ServeHTTP` returns. Reading it before the call gives `""` every time. This is a subtle
  ordering bug that will look like "the route label is always empty."
- It is empty for unmatched requests (404). The label must fall back to a **constant** like
  `"unmatched"` — never `r.URL.Path`, which is unbounded and is precisely the cardinality explosion
  the Prometheus naming docs and OTel's `http.route` rule ("MUST be low-cardinality… Avoid populating
  this with high-cardinality URI paths") warn about.

This means the four fixed routes give a correct, bounded `route` dimension with **no routing library
and no hand-maintained path→name map.**

### 4.4 Other middleware pitfalls worth naming

- **Panic recovery ordering.** A recover middleware must sit outside the logging middleware so the
  panic is logged with its request_id; but a panic *after* `WriteHeader` cannot produce a 500 — the
  status is already on the wire and the connection will just be closed. Log the discrepancy rather
  than pretending the recovery succeeded.
- **Never log the request body.** It's up to 10MB of customer HTML/Markdown here
  (`maxBodyBytes`), potentially containing PII. Log `len(body)` only. Note `decodeConversionRequest`
  currently interpolates decode errors into the response message, which can echo body fragments back
  to the client — worth a separate look in a security pass, out of scope here.
- **Duration boundary.** `time.Since(start)` around `next.ServeHTTP` measures handler time, which
  excludes the kernel/`net/http` time spent reading the request body off a slow client but includes
  the time `io.ReadAll` blocks on it. Fine, as long as it's understood that this is server-side
  processing latency, not client-observed latency.
- **Don't log health probes at Info.** A 1-second-period probe against `/healthz` produces 86k
  lines/day/pod of noise. Either drop them in the middleware or log at Debug.

**Sources (§4):**
[pkg.go.dev/net/http#ResponseWriter](https://pkg.go.dev/net/http#ResponseWriter) and [#ResponseController](https://pkg.go.dev/net/http#ResponseController) ·
[golang/go#54136 — proposal: net/http: add ResponseController](https://github.com/golang/go/issues/54136) ·
[felixge/httpsnoop README](https://github.com/felixge/httpsnoop) ·
[`golang/go@master:src/net/http/request.go`](https://github.com/golang/go/blob/master/src/net/http/request.go), [`server.go`](https://github.com/golang/go/blob/master/src/net/http/server.go)

---

## 5. Request IDs / correlation IDs

### 5.1 The idiomatic Go mechanics

- **Generation: stdlib.** `crypto/rand` → 16 bytes → `hex.EncodeToString`. That is a 128-bit
  identifier with the same collision properties as a UUIDv4 and needs no `google/uuid` dependency.
  (`google/uuid` would arrive anyway as an indirect dep if `client_golang` is ever added — but that's
  not a reason to depend on it directly.)
- **Propagation: `context.Context`, with an unexported key type.** The `context` docs are explicit
  that Values are for "request-scoped data that transits processes and APIs" rather than "passing
  optional parameters to functions", and that the key "should avoid built-in types like `string` to
  prevent collisions across packages" — package authors "should define custom, unexported key types."
  So: `type ctxKey struct{}` (or `type requestIDKey struct{}`), never `ctx.Value("request_id")`.
- **`orchestration.Submit` already propagates it for free.** `Submit(ctx, …)` passes the request
  context straight into the render function, so a request ID placed in the context by middleware is
  visible inside `renderengines`/`lightrender` without any signature changes. This is a real
  structural advantage of the existing design and it should be exploited rather than adding a
  parallel plumbing mechanism.
- **Echo it back.** Set the ID on the response header (`X-Request-Id`) before the handler runs, so
  clients can quote it in a support ticket. It must be set before the first `WriteHeader`, which
  means the middleware must set it on the way *in*, not on the way out.
- **Do not use it as a metric label.** Unbounded cardinality — the Prometheus naming docs call out
  exactly this ("Avoid high-cardinality labels like user IDs…"). It belongs in logs and traces only.

### 5.2 Should you accept a client-supplied ID?

**Yes at a trust boundary you control; no from the open internet — and Envoy's model is the one to
copy**, because it makes the trust decision explicit rather than implicit.

From Envoy's HTTP header documentation, `x-request-id` behaviour is governed by three settings:

- `generate_request_id` (default on) — mints a UUID when none is present.
- `use_remote_address` — determines whether Envoy treats the request as an **edge request** from an
  external client.
- `preserve_external_request_id` — when true, Envoy *keeps* an externally-supplied ID on edge
  requests instead of replacing it.

The resulting policy: **internal-origin requests keep their existing `x-request-id`; edge requests
have it replaced with a fresh UUID unless you explicitly opt in to preserving it.** In other words,
the default at the trust boundary is *sanitise*, and trusting the client is a deliberate,
named configuration choice.

The security considerations for accepting one, in order of practical importance:

1. **Log injection / forgery.** OWASP's Logging Cheat Sheet says to "perform sanitization on all
   event data to prevent log injection attacks e.g. carriage return (CR), line feed (LF) and
   delimiter characters" (CWE-117). *Important nuance:* choosing `slog`'s **JSONHandler already
   defuses the classic CRLF forged-log-line attack**, because `\n` inside a JSON string value is
   escaped. This is a genuine, under-appreciated security argument for structured JSON logging over
   `log.Printf` — but it is not a licence to skip validation, because it doesn't address items 2–4.
2. **Unbounded length.** A client sending a 1MB header value gets it copied into every log record for
   that request, and into the response header. Cap it hard (64 chars is generous).
3. **Charset.** Restrict to `[A-Za-z0-9_-]`. This bounds what can end up in downstream systems that
   are *not* JSON-escaping (a future trace backend, a SQL audit table, a terminal).
4. **Correlation poisoning.** A client can deliberately reuse one ID across many requests, or reuse
   *another tenant's* ID, to make an investigation useless or to make their traffic look like someone
   else's. This is unfixable by validation — it is why the ID must never be treated as an
   authenticated identifier, and why `auth-and-tenancy`'s API key, not the request ID, must be the
   thing that scopes anything.
5. **PII.** A client may put an email or account number in the header. It then lands in logs and
   inherits their retention. Another reason to enforce a strict charset and length.

**Recommendation:** accept `X-Request-Id` only if it matches `^[A-Za-z0-9_-]{1,64}$`; otherwise
generate a fresh one and **do not** log the rejected value. Log the client-supplied ID under a
distinct key (e.g. `client_request_id`) from the server-generated one, so an investigator can always
tell which IDs are trustworthy. When `auth-and-tenancy` lands, "trusted" can be upgraded from
"well-formed" to "well-formed and from an authenticated key."

### 5.3 What about W3C `traceparent` / OpenTelemetry?

If distributed tracing ever arrives, the standard is W3C Trace Context (`traceparent` /
`tracestate`), which OpenTelemetry propagates by default. Two honest notes:

- I was **unable to read the W3C spec's security/privacy considerations** in this session (`w3.org`
  blocked), so I am not going to characterise them. Read them before adopting.
- Adopting `go.opentelemetry.io/otel` + SDK + OTLP exporter is a much larger dependency than
  `client_golang`, and there is currently **one service** — there is nothing to correlate *with*. A
  self-generated request ID gives 100% of the value of trace context inside a single service.
  **Defer.** The migration path is cheap: if the middleware's ID field is later populated from a
  `traceparent` trace-id, every log line already has the right key.

**Sources (§5):**
[pkg.go.dev/context#WithValue](https://pkg.go.dev/context#WithValue) ·
[Envoy: HTTP connection manager headers — `x-request-id`](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_conn_man/headers) (read via `envoyproxy/envoy@main:docs/root/configuration/http/http_conn_man/headers.rst`) ·
[OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html) (read via `OWASP/CheatSheetSeries@master`) ·
[W3C Trace Context](https://www.w3.org/TR/trace-context/) *(cited, not fetched — domain blocked)*

---

## 6. Recommendation for this codebase

The through-line: **this project's stdlib-first preference is not a constraint to work around here —
for four of the five questions it is simply the right answer on the merits.** The one place a
dependency is genuinely arguable is metrics, and even there it is arguable rather than obvious.

### 6.1 Where stdlib is genuinely sufficient (no dependency, not a compromise)

| Concern | Verdict |
|---|---|
| Structured logging | `log/slog`. Complete. zerolog/zap would buy allocation counts that are irrelevant next to a multi-second Chromium render. |
| Health probes | `net/http`. Three handlers and one `atomic.Bool`. |
| Request-logging middleware | `net/http` + `http.ResponseController`'s `Unwrap` convention. Go 1.20 removed the reason `httpsnoop`/`gorilla/handlers` existed. |
| Route label | `http.Request.Pattern` (Go 1.23+). No router dependency. |
| Request IDs | `crypto/rand` + `context`. No `google/uuid`. |
| Queue-depth autoscaling signal | `expvar` JSON + KEDA `metrics-api` scaler with a GJSON `valueLocation`. Genuinely sufficient for the spec's stated autoscaling design. |

### 6.2 Concrete shape

**Logging.** JSON handler on stdout; `slog.SetDefault` as the *first* statement in `main` so the
`CHROMIUM_PATH` fatal is structured; `LevelVar` driven by `LOG_LEVEL`; `AddSource` on in dev. Attach
`service`, `version`, and `engine` as base attributes via `Logger.With` so they are pre-formatted
once. Logger-in-context via an unexported key; use the `*Context` logging methods on request paths.
Keep the existing `log.Printf` startup lines as-is initially — `SetDefault` structures them for free.

**Probes.** Split `/healthz` into three:
- `/livez` — static 200, no dependency checks, ever. (Or keep `/healthz` as the liveness path for
  backwards compatibility and add the two new ones; the name matters less than the semantics.)
- `/readyz` — 200 while serving; **503 as soon as SIGTERM is received**, before
  `httpServer.Shutdown` is called, with a short sleep in between to let endpoint propagation catch
  up. Does **not** consult queue depth. May consult "is the Chromium pool non-empty", but see §6.4.
- startup probe pointed at `/readyz` with `failureThreshold × periodSeconds` > worst-case
  `POOL_SIZE` warmup, because the port is closed during `NewPool` and every probe fails with
  connection-refused until it returns.

Keep `503 QUEUE_FULL + Retry-After` as the one and only capacity mechanism. Explicitly document in
`SPEC-job-orchestration.md` that capacity is deliberately **not** wired to readiness, with the
Envoy-panic-threshold reasoning from §2.3 — because this is exactly the kind of decision someone
will "fix" later without knowing why.

**RED, phase 1 (do now, zero dependencies).** One access-log record per request:
`request_id`, `route` (from `r.Pattern`, read *after* `next.ServeHTTP`, `"unmatched"` fallback),
`method`, `status`, `code` (the error-envelope code, or empty on success), `engine`
(`chromium`/`weasyprint`), `bytes`, `duration_ms`, `outcome` (`ok` / `client_error` /
`render_error` / `queue_full` / `canceled`). That gives Rate, Errors, and Duration in any log
aggregator, plus the error taxonomy split from §3.2 that a plain status-code metric cannot express.
Probe requests excluded or logged at Debug.

**RED, phase 2 (do when autoscaling is wired, still zero dependencies).** `expvar` on an internal
port (not on the public mux): `queue_depth`, `queue_capacity`, `in_flight`, per-engine, plus request
and error counters. Feed KEDA's `metrics-api` scaler. Add the `InFlight()` accessor to
`orchestration.Pool` from §3.7 — `QueueDepth()` alone reads 0 at 100% utilisation and is a poor
scaling trigger on its own.

### 6.3 Where a dependency becomes genuinely justified

I am **not** recommending "add Prometheus." I am recommending a **named trigger** for it, so the
decision is made on evidence rather than reflex:

Adopt `prometheus/client_golang` when **any** of these becomes true:

1. **A real Prometheus/Grafana stack exists in the target environment and is the primary dashboard.**
   Then hand-rolled text exposition is reinventing a solved problem for no benefit, and
   `client_golang`'s guardrails (duplicate registration panics, label-cardinality enforcement,
   correct escaping) start paying for themselves.
2. **Latency percentiles are needed across replicas.** The Prometheus histogram docs are decisive
   here: histograms can be aggregated with `histogram_quantile()`, while "aggregating the precomputed
   quantiles from a summary rarely makes sense" and "averaging the quantiles yields statistically
   nonsensical values." Log-derived percentiles hit the same problem at scale, and getting
   multi-replica histogram aggregation right by hand is exactly the kind of thing worth outsourcing.
3. **More than one module owner is adding metrics.** `auth-and-tenancy` (usage metering) and
   `storage-and-delivery` (webhook delivery, TTL cleanup) will each want their own. A registry with
   guardrails beats a growing hand-rolled file at that point.

Until then: **Option A (structured logs) + Option B (expvar for the autoscaling signal)**, with
Option C (hand-written text exposition) as the intermediate step if a scrape endpoint is needed
before a full Prometheus deployment exists. The metric surface today is ~6 series families over ~4
routes and ~2 engines — small enough that hand-writing it is defensible; large enough that if it
doubles, the library wins.

Explicitly **defer** OpenTelemetry tracing until there is a second service. Cost is high, value with
one service is near zero, and the request-ID field converts to a trace-id field cheaply later.

### 6.4 Honest uncertainties and open questions

- **Is Kubernetes actually the deployment target?** `PLATFORM-SPEC.md` says "Self-hostable-first
  (Docker/Kubernetes)" and names KEDA, but there is no manifest or Dockerfile in the repo. All of §2
  presumes a kubelet. On plain Docker or ECS the probe *semantics* still apply but the failure modes
  differ (ECS target-group draining, Docker's `HEALTHCHECK` has no readiness concept at all).
  **Worth confirming before implementing three endpoints.**
- **Should liveness check the Chromium pool?** I recommended no, but not with full confidence.
  Argument for: if every pool instance has died, the process can never serve and a restart is the
  only remedy — `renderengines.Pool` has no self-healing today. Argument against: the most likely
  cause of mass Chromium death is memory pressure under load, and restarting the whole container
  under load is the textbook cascading failure. **My tentative answer: no liveness dependency;
  instead add pool-instance-death as a logged event + metric, and revisit only if it is observed in
  practice.** Adding self-healing to the pool is the better fix than delegating to the kubelet.
- **`RENDER_ERROR` conflates document faults and engine faults.** This limits how good the Errors
  signal can be, and no amount of metric design fixes it. It is a `render-engines` change, flagged
  here because it surfaced during this research.
- **Latency histogram buckets are unvalidated.** The suggested bucket set in §3.1 is reasoned from
  `load-test-results.md`, not measured against a percentile target. If buckets are chosen, do it from
  the load-test data, and expect to revise once real traffic exists.
- **Three well-known sources on the readiness debate were unreachable** (see §0). The §2.3 conclusion
  rests on Kubernetes' docs, Envoy's docs, and an open Kubernetes issue — all read directly — but a
  reader with access to the Google SRE Book's cascading-failures chapter and the AWS Builders'
  Library health-check article should check whether they add nuance I have missed.
