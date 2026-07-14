# backend/internal/corpusdistill

Corpus-case distiller: scaffolds an agent-eval corpus case from a captured trace bundle (#1290), with inline labeling + dry-run (#1291). The plan-review-miss sibling corpus feed (`planreviewmiss.go`) is documented in `docs/architecture/agent-eval.md` §Plan-review-miss corpus.

## Distill / Preview

- `Distill(r io.Reader, Options) (caseDir, err)` parses a trace bundle (gzipped `.jsonl.gz` OR plain `.jsonl`, auto-detected by the gzip magic `0x1f 0x8b`) via `bundle.ReadEvents`, scores it with `agenteval.Score`, and writes the three-file corpus case (`trace.jsonl` plain + `expected.json` + a `Provenance: PRODUCTION` `case.md` template) under `OutDir/CaseName`.
- `FetchStageTrace` (in `fetch.go`) GETs the redacted bundle from `GET /v0/stages/{stage_id}/trace` with `Authorization: Bearer`.
- `Preview(r io.Reader, Options) (Result, err)` is the pure no-write entrypoint backing `--dry-run`: it shares all parse/score/render logic with `Distill` (both delegate to `prepare`) but touches no filesystem, returning the would-be artifacts (`CaseDir`/`ExpectedJSON`/`CaseMD`/`Card`).

## The `fishhawk-distill-corpus` command

The standalone command `backend/cmd/fishhawk-distill-corpus` (a dev/operator tool kept out of the `fishhawkd` server binary) drives it.

- Flags: `--in` / `--stage-id` / `--case-name` / `--issue` / `--out-dir` / `--force` / `--backend-url` (env `FISHHAWK_BACKEND_URL`) / `--token` (env `FISHHAWK_TOKEN`) / `--signal` / `--narrative` / `--dry-run`.
- The default `--out-dir` (`backend/internal/agenteval/testdata/corpus`) fails loud unless run from the repo root.
- The inline-labeling flags `--signal`/`--narrative` pre-fill the `case.md` distilled-signal sections (omitted → the #1290 `TODO(operator)` template, byte-for-byte unchanged).
- `--dry-run` scores + prints the would-be case without writing any file (exit 0 on preview, 1 on a genuine error).

Automates the mechanical half of the #819 corpus buildout so the operator can add + label + select in one workflow; case selection stays operator curation.
