# specialist-job-v1

Field reference for `docs/spec/specialist-job-v1.schema.json` (ADR-097 D2/D6, #4300). Go validator: `backend/internal/specialistjob` (`ParseRequest`, `ParseCompletion`, `ValidateResult`, `RenderDraft`). Embedded mirror: `backend/internal/specialistjob/schemas/` (written by `scripts/sync-schemas`' `specialist-job-*` arm; backend-only).

A specialist job is a bounded, typed piece of evidence work a seat requests about a run, campaign or commit. It never mutates run or stage state. The schema document defines three documents as `$defs`; the root `anyOf` validates any one of them, and consumers validate against the specific def.

| Document | `$def` | Written by | Read by |
|---|---|---|---|
| Request | `request` | the requesting seat (POST `/v0/specialist-jobs`) | fishhawkd `specialistjob.ParseRequest` |
| Completion | `completion` | `fishhawk-runner specialist-job` (POST `.../result`) | fishhawkd `specialistjob.ParseCompletion` |
| Result | `result` | fishhawkd (composed) | GET `/v0/specialist-jobs/{job_id}`, the `specialist_job_completed` audit row |

## Validation layers

Schema layer: closed kind set, closed per-kind inputs/evidence (`additionalProperties:false` at every object level; `inputs`, `evidence` and inventory `args` are closed by the `if/then`-selected def), every list/string bounded, `head_sha` required for every kind except `draft`.

Go layer (`ParseRequest` / `ParseCompletion` only; a bare `check-jsonschema` run does NOT enforce these):

| Rule | Sentinel |
|---|---|
| No object repeats a member name (the map and struct decoders read a duplicate differently) | `ErrDuplicateMember` |
| Battery `control_path`: not blank, not absolute, no `..` segment, no backslash, not `_test.go` (mirrors `plan.checkCounterfactuals`) | `ErrInvalidControlPath` |
| Battery `mutation`: exactly one form — `find`+`replace` (replace may be empty, must differ from find) or `patch` | `ErrInvalidMutationForm` |
| Every Go package (`tests.package`, fan-in `packages`, `go_list` `packages`): not absolute, no `..` segment, no leading `-` | `ErrInvalidPackagePath` |
| Every repo path (probe paths, `focus_files`, `watch_paths`, `git_log` `paths`, `ls_files` `pathspec`): not absolute, no `..`, no backslash, no leading `-` | `ErrInvalidRepoPath` |
| Completion `kind` equals the stored job's kind | `ErrKindMismatch` |

Schema violations return `*SchemaError{Path, Message}`; `Path` is the deepest failing JSON Pointer and, for an unknown member, names it (`/inputs/bogus`). Malformed JSON and typed-decode failures return `*ParseError`.

## Request (`$defs.request`)

| Field | Type | Notes |
|---|---|---|
| `schema_version` | `"specialist-job-v1"` | required |
| `kind` | `counterfactual_battery` \| `fanin_build` \| `overlap_scan` \| `plan_crosscheck` \| `inventory` \| `draft` | required |
| `subject` | object, ≥1 member | `run_id` (uuid), `campaign_id` (uuid), `head_sha` (40-hex), `repo` (`owner/name`). `head_sha` required unless `kind` is `draft`. |
| `inputs` | object | selected by `kind` (below) |

There is NO `requester` member: fishhawkd derives the requester from the authenticated identity, so a body naming one is refused (`/requester`).

### Shared defs

- `run-ref`: `{run_id, head_sha, base_sha}`, all required; the diff spans `base_sha..head_sha`.
- `probe-question`: `{id (^[a-z0-9][a-z0-9_-]*$, ≤64), question (≤1000, recorded never executed), probe, expect (pass|fail)}`.
- `probe`: exactly one of `path_exists {path}`, `file_contains {path, literal ≤512}` (byte substring), `go_test {tests {run, package}}`. `expect: pass` means the proposition holds (path exists / file contains literal / tests pass). No free-form shell anywhere.

### Inputs per kind

| Kind | Inputs | Bounds |
|---|---|---|
| `counterfactual_battery` | `mutations[]` of `battery-mutation`, `probes[]` of `probe-question`; at least one non-empty | ≤16 each; find/replace ≤8192, patch ≤32768 |
| `plan_crosscheck` | `questions[]`, `focus_files[] {path, expect: present\|absent}`, `compare_runs[]` of `run-ref`; at least one of questions/focus_files non-empty | 16 / 64 / 8 |
| `overlap_scan` | `runs[]` of `run-ref` (required), `watch_paths[]` (`path.Match` globs) | 2..16 / ≤32 |
| `fanin_build` | `slices[] {name, head_sha}`, `packages[]` (both required) | 1..16 each |
| `inventory` | `command` (`git_log` \| `ls_files` \| `go_list`), `args` selected by command: `git_log {paths[], max_count}`, `ls_files {pathspec[], max_entries}`, `go_list {packages[]}` | paths ≤16, max_count 1..200, max_entries 1..2000, packages 1..16 |
| `draft` | `document_kind` (`adr` \| `issue`), `title`, `brief`, `outline[] {heading, body}` | title ≤200, brief ≤8192, outline ≤16 of heading ≤120 / body ≤8192 |

`battery-mutation` is exactly the field set of `plan-standard-v1` `$defs.counterfactual` (`control_path`, `mutation {find, replace | patch}`, `tests {run, package}`, `expect: red`) with bounds added; `TestBatteryMutationShapeMatchesPlanCounterfactual` pins the property keys, required sets and `expect` enum of all three defs.

## Completion (`$defs.completion`)

| Field | Type | Notes |
|---|---|---|
| `schema_version` | `"specialist-job-v1"` | required |
| `execution_nonce` | string ≤128 | the nonce the admission issued; required |
| `kind` | kind | must equal the job's kind (`ErrKindMismatch`) |
| `verdict` | `pass` \| `fail` \| `inconclusive` | required |
| `reason` | `kind_not_executable` \| `not_runnable` \| `admission_timeout` \| `materialize_failed` \| `unresolvable_sha` \| `execution_error` \| `timeout` \| `probe_error` \| `inputs_invalid` | required when `evidence` is absent |
| `evidence` | object | selected by `kind`; may be absent ONLY with `verdict: inconclusive` and a `reason` |
| `duration_ms` | integer ≥0 | required |

### Evidence per kind

| Kind | Evidence |
|---|---|
| `counterfactual_battery` | `mutations[] {control_path, observed: red\|green\|timeout\|error, baseline: red\|green\|timeout\|error\|skipped, reason?, mutation_sha256 (64-hex, the runner's counterfactualMutationSHA256)}`, `probes[]` of `probe-evidence` |
| `overlap_scan` | `runs[] {run_id, resolved, changed_files ≤500, truncated}`, `overlaps[] {run_a, run_b, paths ≤500, truncated}` (≤120), `watch_hits[] {run_id, watch_path, paths}` |
| `fanin_build` | `slices[] {name, head_sha, outcome: merged\|conflict\|error, conflict_paths}`, `packages[] {package, outcome: pass\|fail\|timeout\|error\|skipped, reason?}` |
| `plan_crosscheck` | `questions[]` of `probe-evidence`, `focus_files[] {path, expect, observed: present\|absent\|error, matched, touched_by ≤8 run ids}`, `compare_runs[] {run_id, resolved, overlap_paths}` |
| `inventory` | `command`, `lines[]` (≤2000 of ≤1000 chars, redacted), `truncated`, `exit_code` |
| `draft` | `text` (≤32768) |

`probe-evidence`: `{id, observed: pass|fail|error, expected: pass|fail, matched, reason?}`. Per-item `reason` is a snake_case token from the executor's vocabulary (a battery mutation uses the E85.3 counterfactual reason set).

### Verdict rules

| Kind | fail | inconclusive | pass |
|---|---|---|---|
| `counterfactual_battery` | any mutation observed `green`, or any probe mismatch | any mutation `timeout`/`error` (including a non-green baseline), or a probe `error` | otherwise |
| `fanin_build` | any slice `conflict` or package `fail` | any `timeout`/`error` | otherwise |
| `overlap_scan` | any pairwise overlap or watch hit | any unresolvable SHA | otherwise |
| `plan_crosscheck` | any question/focus mismatch, or a focus file touched by a compare run | unresolvable SHA or probe `error` | otherwise |
| `inventory` | — | non-zero exit | exit 0 |
| `draft` | — | — | rendered |
| unregistered kind | — | `reason: kind_not_executable`, no evidence | — |

## Result (`$defs.result`)

Server-composed, never accepted from a client: `{job_id, kind, requester {seat: operator|first_officer, actor_kind, subject}, subject, verdict, reason?, evidence?, charged_to {seat, subject}, duration_ms}`. `requester` comes from the stored job row (derived from the authenticated identity at request time: `actor_kind`/`subject` are the identity's, a delegated operator-agent token is `actor_kind: agent`); `charged_to` is the requester's seat and subject. Same evidence selection and inconclusive rule as the completion.

## Draft render

`draft` is rendered in-process by fishhawkd at request time with `specialistjob.RenderDraft`: `# title`, the brief, each outline section in order, then every canonical section the outline did not name (case-insensitive heading match) as `_TBD_` — ADR: Context, Options, Recommendation, Decision, Consequences; issue: Summary, Scope, Done means. Deterministic, pure, model-free, bounded to 32 KiB (`ErrDraftTooLarge`). A draft writes nothing beyond its two audit rows (no work item is filed). A model-written draft would be a later kind amendment.

## Execution boundary

- Every non-draft kind runs under `fishhawk-runner specialist-job` in a freshly materialized scratch clone, never the lineage worktree or the source checkout; every untrusted execution gets its own clone and no git call runs in a clone after code executed in it.
- Every `go test` / `go list` goes through the runner's gate exec seam under the verify lock.
- The runner's job HTTP client admits only GET `/v0/specialist-jobs/{id}` and POST `/v0/specialist-jobs/{id}/{admission,result}`; its git guard refuses `push` and remote edits.
- Runner jobs are admitted through the `local-verify:<host>` concurrency group (never an implement slot) and yield to verify-stage holders and waiters; a running job heartbeats by re-POSTing admission.

## HTTP surface

Contract of record: `docs/api/v0.openapi.yaml`.

| Route | Scope | Purpose |
|---|---|---|
| POST `/v0/specialist-jobs` | `write:stages`, non-run-bound identity | file a request; `draft` answers 201 completed, other kinds 201 queued |
| GET `/v0/specialist-jobs/{job_id}` | `read:runs`, own account | job state, inputs, verdict, result, derived `stale`; NEVER returns `execution_nonce` |
| POST `/v0/specialist-jobs/{job_id}/admission` | `write:stages` | admit-or-queue, heartbeat with the same nonce, stale-running takeover under a new nonce |
| POST `/v0/specialist-jobs/{job_id}/result` | `write:stages` | ship the completion; touches only `specialist_jobs` and the audit chain |

## Audit payloads

- `specialist_job_requested`: `{job_id, kind, requester, subject, inputs}` — `inputs` is the request's inputs member, recorded as semantic JSON (JSONB normalizes key order and whitespace). On the run chain when `subject.run_id` is set, else the account's global chain.
- `specialist_job_completed`: `{job_id, kind, requester (from the job row), subject, verdict, reason, evidence, charged_to, duration_ms, requested_sequence, provenance: "bearer"}`. Result provenance is the forwarded operator bearer, not a runner signature: specialist results are not ADR-089 D4 blocking-grade evidence until #4363.
