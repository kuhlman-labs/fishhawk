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
// when omitted.
type InitInput struct {
	Preset string `json:"preset,omitempty" jsonschema:"workflow autonomy preset: one of low, medium, high; defaults to medium when omitted"`
}

// InitOutput carries the starter workflow spec scaffold. The conversational
// agent writes WorkflowYAML to TargetPath in the target repo — fishhawk_init
// itself writes no file (preset-only; the delta options + the AGENTS.md/CLAUDE.md
// bridge the CLI `fishhawk init` performs are a follow-up, since the
// delta-applying generator lives only in cli/internal/spec).
type InitOutput struct {
	Preset       string `json:"preset" jsonschema:"the resolved preset (echoes the default when the input was omitted)"`
	WorkflowYAML string `json:"workflow_yaml" jsonschema:"the canonical workflow-v2 preset spec bytes to write to the repo"`
	TargetPath   string `json:"target_path" jsonschema:"the repo-relative path the scaffold should be committed to (.fishhawk/workflows.yaml)"`
}

// registerDoctor wires the fishhawk_doctor tool (E29.6 / #1506): the in-band
// counterpart to the CLI `fishhawk doctor` (E29.4/E29.5). It wraps
// GET /v0/onboarding/readiness so a connecting Claude Code agent can drive a
// conversational "help me onboard a repo" flow — one onboarding engine, another
// frontend. Read-only per ADR-021. Five checks since #3161, the fifth being the
// merge-gate reconciliation of the published check against the forge — and
// forge-family-aware since E45.43 / #3348 (four checks on GitLab, where the
// merge gate is omitted by design).
func registerDoctor(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_doctor",
		Description: strings.TrimSpace(`
Use this when onboarding a repository to Fishhawk and you need its first-run
readiness before starting a run — the in-band counterpart to the CLI
` + "`fishhawk doctor`" + ` (E29.4/E29.6). It wraps GET /v0/onboarding/readiness and
returns five server-side-only checks the first feature_change run needs (four
on GitLab — see merge_gate below):

  - app     — is the GitHub App installed on the target repo (installation_id
              when it is, a reason when it is not). On GitLab there is no App:
              installed means the project is resolvable with the deployment
              GitLab credential, and note says so; a not-visible project
              carries a reason naming the fishhawkd installation register
              command.
  - spec    — the committed .fishhawk/workflows.yaml fetch + parse + validate
              state (source fetched|unavailable, valid, error, note). Only
              meaningful once the app is installed (on GitLab: once the
              project resolved).
  - reviewers — per spec-declared reviewer availability on THIS deployment
              (available, plus a missing_hint naming the env var to set when a
              provider cannot be resolved). Empty when the spec is unavailable
              or invalid.
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
              deliberate, not a stale backend.

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
repo or forge).
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

preset defaults to medium when omitted. This tool is PRESET-ONLY: it returns the
scaffold bytes for the conversational agent to write to target_path
(.fishhawk/workflows.yaml) — it writes no file itself, and the delta options
(budget / single-reviewer / human-gates) plus the AGENTS.md/CLAUDE.md bridge the
CLI performs are a follow-up. Run fishhawk_doctor first to see whether a spec is
already present. An unknown preset returns a clean tool error naming the valid
tiers.
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
	data, err := spec.PresetBytes(spec.Preset(preset))
	if err != nil {
		return nil, InitOutput{}, fmt.Errorf("unknown preset %q: want one of low, medium, high", preset)
	}
	return nil, InitOutput{
		Preset:       preset,
		WorkflowYAML: string(data),
		TargetPath:   specFileName,
	}, nil
}
