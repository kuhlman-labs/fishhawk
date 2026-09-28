# hostload — verify-preflight host load sampling

Long-form contract for `runner/internal/hostload`. Anchor issue: **#3663** (E51.11).

## The problem

When the host is CPU-starved — in the #3663 incident by orphaned busy-loops a
previous stage leaked (see `runner/internal/procsweep/README.md`) — a
committed-tree verify fails on container-start deadlines and test timeouts in
packages the diff never touched. The runner classified that as **category A**: an
artifact defect the agent must fix. It is not; it is infrastructure, and the
right classification is **category C** (retryable in place).

## What it reads

`Read(ctx)` returns a `Sample{Load1, Cores, Top}`.

**Load1** — the **1-minute** load average, the most responsive of the three. The
sample is taken immediately before each verify, where a 15-minute average would
misclassify a host that has just recovered. Three sources, tried in order, each
falling through on an unreadable *or unparseable* answer:

1. `/proc/loadavg` (Linux) — first whitespace field.
2. `sysctl -n vm.loadavg` (Darwin) — prints `{ 7.19 6.83 5.76 }`, so the braces
   are trimmed before the first field is read.
3. `uptime` — parsed with a regex tolerating BOTH spellings: Linux procps prints
   the singular `load average:`, macOS the plural `load averages:`. A
   comma-decimal locale (`3,50`) is normalised.

Every source unreadable → `Read` returns an **error**, which the caller treats as
a fail-open degrade (one printed reason, no classification change).

**Cores** — `runtime.NumCPU()`.

**Top** — the top 5 CPU consumers from `ps -axo pid=,pcpu=,comm=`, sorted
**descending in Go**. Never `ps -r` or `--sort=-pcpu`: those flags diverge
between BSD and procps, so the ordering is done here where it is identical on
every host. A failed `ps` is **not** an error — the consumers are evidence for
the failure message, not the decision input.

## The threshold

`Overloaded(s, factor)` is `Load1 > factor*Cores`, strictly greater, with
`DefaultFactor = 4.0`.

A 10-core host must exceed **40** — far above the busy-build band, so an ordinary
`go test -race ./...` loop on this repo never trips it, while the #3663 incident
(~140 on a 10-core host, **14x**) is unambiguous. A sample with `Cores <= 0`
decides **nothing**: a zero-core reading is not evidence of starvation.

If 4.0 proves wrong the fix is a one-line change; the threshold table in
`hostload_test.go` straddles it at just-below / exactly-at / just-above.

## The lead line

`Reason(s)` renders a single line:

```
host_overloaded: 1-min load average 140.2 on 10 cores (14.0x); top CPU: pid 123 sh 99.1%, pid 124 busyloop 98.4%
```

It is **PREPENDED** to the failure evidence, never substituted for it: the
verify's own output follows verbatim, so the reviewer still reads the real test
failures. That, plus the classification firing only when the verify actually
FAILED, is what bounds the cost of a false positive.

## How the runner uses it

`runner/cmd/fishhawk-runner/verifyhostload.go` holds `readHostLoad` (the
injectable seam) and `hostLoadProbe`. See
`runner/cmd/fishhawk-runner/README.md` § "Host-load verify preflight" for the
call sites, the `verify_host_overloaded` event, and the test-insulation contract.
