# Acceptance preview provisioning — hook contract

Build-system-agnostic reference for the acceptance-stage preview provisioning hooks (E31.18 / #1569, E36.2 / #1640). The dogfood loop wires these to `scripts/dev preview`; this document specifies the contract independent of any build system, so an operator running a non-fishhawk stack can provision a verifiable preview instance.

Scope: this is runner-deployment configuration, deliberately **not** part of the workflow spec surface (`.fishhawk/workflows.yaml`). The hooks are read from the runner-process environment. The runner-internal narrative lives in `runner/README.md` ("Acceptance target-identity gate + preview provisioning") and `docs/ARCHITECTURE.md` §7/§10; this file is the external-operator contract.

## Why the gate exists

The acceptance stage validates a **running instance** at the first spec-declared `egress.target_hosts` entry. Without provisioning, that is whatever build happens to answer there — typically the orchestrating deployment on current `main`, not the run's merge candidate. Before the acceptance agent spawns, the runner provisions the preview (optional), waits for it to become ready, and verifies its build identity against the merge-candidate head SHA. A wrong or unverifiable build fails the stage **pre-spawn** rather than validating the wrong code.

## The two hooks

Both hooks run via `sh -c` in the operator's dispatch `working_dir` — the checkout the run was dispatched from (e.g. the one carrying the untracked `.env`) — falling back to the runner's current working directory when no `working_dir` was dispatched. Anchoring to the dispatch checkout means a **relative** provision command like `scripts/dev preview` resolves the operator's checkout even when the driving session launched from a git worktree, rather than the runner-inherited process cwd (#1746).

| Env var | Role | When it runs |
|---|---|---|
| `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` | **provision** — build and serve the merge candidate | once, before the identity gate |
| `FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD` | **teardown** — stop and remove the preview instance | deferred; on **every** post-provision return |

The teardown hook is deferred the moment it is configured, so it runs on the happy path (after the verdict ships) **and** on every pre-spawn failure that occurs after provisioning began (readiness timeout, stale/unreachable target, any gate failure) **and** on a stage cancelled or killed mid-validation (the SIGTERM chain from the MCP verb). It is best-effort: a non-zero teardown exit logs `acceptance_preview_teardown_failed` and never changes the stage outcome.

The teardown is **detached from the runner's cancellation** and **bounded by `FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS`** ([#3394](https://github.com/kuhlman-labs/fishhawk/issues/3394)). The deferred closure runs *after* a cancelled stage's context is already done, and `exec.CommandContext` refuses to start a process on a done context — so a teardown bound to the runner context was refused before it forked and the preview leaked on exactly the cancel path. It now runs under `context.WithoutCancel` re-wrapped in the provision timeout, so it outlives the cancellation that triggered it without being able to stall runner exit indefinitely. The residual is a `SIGKILL` of the runner process itself, which no defer survives; the next `scripts/dev preview` reclaims the port (below).

### `auto_preview`: the loop-side entry point to the same contract (E68.43 / [#3321](https://github.com/kuhlman-labs/fishhawk/issues/3321))

Neither variable need be exported by hand. `fishhawk_dispatch_stage(stage:"acceptance", auto_preview:true)` sets **both** `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` and `FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD` **on the spawned runner's environment** for that dispatch ([#3394](https://github.com/kuhlman-labs/fishhawk/issues/3394)), so the runner provisions the target and tears it down through exactly the pipeline this document specifies — nothing about the hook contract, the injected variables, the timeouts, the readiness contract or the exit-code semantics changes. It is one call instead of the provision-then-re-dispatch dance.

Three properties worth stating explicitly:

- **The operator's value always wins, for either variable.** An exported `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` or `FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD` is already in the verb's `os.Environ()`, so `auto_preview` appends nothing for it and the runner receives that value unchanged.
- **The built-in defaults are `scripts/dev preview` and its counterpart `scripts/dev preview-down`**, which are **fishhawk's own dogfood defaults** and are meaningless in a non-fishhawk checkout. A non-fishhawk stack must set `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` — which, per the point above, overrides the default everywhere it is used or rendered. Without it, an `auto_preview` dispatch in such a repo results in the ordinary category-C `acceptance_preview_provision_failed` with a bounded output tail.
- **The default teardown is injected only alongside the default provision.** The teardown resolver keys on the provision's *source*, not on the `auto_preview` flag: an operator-set `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` with no teardown set gets **no** `scripts/dev preview-down` injected, because that command only knows how to take down what `scripts/dev preview` stood up — pairing it with a foreign provision hook would tear down the wrong thing. Such an operator keeps ownership of their own teardown and the runner still emits the advisory `acceptance_preview_teardown_missing` when none is configured. Before #3394 `auto_preview` injected only the provision hook, so every auto_preview dispatch drew that warning and leaked the preview `fishhawkd` on the target port; the leak then blocked the next run's provision.

The same default command is rendered — as a **`run_preview`** action naming `scripts/dev preview <expected head sha>` — on the `needs_target` dispatch refusal and in `next_actions` immediately before a suggested acceptance dispatch, so the bring-up is visible whether or not `auto_preview` is used. Details: `backend/internal/mcpserver/README.md`.

### Injected environment

At call time the runner adds two variables to each hook's environment:

| Injected var | Value |
|---|---|
| `FISHHAWK_PREVIEW_SHA` | the expected merge-candidate head SHA (the identity the preview must serve) |
| `FISHHAWK_PREVIEW_TARGET_HOST` | the first declared `egress.target_hosts` entry (host or `host:port`, scheme-less) |

The hook runs under the credential-stripped `sanitizedGateEnv` allow-list (ADR-029 item 4, shared with the compile/test/verify gates): `PATH`, `HOME`, and the Go toolchain vars (an explicit Go-name set, `gateEnvAllowGo` — NOT a bare `GO*` prefix, which also admitted `GOOGLE_*` credentials, #2504) survive so a Go build works, but the runner's secrets do **not** reach the hook. `FISHHAWK_GITHUB_TOKEN` / `GITHUB_TOKEN` / `GH_TOKEN`, `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`, and `FISHHAWK_API_TOKEN` are all absent. The provision hook builds and runs untrusted, committed, agent-authored merge-candidate code before the ADR-050 acceptance egress proxy contains anything, so it must never inherit the runner's credentials.

## Timeouts

| Env var | Default | Governs |
|---|---|---|
| `FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS` | 300 | per-command budget for **both** the provision and teardown hooks (the provision command typically includes a build) |
| `FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS` | 60 | post-provision readiness-poll budget |

Each accepts a positive integer number of seconds; an unset, unparsable, or non-positive value falls back to the default.

## Readiness contract

After a successful provision, the runner polls `<scheme>://<host>/healthz` every 2 seconds until the served build identity is **verified** or the ready budget expires. Scheme order is http-first for loopback and IP-literal hosts, https-first otherwise, always falling back to the other scheme. The `/healthz` response must be `200` with a JSON body carrying a `git_sha` field:

- **verified** — `git_sha` is a ≥7-character prefix of `FISHHAWK_PREVIEW_SHA`. The gate proceeds and the agent spawns.
- A `-dirty`-suffixed `git_sha` is treated as **stale** (fail-closed): a dirty build is not the committed merge candidate, even when the prefix matches.

**The target MUST expose `git_sha` on `/healthz` to be verifiable.** A target that answers but exposes no build identifier (missing/`unknown`/too-short `git_sha`, non-200, or non-JSON) is classified **unverifiable** → the runner warns and proceeds (a missing identifier on an otherwise-reachable target is never a hard fail). If you want acceptance to actually gate on identity, serve `git_sha`.

**A missing EXPECTATION is different, and fails closed.** When the backend sends no `acceptance_expected_head_sha` at all (an older backend, or backend-side ledger resolution failure) there is nothing to compare the target against, so the gate fails the stage pre-spawn with `acceptance_expected_head_unresolved` — before any provision command runs — rather than validating whatever build answers at the declared host (#3091).

Without a provision command the gate is single-shot against a fixed instance: only connection failures are retried (3 quick attempts absorb a blip), and a definitive stale/unverifiable answer gates immediately.

## Exit-code semantics

| Path | Outcome |
|---|---|
| provision exits non-zero **or** times out | stage fails **pre-spawn**, category C, reason `acceptance_preview_provision_failed` (exit state + a bounded output tail in the detail) |
| provision succeeds, target verified | agent spawns |
| provision succeeds, target stale | stage fails pre-spawn, category C, reason `acceptance_target_stale` (expected-vs-got in the detail); teardown runs |
| provision succeeds, target never ready | stage fails pre-spawn, category C, reason `acceptance_target_unreachable` (`not ready within <budget>` in the detail); teardown runs |
| provision succeeds, target unverifiable | warn `acceptance_target_unverified`, agent spawns |
| stage cancelled after provision (SIGTERM chain, mid-validation) | runner exits `130` with `runner_cancelled`; **teardown runs** — it is detached from the runner's cancellation and bounded by the provision timeout (#3394) |
| declared target, backend sent NO expected head SHA | stage fails pre-spawn, category C, reason `acceptance_expected_head_unresolved`; **no provision command runs** and no teardown is returned |
| teardown exits non-zero | logged `acceptance_preview_teardown_failed`; **stage outcome unchanged** |

A category-C failure is a pre-spawn infrastructure failure: the acceptance agent never runs, and no verdict ships.

### Leaked previous-run preview

Two controls already hold, so a preview that outlived its run (a `SIGKILL`ed runner, or a pre-#3394 `auto_preview` dispatch) can never silently become the next run's validated target:

- **`scripts/dev preview` reclaims the port before provisioning.** It runs the same `_down_port_fallback` reclaim `scripts/dev down` uses ([#1675](https://github.com/kuhlman-labs/fishhawk/issues/1675)): a stale `fishhawkd`-named listener on the preview port is `TERM`→`KILL`ed and provisioning proceeds; a **foreign** holder fails loud — `preview port <port> already has a foreign listener — run 'scripts/dev preview-down', or kill the pid, then retry` — and is never killed.
- **The identity gate fails closed on a SHA mismatch.** A squatter serving a different `git_sha` (or a `-dirty` one) is classified `stale` and the stage fails pre-spawn, category C, `acceptance_target_stale` (`TestAcceptanceTargetGate_Stale`), never validated.

Both are on the `scripts/dev` / runner-gate side; there is no separate reclaim step in the verb.

## Event vocabulary

The runner logs these JSON events (one per line) to its log sink:

| Event | Meaning |
|---|---|
| `acceptance_preview_provisioned` | provision command succeeded; carries `expected_sha` |
| `acceptance_target_verified` | target serves the expected build; carries the observed `git_sha` |
| `acceptance_target_unverified` | target reachable but identity not comparable (proceeds); carries a `reason` |
| `acceptance_preview_provision_failed` | provision hook non-zero exit / timeout (pre-spawn category-C fail) |
| `acceptance_target_stale` | target serves a different (or `-dirty`) build (pre-spawn category-C fail) |
| `acceptance_target_unreachable` | no scheme reached the target, or it never became ready (pre-spawn category-C fail) |
| `acceptance_expected_head_unresolved` | a target is declared but the backend resolved no merge-candidate head, so identity cannot be verified against anything (pre-spawn category-C fail, before provisioning) |
| `acceptance_preview_teardown_failed` | teardown hook non-zero exit (advisory; outcome unchanged) |
| `acceptance_preview_teardown_missing` | **advisory**: a provision command is configured but no teardown command is — the provisioned instance will not be torn down |
| `acceptance_tree_provisioned` | the merge-candidate checkout was created; carries `path` + `head_sha` (see below) |
| `acceptance_tree_skipped` | merge-candidate checkout skipped — empty expectation, no dispatch dir, or a non-git dispatch dir (warn-and-proceed) |
| `acceptance_tree_stale_swept` | a leftover checkout dir from a crashed prior run was removed before provisioning |
| `acceptance_tree_fetch_failed` | the head SHA was absent locally and the bare-SHA fetch failed (may still `worktree add` from a reachable object) |
| `acceptance_tree_failed` | `git worktree add` failed (unfetchable/invalid SHA); the agent spawns unprovisioned (warn-and-proceed) |
| `acceptance_tree_removed` | teardown removed the checkout (a `"fallback":"rm_prune"` field marks the `os.RemoveAll` + prune path) |
| `acceptance_tree_teardown_failed` | teardown could not remove the checkout even via the fallback (advisory; outcome unchanged) |

`acceptance_preview_teardown_missing` is a misconfiguration warning, not a failure: it fires only on the path where provisioning actually runs (past the no-hosts and no-expectation skips), and it does **not** block provisioning — an operator whose provision command tears itself down is not affected.

## No-acceptance-stage path

These hooks are invoked **only** by the acceptance stage's pre-spawn gate. A `.fishhawk/workflows.yaml` that declares no acceptance stage never reaches the gate, so `FISHHAWK_ACCEPTANCE_PREVIEW_*` env — set or unset — is inert: no provision, no teardown, no dangling preview instance, no gate events. Setting these variables in an environment that also runs non-acceptance stages (plan, implement, review) is harmless; those stages structurally skip the gate.

## Merge-candidate tree for repository-content criteria

The acceptance agent spawns in a fresh **empty** temp dir (diff-withholding, ADR-049 #4): it has no repository checkout. Some acceptance criteria are nonetheless repository-content criteria — a `verify_hint` naming an in-repository check (Posture B in the acceptance prompt). To evaluate those correctly the runner provisions, after the identity gate passes and **before** the agent spawns, a disposable read-only checkout of the merge candidate:

- **What.** A `git worktree add --detach` of the same `acceptance_expected_head_sha` the identity gate verified, at the run/stage-keyed path `/tmp/fishhawk-acceptance-tree-<run>-<stage>`. The path format is mirrored byte-for-byte between the backend prompt (`prompt.AcceptanceTreePath`) and the runner (`acceptanceTreePath`), so the tree the prompt names is the tree the runner creates.
- **Source.** The checkout is taken against the operator's dispatch `working_dir` — the repo the run's lineage worktrees already hang off — so no network clone is needed on the common path. If the head object is not present locally the runner attempts a bare-SHA `git fetch origin <sha>` first.
- **Why.** Without this tree a Posture B check greps whatever checkout it finds on the host — the dispatch checkout or a lineage worktree — either of which `working_tree_restored` may have detached back to `main`. A reference the PR deletes then appears to remain and a criterion the PR head satisfies false-fails `assertion_fail` (`#1881`). The prompt names this checkout as the ONLY sanctioned tree, forbids evaluating a repository-content criterion against any other checkout, and instructs the agent to mark the criterion `skipped` when the sanctioned tree is absent.
- **Warn-and-proceed.** Provisioning **never** fails the stage. An empty expectation (a pre-#1569 backend), a non-git dispatch dir (e.g. a CI runner with no local checkout), or an unfetchable SHA emits `acceptance_tree_skipped` / `acceptance_tree_failed` and the agent spawns unprovisioned — an honest skipped criterion beats a false `assertion_fail`, and the preview-target criteria are unaffected.
- **Teardown.** The checkout is removed on **every** post-provision return (`git worktree remove --force`, with a `git worktree unlock` + `os.RemoveAll` + `git worktree prune` fallback that survives the macOS `/tmp`→`/private/tmp` symlink registration mismatch and — via the unlock, which a plain prune would skip — a locked worktree entry), emitting `acceptance_tree_removed` or `acceptance_tree_teardown_failed`; teardown is best-effort and never changes the stage outcome.

The agent's `WorkingDir` is unchanged — it remains the fresh empty temp dir. The merge-candidate checkout is a separate, sanctioned tree the agent is pointed at only for repository-content criteria. "Read-only" here is a **prompt-directed convention** the agent is instructed to honor, not a mechanically enforced property: the checkout is a `git worktree` sharing the dispatch repo's git admin dir and object store, so it is not an isolated clone. This does not widen the ADR-050 containment boundary (credential-free env + no MCP token + egress proxy — filesystem isolation was never part of it); pointing the agent at this tree just replaces a wrong-checkout grep with the right one.

## Seeded fixtures (E72.2 / #3326)

A freshly provisioned preview is EMPTY: no runs, no stages, no audit history. Criteria that need "a run parked at the plan gate" or "a trace upload that trips `spend_alert`" therefore cannot be driven from the preview's public surface alone (`POST /v0/runs` needs `write:runs`, which the credential-free acceptance agent does not hold). The dev-only seeded-fixture surface closes that gap:

- **Flag.** The preview binary runs with `--dev-fixtures` / `FISHHAWKD_DEV_FIXTURES=1` and a database (`scripts/dev preview` passes it on the serve line). The flag registers three routes; without it the mux never learns the paths. Under the same flag with `FISHHAWKD_S3_BUCKET` unset the daemon also selects an **in-memory trace store**, so `POST /v0/runs/{id}/trace` → `cost_recorded` → `unpriced_model_alert` / `spend_alert` is drivable on the preview (#1874; previously `503 trace_upload_unconfigured`). **The same flag also puts the daemon in DEV MODE** (E72.13 / #3500, see below): forge writes are DENIED and host-dispatch is refused for every caller, which a one-line `DEV MODE ACTIVE` `WARN` names at boot (E45.76 / #3601). If the in-memory trace store is the only thing you want, `--dev-trace-store` / `FISHHAWKD_DEV_TRACE_STORE=1` selects it WITHOUT mounting a dev surface, so forge writes stay live. Never enable either on a production deployment.
- **Routes.** `GET /v0/dev/fixtures` lists the catalog (`{scenarios: [{name, description}]}`); `POST /v0/dev/fixtures {"scenario": "<name>"}` materializes one scenario through the run / artifact / audit / approval repositories and answers `201 {scenario, runs: {<run-key>: {run_id, stages: {<stage-key>: <stage_id>}}}}`; `POST /v0/dev/sign` signs a raw bundle body with the base64 `private_key` from `POST /v0/runs/{id}/signing-key` (header `X-Fishhawk-Dev-Private-Key`) and returns the hex `signature` for `X-Fishhawk-Signature` — the sandbox is not assumed to carry Ed25519 tooling. All three are loopback-only (`403 dev_surface_loopback_only` otherwise; the runner's egress proxy dials the preview from 127.0.0.1, so the sandbox qualifies) and credential-less (no cookie, no bearer, no CSRF token — the csrf middleware passes a session-less identity, so there is no exemption entry to maintain).
- **Catalog names** (closed set, `backend/internal/devfixtures/catalog`): `grooming-confirm-gate` (a `backlog_grooming` run with groom succeeded and confirm parked at `awaiting_approval`), `plan-gate-parked` (a `feature_change` run with the plan stage parked at `awaiting_approval` carrying a valid `standard_v1` plan artifact), `trace-upload-target` (a `feature_change` run with the plan stage `dispatched` plus a four-row backdated `cost_recorded` spend baseline), `acceptance-dispatched` (a `feature_change` run with plan/implement/review `succeeded` and the acceptance stage `dispatched` behind a `pull_request_opened` head and an `acceptance_dispatched` anchor; it carries an approved `standard_v1` plan whose `verification.acceptance_criteria` declares two drivable criteria plus a `pull_request` artifact, so the acceptance prompt serves real criteria ids and a signed transcript + verdict shipped to `/acceptance/transcript` then `/acceptance` record a resolvable head — E72.5 / #3329, #3397). Criteria reference a scenario by its CATALOG NAME in `verify_hint`.
- **Fresh ids per apply.** Every POST mints NEW rows and returns their ids — read them from the 201 body; never assume stable ids, and never grep for a seeded run in a list.
- **The spend baseline ages with wall-clock time.** `trace-upload-target`'s four rows are aged 1h5m / 2h / 3h / 4h from apply time. Guaranteed at every seed clock: the 2h / 3h / 4h rows populate three distinct prior hour buckets inside `spendalert`'s 24h window, so a ~$5 upload in the seed hour or the hour after trips (ratio ≈ 3750–5000 against the ≈$0.001 rows). Bucket distinctness beyond that depends on where in the hour the seed lands: the 1h5m row occupies seed-hour minus one only when the seed is at or past :05; an earlier seed (10:02 → 08:57) folds it into the 2h row's bucket, so the youngest populated bucket is then seed-hour minus two — the alert still trips, only the ratio moves. Seed and upload **contiguously** — a long gap moves the baseline out of the comparison. `unpriced_model_alert` dedups once per window across ALL runs, so an exactly-one assertion holds on a fresh preview target only.
- **404 ⇒ not provisioned ⇒ Posture A skip.** A `404` on `GET /v0/dev/fixtures` means the target was NOT started with the surface (an older `scripts/dev`, or a custom provision command that omitted the flag). The acceptance agent marks every seeded criterion `skipped`, never `failed`: the absence of the dev surface is a provisioning fact, not evidence against the change.

Long-form contract (scenario schema, validator refusals, the applier's exact stage walk): `backend/internal/devfixtures/README.md`. Route contract: `docs/api/v0.md` § "Dev fixtures (preview only)".

## Stub forge (E72.3 / #3327)

Seeded fixtures supply Fishhawk-side state; they cannot supply a FORGE. A criterion like "closing the contract child closes the parent issue with a comment" needs a GitHub or GitLab that the preview's webhook receivers, watchers and forge adapters can talk to — and the acceptance sandbox is default-deny: it reaches only the spec-declared egress host (`localhost:8090`), holds no forge credential, and `.fishhawk/**` is in the implement stage's `forbidden_paths`, so a standalone stub on a second port would be unreachable by the very agent the surface exists for. The stub forge therefore lives IN-PROCESS, inside the preview's own `fishhawkd`:

- **Flag.** The preview binary runs with `--dev-stub-forge` / `FISHHAWKD_DEV_STUB_FORGE=1` (`scripts/dev preview` passes it on the serve line beside `FISHHAWKD_DEV_FIXTURES=1`, never on migrate). Under the flag the daemon serves the GitHub and GitLab API subsets the product calls from persistent in-memory state, wires `cfg.GitHub` and the registered `gitlab` forge to that state through an in-process round-tripper (no second port, no second binary), defaults the webhook secrets when the operator set none, and registers the control routes below. It refuses to coexist with a configured GitHub App or GitLab token. Never enable it on a production deployment.
- **Control routes** (loopback-only — `403 dev_surface_loopback_only` otherwise — and credential-less, the same posture as `/v0/dev/fixtures`): `GET /v0/dev/forge` snapshots state (`{github: {issues, pulls}, gitlab: {issues, merge_requests}, requests}` — the issues, pull/merge requests and comments the preview seeded or wrote, plus the arrival-ordered log of every forge API request the preview made); `DELETE /v0/dev/forge` resets to empty (204); `POST /v0/dev/forge/issues {forge, repo, project_id (gitlab only), number, state, state_reason, title, comments: [text]}` seeds an issue (201; `400 validation_failed` on an unknown forge, a non-positive number, or gitlab without `project_id`); `GET /v0/dev/forge/issues?forge=&repo=&project_id=&number=` reads one back with its comments in arrival order (`404 stub_issue_not_found`); `POST /v0/dev/forge/pulls` seeds a pull/merge request (same shape plus `merged`, `merge_commit_sha`, `merged_at`, `head_sha`).
- **Delivery contract.** `POST /v0/dev/forge/deliveries {forge: "github"|"gitlab", event, delivery_id (optional), payload: object}` builds the raw body from `payload`, mints a UUID delivery id when absent, signs it preview-side (`X-Hub-Signature-256` = HMAC-SHA256 over the bytes sent, hex, `sha256=` prefix — GitHub's documented format — plus `X-GitHub-Event` / `X-GitHub-Delivery`; or `X-Gitlab-Token` plus `X-Gitlab-Event` / `X-Gitlab-Event-UUID`), and dispatches the request IN-PROCESS through the preview's real receiver, answering `200 {delivery_id, status, body}` where `status` is the receiver's own HTTP status (202 = accepted; `503 stub_forge_webhook_unconfigured` when that family's secret is empty). The acceptance agent never holds a webhook secret. Because both receivers run their consumers synchronously before answering, every side effect — the parent close, the comment, the `split_parent_closed` audit row — has already happened when the 200 returns: read state immediately, no polling. Required payload fields: a GitHub `issues` payload needs `installation.id`, `repository.full_name`, `action` and `issue.number`; a GitLab `Issue Hook` payload needs `project.id`, `project.path_with_namespace`, `object_attributes.iid` and `object_attributes.action`. The payload bytes are signed and delivered VERBATIM — never re-marshaled — so key order and every numeric value (including identifiers above 2^53, which a float64 round-trip would silently alter) reach the receiver exactly as sent.
- **The `split-parent-linked` recipe.** The parent-close watcher acts only on a `split_children_filed` linkage row it can read, and a fresh preview has none. Seed the scenario first: `POST /v0/dev/fixtures {"scenario": "split-parent-linked"}` materializes two `feature_change` runs, each carrying one global-readable `split_children_filed` row (parent `stub/parent-close#100`, contract child `#103`, `parent_forge` `github` / `gitlab`). Then seed the parent open (`POST /v0/dev/forge/issues {"forge":"github","repo":"stub/parent-close","number":100,"state":"open"}`, or the gitlab twin with `project_id`), deliver the child close (`POST /v0/dev/forge/deliveries` with a GitHub `issues` / `closed` payload for `#103` under `repository.full_name` `stub/parent-close`, or a GitLab `Issue Hook` / `close` for iid 103), and read the parent back via `GET /v0/dev/forge/issues` — expect state `closed` (GitHub with `state_reason` `completed`), exactly one comment naming `#103`, and one `split_parent_closed` row on `GET /v0/audit?category=split_parent_closed`. A redelivery with a fresh `delivery_id` leaves one comment. Without the scenario the delivery is a correct no-op — a receiver 202 with nothing closed is the expected outcome, not a defect.
- **`DELETE /v0/dev/forge` between criteria.** The stub's state and request log persist for the life of the preview process; reset before a criterion whose exactly-one assertion would otherwise count a neighbour's comment or request.
- **404 ⇒ not provisioned ⇒ Posture A skip.** A `404` on `GET /v0/dev/forge` means the target was NOT started with the stub forge (an older `scripts/dev`, or a custom provision command that omitted the flag). The acceptance agent marks every criterion that depends on the stub `skipped`, never `failed`: the absence of the dev surface is a provisioning fact, not evidence against the change. The plan gate's `undecidable_criterion` classifier treats a `verify_hint` naming `/v0/dev/forge` or "stub forge" as sandbox-decidable evidence, so such criteria are not misjudged as needing a live forge.
- **Never in production.** The stub answers with dev constants for the installation token and both webhook secrets; a deployment that never sets the flag is byte-for-byte unaffected (routes unregistered, `cfg.GitHub` untouched, forge registry untouched, host-dispatch admits its normal auth ladder, `/healthz` carries no `dev_mode`, prompt responses carry no `forge_writes`). Either dev flag puts the daemon in **dev mode** (E72.13 / #3500, next section): its host-dispatch spawn marker refuses every caller, `/healthz` advertises `dev_mode: true`, and every prompt response carries `forge_writes: "deny"`.

## Dev-mode spawn refusal + `FISHHAWK_FORGE_WRITES` (E72.13 / #3500)

The preview binds `localhost:8090`, so its `/mcp` route and its `POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch` spawn marker are reachable from the sandbox — and a runner spawned from the preview pushes branches and opens PRs on the REAL forge with the operator's credentials, which is the #3500 escape. Three fail-closed layers close it, none depending on HOW the acceptance agent obtained a credential:

- **Server-side (authoritative).** A `fishhawkd` with either dev surface mounted (`FISHHAWKD_DEV_FIXTURES=1` / `FISHHAWKD_DEV_STUB_FORGE=1` — exactly what `scripts/dev preview` sets) is in dev mode. Its host-dispatch marker refuses EVERY caller — anonymous, an `fhm_` token, a `write:runs` operator token alike — with `403 host_dispatch_refused_dev_mode` (details `{reason: "dev_mode", dev_surfaces: [...]}`), checked before the auth ladder, and appends a `host_dispatch_refused` audit row to the run chain. All four MCP host-spawn verbs (`fishhawk_run_stage` / `dispatch_stage` / `run_children` / `drive_run`, the embedded `/mcp` route included) call that marker fail-closed before `cmd.Start`, so no runner is spawned. `/healthz` advertises `dev_mode: true`; every prompt response carries `forge_writes: "deny"`.
- **Runner-side.** A runner refuses pre-spawn (`runner_failed`, reason `forge_writes_denied`, category C — before any agent invocation, branch push, PR open or corpus push) when ANY of its env carrying `FISHHAWK_FORGE_WRITES=deny`, the fetched prompt carrying `forge_writes: "deny"`, or its backend's `/healthz` advertising `dev_mode: true` (probed once pre-spawn on both the `--fetch-prompt` and `--prompt-file` launch paths, so a direct launch that fetches nothing is covered; unreachable / non-JSON answers fail OPEN — E72.16 / #3510) holds. The acceptance invocation env ALWAYS carries `FISHHAWK_FORGE_WRITES=deny` (a fixed `acceptenv` injection; a `FISHHAWK_ACCEPTANCE_ENV_FISHHAWK_FORGE_WRITES` passthrough is refused), so every descendant runner the agent could spawn inherits the deny; `scripts/dev preview` sets the same variable on the preview daemon's serve line so runners its embedded `/mcp` verbs spawn inherit it too. Contract: `runner/README.md` § "Forge-writes gate".
- **Sandbox egress (defence in depth).** The egress proxy denies, on any admitted host, the plain-HTTP verbs that mint a run, spawn a runner or mint a token (`POST /v0/runs`, host-dispatch, auto-drive, campaign start/run/resume, `POST /v0/tokens`, `/v0/tokens/login`, and `POST /mcp` whose `Mcp-Name` names a spawn tool). Read it honestly: Go clients never proxy loopback targets and the `Mcp-Name` header is client-supplied, so this binds curl-shaped clients only — `runner/internal/egressproxy/README.md` states both bypasses.

For the acceptance agent this means: the preview's spawn verbs are NOT a drivable surface. A criterion that would need a real runner spawned from the preview is `skipped` with the refusal as evidence, never `failed`, and `403 host_dispatch_refused_dev_mode` on the preview is the expected, correct answer — not a defect in the change under test.

Long-form contract (state model, served API subsets, the in-process transport, faults): `backend/internal/forge/stub/README.md`. Route contract: `docs/api/v0.md` § "Stub forge (preview only)". The acceptance prompt's `### Stub forge` section (`backend/internal/prompt/README.md`) states the same rules to the validator.

### OAuth authorization server on the preview (E66.36 / #2473)

`scripts/dev preview` forwards exactly three preview-safe `FISHHAWKD_OAUTH_*`
keys (`FISHHAWKD_OAUTH_ISSUER`, `FISHHAWKD_OAUTH_RESOURCE`,
`FISHHAWKD_OAUTH_REQUIRE_LOOPBACK`) from the operator's `.env` onto the
preview's serve exec line — never a credential-bearing key. The AS is
enabled iff the operator's `.env` carries `FISHHAWKD_OAUTH_ISSUER`; a preview
started without it answers every `/v0/oauth/*` route with `503
oauth_as_unconfigured`, which is a target-capability skip, not a code
failure.

When the AS is enabled, drive the four routes on the **preview's own host**
(`localhost:8090`) directly rather than following the origins the metadata
document advertises — those are the operator's TLS front-end issuer (e.g.
`https://localhost:8443`), which the sandbox's default-deny egress would
refuse. The expected non-503 response per route is: `GET
/.well-known/oauth-authorization-server` → 200 (metadata); the protected
resource metadata route → 200 (PRM); `POST /v0/oauth/authorize` → a 4xx from
the request-validation ladder (malformed/missing parameters, unknown
client, …, never 503); `POST /v0/oauth/token` → a 4xx (invalid grant, unknown
client, …, never 503). Consent and code-binding criteria still need an
authenticated session the sandbox cannot obtain, and stay
`requires_live_validation`.

## Replayable scenario corpus (E72.4 / #3328)

Every drivable criterion that PASSES is persisted as a scenario under `acceptance/scenarios/issue-<N>/<criterion-id>.yaml` (id `scenario:issue-<N>/<criterion-id>`, origin `{issue, pr, run_id, head_sha, recorded_at}` — `pr: 0` means UNKNOWN, never the issue number) and replayed FIRST against every later preview head as a regression pass. The runner side, in order:

- **Prompt-served inputs.** The acceptance prompt response carries `acceptance_run_branch`, `acceptance_pull_request_number`, `acceptance_issue_number`, `acceptance_criteria` and the FULL `acceptance_retired_scenarios` entries (`{id, reason, run_id, pr, retired_at}`). The runner never derives any of these from prose.
- **Drop reporter armed at the fetch.** When retirements are served, a deferred reporter is armed from the PARSED wire response — the instant `FetchPrompt` succeeds, BEFORE the prompt temp file is created and before the version-skew check, the target gate and `provisionAcceptanceTree` (#3396: the prompt-file I/O window is covered; a network failure delivered nothing and arms nothing) — and disarmed ONLY by a successful `acceptance_scenarios_pushed` report (or by proving every served id already sits in `retired.yaml` at HEAD). Every other exit ships outcome `acceptance_scenario_retirement_dropped {retired, reason}` best-effort; the reason names the exit path (`fetch_prompt_failed`, `stage_exited_before_persist`, `acceptance_scenario_removed_without_retirement`, `acceptance_verdict_invalid`, `persist_skipped:no_run_branch`, `persist_refused:dirty_before_write: …`, `persist_refused:symlinked_corpus_path: …`, `persist_failed:scenario_write: …`, `persist_failed:push: …`, `push_report_failed: …`). The backend writes the same category itself, with reason `approval_chain_unreadable` and an EMPTY `retired` list, when the dispatch prompt's approval-chain read fails (the retirements that read would have named are not served that fetch; criteria still fail open) — see the event vocabulary below. A run CANCELLED before its acceptance stage ever fetched the retirements (`POST /v0/runs/{id}/cancel`, either budget tripwire, or a pull request closed without merge) has no runner to report anything, so the backend's cancel sinks write the row themselves with reason `run_cancelled_before_acceptance`, the FULL `retired` entries and a `cancel_source` (#3389; skipped once the acceptance stage is `running` or terminal, where this reporter owns it). Residual, stated: a runner SIGKILLed between the fetch and the deferred report emits nothing.
- **Removal guard (pre-spawn, category-B).** `git diff --name-status --no-renames <merge-base>..HEAD -- acceptance/scenarios/` in the provisioned tree: any status other than a pure addition (`A`) — DELETED, MODIFIED, or TYPE-CHANGED (`T`: the YAML replaced by a symlink, which would otherwise make `scenario.Load` fail and skip replay wholesale) — of a scenario whose id is in neither `retired.yaml@HEAD` nor the served retirements fails `acceptance_scenario_removed_without_retirement`; a `retired.yaml` entry new at HEAD whose id is not among this run's approved retirements fails `acceptance_scenario_retirement_unledgered`. `--no-renames` is load-bearing — rename detection pairs a deletion with a similar new file and would hide it. An unresolvable merge base emits `acceptance_scenario_guard_unresolved` and proceeds.
- **Corpus load + prompt section.** `loadReplayCorpus` reads the corpus and ledger from the provisioned tree, excludes ledger ∪ served retirements, samples with the run id as seed (`FISHHAWK_ACCEPTANCE_REPLAY_MAX_SCENARIOS`, default 25, `0` disables; `FISHHAWK_ACCEPTANCE_REPLAY_TIME_CAP_SECS`, default 600), appends the `### Regression corpus` section to the prompt and re-reads it into the invocation. That section also instructs the agent that every `steps_taken` — for a replayed scenario and for a criterion — must be a COMPLETE standalone recipe, never a back-reference to another listed scenario, because a re-record rewrites the referenced file and the referent disappears (#3412). The verdict is validated against criteria ids ∪ served scenario ids, then the `replay` object `{cap, corpus_size, served, sampled_out, retired_excluded, seed, scenarios}` is injected AFTER validation and BEFORE redaction — only when the corpus held at least one scenario; an empty/absent corpus ships the body unchanged and the backend records `replay: null` (the runner log's `acceptance_replay_corpus_loaded` still carries the zero counts).
- **Persist after the ship.** After `acceptance_shipped`: a passed verdict is Composed and Written (skipped with `acceptance_scenario_record_skipped reason=no_issue_number` when the run has no issue trigger — the PR number is never substituted). A re-record of an existing scenario id AMENDS the prior file rather than replacing it (#3412): the richer prior `steps` recipe is kept (a `not recorded by the validator` fallback on either side never wins), the new pass supplies the whole `origin`, `statement`, `verify_hint`, `seed` and any non-empty `assertions`, and when kept prior steps displace genuine new ones the file discloses it via `steps_carried_from` so it cannot assert a fresh origin beside older steps (`acceptance_scenario_amended {steps_kept}`; an undecodable prior file is replaced as before, `acceptance_scenario_amend_skipped`). The served retirements are merged into `retired.yaml` on every verdict, reason preserved; ONLY bytes the runner wrote this pass can enter the commit — the tree must be clean ANYWHERE before the first write (a planted corpus file or a hand-edited `retired.yaml` is `persist_refused dirty_before_write`, since the agent's WorkingDir is hygiene, not a filesystem boundary), a COMMITTED symlink at `acceptance/` or `acceptance/scenarios/` is `persist_refused symlinked_corpus_path: …` naming the component and one below the corpus dir (`issue-<N>/`, a leaf, `retired.yaml`) is refused inside `scenario.Write`/`WriteRetired` as `persist_failed scenario_write:`/`retired_ledger_write:` — checked by `Lstat` BEFORE anything is created, so a symlink cannot redirect a corpus write outside the tree (#3396), and afterwards exactly the written paths are staged by name, never `add -A` over the directory (anything else dirty is `persist_refused dirty_unwritten`); the commit is made in the detached acceptance tree with the run's author and a DCO sign-off under the hardened git config, and pushed PINNED to its SHA to `acceptance_run_branch` (a plain push — a run-branch tip that moved makes it non-fast-forward and `persist_failed`). `acceptance_scenarios_pushed {branch, head_sha, base_sha, scenario_ids, retired}` is then reported. Every non-pushed outcome (`persist_skipped` no_acceptance_tree / no_run_branch / no_remote / unchanged, `persist_refused`, `persist_failed`) is best-effort: the verdict outcome never changes. Because the scenario commit lands AFTER the verdict bound to the pre-scenario head, GitHub's "dismiss stale reviews" dismisses an existing PR approval when it lands — re-approve after the acceptance stage, the same shape as the vouch-commit dismissal.

Event vocabulary added: `acceptance_scenario_retirement_reporter_armed`, `acceptance_scenario_guard_passed` / `acceptance_scenario_guard_unresolved`, `acceptance_replay_inject_failed`, `acceptance_scenario_record_skipped`, `acceptance_scenario_amended {scenario_id, prior_run_id, prior_head_sha, steps_kept}` / `acceptance_scenario_amend_skipped {scenario_id, detail}` (#3412), `acceptance_scenarios_persisted {outcome, reason, head_sha, base_sha, branch, scenario_ids, amended_ids, retired_ids}`, `acceptance_scenarios_pushed`, `acceptance_scenarios_push_report_failed`, `acceptance_scenario_retirement_persisted {how}`, `acceptance_scenario_retirement_dropped` / `_drop_reported` / `_drop_report_failed` / `_drop_unreported`; backend-side (`fishhawkd` log, #3396): `acceptance_retirements_unserved {run_id, stage_id, error}` on either prompt path, with the dispatch path also appending the `acceptance_scenario_retirement_dropped` audit row `reason=approval_chain_unreadable, retired: []`; and (#3389) `acceptance_retirements_cancel_chain_unreadable {run_id, cancel_source, error}` when a run-cancel sink's approval-chain read fails, alongside the cancel sinks' own `acceptance_scenario_retirement_dropped` row `reason=run_cancelled_before_acceptance, cancel_source=operator_cancel | run_budget_exceeded | stage_budget_exceeded | stage_cancelled`. Long-form: `runner/cmd/fishhawk-runner/README.md` § "Replayable scenario corpus"; the YAML shape and sampler: `runner/internal/scenario/README.md`.

## Least-privilege guidance

The provisioned preview runs untrusted merge-candidate code. When the preview shares infrastructure with anything you care about — most commonly a shared Postgres — scope the preview binary's credentials to a throwaway resource, never an admin credential:

- Give the preview a database role that owns **only** a throwaway `<db>_preview` database and is denied `CONNECT` to your real database.
- Reserve the admin credential for the privileged provisioning step; never hand it to the branch binary.

The dogfood `scripts/dev preview` implements exactly this (a normalized non-superuser `fishhawk_preview` role, E31.19 / #1577); mirror the posture in a custom provision command.

## Worked example: docker-compose

A self-contained provision/teardown pair using Docker Compose. It builds and serves the merge candidate at `FISHHAWK_PREVIEW_SHA` and exposes `/healthz` with a matching `git_sha`.

Wire the hooks (runner-process env):

```sh
export FISHHAWK_ACCEPTANCE_PREVIEW_CMD="docker compose -p fishhawk-preview up -d --build"
export FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD="docker compose -p fishhawk-preview down -v"
# optional: widen the budgets if the image build is slow
export FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS=600
export FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS=120
```

`docker-compose.yml` — the build consumes `FISHHAWK_PREVIEW_SHA` (injected by the runner) as a build arg, and the served `/healthz` echoes it back as `git_sha`:

```yaml
services:
  preview:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        # FISHHAWK_PREVIEW_SHA is injected into the provision hook's env by
        # the runner; stamp it into the binary so /healthz can serve it.
        GIT_SHA: ${FISHHAWK_PREVIEW_SHA:?provision hook must receive FISHHAWK_PREVIEW_SHA}
    ports:
      # FISHHAWK_PREVIEW_TARGET_HOST is <host>:<port>; publish that port.
      - "8090:8090"
    environment:
      APP_ADDR: ":8090"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8090/healthz"]
      interval: 2s
      timeout: 3s
      retries: 30
```

Contract the image must satisfy for the runner's identity probe to reach `verified`:

- `GET /healthz` returns `200` with a JSON body containing `git_sha`.
- That `git_sha` is the built commit — a ≥7-character prefix of `FISHHAWK_PREVIEW_SHA` — and is **not** `-dirty`. Build from a clean checkout of the exact SHA so the stamp is the committed identity.
- The service listens on the port named in `FISHHAWK_PREVIEW_TARGET_HOST` (here `8090`, matching the first declared `egress.target_hosts` entry).

Flow at run time: the runner runs the provision command (`docker compose … up -d --build`) with `FISHHAWK_PREVIEW_SHA` in its env → polls `http://<host>:8090/healthz` every 2s → reads `git_sha`, matches it against `FISHHAWK_PREVIEW_SHA` → `verified` → spawns the acceptance agent → after the verdict ships, the deferred teardown (`docker compose … down -v`) removes the containers and volumes.

`down -v` removes the named project's volumes so no preview state persists between runs; `-p fishhawk-preview` isolates the project name so teardown targets exactly what provision created.
