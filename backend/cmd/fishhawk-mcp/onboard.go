package main

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
// case), mirroring fishhawk_file_issue's resolver.
type DoctorInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
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
	WorkflowYAML string `json:"workflow_yaml" jsonschema:"the canonical workflow-v1 preset spec bytes to write to the repo"`
	TargetPath   string `json:"target_path" jsonschema:"the repo-relative path the scaffold should be committed to (.fishhawk/workflows.yaml)"`
}

// registerDoctor wires the fishhawk_doctor tool (E29.6 / #1506): the in-band
// counterpart to the CLI `fishhawk doctor` (E29.4/E29.5). It wraps
// GET /v0/onboarding/readiness so a connecting Claude Code agent can drive a
// conversational "help me onboard a repo" flow — one onboarding engine, another
// frontend. Read-only per ADR-021.
func registerDoctor(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_doctor",
		Description: strings.TrimSpace(`
Use this when onboarding a repository to Fishhawk and you need its first-run
readiness before starting a run — the in-band counterpart to the CLI
` + "`fishhawk doctor`" + ` (E29.4/E29.6). It wraps GET /v0/onboarding/readiness and
returns four server-side-only checks the first feature_change run needs:

  - app     — is the GitHub App installed on the target repo (installation_id
              when it is, a reason when it is not).
  - spec    — the committed .fishhawk/workflows.yaml fetch + parse + validate
              state (source fetched|unavailable, valid, error, note). Only
              meaningful once the app is installed.
  - reviewers — per spec-declared reviewer availability on THIS deployment
              (available, plus a missing_hint naming the env var to set when a
              provider cannot be resolved). Empty when the spec is unavailable
              or invalid.
  - scopes  — whether the caller token holds the run-driving scope subset
              (adequate, required[], missing[]); a cookie-session caller
              bypasses scope enforcement and is adequate by construction.

repo defaults to GITHUB_REPOSITORY env when omitted. The endpoint gates on
AUTHENTICATION only, so a token with a scope gap still gets a report naming its
gap rather than a 403. Pair with fishhawk_init to scaffold a missing spec. Tool
errors: authentication_required (401), validation_failed (400, malformed repo).
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
CLI ` + "`fishhawk init`" + ` (E29.6). It returns the canonical workflow-v1 preset
scaffold for the chosen autonomy tier, generated IN-PROCESS from the backend's
embedded preset library (there is no HTTP generation endpoint):

  - low    — human-led: no operator_agent block, every judgment point pages the
             human.
  - medium — the recommended default: the operator agent may approve / route
             fixup / retry under named conditions; waive and merge stay human.
  - high   — adds may_waive: solo_low and may_merge: gates_resolved_ci_green on
             top of medium.

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
// when omitted (a fast local fail before the HTTP hop when neither is present)
// and delegates the four readiness probes to the backend, mapping the two 4xx
// surfaces onto clean tool errors.
func (r *runResolver) doctor(ctx context.Context, _ *mcp.CallToolRequest, in DoctorInput) (*mcp.CallToolResult, DoctorOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, DoctorOutput{}, fmt.Errorf("repo is required: pass repo as owner/name or set GITHUB_REPOSITORY in the environment")
	}

	report, err := r.api.OnboardingReadiness(ctx, repo)
	if err != nil {
		// Map the two backend 4xx surfaces onto operator-actionable tool
		// errors rather than a bare "HTTP 401 (authentication_required)".
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "authentication_required":
				return nil, DoctorOutput{}, fmt.Errorf("onboarding readiness: %w: set FISHHAWK_API_TOKEN to an authenticated operator token", err)
			case "validation_failed":
				return nil, DoctorOutput{}, fmt.Errorf("onboarding readiness: %w: repo must be in owner/name format", err)
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
