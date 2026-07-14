# runner/internal/otelemit

Per-stage OpenTelemetry GenAI trace emission (`EmitStage`), plus the local collector story (#649 / #679 compose, #895 k8s).

## Gating

Emission is gated by `OTEL_EXPORTER_OTLP_ENDPOINT`: unset = a no-op disabled Emitter (the default, so the loop is unaffected); set = an OTLP/HTTP exporter POSTing to `{endpoint}/v1/traces`.

## Span shape

Per run: a `stage <name>` parent span (attrs `fishhawk.run_id`, `fishhawk.stage`) with a `chat <model>` child carrying GenAI-semconv attrs (`gen_ai.system=anthropic`, `gen_ai.operation.name=chat`, `gen_ai.request.model`, `gen_ai.usage.input_tokens` / `output_tokens`, optional `gen_ai.request.temperature`) plus `fishhawk.*` cost/repro attrs (`cost.usd`, `cost.estimated`, `cost.priced`, `pricing.as_of`, `latency_ms`, `repro.temperature_available`).

Every stage of a run stitches under one deterministic trace id (`otelemit.TraceIDFromRunID`, a sha256-prefix of the run id) since each `fishhawk_run_stage` spawns a fresh short-lived runner process.

## Local collector (compose, dev only)

`docker-compose.yml` ships a Jaeger all-in-one behind the opt-in `otel` profile (omitted from the default `docker compose up -d`; `docker compose config --services` excludes it, `docker compose --profile otel config --services` includes it).

Bring it up with `docker compose --profile otel up -d`, set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`, and view the per-run trace tree at the Jaeger UI on http://localhost:16686.

## Local collector (k8s, #895)

The Helm chart ships the same Jaeger all-in-one as a dev-only in-cluster service gated by `jaeger.enabled` (default false in `values.yaml`; `values-local.yaml` turns it on; `fishhawk.validateSecrets` fails the render outside `profile: local`, mirroring the postgres/minio dev-only guards).

`deploy/helm/fishhawk/templates/jaeger.yaml` is a single-replica Deployment + ClusterIP Service exposing the UI (16686) + OTLP HTTP (4318) + OTLP gRPC (4317) with in-memory storage (no PVC; ephemeral is fine for local inspection).

`scripts/dev k8s` opens a port-forward for the UI + OTLP HTTP after the `/healthz` gate (tracked in `.fishhawk/k8s-jaeger-pf.pid`, torn down by `k8s-down`), so the host-spawned runner reaches the collector at the host's `localhost:4318` — the in-cluster Service DNS name is NOT reachable from the host where `fishhawk-mcp` spawns the runner.

Operator quickstart: `docs/deploy/kubernetes.md` ("Tracing (Jaeger)").

## Execution-locality caveat

The endpoint must be reachable from where the runner ACTUALLY executes. The standard dogfood loop fires `workflow_dispatch` and the runner executes on a GitHub-hosted runner (`.github/workflows/fishhawk.yml`, `runs-on: ubuntu-latest`, `uses: ./runner`), where `localhost:4318` is the CI host's loopback, not the operator's.

End-to-end local viewing therefore requires the runner to run on a host that can reach the local collector — invoke `fishhawk-runner` locally against the local backend + collector (the documented verification path; note the `runner_kind=local` flow spawns the runner on the operator's host and so CAN reach `localhost:4318`).

Exporting from the GHA job (a job-level `OTEL_EXPORTER_OTLP_ENDPOINT` + a reachable/tunneled collector) is deferred human-led `.github/workflows/**` work.
