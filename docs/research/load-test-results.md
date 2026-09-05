# Load Test Results: Heavy Multi-Page Document

Raw data from `internal/api/loadtest_test.go` (`TestLoadHeavyDocument`), run with
`LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument -v`.
Generated 2026-08-31T04:24:47Z.

## Test environment

- CPU cores (`runtime.NumCPU()`): 4
- This is a sandboxed development environment, not production-target hardware —
  absolute numbers below should be re-validated on real deployment hardware before
  being treated as SLAs, per PLATFORM-SPEC.md's own framing ("targets to validate
  with our own load tests, not facts").

## Methodology

- **Document under test**: `heavyReportHTML(50)` — 3 embedded synthetic chart images
  (400x200 PNG each, base64-inlined) plus a 50-row itemized data table, 21597 bytes of
  HTML. Empirically calibrated to render to exactly **3 PDF pages** at A4/default
  margins (calibration sweep: 30 rows -> 2 pages, 50 -> 3, 70 -> 5, 90 -> 7, 110 -> 8).
- **Path under test**: real HTTP, `POST /v1/pdf/html`, via `httptest.Server` wrapping
  the actual `internal/api` routes — not a direct in-process `Pool.RenderHTML` call.
- **Pool config**: 3 Chromium instances, 4 max concurrent renders per instance
  (12 total concurrent-render capacity).
- **Load pattern**: for each concurrency level, N workers each fire 8 sequential
  requests; latency is measured per-request (queue wait + render + response transfer
  combined, i.e. what a real client actually experiences), throughput is
  successful-requests / wall-clock-duration-of-the-whole-batch.
- **Resource sampling**: total resident memory (RSS) across every `headless_shell`
  process, sampled every 300ms throughout the run via `ps`; baseline taken at idle
  right after pool warm-up, peak taken as the max sample across the entire sweep.

## Results

- Baseline Chromium RSS (idle, 3 warm instances): **1052.4 MB**
- Peak Chromium RSS observed during the full sweep: **2256.4 MB**

| Concurrency | Requests | Succeeded | Failed | Wall time | Throughput (req/s) | p50 | p90 | p99 | min | max |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 8 | 8 | 0 | 648ms | 12.34 | 79ms | 86ms | 86ms | 75ms | 87ms |
| 2 | 16 | 16 | 0 | 782ms | 20.47 | 96ms | 106ms | 107ms | 89ms | 110ms |
| 4 | 32 | 32 | 0 | 1.335s | 23.98 | 157ms | 187ms | 219ms | 96ms | 226ms |
| 8 | 64 | 64 | 0 | 2.498s | 25.62 | 293ms | 365ms | 418ms | 168ms | 422ms |
| 16 | 128 | 128 | 0 | 5.024s | 25.48 | 552ms | 832ms | 956ms | 151ms | 1.022s |


## Follow-up: job-orchestration backpressure (re-run with admission control)

Raw data from `internal/api/loadtest_test.go` (`TestLoadHeavyDocument_WithOrchestration`), run with
`LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument_WithOrchestration -v`.
Generated 2026-08-31T04:24:54Z.

Direct answer to this file's own "Fix recommendation" above: same pool config (3 instances x 4),
now gated by an `internal/orchestration.Pool` sized to the pool's real capacity
(Workers=12, QueueCapacity=12, admits 24 in flight at once), fired at 3x that capacity
(72 concurrent requests) — deliberately far beyond what the earlier run's 16-way sweep reached.

| Total requests | Succeeded (200) | Rejected (503) | Other failures |
|---|---|---|---|
| 72 | 24 | 48 | 0 |

503 rejection latency: min=42ms p50=75ms max=93ms — every rejection is an immediate admission-control
decision, not a request that waited and then failed.

**Conclusion**: this closes the gap the original run's point 3 identified. At 3x admitted capacity,
48 of 72 requests were rejected immediately (bounded, well under the unbounded latency growth
previously observed) while the remaining 24 succeeded normally — the platform now has a real ceiling
(reject past admitted capacity) instead of only queueing past it.
