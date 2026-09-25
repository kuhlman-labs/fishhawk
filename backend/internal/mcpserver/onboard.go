package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// DoctorInput is the fishhawk_doctor tool's input schema (E29.6 / #1506).
// Repo falls back to the GITHUB_REPOSITORY env when omitted (the in-runner
// case), mirroring fishhawk_file_issue's resolver, then to GitLab CI's
// CI_PROJECT_PATH (E45.43 / #3348). Forge names the family; omitted, it
// defaults to gitlab only when CI_PROJECT_PATH supplied the repo, else the
// backend resolves it from its account registry (defaulting to github).
type DoctorInput struct {
	Repo  string `json:"repo,omitempty" jsonschema:"target repo: owner/name on GitHub, or a namespace/project path (nested groups allowed) on GitLab; falls back to GITHUB_REPOSITORY then CI_PROJECT_PATH env when omitted"`
	Forge string `json:"forge,omitempty" jsonschema:"forge family of repo: github or gitlab; omitted → gitlab when CI_PROJECT_PATH supplied the repo, otherwise the backend resolves it from the account registry and defaults to github"`
}

// DoctorOutput wraps the readiness report. Kept under a `report` key so the
// client indexes on a stable shape.
//
// RunnerCredentials is a SIBLING of Report, never a field inside it (E45.82 /
// #3617). `report` is the byte-mirror of what fishhawkd served — the
// re-emission tests pin its json tags against the backend's — and this rung is
// computed LOCALLY by this MCP process about its OWN environment. Folding it
// into `report` would render a locally-computed fact as something the daemon
// answered, which is a false claim about provenance.
type DoctorOutput struct {
	Report            OnboardingReadinessReport `json:"report"`
	RunnerCredentials *runnerCredentialsRung    `json:"runner_credentials,omitempty" jsonschema:"computed LOCALLY by this MCP server about its OWN process environment (NOT served by fishhawkd): whether the RUNNER-held GitLab push credential a local runner would inherit is present; reports PRESENCE only and says nothing about a gitlab_ci runner"`
}

// gitLabRunnerPushCredentialEnv is the env var the RUNNER reads its GitLab
// push / MR-open credential from (E45.82 / #3617). It is DUPLICATED here on
// purpose: the runner and backend are separate Go modules with no import edge,
// so the literal already has independent copies in
// runner/cmd/fishhawk-runner/main.go, runner/cmd/fishhawk-runner/gateenv.go,
// runner/internal/agentenv, runner/internal/acceptenv and
// cli/cmd/fishhawk/doctor_verify.go. Each side's tests assert the literal, so a
// rename in one module reddens the other rather than silently decoupling this
// rung from the runner it describes.
//
// Note the near-identical daemon-side name: FISHHAWKD_GITLAB_TOKEN (trailing
// D) is fishhawkd's own REST credential and is what the `app` rung already
// covers. This rung is about the OTHER one.
const gitLabRunnerPushCredentialEnv = "FISHHAWK_GITLAB_TOKEN"

// runnerCredentials rung statuses.
const (
	runnerCredentialPresent       = "present"
	runnerCredentialMissing       = "missing"
	runnerCredentialNotApplicable = "not_applicable"
	runnerCredentialUnknown       = "unknown"
)

// runnerCredentialsScope is the FIXED sentence every report carries. It states
// exactly what the rung observed and, just as importantly, the two things it
// does NOT claim: the token's validity/scope, and anything at all about a
// gitlab_ci runner (binding condition 2 — this process cannot read a CI/CD
// variable on the GitLab instance).
const runnerCredentialsScope = "This rung reads the fishhawk-mcp process's OWN environment, which a LOCAL runner spawned by fishhawk_run_stage / fishhawk_dispatch_stage / fishhawk_drive_run / fishhawk_run_children inherits verbatim (append(os.Environ(), ...)). It reports PRESENCE only — never the value, never the token's validity or scope. It describes the MCP server's spawning environment ONLY (local runner) and does NOT apply to a gitlab_ci runner, whose credential is a CI/CD variable on the GitLab instance that this process cannot see."

// runnerCredentialsRung is the LOCALLY-computed doctor rung answering the
// second of a GitLab run's two credentials (E45.82 / #3617): fishhawkd's own
// FISHHAWKD_GITLAB_TOKEN is covered by the `app` rung, but the RUNNER's
// FISHHAWK_GITLAB_TOKEN was covered by nothing — so a registration that omitted
// it produced a run that planned and implemented correctly and then failed at
// the push, after a complete paid agent pass.
//
// The daemon genuinely cannot answer this: the runner's spawning environment is
// not its own. But the MCP SERVER is that environment on the local channel, so
// the rung reports a fact it actually holds instead of "cannot determine".
//
// Unexported deliberately, like onboardingTraceStore and
// onboardingGitLabRegistration: export_surface_test.go pins the package's
// exported-name baseline, and jsonschema reflection reaches this type through
// the exported DoctorOutput.
type runnerCredentialsRung struct {
	Status      string `json:"status" jsonschema:"present (the variable is set in this process's environment), missing (gitlab forge and the variable is absent or whitespace-only), not_applicable (a github-family report: the runner mints from the App installation broker), or unknown (the report named no forge family, so applicability could not be decided)"`
	Variable    string `json:"variable" jsonschema:"the env var this rung reports on: FISHHAWK_GITLAB_TOKEN, the RUNNER's push/MR-open credential - note the near-identical FISHHAWKD_GITLAB_TOKEN (trailing D) is the DAEMON's REST credential, a different variable covered by the app rung"`
	Forge       string `json:"forge,omitempty" jsonschema:"the forge family this verdict was computed for, echoed from the report; empty when the report named none"`
	Scope       string `json:"scope" jsonschema:"the fixed sentence bounding this rung's claim: whose environment was read, that it is PRESENCE only, and that it says nothing about a gitlab_ci runner"`
	Note        string `json:"note,omitempty" jsonschema:"the human sentence for the not_applicable and unknown verdicts, and the consequence sentence on missing"`
	Remediation string `json:"remediation,omitempty" jsonschema:"on status missing: the operator next step, naming where the variable must live (the environment that launches fishhawk-mcp)"`
}

// runnerCredentials computes the rung from the report's own forge family and
// the injected env seam. Four branches, one per status; a nil getenv is
// tolerated (treated as unset) so no test or embedding path panics — but
// NewServer wires os.Getenv, so the nil fallback never fires in production.
func runnerCredentials(forgeFamily string, getenv func(string) string) *runnerCredentialsRung {
	rung := &runnerCredentialsRung{
		Variable: gitLabRunnerPushCredentialEnv,
		Forge:    strings.TrimSpace(forgeFamily),
		Scope:    runnerCredentialsScope,
	}
	switch rung.Forge {
	case "gitlab":
		value := ""
		if getenv != nil {
			value = getenv(gitLabRunnerPushCredentialEnv)
		}
		if strings.TrimSpace(value) == "" {
			rung.Status = runnerCredentialMissing
			rung.Note = "A gitlab run needs TWO credentials: fishhawkd's FISHHAWKD_GITLAB_TOKEN (the app rung above) and the RUNNER's " + gitLabRunnerPushCredentialEnv + ". This one is absent here, so a local runner spawned from this process would refuse the implement stage at prompt-fetch time (runner_failed, reason gitlab_push_credential_missing) — or, on a runner that predates that refusal, fail at the push after a complete paid agent pass."
			rung.Remediation = "Export " + gitLabRunnerPushCredentialEnv + " (a GitLab access token with `api` scope) in the environment that LAUNCHES fishhawk-mcp — an MCP registration that omits it is the common cause: `claude mcp add fishhawk <cmd> -e " + gitLabRunnerPushCredentialEnv + "=<token>` — then reconnect the MCP server (/mcp) so the new environment takes effect."
			return rung
		}
		rung.Status = runnerCredentialPresent
		rung.Note = "The variable is set in this process's environment, so a local runner spawned from here inherits it. Presence is not validity: this rung never reads the value, so an expired, revoked or wrong-scope token still reports present."
		return rung
	case "github":
		rung.Status = runnerCredentialNotApplicable
		rung.Note = "A github-family run mints its push credential from the GitHub App installation broker (with the local `gh` CLI token as the fallback), so no runner-held env credential applies."
		return rung
	default:
		rung.Status = runnerCredentialUnknown
		rung.Note = "The readiness report named no forge family (an older fishhawkd serves no `forge` key), so whether a runner-held GitLab credential applies could not be decided. This is NOT a finding that the credential is missing."
		return rung
	}
}

// InitInput is the fishhawk_init tool's input schema (E29.6 / #1506). Preset
// selects the autonomy tier; it defaults to "medium" (the recommended default)
// when omitted. Shape selects the repository shape; it defaults to "app" when
// omitted, and is chosen explicitly — never inferred from the working
// directory.
type InitInput struct {
	Preset string `json:"preset,omitempty" jsonschema:"workflow autonomy preset: one of low, medium, high; defaults to medium when omitted"`
	Shape  string `json:"shape,omitempty" jsonschema:"repository shape: app (a repository with a test entrypoint) or config-only (a config- or docs-only repository with none, dropping the verify block and tests_added_or_updated and raising max_files_changed); defaults to app when omitted"`
}

// InitOutput carries the starter workflow spec scaffold. The conversational
// agent writes WorkflowYAML to TargetPath in the target repo — fishhawk_init
// itself writes no file (preset-only; the delta options + the AGENTS.md/CLAUDE.md
// bridge the CLI `fishhawk init` performs are a follow-up, since the
// delta-applying generator lives only in cli/internal/spec).
type InitOutput struct {
	Preset       string `json:"preset" jsonschema:"the resolved preset (echoes the default when the input was omitted)"`
	Shape        string `json:"shape" jsonschema:"the resolved repository shape (echoes app when the input was omitted)"`
	WorkflowYAML string `json:"workflow_yaml" jsonschema:"the canonical workflow-v2 preset spec bytes to write to the repo"`
	TargetPath   string `json:"target_path" jsonschema:"the repo-relative path the scaffold should be committed to (.fishhawk/workflows.yaml)"`
	NextStep     string `json:"next_step" jsonschema:"what to do with the scaffold next: write it, then validate it with fishhawk_validate BEFORE committing (E45.65 / #3579)"`
}

// initNextStep is the fixed next_step sentence fishhawk_init returns (E45.65 /
// #3579). It points the agent at fishhawk_validate for the pre-commit check
// and says why fishhawk_doctor cannot stand in for it: the doctor's spec rung
// reads the DEFAULT BRANCH, so re-running it against an uncommitted file is
// the dead loop the issue reported.
const initNextStep = "Write workflow_yaml to target_path in the checkout, then call fishhawk_validate (working_dir = the checkout, or workflow_spec = these bytes) and fix any diagnostics until valid:true BEFORE committing. Do not re-run fishhawk_doctor to confirm the file: its spec rung reads the DEFAULT BRANCH and stays unavailable until the spec is merged."

// registerDoctor wires the fishhawk_doctor tool (E29.6 / #1506): the in-band
// counterpart to the CLI `fishhawk doctor` (E29.4/E29.5). It wraps
// GET /v0/onboarding/readiness so a connecting Claude Code agent can drive a
// conversational "help me onboard a repo" flow — one onboarding engine, another
// frontend. Read-only per ADR-021. Five checks since #3161, the fifth being the
// merge-gate reconciliation of the published check against the forge — and
// forge-family-aware since E45.43 / #3348 (the GitHub-shaped merge_gate is
// omitted on GitLab by design; since E45.66 / #3580 the fifth check on GitLab
// is the GitLab-shaped gitlab_merge_gate rung, and since E45.68 / #3582 GitLab
// carries a sixth, gitlab_registration). Since E45.75 / #3600 both families
// also carry the deployment-scoped trace_store rung, since E45.90 / #3625 the
// deployment-scoped review_grounding rung, and since E45.94 / #3646 the
// HYBRID-scoped work_item_provider rung (a repo-resolved provider id compared
// against a deployment-registered set).
func registerDoctor(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_doctor",
		Description: strings.TrimSpace(`
Use this when onboarding a repository to Fishhawk and you need its first-run
readiness before starting a run — the in-band counterpart to the CLI
` + "`fishhawk doctor`" + ` (E29.4/E29.6). It wraps GET /v0/onboarding/readiness and
returns five server-side-only checks the first feature_change run needs on
GitHub and six on GitLab (the fifth on GitLab being gitlab_merge_gate and the
sixth gitlab_registration — see merge_gate, gitlab_merge_gate and
gitlab_registration below), plus on both families the deployment-scoped
trace_store and review_grounding rungs and the hybrid-scoped
work_item_provider rung (see below):

  - app     — is the Fishhawk-specific authorization a run needs in place? On
              GitHub: is the GitHub App installed on the target repo
              (installation_id when it is, a reason when it is not). On
              GitLab there is no App: installed is true ONLY when a gitlab
              installation is registered for EXACTLY this project path
              (fishhawkd installation register — the authorization
              POST /v0/runs checks) AND the project resolves with the
              deployment GitLab credential; note says so. The weaker fact is
              its own field: resolvable (gitlab only; absent when the gitlab
              forge is unconfigured, i.e. never read). A resolvable but
              unregistered project reports installed:false with a reason
              pointing at gitlab_registration.remediation; a not-visible
              project reports resolvable:false with a reason naming the
              credential.
  - spec    — the committed .fishhawk/workflows.yaml fetch + parse + validate
              state (source fetched|unavailable, valid, error, note). Only
              meaningful once the app is installed (on GitLab: once the
              project RESOLVED — the spec and gitlab_merge_gate cascades key
              on resolvable, not on registration, so an unregistered project
              still gets its spec read).
  - reviewers — per spec-declared reviewer availability on THIS deployment
              (available, plus a missing_hint naming the env var to set when a
              provider cannot be resolved). Each also carries the model-id
              honesty fields: model_status (verified | rejected | unverifiable),
              model_hint (did-you-mean on a rejection, the verbatim-to-vendor
              warning when unverifiable, or the unpriced $0 note), and priced
              (whether the pricing table knows the model family; computed from
              the resolved deployment default when the spec omits the model,
              absent only when the resolved model id is unavailable, false flags
              usage booked at $0). Empty when the spec is unavailable or invalid.
  - scopes  — whether the caller token holds the run-driving scope subset
              (adequate, required[], missing[]); a cookie-session caller
              bypasses scope enforcement and is adequate by construction.
  - merge_gate — a FORGE-CONFIG read (#3161): does the repo's protection on its
              REAL default branch actually REQUIRE the fishhawk_audit_complete
              Check Run Fishhawk publishes? status is required | not_required |
              unknown. Read it FAIL-CLOSED: "unknown" means the question could
              NOT be settled — no installation, a 403 from a missing
              ` + "`administration: read`" + ` scope, a rulesets endpoint that 404s, a
              ref_name condition this version cannot evaluate, or a transport
              error — and reason names which. An unknown is NOT evidence the
              check is unrequired. When required, sources[] names each surface
              that requires it with its OWN bypass posture; bypassable is true
              only when EVERY requiring source can be bypassed, since a merger
              has to get past all of them. The key is OMITTED ENTIRELY against
              an older fishhawkd that does not serve it — that absence means
              this backend cannot answer, which is NOT the same claim as
              status unknown, and merge_gate is never emitted with an empty
              status. The key is ALSO OMITTED on a GitLab-family report, by
              design: branch protection and rulesets are GitHub-only surfaces
              the backend never reads there, so its absence on GitLab is
              deliberate, not a stale backend — GitLab gets the SEPARATE
              gitlab_merge_gate rung instead.
  - gitlab_merge_gate — GitLab only (E45.66): the GitLab-shaped sibling of
              merge_gate, under its OWN key because it answers a DIFFERENT
              question. GitLab has no per-context required status check, so
              nothing can say whether a named check gates the merge; this rung
              answers what GitLab CAN answer — is the project's REAL default
              branch protected (protected, matched_rules naming EVERY exact or
              wildcard rule that covers it, push/merge access levels as the
              effective union across those rules, allow_force_push) and does
              the project require a successful head pipeline to merge
              (pipeline_must_succeed, plus the informational
              allow_skipped_pipeline and discussions_must_be_resolved).
              status is pipeline_gated | not_pipeline_gated | unknown. Read it
              FAIL-CLOSED exactly like merge_gate: "unknown" means the question
              could NOT be settled — no gitlab forge, project not visible, an
              adapter without protected-branch reads, an unresolved default
              branch, a 403 (the protected_branches API needs at least the
              Maintainer role), a transport error — and reason names which;
              every signal that was never read is ABSENT, never false.
              not_pipeline_gated is a positive finding whose detail names what
              is off (unprotected default branch and/or pipeline not required)
              and remediation names the GitLab settings to change. It is NOT a
              statement that the fishhawk_audit_complete commit status
              individually gates the merge — note says so on every report —
              and approval rules are not read; confirm those by hand. The key
              is OMITTED on a github-family report and against an older
              fishhawkd; absence means no claim, not status unknown.
  - gitlab_registration — GitLab only (E45.68): is an installations row
              registered for EXACTLY this project path? This is the check
              POST /v0/runs performs — through the same registry seam and the
              same exact-path rule — and refuses 422
              gitlab_project_not_registered on, so status registered means a
              run for this path will pass that check. status is registered |
              not_registered | unknown. not_registered is a POSITIVE finding
              (no row, or an ambiguous double registration); read unknown
              FAIL-CLOSED — the registry could NOT answer, either
              registry_unwired (no installation registry / no database on
              this deployment) or registry_lookup_failed, and reason names
              which; unknown is NOT evidence the project is unregistered.
              When registered, installation_ref is the registered
              gitlab:<project_id> (the project a run would act on) and
              resolved_ref the id the path resolved to with the deployment
              credential; ref_matches compares them and is set ONLY when both
              are known — false means the registration is bound to a
              DIFFERENT project than the path resolves to (detail names both
              refs), which a run would silently act on. remediation is a
              copy-pasteable fishhawkd installation register command carrying
              the REAL resolved project id when the path resolved — a human
              action on the fishhawkd host, never performed by an agent. The
              rung does NOT check the account_key binding the webhook
              receiver additionally enforces (note says so). The key is
              OMITTED on a github-family report and against an older
              fishhawkd; absence means no claim, not status unknown.
  - trace_store — DEPLOYMENT-scoped, both families (E45.75): will this
              fishhawkd accept a run's trace bundle, or will POST
              /v0/runs/{id}/trace respond 503 AFTER the agent has run and been
              billed? kind is s3 (durable S3/RustFS) | memory (the
              --dev-fixtures in-memory store: configured but EPHEMERAL, lost on
              restart) | none (configured:false; remediation names
              FISHHAWKD_S3_BUCKET and make s3-init) | other (a non-S3,
              non-memory store, no durability claim). It is a fact about the
              deployment, not the repo, so it NEVER cascades: a not-installed
              repo or an unavailable spec still carries it. The key is ABSENT
              only against an older fishhawkd; absence means the backend
              cannot answer, which is NOT the same claim as configured:false.
  - review_grounding — DEPLOYMENT-scoped, both families (E45.90): does this
              fishhawkd ground its plan- and implement-review agents against an
              exported read-only source tree at the reviewed commit, or leave
              them DIFF-ONLY? enabled:false is the SUPPORTED DEFAULT, not a
              defect — grounding ships DORMANT behind FISHHAWKD_REVIEW_GROUNDING
              — so report it as an opt-in posture, never as a misconfiguration.
              When it is off, a reviewer that cannot settle a diff-invisible
              question downgrades it to UNTRACED / UNESTABLISHED and calibrates
              the severity DOWN, which is why an operator seeing such findings
              wants to know the switch exists. adapters[] states the read bound
              a GROUNDED reviewer runs under, PER ADAPTER, and renders in BOTH
              postures because it is what an operator deciding whether to opt in
              needs. The two bounds are NOT equivalent and must never be
              collapsed into one word: codex is bound=confined (a synthesized
              confined permission profile — an OS-level deny-by-default
              allowlist; an out-of-tree read returns EPERM), claude is
              bound=blocklist (a bounded --disallowed-tools deny-rule list over
              credential roots, enforced at the TOOL layer: defence-in-depth,
              NOT confinement). The table is a STATIC restatement of the shipped
              posture, never a live probe of the adapter argv. It is a fact
              about the deployment, not the repo, so it NEVER cascades: a
              not-installed repo or an unavailable spec still carries it. The
              key is ABSENT only against an older fishhawkd; absence means the
              backend cannot answer, which is NOT the same claim as
              enabled:false.
  - work_item_provider — HYBRID-scoped, both families (E45.94): can this
              deployment file work items for this repo AT ALL? It answers the
              two facts a 501 provider_unimplemented already prints, but
              AHEAD of the call: provider is the work-item provider id the
              REPO's .fishhawk/work-management.yaml conventions resolve to (a
              REPO fact), registered[] is the provider set wired on THIS
              deployment at startup (a DEPLOYMENT fact), and status is their
              INTERSECTION: registered | unregistered | unknown.
              status=unregistered is a FAILURE, not data — report it as one:
              fishhawk_start_campaign, fishhawk_file_issue and the whole
              backlog-grooming loop respond 501 provider_unimplemented on
              this deployment, so do NOT start a campaign before it is fixed.
              missing_hint names the resolved provider's REAL startup env
              vars plus the restart requirement (registration is a STARTUP
              fact: a credential set after boot changes nothing until
              fishhawkd restarts), or — when registered[] is EMPTY, i.e. NO
              provider is wired at all — a distinguished hint naming all
              three credential sets rather than one provider's; an id this
              build has no startup configuration for draws a generic hint
              that fabricates no env var. Read status=unknown FAIL-CLOSED:
              the repo's conventions could NOT be resolved (reason names the
              closed-set class conventions_unresolved), so the question was
              not SETTLED — it is NOT evidence the provider is unregistered,
              and it must never be rendered as a pass. registered[] is served
              even on unknown, because the deployment registry answers when
              the repo's conventions do not. campaign_sources[] (#3658) is the
              campaign sources the resolved provider serves, in the fixed
              order epic_ref, items — ALWAYS an array: [] on unregistered and
              unknown (no capability claim is made), and [] on a registered
              provider means fishhawk_start_campaign refuses 501 in either
              mode (gitlab serves both since #3658; its Premium group epics
              stay refused). Because the resolved provider is
              repo-scoped, this rung is hybrid — but it is set OUTSIDE every
              repo-scoped cascade, so a not-installed repo or an unavailable
              spec still carries it. The key is ABSENT only against an older
              fishhawkd; absence means the backend cannot answer, which is
              NOT the same claim as unregistered.

Alongside the report key — a SIBLING key, never a field inside it — the output
carries runner_credentials, the one rung computed LOCALLY by this MCP server
about its own process environment and NOT served by fishhawkd:

  - runner_credentials — a GitLab run needs TWO credentials: fishhawkd's
              FISHHAWKD_GITLAB_TOKEN (what the app rung covers) and the RUNNER
              process's FISHHAWK_GITLAB_TOKEN, which pushes the run branch and
              opens the merge request. The daemon cannot see the runner's
              environment — but this MCP server IS that environment on the local
              channel: fishhawk_run_stage, fishhawk_dispatch_stage,
              fishhawk_drive_run and fishhawk_run_children each spawn the runner
              with append(os.Environ(), ...), so a local runner inherits this
              process's variables verbatim. status is present | missing |
              not_applicable (a github-family run mints from the App
              installation broker) | unknown (the report named no forge family,
              so applicability could not be decided — NOT a finding that the
              credential is missing). It reports PRESENCE only: never the value,
              and never the token's validity or scope, so an expired or
              wrong-scope token still reports present. It describes the MCP
              server's spawning environment ONLY (local runner) and says NOTHING
              about a gitlab_ci runner, whose credential is a CI/CD variable on
              the GitLab instance this process cannot read. On missing,
              remediation names where the variable must live — the environment
              that LAUNCHES fishhawk-mcp.

The report's forge field names the family that answered (github|gitlab). repo
is owner/name on GitHub, or a namespace/project path on GitLab — nested groups
(group/subgroup/project) are accepted; a nested path on the github family is a
400 telling you to pass forge=gitlab. forge is optional: omitted, the backend
resolves the family from its account registry (the namespace registered via
fishhawkd account create) and defaults to github when the owner is unknown —
so pass forge=gitlab explicitly for a GitLab project whose namespace is not
registered. repo defaults to GITHUB_REPOSITORY env when omitted, then to GitLab
CI's CI_PROJECT_PATH (which also defaults forge to gitlab). The endpoint gates
on AUTHENTICATION only, so a token with a scope gap still gets a report naming
its gap rather than a 403. Pair with fishhawk_init to scaffold a missing spec.
Tool errors: authentication_required (401), validation_failed (400, malformed
repo or forge). The fishhawk://onboarding-skill resource walks the full
onboarding flow (doctor → init → validate → commit); fishhawk_validate is the
pre-commit spec check — this tool's spec rung reads the DEFAULT BRANCH, so it
cannot confirm an uncommitted file.
`),
	}, resolver.doctor)
}

// registerInit wires the fishhawk_init tool (E29.6 / #1506): the in-band
// preset-scaffold counterpart to the CLI `fishhawk init`. It generates the
// starter spec IN-PROCESS via backend/internal/spec.PresetBytes — there is NO
// HTTP generation endpoint (spec generation is CLI-local), and the fishhawk-mcp
// binary is built from the backend module (ADR-021) so it may import
// backend/internal/spec directly (it already does for spec parsing).
func registerInit(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_init",
		Description: strings.TrimSpace(`
Use this when onboarding a repository that has no .fishhawk/workflows.yaml yet
and you need a starter workflow spec to commit — the in-band counterpart to the
CLI ` + "`fishhawk init`" + ` (E29.6). It returns the canonical workflow-v2 preset
scaffold for the chosen autonomy tier, generated IN-PROCESS from the backend's
embedded preset library (there is no HTTP generation endpoint). The three
scaffolds are one shared base plus a single ` + "`autonomy:`" + ` line:

  - low    — human-led: nothing delegated, every judgment point pages the
             human.
  - medium — the recommended default: the operator agent may approve / route
             fixup / retry under named conditions; waive and merge stay human.
  - high   — adds waive (solo low-severity concern) and merge (gates resolved,
             CI green) on top of medium.

preset defaults to medium when omitted. The library also has a SHAPE axis,
selected by the optional shape input and chosen EXPLICITLY — never inferred from
the working directory:

  - app         — the default: a repository with a test entrypoint. The
                  implement stage runs an execution-grounded verifier and
                  requires tests to be added.
  - config-only — a config- or docs-only repository with no test entrypoint:
                  the implement stage omits the verifier (a commented starter
                  only), drops tests_added_or_updated, keeps ci_green and raises
                  max_files_changed so a docs reorganisation fits.

shape defaults to app when omitted; the output echoes the resolved shape. This
tool is PRESET-ONLY: it returns the scaffold bytes for the conversational agent
to write to target_path (.fishhawk/workflows.yaml) — it writes no file itself,
and the delta options (budget / single-reviewer / human-gates) plus the
AGENTS.md/CLAUDE.md bridge the CLI performs are a follow-up. Run fishhawk_doctor
first to see whether a spec is already present. An unknown preset or shape
returns a clean tool error naming the valid values. The output's next_step says what follows: write the bytes, then call
fishhawk_validate on them BEFORE committing — fishhawk_doctor's spec rung reads
the DEFAULT BRANCH, so it cannot confirm an uncommitted file. Read
fishhawk://onboarding-skill for the full walk (doctor → init → validate →
commit).
`),
	}, resolver.init)
}

// doctor is the fishhawk_doctor tool handler. It resolves repo from the env
// when omitted (a fast local fail before the HTTP hop when none is present)
// and delegates the readiness probes to the backend, mapping the two 4xx
// surfaces onto clean tool errors.
//
// Repo ladder: in.Repo > GITHUB_REPOSITORY > CI_PROJECT_PATH (GitLab CI's
// predefined path_with_namespace variable). Forge ladder: in.Forge > "gitlab"
// when CI_PROJECT_PATH supplied the repo > "" (the backend resolves it from
// the account registry, defaulting to github).
func (r *runResolver) doctor(ctx context.Context, _ *mcp.CallToolRequest, in DoctorInput) (*mcp.CallToolResult, DoctorOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	forgeFamily := strings.TrimSpace(in.Forge)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		if repo = strings.TrimSpace(r.getenv("CI_PROJECT_PATH")); repo != "" && forgeFamily == "" {
			forgeFamily = "gitlab"
		}
	}
	if repo == "" {
		return nil, DoctorOutput{}, fmt.Errorf("repo is required: pass repo (owner/name, or a nested namespace/project path for GitLab) or set GITHUB_REPOSITORY / CI_PROJECT_PATH in the environment")
	}

	report, err := r.api.OnboardingReadiness(ctx, repo, forgeFamily)
	if err != nil {
		// Map the two backend 4xx surfaces onto operator-actionable tool
		// errors rather than a bare "HTTP 401 (authentication_required)".
		// The backend's validation message already names the accepted repo
		// shapes per forge and the forge enum, so it is wrapped, not restated.
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "authentication_required":
				return nil, DoctorOutput{}, fmt.Errorf("onboarding readiness: %w: set FISHHAWK_API_TOKEN to an authenticated operator token", err)
			case "validation_failed":
				return nil, DoctorOutput{}, fmt.Errorf("onboarding readiness: %w: check the repo shape for the forge (owner/name on GitHub; namespace/project, nested groups allowed, on GitLab) and that forge is github or gitlab", err)
			}
		}
		return nil, DoctorOutput{}, fmt.Errorf("onboarding readiness: %w", err)
	}
	out := DoctorOutput{Report: *report}
	// The one LOCALLY-computed rung (E45.82 / #3617): keyed on the family the
	// backend reported, answered from this process's own environment. It is
	// attached AFTER the report is copied verbatim, so `report` stays the
	// byte-mirror of what fishhawkd served.
	out.RunnerCredentials = runnerCredentials(report.Forge, r.getenv)
	return nil, out, nil
}

// init is the fishhawk_init tool handler. It resolves the preset (defaulting to
// medium), validates it against the backend's embedded preset set via
// spec.PresetBytes (an unknown preset fails closed with a clean error naming the
// valid tiers), and returns the canonical scaffold bytes IN-PROCESS.
func (*runResolver) init(_ context.Context, _ *mcp.CallToolRequest, in InitInput) (*mcp.CallToolResult, InitOutput, error) {
	preset := strings.TrimSpace(in.Preset)
	if preset == "" {
		preset = string(spec.PresetMedium)
	}
	shape := strings.TrimSpace(in.Shape)
	if shape == "" {
		shape = string(spec.ShapeApp)
	}
	if shape != string(spec.ShapeApp) && shape != string(spec.ShapeConfigOnly) {
		return nil, InitOutput{}, fmt.Errorf("unknown shape %q: want one of app, config-only", shape)
	}
	data, err := spec.PresetShapeBytes(spec.Preset(preset), spec.Shape(shape))
	if err != nil {
		return nil, InitOutput{}, fmt.Errorf("unknown preset %q: want one of low, medium, high", preset)
	}
	return nil, InitOutput{
		Preset:       preset,
		Shape:        shape,
		WorkflowYAML: string(data),
		TargetPath:   specFileName,
		NextStep:     initNextStep,
	}, nil
}
