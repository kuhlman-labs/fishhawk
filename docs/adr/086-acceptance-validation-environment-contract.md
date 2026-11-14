---
id: ADR-086
title: "Acceptance validation-environment contract: operator instructions, declared credential bindings, named provisioners, per-run side-effect namespace"
status: accepted
date: 2026-09-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/3791
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-086: Acceptance validation-environment contract: operator instructions, declared credential bindings, named provisioners, per-run side-effect namespace

## Context

Epic: #3324 [E72]. The acceptance stage (ADR-049/ADR-050) validates the running candidate at `egress.target_hosts[0]` against plan acceptance criteria the PLAN AGENT writes per run. There is no operator-authored, repo-stable way to say HOW to validate in a given project (fixture repos, seeded accounts, which flows are end-to-end, how to clean up), and no safe way to validate a product whose behaviour spans an external system (a GitHub/GitLab sandbox org, Stripe test mode, a staging SaaS). The pattern generalizes beyond Fishhawk: every non-trivial project needs the same four things.

Current state (verified 2026-09-27):
- Egress: `egress.target_hosts` IS enforced on acceptance (runner/internal/egressproxy BuildAllowlist = target_hosts + model API + backend; macOS Seatbelt denies direct outbound, runner/internal/netsandbox; Linux is env-only `UnavailableNonDarwin`). Hosts not URLs: admitting `api.github.com` admits every repo — only the TOKEN can scope to a sandbox.
- Target credentials: `FISHHAWK_ACCEPTANCE_ENV_<NAME>` host passthrough (runner/internal/acceptenv/acceptenv.go:46). It REFUSES `GH_TOKEN`/`GITHUB_TOKEN`/`FISHHAWK_*_TOKEN` by design, so a scoped forge token cannot be injected today.
- Ambient credentials: acceptenv allow-lists `HOME`, and Seatbelt restricts network only. So `~/.config/gh/hosts.yml`, `~/.gitconfig` credential helpers, `~/.git-credentials`, the macOS keychain (`gh auth token`, the #3393 vector) and the ssh-agent socket are all readable by the acceptance agent. Egress is the ONLY barrier: adding a forge host to `target_hosts` today makes the operator's own credentials usable (#3393 / #3500 class).
- Redaction (redaction/redaction.go) is pattern-only: no `gho_`/`ghu_`/`ghr_`/`glpat-`, and no redact-by-known-value, so an injected token that misses a pattern lands verbatim in evidence and the raw bundle.
- Provisioning: `FISHHAWK_ACCEPTANCE_PREVIEW_CMD`/`_TEARDOWN_CMD` are runner-host env by the recorded #1953 decision (2026-07-15): "DEPLOYMENT-SIDE operator config, never a workflow-spec/repo-committed surface — the agent must not be able to edit a host-command hook"; restated at docs/acceptance-preview.md:5. Consequence: a project cannot declare its target setup portably; a cloud/CI runner has no way to learn it.
- Side effects: nothing namespaces or cleans up external artifacts per run (#3500 left four real PRs, #3495–#3499, to hand-close).

## Options

A. Status quo + docs. Operators use `target_hosts` + `FISHHAWK_ACCEPTANCE_ENV_*`. Rejected: forge tokens are refused by design, and the ambient-credential hole makes admitting a forge host unsafe.

B. Spec declares WHAT (instructions, credential binding NAMES, a provisioner NAME, the namespace contract); the runner host keeps every secret VALUE and every host COMMAND. Ambient-credential isolation and value-based redaction are preconditions. Preserves #1953.

C. As B but provisioning/cleanup commands inline in the spec. Rejected: reverses #1953 — an agent-editable host command running pre-containment on the runner host.

D. Store customer secrets in fishhawkd (encrypted at rest). Rejected as the default, kept as a deferred last-resort source (f): liability and key management outweigh the UX gain when (b)–(e) exist. Per-run minting (E65.1 #2312) and workload identity are adopted as sources (b)/(c) of decision 2.

## Recommendation

Option B, as five decisions:

1. `instructions` (string) on an acceptance stage in workflow-v2 (additive optional field). Rendered by backend/internal/prompt as a trusted operator section (the approval-conditions pattern: unfenced, byte-capped with refuse-at-validation, proposed 8 KiB) before `### Target instance`. Source is the ADMISSION-TIME spec snapshot (`runs.workflow_spec`, identified by `workflow_sha`), never the candidate tree, so a run cannot edit its own acceptance instructions. `instructions_file` is deferred: no spec field reads a repo file today and ADR-067 excludes cross-file include.

2. `credentials: [{env: <NAME>, secret: <logical_name>}]` on an acceptance stage. The spec names a LOGICAL secret only (`^[a-z][a-z0-9_]{0,63}$`), never a host variable. **Values are scoped per repository and per workflow**: every lookup is keyed by `(repo, workflow, secret)` with fallback to `(repo, secret)`. `repo` and `workflow` are taken from the RUN RECORD (the admitted repo and the admission-selected workflow), never from spec text. There is no unscoped tier, so repo A's spec can never reach repo B's credential.

   **Prefer sources that hold no secret in Fishhawk.** Values come from pluggable SOURCES behind one runner-side resolver interface. The operator maps each `(repo, workflow, secret)` to a source. Build order:
   - (a) **runner-host secrets file** (#3795): `FISHHAWK_ACCEPTANCE_SECRETS_FILE`, 0600 + owner-checked, values by indirection (`from_env`/`from_file`). For the local runner.
   - (b) **GitHub App per-run minting**: fishhawkd mints an installation token for a SANDBOX installation at acceptance spawn, narrowed to named repositories and permissions, TTL sized to the stage budget. It reuses the existing installation-token machinery (backend/internal/server/installationtoken.go) and the E65.1 #2312 mint-at-dispatch design. No new stored secret. Best answer for GitHub targets.
   - (c) **per-run workload identity**: fishhawkd signs a short-lived JWT (claims: `repo`, `workflow`, `run`, `stage`, audience per target), published via its existing JWKS (backend/internal/server/oauthas.go). The RUNNER, never the agent, exchanges it at the customer's Vault / AWS STS / GCP WIF for a credential. Scoping is enforced by the customer's policy on those claims. General answer, including GitLab and cloud targets; Fishhawk holds no customer secret.
   - (d) **secret-manager references** (`vault://`, `aws-sm://`, `gcp-sm://`, `op://`), resolved by the runner using its own identity or the (c) token.
   - (e) CI-platform secrets: for `runner_kind: github_actions`, repository/environment secrets reach the runner env and are mapped by (a)'s indirection. No new code beyond docs.
   - (f) **fishhawkd-held encrypted store** (#3797): DEFERRED, last resort only for teams with no secret manager. Holding customer secrets makes fishhawkd a breach target and needs key management built from scratch, and the cost grows under multi-tenancy (E44).

   Common rules for every source: a declared `env` MAY name a deny-listed variable (`GH_TOKEN`, `GITLAB_TOKEN`) ONLY through this channel, and an ambient runner-process `GH_TOKEN` is never a source. Unresolved → category C pre-spawn naming the logical secret, the scope key and the source tried. Audit records names, scope keys and source type only. Every resolved value is redacted BY VALUE in the verdict, transcript, trace and raw bundle.

   GitLab caveat: no equivalent of App installation tokens (minting a project access token needs a stored maintainer token and has a 1-day minimum expiry), so GitLab targets use (c)/(d) or (a).

3. Ambient isolation precondition: the acceptance agent always runs with a run-scoped synthetic `HOME` (+ `XDG_*`); where Seatbelt is available it additionally denies the ssh-agent socket and keychain (securityd mach-lookup). A stage declaring `credentials` fails closed pre-spawn when isolation cannot be established.

4. `target.provisioner: <name>` names an operator-REGISTERED provisioner (`FISHHAWK_ACCEPTANCE_PROVISIONER_<NAME>_CMD` / `_TEARDOWN_CMD` / timeouts on the runner host). The command text never enters the repo (#1953 holds). Unregistered name → category C naming the missing var. `provisioner: none` declares an externally managed target (e.g. a Vercel/Render preview) where only the identity gate runs. Absent → today's `FISHHAWK_ACCEPTANCE_PREVIEW_CMD` behaviour.

5. Per-run namespace: the runner exports `FISHHAWK_ACCEPTANCE_NAMESPACE` (`fh-acc-<run8>`) and the prompt renders it. The output contract requires every external artifact the agent creates (branch, repo, issue, PR, record) to carry it and be listed in the verdict's `external_artifacts[]`. An optional operator-registered `FISHHAWK_ACCEPTANCE_CLEANUP_CMD` runs after verdict ship with the namespace + artifact list (under `context.WithoutCancel`). A cleanup failure is an audited warning, never a verdict change.

## Decision

**ACCEPTED 2026-09-27 (founder ratified).** Option B, all five decisions as written under Recommendation:

1. `instructions` on an acceptance stage: trusted, capped, taken from the admission-time spec snapshot. `instructions_file` is deferred.
2. `credentials` bindings name a logical secret. Every lookup is keyed by `(repo, workflow, secret)` → `(repo, secret)` from the RUN RECORD, with no unscoped tier. Values come from store-free sources in order: (a) runner-host secrets file, (b) GitHub App per-run minting, (c) fishhawkd-signed per-run workload identity, (d) secret-manager references, (e) CI-platform secrets. (f), the fishhawkd-held encrypted store, is DEFERRED as a last resort.
3. Ambient-credential isolation and value-based redaction are PRECONDITIONS for any credential binding.
4. `target.provisioner` names an operator-registered provisioner. **This re-scopes, and does not reverse, the #1953 decision:** provisioning command text stays deployment-side and is never repo-committed or agent-editable. Only the provisioner NAME moves into the spec.
5. Per-run namespace `FISHHAWK_ACCEPTANCE_NAMESPACE`, verdict `external_artifacts[]`, and an operator-registered post-verdict cleanup hook running with the stage's scoped credentials.

Slices: #3792, #3793, #3794, #3795, #3796, #3798, #3799, #3800, #3802, #3803, #3804 (and deferred #3797) under E72 #3324.

## Consequences

- Additive optional workflow-v2 fields: no major bump. Each field slice touches the canonical schema + `scripts/sync-schemas`, both spec modules (backend typed structs, whose decode is `DisallowUnknownFields`; CLI raw-map checks), docs/spec/workflow-v2.md (`TestMissingSchemaFieldNames`) and the generated site reference (`scripts/gen-site-reference`).
- A secret registered for one repo MUST be unresolvable from any other repo's run; this is the load-bearing test of decision 2.
- The credential binding list crosses backend→runner on `promptResponse`/`FetchedPrompt`, so it needs a backend/internal/wirecontract manifest update.
- The `external_artifacts[]` verdict field is an acceptance-artifact schema change.
- The new prompt section must stay above `### Output contract` (`TestBuild_Acceptance_ClosedFieldSet_LockstepWithValidator`).
- docs/acceptance-preview.md, runner/README.md §Acceptance, docs/ARCHITECTURE.md §10 pointer rows.
- Residuals, stated:
  - Token scope is the only repo-level boundary: egress is host-granular.
  - Git over ssh through the proxy is unsupported (HTTPS only).
  - On Linux, network containment remains env-only until E51 (#2133).
  - Cleanup is best-effort; the namespace makes leftovers attributable, not impossible.
- Generalizes to non-Fishhawk repos: nothing in the grammar is forge-specific.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2041} -->
