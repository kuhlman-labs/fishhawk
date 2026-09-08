---
title: Tracing a run with OpenTelemetry
description: Export every run as an OpenTelemetry trace to any OTLP-compatible backend with one environment variable, and where that endpoint is reachable from for each runner kind.
---

The runner can export each run as an [OpenTelemetry](https://opentelemetry.io/)
trace: one span per stage, a child span per model call, carrying token counts, a
cost estimate, and the GenAI semantic-convention attributes. It is off by
default and turns on with a single environment variable.

## What it is

The runner speaks standard **OTLP over HTTP**. When you set an endpoint, it
POSTs spans directly to the collector you configure — Fishhawk is not in the
data path, and no span leaves the environment the runner executes in except to
the endpoint you name. This is consistent with the same posture the rest of the
product takes: bring-your-own model key (ADR-072) and agent execution on your
own infrastructure (ADR-074).

The export is vendor-neutral. Any OTLP-compatible backend receives the spans;
Fishhawk names none and integrates with none specifically.

## Turning it on

Set `OTEL_EXPORTER_OTLP_ENDPOINT` in the environment that spawns the runner:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example:4318
```

Spans are POSTed to `{endpoint}/v1/traces`. The runner honours the standard
`OTEL_EXPORTER_OTLP_*` variables, so an authenticated collector is configured
the usual way — `OTEL_EXPORTER_OTLP_HEADERS` for a bearer token, the standard
TLS variables for a private CA.

**Unset is a no-op.** With `OTEL_EXPORTER_OTLP_ENDPOINT` empty or unset the
runner constructs no exporter at all; the run loop is byte-for-byte unaffected.
There is no partial state and no separate on/off flag — the presence of the
endpoint is the switch.

## What a run looks like as a trace

Each stage invocation produces two spans:

- **`stage <name>`** — the parent span, carrying `fishhawk.run_id` and
  `fishhawk.stage`. Its span **status** records the stage outcome, so a failed
  stage is `Error` and a clean one is `Ok`.
- **`chat <model>`** — a child span for the model call, carrying the GenAI
  attributes and the Fishhawk cost/reproducibility attributes below.

The resource attribute `service.name` is `fishhawk-runner` on every span.

## Attributes

| Attribute | On span | Meaning |
|---|---|---|
| `service.name` | resource | Always `fishhawk-runner`. |
| `fishhawk.run_id` | `stage <name>` | The run this stage belongs to. |
| `fishhawk.stage` | `stage <name>` | The stage name (`plan`, `implement`, …). |
| `gen_ai.system` | `chat <model>` | Always `anthropic`. |
| `gen_ai.operation.name` | `chat <model>` | Always `chat`. |
| `gen_ai.request.model` | `chat <model>` | The model invoked. |
| `gen_ai.usage.input_tokens` | `chat <model>` | Input tokens. |
| `gen_ai.usage.output_tokens` | `chat <model>` | Output tokens. |
| `gen_ai.request.temperature` | `chat <model>` | Present **only** when the agent surfaced a temperature. |
| `fishhawk.cost.usd` | `chat <model>` | Estimated cost in USD (see below). |
| `fishhawk.cost.estimated` | `chat <model>` | Always `true` — the cost is never a billed figure. |
| `fishhawk.cost.priced` | `chat <model>` | `false` when the model is absent from the price table. |
| `fishhawk.pricing.as_of` | `chat <model>` | The date the price table was current as of. |
| `fishhawk.latency_ms` | `chat <model>` | Model-call latency in milliseconds. |
| `fishhawk.repro.temperature_available` | `chat <model>` | Whether a temperature was recorded for this call. |

**Cost is an estimate, always.** `fishhawk.cost.usd` is derived from a
checked-in price table, so `fishhawk.cost.estimated` is always `true`. When the
model is not in that table `fishhawk.cost.priced` is `false` and the cost is
`0` — that is *"not priced"*, not *"free"*. Read `fishhawk.cost.priced` before
trusting a `0`.

## One trace per run

Each stage runs in a fresh, short-lived runner process, so there is no
in-process parent to share across stages. Instead every stage of a run derives
the **same** 16-byte trace id deterministically from the run id (a sha256
prefix, `TraceIDFromRunID`) and parents its spans to a synthesized run-root span
context.

The consequence for you: searching your backend for a run id returns **one**
trace tree containing that run's plan, implement, review, and acceptance stages
together. Each span keeps its own random span id, so nothing collides across
stages or across runs.

## Which runner kinds it works with

The one rule: the endpoint must be reachable **from where the runner actually
executes**. The runner inherits the environment of the process that spawns it,
so the endpoint has to be resolvable and reachable in that environment — not in
yours, if the two differ.

| Runner kind | Reachable? | Why |
|---|---|---|
| `local` | Yes, end to end | The runner runs on your host, so it reaches a collector on that host or network directly. |
| `github_actions` | Not out of the box | The runner executes on a GitHub-hosted runner. Its `localhost` is the CI host's loopback, not yours, so it needs a collector reachable from the CI job and a job-level `OTEL_EXPORTER_OTLP_ENDPOINT` set in your own workflow file. |
| `gitlab_ci` | Not out of the box | Same shape and requirement — the runner executes in the GitLab CI job, so the endpoint must be reachable from that job. |

The `gitlab_ci` verdict is a **reachability consequence**, not an exercised
result: it follows from the runner executing inside the CI job, the same
structural argument as `github_actions`, rather than from an observed
end-to-end trace on GitLab.

A future Kubernetes runner (ADR-075,
[#2311](https://github.com/kuhlman-labs/fishhawk/issues/2311)) removes this
constraint for the hosted case: a Job in your own cluster reaches your own
collector with no tunnelling. That is future work, not a shipped capability
today.

## Local development collector

For inspecting your own runs during development, this repository ships a
**Jaeger** all-in-one as its local dev collector — that is its role here, a
local inspection tool, not a recommended production backend.

```sh
docker compose --profile otel up -d
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
# view the per-run trace tree at http://localhost:16686
```

The Kubernetes chart ships the same all-in-one gated by `jaeger.enabled`, whose
render is refused outside `profile: local`. The operator quickstart is in
[`docs/deploy/kubernetes.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/deploy/kubernetes.md)
(§ "Tracing (Jaeger)"), and the implementation reference for the span shape and
attribute keys is
[`runner/internal/otelemit/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/runner/internal/otelemit/README.md).
