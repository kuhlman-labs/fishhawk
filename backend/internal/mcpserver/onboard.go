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
type DoctorOutput struct {
	Report OnboardingReadinessReport `json:"report"`
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
// carries a sixth, gitlab_registration).
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
gitlab_registration below):

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
	return nil, DoctorOutput{Report: *report}, nil
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
