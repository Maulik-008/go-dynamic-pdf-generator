# Soak Test: Chromium memory growth over sustained rendering

Raw data from `internal/api/soak_test.go` (`TestSoakMemoryGrowth`), run with
`SOAK=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestSoakMemoryGrowth -v -timeout 900s`.
Generated 2026-08-30T15:33:49Z.

## Why this test exists

`docs/research/crash-recovery-research.md` recommends recycling a browser after N
renders to bound memory growth, and notes that Gotenberg ships `restart-after=100`
— but also records, explicitly, that **no published source justifies that number**.
It is a shipped default, not a measured optimum. This test measures the actual
growth curve in this environment so the threshold (or the decision not to have one)
rests on data.

## Method

- **One** Chromium instance (pool size 1, concurrency 1), so every sample is
  attributable to that instance.
- Document under test: the same heavy 3-page fixture as the load test
  (`heavyReportHTML`) — embedded images plus a large data table.
- 3 warm-up renders before the baseline sample, so startup allocation is not
  counted as render-driven growth.
- 400 renders, sampling total `headless_shell` RSS every 25.
- Environment: 4 CPU cores. Sandboxed dev box, not production hardware.

## Results

Total wall time: 34s

| Renders | RSS (MB) | Delta vs baseline (MB) | Last render |
|---|---|---|---|
| 0 | 356.5 | +0.0 | - |
| 25 | 360.2 | +3.7 | 104ms |
| 50 | 361.6 | +5.1 | 82ms |
| 75 | 362.4 | +5.9 | 87ms |
| 100 | 363.5 | +7.0 | 76ms |
| 125 | 363.5 | +7.0 | 82ms |
| 150 | 360.5 | +4.0 | 88ms |
| 175 | 363.7 | +7.2 | 77ms |
| 200 | 363.7 | +7.2 | 78ms |
| 225 | 361.1 | +4.6 | 94ms |
| 250 | 364.2 | +7.7 | 99ms |
| 275 | 364.2 | +7.7 | 91ms |
| 300 | 364.2 | +7.7 | 81ms |
| 325 | 363.2 | +6.7 | 84ms |
| 350 | 364.5 | +8.0 | 81ms |
| 375 | 364.3 | +7.8 | 100ms |
| 400 | 362.3 | +5.8 | 80ms |

**Total growth over 400 renders: +5.8 MB (0.0146 MB/render).**

## Conclusion

_Written by hand from the numbers above — deliberately not generated, so the
interpretation is a judgement someone made and can be argued with._

**Decision: do not implement recycle-after-N. The premise does not hold here.**

The measured growth is +5.8 MB across 400 renders — about **1.6% of the 356 MB
baseline**, or 0.015 MB per render. At that rate an instance would need roughly
**34,000 renders** to grow by 500 MB.

More telling than the total is the *shape* of the curve. It does not climb: it
oscillates between roughly 360 and 364 MB, and it goes **down** at 150, 225, 325
and 400 renders. A genuine leak is monotonic. A series that repeatedly returns to
where it was is measurement noise and ordinary allocator/GC behaviour, not
accumulation. Render latency tells the same story — 76-104ms throughout, with no
upward drift.

So the feature would cost something and buy nothing. Adopting Gotenberg's
`restart-after=100` would have destroyed and rebuilt a perfectly healthy browser
**four times** during this run, each costing ~90ms plus the loss of warm state,
to reclaim memory that was not being lost. Recycling would also need drain
machinery (acquiring every outstanding concurrency token before swapping) that
the crash-recovery path legitimately does not, since a dead instance's in-flight
renders already fail immediately rather than being interrupted mid-work. That is
real complexity added against a measured non-problem.

This is not a claim that Gotenberg's default is wrong for Gotenberg. It runs a
different Chromium build, on different workloads, in a different process model,
and it must be safe across all of them. The point is narrower and the reason this
test exists: **the number was never justified by a published measurement**, so
adopting it here would have been cargo-culting, and the measurement says this
service does not need it.

### What would change this decision

Stated up front so the conclusion can be falsified rather than assumed permanent:

1. **A different document profile.** This is one fixture — a table-heavy 3-page
   report with embedded images and no JavaScript. Documents that execute heavy
   JS, hold large DOMs, or load many distinct fonts and images could allocate
   very differently. Re-run with a representative fixture before trusting this
   for a workload that does not look like the test.
2. **A longer horizon.** 400 renders is ~34 seconds, not days of uptime. A slow
   leak of well under 0.015 MB/render would be invisible here and still matter
   after a week. The right follow-up is a multi-hour soak in staging, not a
   bigger number in this test.
3. **Any observed growth in production.** `PoolStats` now reports restarts and
   hangs, and the deployment guide documents memory budgeting. If real traffic
   shows RSS climbing monotonically per instance, revisit this with that data.

Until one of those produces contrary evidence, the pool self-heals reactively —
when an instance actually dies or hangs — rather than pre-emptively recycling
healthy ones on a schedule nobody measured.
