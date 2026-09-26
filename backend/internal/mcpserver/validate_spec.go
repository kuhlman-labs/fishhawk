package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ValidateSpecInput is the fishhawk_validate tool's input schema (E45.65 /
// #3579). Precedence mirrors fishhawk_start_run's spec resolution: inline
// WorkflowSpec bytes win; otherwise the spec is discovered from WorkingDir
// (the `.fishhawk/workflows.yaml` walk, bounded by the `.git` dir) or read
// from the explicit SpecFile. At least one of the three is required.
type ValidateSpecInput struct {
	WorkflowSpec string `json:"workflow_spec,omitempty" jsonschema:"inline workflow spec YAML to validate; wins over working_dir and spec_file when set"`
	WorkingDir   string `json:"working_dir,omitempty" jsonschema:"absolute path of the checkout to discover .fishhawk/workflows.yaml in (walks up to the dir holding .git); defaults to the process cwd when only spec_file is omitted. Over the HTTP MCP transport the path must resolve inside an operator-configured allowed checkout root (fishhawkd --mcp-allowed-roots / FISHHAWKD_MCP_ALLOWED_ROOTS, fishhawk-mcp --allowed-roots / FISHHAWK_MCP_ALLOWED_ROOTS); a path outside every root — or any path at all when no root is configured — is refused path_outside_allowed_roots."`
	SpecFile     string `json:"spec_file,omitempty" jsonschema:"explicit path to a workflow spec file; the file must exist. Over the HTTP MCP transport the path must resolve inside an operator-configured allowed checkout root (fishhawkd --mcp-allowed-roots / FISHHAWKD_MCP_ALLOWED_ROOTS, fishhawk-mcp --allowed-roots / FISHHAWK_MCP_ALLOWED_ROOTS); a path outside every root — or any path at all when no root is configured — is refused path_outside_allowed_roots."`
}

// ValidateSpecDiagnostic is one validation failure. Kind is yaml (the input is
// not a YAML document), schema (the document violates the version-routed
// workflow JSON Schema), validation (a semantic rule the schema cannot
// express — stage references, role lookups, stage-binding rules) or other.
// Path is the JSON pointer the validator reported (empty for a yaml failure);
// Workflow, StageIndex and Stage are derived from it so the agent gets the
// failing workflow AND stage without re-parsing prose: Workflow is the
// segment after `/workflows/`, StageIndex the integer after `/stages/`, and
// Stage the `id` of workflows.<wf>.stages[<idx>] read back from the input
// bytes the verb was given. Stage is empty when the pointer carries no stages
// segment, when the id cannot be read from the input, or when the workflow
// declares `extends` (the reported index then addresses the MERGED stage list
// the reuse pass produced, not the author's own list, so reading the author's
// list by index would name the wrong stage). One pointer-less shape is
// still resolved: the workflow-v2 reuse pass rejects a duplicate stage id at
// /workflows/<wf>/stages with no index, so Stage is recovered from the
// rejection's own `duplicate stage id "<id>"` message and StageIndex stays
// absent.
type ValidateSpecDiagnostic struct {
	Kind       string `json:"kind" jsonschema:"one of yaml, schema, validation, other"`
	Path       string `json:"path" jsonschema:"JSON pointer to the offending location; empty for a yaml failure"`
	Workflow   string `json:"workflow,omitempty" jsonschema:"the failing workflow's key, derived from path; empty when path carries no /workflows/ segment"`
	StageIndex *int   `json:"stage_index,omitempty" jsonschema:"the failing stage's index into workflows.<workflow>.stages, derived from path; absent when path carries no /stages/ segment"`
	Stage      string `json:"stage,omitempty" jsonschema:"the failing stage's id, read from the input at workflows.<workflow>.stages[stage_index].id; empty when it cannot be read"`
	Message    string `json:"message" jsonschema:"the validator's message"`
}

// ValidateSpecOutput is the structured verdict. A validation FAILURE is a
// Valid:false result carrying Diagnostics — never a tool error; a tool error
// is reserved for an input the verb could not read at all.
type ValidateSpecOutput struct {
	Valid             bool                     `json:"valid" jsonschema:"true when the spec parses, satisfies the schema and every semantic rule the validator enforces"`
	Source            string                   `json:"source" jsonschema:"inline when workflow_spec was validated, file when a discovered or explicit file was"`
	Path              string                   `json:"path,omitempty" jsonschema:"absolute path of the validated file; absent for inline input"`
	Version           string                   `json:"version,omitempty" jsonschema:"the spec's declared version; set only when valid"`
	Diagnostics       []ValidateSpecDiagnostic `json:"diagnostics" jsonschema:"the validation failures; empty when valid. The validator stops at the FIRST failure, so this carries at most one entry per call"`
	Hint              string                   `json:"hint,omitempty" jsonschema:"an operator hint beyond the diagnostic; today only the stale-binary hint on an unsupported /version"`
	CharterRequiredBy []string                 `json:"charter_required_by" jsonschema:"workflows that produce a grooming_report and therefore REQUIRE a repository charter at run creation — a rule this verb cannot check"`
	Checked           string                   `json:"checked" jsonschema:"what this verb verified"`
	NotChecked        []string                 `json:"not_checked" jsonschema:"what this verb did NOT verify and where each is checked instead"`
}

// validateSpecChecked is the one-sentence statement of what the verb checks.
// It names the validator so the agent can see it is the SAME one
// fishhawk_start_run's local pre-parse and POST /v0/runs run.
const validateSpecChecked = "YAML parse; the version-routed workflow JSON Schema (v0/v1/v2); workflow-v2 removed-forms sweep and defaults/extends reuse resolution; stage-reference resolution (duplicate stage ids, needs, inputs.from_stage); and the server-side stage-binding rules — via backend/internal/spec.ParseBytes, the validator fishhawk_start_run and POST /v0/runs run."

// validateSpecNotChecked enumerates the rules the verb does NOT enforce and
// where each is enforced instead. The charter rule needs the work-management
// layer (workmgmt), which the ADR-064 TestNoBoardReadOnMCPToolSurface
// invariant forbids this package from reaching; reviewer model ids are a
// deployment-oracle question (E45.64 / #3578); deployment wiring is
// fishhawk_doctor's whole job.
func validateSpecNotChecked() []string {
	return []string{
		"the mandatory-charter rule for grooming_report-producing workflows (see charter_required_by): enforced by the CLI `fishhawk validate` and at run creation",
		"reviewer model ids against the deployment's model oracle (E45.64): read fishhawk_doctor's reviewers[].model_status after the spec is merged",
		"deployment wiring (App installation, reviewer providers, token scopes, merge gate): fishhawk_doctor",
	}
}

// registerValidateSpec wires the fishhawk_validate tool (E45.65 / #3579): the
// in-band, pre-commit spec check an MCP-driven onboarding was missing. Before
// it, an agent that wrote fishhawk_init's scaffold could only learn the spec
// was bad after commit → push → merge, because fishhawk_doctor's spec rung
// reads the DEFAULT BRANCH. The verb runs backend/internal/spec.ParseBytes
// in-process — no HTTP call — so it is the same validator fishhawk_start_run's
// local pre-parse and POST /v0/runs already run, not a second implementation.
func registerValidateSpec(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_validate",
		Description: strings.TrimSpace(`
Use this when you have written or edited a .fishhawk/workflows.yaml — after
fishhawk_init, or after correcting a diagnostic — and need to know whether it is
a valid workflow spec BEFORE committing it. It is the local pre-commit check:
it runs the SAME validator fishhawk_start_run's local pre-parse and
POST /v0/runs run (backend/internal/spec.ParseBytes), in-process, with no HTTP
call, so a spec this verb accepts is one run creation accepts on the same
rules. Do NOT loop on fishhawk_doctor instead: its spec rung reads the
DEFAULT BRANCH, so it cannot confirm an uncommitted or unmerged file.

Input, in precedence order: workflow_spec (the inline YAML — wins when set),
else working_dir (the checkout to discover .fishhawk/workflows.yaml in, walking
up to the dir holding .git) and/or spec_file (an explicit path). One of the
three is required. Over the HTTP MCP transport working_dir and spec_file must
resolve inside an operator-configured allowed checkout root, and so must the
file the discovery walk selects, or the call is refused
path_outside_allowed_roots having read nothing; inline workflow_spec reads no
filesystem and is unaffected.

Output is structured, not prose: valid, source (inline|file), path, version,
diagnostics[] {kind: yaml|schema|validation|other, path (JSON pointer),
workflow, stage_index, stage (the failing stage's id, read from the input),
message}, hint (the stale-binary hint when /version is unsupported by this
binary — run /mcp to reconnect), charter_required_by[], checked and
not_checked[]. A validation FAILURE is a valid:false RESULT with diagnostics,
never a tool error; a tool error means the input could not be read (no input,
a missing spec_file, a working_dir with no spec). The validator stops at the
first failure, so diagnostics carries at most one entry — fix it and call again.

What it does NOT check, stated so you do not read valid:true as "ready":
  - the mandatory-charter rule — a workflow producing a grooming_report
    REQUIRES a repository charter, which needs the work-management layer this
    tool cannot reach; charter_required_by[] names those workflows, and the
    rule is enforced by the CLI ` + "`fishhawk validate`" + ` and at run creation.
  - reviewer model ids (E45.64) — a deployment-oracle question; read
    fishhawk_doctor's reviewers[].model_status once the spec is merged.
  - deployment wiring (App installation, reviewer providers, token scopes,
    merge gate) — fishhawk_doctor.

Without MCP, ` + "`fishhawk validate <path>`" + ` is the CLI equivalent. The
fishhawk://onboarding-skill resource walks the full flow (doctor → init →
validate → commit).
`),
	}, resolver.validateSpec)
}

// validateSpec is the fishhawk_validate tool handler. Input precedence is
// inline workflow_spec > discoverSpec(working_dir, spec_file); no input at
// all, an unreadable spec_file, or a working_dir holding no spec are TOOL
// errors (the verb could not read anything to validate). A validation
// failure is a Valid:false RESULT with one classified diagnostic and a nil
// tool error — the failure IS the answer. The spec bytes are never echoed
// back, keeping the response small.
func (r *runResolver) validateSpec(_ context.Context, _ *mcp.CallToolRequest, in ValidateSpecInput) (*mcp.CallToolResult, ValidateSpecOutput, error) {
	out := ValidateSpecOutput{
		Diagnostics:       []ValidateSpecDiagnostic{},
		CharterRequiredBy: []string{},
		Checked:           validateSpecChecked,
		NotChecked:        validateSpecNotChecked(),
	}

	var data []byte
	switch {
	case strings.TrimSpace(in.WorkflowSpec) != "":
		data = []byte(in.WorkflowSpec)
		out.Source = "inline"
	case strings.TrimSpace(in.WorkingDir) != "" || strings.TrimSpace(in.SpecFile) != "":
		dir := strings.TrimSpace(in.WorkingDir)
		// Confinement (E66.63 / #3589) BEFORE discoverSpec, so a refused path
		// is never opened. Both inputs are confined as SUPPLIED; the file the
		// walk selects is confined per-candidate by confineSpecCandidate
		// below, which covers a discovered symlink escape (binding condition
		// 1). The inline workflow_spec arm above is deliberately untouched: it
		// reads no filesystem, so it stays usable over HTTP with no roots
		// configured.
		if err := r.confinePath("working_dir", dir); err != nil {
			return nil, ValidateSpecOutput{}, err
		}
		if err := r.confinePath("spec_file", strings.TrimSpace(in.SpecFile)); err != nil {
			return nil, ValidateSpecOutput{}, err
		}
		if dir == "" {
			dir = "."
		}
		found, err := r.discoverSpecConfined(dir, strings.TrimSpace(in.SpecFile))
		if err != nil {
			return nil, ValidateSpecOutput{}, fmt.Errorf("read workflow spec: %w", err)
		}
		if found == nil {
			return nil, ValidateSpecOutput{}, fmt.Errorf("no %s found walking up from %s to its .git root: pass workflow_spec (inline YAML) or spec_file (an explicit path)", specFileName, dir)
		}
		data = found.Contents
		out.Source = "file"
		out.Path = found.Path
	default:
		return nil, ValidateSpecOutput{}, fmt.Errorf("one of workflow_spec, working_dir or spec_file is required")
	}

	sp, err := spec.ParseBytes(data)
	if err != nil {
		out.Valid = false
		out.Diagnostics = []ValidateSpecDiagnostic{classifySpecError(err, data)}
		// annotateStaleSpecError wraps ONLY the unsupported-/version shape;
		// any other error passes through unchanged, so a changed message is
		// the annotation itself.
		if annotated := annotateStaleSpecError(err); annotated.Error() != err.Error() {
			out.Hint = annotated.Error()
		}
		return nil, out, nil
	}

	out.Valid = true
	out.Version = sp.Version
	for name, wf := range sp.Workflows {
		if spec.WorkflowRequiresCharter(wf) {
			out.CharterRequiredBy = append(out.CharterRequiredBy, name)
		}
	}
	sort.Strings(out.CharterRequiredBy)
	return nil, out, nil
}

// classifySpecError maps a spec.ParseBytes error onto one diagnostic, keyed
// by the concrete error type: *spec.YAMLError → yaml (no pointer),
// *spec.SchemaError → schema, *spec.ValidationError → validation, anything
// else → other with the raw message. The pointer-derived workflow/stage
// fields are filled from the input bytes for the two pointer-bearing kinds.
func classifySpecError(err error, input []byte) ValidateSpecDiagnostic {
	var ye *spec.YAMLError
	var se *spec.SchemaError
	var ve *spec.ValidationError
	switch {
	case errors.As(err, &ye):
		return ValidateSpecDiagnostic{Kind: "yaml", Message: ye.Error()}
	case errors.As(err, &se):
		d := ValidateSpecDiagnostic{Kind: "schema", Path: se.Path, Message: se.Message}
		d.Workflow, d.StageIndex, d.Stage = locateStage(se.Path, input)
		return d
	case errors.As(err, &ve):
		d := ValidateSpecDiagnostic{Kind: "validation", Path: ve.Path, Message: ve.Message}
		d.Workflow, d.StageIndex, d.Stage = locateStage(ve.Path, input)
		if d.Stage == "" && d.StageIndex == nil {
			d.Stage = duplicateStageIDFromMessage(ve.Message)
		}
		return d
	default:
		return ValidateSpecDiagnostic{Kind: "other", Message: err.Error()}
	}
}

// locateStage derives the failing workflow key, stage index and stage id from
// a validator JSON pointer of the shape `/workflows/<wf>[/stages/<idx>/...]`.
// The workflow is the pointer's second segment (RFC 6901-unescaped); the
// stage index is the integer after a `stages` segment directly under it; the
// stage id is read from the INPUT bytes at workflows.<wf>.stages[<idx>].id.
// Every failure to derive yields the zero value for that field rather than an
// error — the diagnostic's path and message still stand on their own.
func locateStage(pointer string, input []byte) (workflow string, stageIndex *int, stage string) {
	if !strings.HasPrefix(pointer, "/workflows/") {
		return "", nil, ""
	}
	segs := strings.Split(pointer[1:], "/")
	if len(segs) < 2 || segs[1] == "" {
		return "", nil, ""
	}
	workflow = unescapePointerSegment(segs[1])
	if len(segs) < 4 || segs[2] != "stages" {
		return workflow, nil, ""
	}
	idx, err := strconv.Atoi(segs[3])
	if err != nil || idx < 0 {
		return workflow, nil, ""
	}
	stageIndex = &idx
	return workflow, stageIndex, stageIDAt(input, workflow, idx)
}

// duplicateStageIDMessagePrefix is the text both duplicate-stage-id
// rejections open with: the stage-reference rule's
// spec.MsgFmtDuplicateStageID (pointer /workflows/<wf>/stages/<idx>/id) and
// the workflow-v2 reuse pass's own check (pointer /workflows/<wf>/stages — NO
// index, because it rejects the author's list before any stage is
// addressed). Derived from the exported format so a reworded rule fails the
// tests that assert on it rather than silently emptying stage.
var duplicateStageIDMessagePrefix = strings.SplitN(spec.MsgFmtDuplicateStageID, "%q", 2)[0]

// duplicateStageIDFromMessage recovers the offending stage id from a
// duplicate-stage-id rejection whose pointer carries no stage index (the
// workflow-v2 reuse pass's shape). The id is the Go-quoted token right after
// the shared prefix; anything else yields "".
func duplicateStageIDFromMessage(msg string) string {
	if !strings.HasPrefix(msg, duplicateStageIDMessagePrefix) {
		return ""
	}
	quoted, err := strconv.QuotedPrefix(msg[len(duplicateStageIDMessagePrefix):])
	if err != nil {
		return ""
	}
	id, err := strconv.Unquote(quoted)
	if err != nil {
		return ""
	}
	return id
}

// stageIDAt reads workflows.<wf>.stages[<idx>].id out of the raw input bytes
// with a minimal, permissive decode — the document already failed the real
// parser, so this decode tolerates anything and returns "" on any miss: the
// document is not a mapping, the workflow is absent, it declares `extends`
// (the reported index addresses the merged stage list, not this one), the
// index is out of range, or the id is not a string.
func stageIDAt(input []byte, workflow string, idx int) string {
	var doc struct {
		Workflows map[string]struct {
			Extends string `yaml:"extends"`
			Stages  []struct {
				ID string `yaml:"id"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(input)).Decode(&doc); err != nil {
		return ""
	}
	wf, ok := doc.Workflows[workflow]
	if !ok || wf.Extends != "" || idx >= len(wf.Stages) {
		return ""
	}
	return wf.Stages[idx].ID
}

// unescapePointerSegment reverses RFC 6901's two escapes (~1 → /, ~0 → ~),
// in that order, so a workflow key carrying either character round-trips.
func unescapePointerSegment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}
