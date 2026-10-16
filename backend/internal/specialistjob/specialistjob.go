// Package specialistjob is the Go half of the specialist-job contract
// (ADR-097 D2/D6, #4300): the typed request a seat files, the completion the
// runner ships, and the deterministic draft render.
//
// The canonical schema is docs/spec/specialist-job-v1.schema.json; this
// package embeds its scripts/sync-schemas mirror (schemas/) and compiles it
// once at init. Field reference: docs/spec/specialist-job-v1.md.
//
// # Two layers, one contract
//
// The SCHEMA layer carries the closed kind set, the closed per-kind inputs
// and evidence shapes (additionalProperties:false at every object level), the
// list and string bounds, and the head_sha requirement for runner-executed
// kinds.
//
// The GO layer carries the rules JSON Schema cannot express, each with a
// named sentinel so a caller asserts error identity with errors.Is:
//
//   - ErrDuplicateMember: no object repeats a member name, so the schema
//     layer and the typed decode read ONE interpretation of the document.
//   - ErrInvalidControlPath / ErrInvalidMutationForm: the battery
//     counterfactual rules, mirroring backend/internal/plan's
//     checkCounterfactuals.
//   - ErrInvalidPackagePath / ErrInvalidRepoPath: every package and path a
//     job executes or reads is repo-relative, never absolute, never escaping
//     with `..`, and never starting with `-` (flag injection into go test,
//     go list or git).
//   - ErrKindMismatch: a completion's kind must equal the stored job's kind.
//
// SCHEMA-ONLY VALIDATION IS NOT SUFFICIENT. Every consumer calls
// ParseRequest / ParseCompletion.
package specialistjob

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

//go:embed schemas/specialist-job-v1.schema.json
var schemaFS embed.FS

// embeddedSchemaPath is the embed-relative path of the mirrored canonical
// schema. scripts/sync-schemas' specialist-job-* case arm writes it.
const embeddedSchemaPath = "schemas/specialist-job-v1.schema.json"

// schemaID is the canonical document's absolute $id. The compiler registers
// the embedded document under it so validation errors never carry a host path.
const schemaID = "https://fishhawk.dev/spec/specialist-job-v1.schema.json"

// SchemaVersion is the value the schema_version discriminator must carry.
const SchemaVersion = "specialist-job-v1"

// MaxDraftBytes bounds RenderDraft's output (the schema's evidence-draft
// text maxLength).
const MaxDraftBytes = 32 * 1024

// Kind is the closed specialist-job kind set.
type Kind string

// The six kinds of the closed kind enum ($defs.kind).
const (
	KindCounterfactualBattery Kind = "counterfactual_battery"
	KindFaninBuild            Kind = "fanin_build"
	KindOverlapScan           Kind = "overlap_scan"
	KindPlanCrosscheck        Kind = "plan_crosscheck"
	KindInventory             Kind = "inventory"
	KindDraft                 Kind = "draft"
)

// AllKinds lists every kind in schema order.
var AllKinds = []Kind{KindCounterfactualBattery, KindFaninBuild, KindOverlapScan, KindPlanCrosscheck, KindInventory, KindDraft}

// RunnerExecuted reports whether the kind is executed by the runner. Only
// draft is rendered in-process.
func (k Kind) RunnerExecuted() bool { return k != KindDraft }

// Verdict is the job verdict.
type Verdict string

// The three verdicts ($defs.verdict).
const (
	VerdictPass         Verdict = "pass"
	VerdictFail         Verdict = "fail"
	VerdictInconclusive Verdict = "inconclusive"
)

// Seat is the requesting seat (ADR-089). Server-derived, never client-read.
type Seat string

// The two seats ($defs.seat).
const (
	SeatOperator     Seat = "operator"
	SeatFirstOfficer Seat = "first_officer"
)

// RequestScope is the narrow scope a first-officer seat files a request with
// (E85.6 / #4148). POST /v0/specialist-jobs honors it ONLY from a seat
// identity (a first_officer/ token subject); every other caller still needs
// write:stages, so a non-seat token holding only this scope is refused. It is
// not in the operator default scope set.
const RequestScope = "request:specialist_jobs"

// Sentinel errors for the rules the schema cannot express.
var (
	// ErrDuplicateMember: an object repeats a member name. map decoding
	// replaces and struct decoding merges, so the schema layer and the typed
	// decode would read different documents.
	ErrDuplicateMember = errors.New("specialistjob: object repeats a member name")
	// ErrInvalidControlPath: a battery control_path is blank, absolute,
	// contains `..` or a backslash, or names a _test.go file.
	ErrInvalidControlPath = errors.New("specialistjob: invalid counterfactual control_path")
	// ErrInvalidMutationForm: a battery mutation does not declare exactly one
	// well-formed form (find + replace, or patch).
	ErrInvalidMutationForm = errors.New("specialistjob: invalid counterfactual mutation form")
	// ErrInvalidPackagePath: a Go package path is absolute, contains `..`, or
	// starts with `-`.
	ErrInvalidPackagePath = errors.New("specialistjob: invalid package path")
	// ErrInvalidRepoPath: a repo path is absolute, contains `..` or a
	// backslash, or starts with `-`.
	ErrInvalidRepoPath = errors.New("specialistjob: invalid repo-relative path")
	// ErrKindMismatch: a completion's kind differs from the job's kind.
	ErrKindMismatch = errors.New("specialistjob: completion kind does not match the job kind")
	// ErrDraftTooLarge: RenderDraft's output exceeds MaxDraftBytes.
	ErrDraftTooLarge = errors.New("specialistjob: rendered draft exceeds the size bound")
)

// ParseError reports a document that is not well-formed JSON or does not
// decode into the typed structs.
type ParseError struct {
	Msg   string
	Cause error
}

func (e *ParseError) Error() string { return "specialist job parse error: " + e.Msg }
func (e *ParseError) Unwrap() error { return e.Cause }

// SchemaError reports a schema-layer violation. Path is the JSON Pointer of
// the offending instance; for an unknown member it names the member itself
// (e.g. /inputs/bogus).
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("specialist job schema error at %s: %s", e.Path, e.Message)
}

// Subject is what the job is about.
type Subject struct {
	RunID      string `json:"run_id,omitempty"`
	CampaignID string `json:"campaign_id,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Repo       string `json:"repo,omitempty"`
}

// RunRef names a run and the commit range its diff spans.
type RunRef struct {
	RunID   string `json:"run_id"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
}

// BatteryMutation is one counterfactual: the plan-standard-v1
// $defs.counterfactual field set.
type BatteryMutation struct {
	ControlPath string       `json:"control_path"`
	Mutation    MutationForm `json:"mutation"`
	Tests       TestsSpec    `json:"tests"`
	Expect      string       `json:"expect"`
}

// MutationForm is find + replace, or patch. Replace is a pointer so an empty
// replace (delete the match) is distinguishable from an absent one.
type MutationForm struct {
	Find    string  `json:"find,omitempty"`
	Replace *string `json:"replace,omitempty"`
	Patch   string  `json:"patch,omitempty"`
}

// TestsSpec names go test -run selector and package.
type TestsSpec struct {
	Run     string `json:"run"`
	Package string `json:"package"`
}

// Probe is exactly one structured probe.
type Probe struct {
	PathExists   *PathExistsProbe   `json:"path_exists,omitempty"`
	FileContains *FileContainsProbe `json:"file_contains,omitempty"`
	GoTest       *GoTestProbe       `json:"go_test,omitempty"`
}

// PathExistsProbe passes when Path exists at the subject head.
type PathExistsProbe struct {
	Path string `json:"path"`
}

// FileContainsProbe passes when Path contains Literal at the subject head.
type FileContainsProbe struct {
	Path    string `json:"path"`
	Literal string `json:"literal"`
}

// GoTestProbe passes when the named tests pass at the subject head.
type GoTestProbe struct {
	Tests TestsSpec `json:"tests"`
}

// ProbeQuestion is a probe plus its expected outcome (pass | fail).
type ProbeQuestion struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Probe    Probe  `json:"probe"`
	Expect   string `json:"expect"`
}

// BatteryInputs are the counterfactual_battery inputs.
type BatteryInputs struct {
	Mutations []BatteryMutation `json:"mutations,omitempty"`
	Probes    []ProbeQuestion   `json:"probes,omitempty"`
}

// FocusFile is a plan_crosscheck focus file and its expected presence.
type FocusFile struct {
	Path   string `json:"path"`
	Expect string `json:"expect"`
}

// CrosscheckInputs are the plan_crosscheck inputs.
type CrosscheckInputs struct {
	Questions   []ProbeQuestion `json:"questions,omitempty"`
	FocusFiles  []FocusFile     `json:"focus_files,omitempty"`
	CompareRuns []RunRef        `json:"compare_runs,omitempty"`
}

// OverlapInputs are the overlap_scan inputs.
type OverlapInputs struct {
	Runs       []RunRef `json:"runs"`
	WatchPaths []string `json:"watch_paths,omitempty"`
}

// FaninSlice is one slice head to merge.
type FaninSlice struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
}

// FaninInputs are the fanin_build inputs.
type FaninInputs struct {
	Slices   []FaninSlice `json:"slices"`
	Packages []string     `json:"packages"`
}

// Inventory command ids. The host.* ids (#4330) are read-only, argument-free
// host diagnostics: the schema closes their args to the empty object, and the
// runner executes them from a per-OS table without cloning the subject.
const (
	InventoryGitLog  = "git_log"
	InventoryLsFiles = "ls_files"
	InventoryGoList  = "go_list"

	InventoryHostMemory     = "host.memory"
	InventoryHostTopMemory  = "host.top_memory"
	InventoryHostLoad       = "host.load"
	InventoryHostContainers = "host.containers"
)

// InventoryCommands is the closed inventory command set, in schema order. The
// schema's inputs-inventory and evidence-inventory command enums must equal it
// (TestInventoryCommands_MatchSchemaEnums), so the schema layer refuses every
// other id and a host.* id carrying any args member; the path and package
// rules in checkRequestSemantics have nothing to check for a host.* id.
var InventoryCommands = []string{
	InventoryGitLog, InventoryLsFiles, InventoryGoList,
	InventoryHostMemory, InventoryHostTopMemory, InventoryHostLoad, InventoryHostContainers,
}

// InventoryArgs is the union of the per-command args objects; the schema's
// if/then arms admit only the members of the selected command.
type InventoryArgs struct {
	Paths      []string `json:"paths,omitempty"`
	MaxCount   int      `json:"max_count,omitempty"`
	Pathspec   []string `json:"pathspec,omitempty"`
	MaxEntries int      `json:"max_entries,omitempty"`
	Packages   []string `json:"packages,omitempty"`
}

// InventoryInputs are the inventory inputs.
type InventoryInputs struct {
	Command string        `json:"command"`
	Args    InventoryArgs `json:"args"`
}

// OutlineSection is one draft outline section.
type OutlineSection struct {
	Heading string `json:"heading"`
	Body    string `json:"body,omitempty"`
}

// DraftInputs are the draft inputs.
type DraftInputs struct {
	DocumentKind string           `json:"document_kind"`
	Title        string           `json:"title"`
	Brief        string           `json:"brief,omitempty"`
	Outline      []OutlineSection `json:"outline,omitempty"`
}

// Request is a parsed POST /v0/specialist-jobs body. Inputs is the request's
// raw inputs member (recorded verbatim on the audit chain); exactly one of
// the typed pointers, the one matching Kind, is set.
type Request struct {
	SchemaVersion string          `json:"schema_version"`
	Kind          Kind            `json:"kind"`
	Subject       Subject         `json:"subject"`
	Inputs        json.RawMessage `json:"inputs"`

	Battery    *BatteryInputs    `json:"-"`
	Crosscheck *CrosscheckInputs `json:"-"`
	Overlap    *OverlapInputs    `json:"-"`
	Fanin      *FaninInputs      `json:"-"`
	Inventory  *InventoryInputs  `json:"-"`
	Draft      *DraftInputs      `json:"-"`
}

// Completion is the runner's POST .../result body. Evidence stays raw so the
// envelope is a stable wire contract; DecodeEvidence yields the typed form.
// The runner's upload.SpecialistJobCompletion is pinned to these json tags by
// the backend/internal/wirecontract manifest.
type Completion struct {
	SchemaVersion  string          `json:"schema_version"`
	ExecutionNonce string          `json:"execution_nonce"`
	Kind           Kind            `json:"kind"`
	Verdict        Verdict         `json:"verdict"`
	Reason         string          `json:"reason,omitempty"`
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	DurationMS     int64           `json:"duration_ms"`
}

// Requester is the server-derived requesting identity.
type Requester struct {
	Seat      Seat   `json:"seat"`
	ActorKind string `json:"actor_kind"`
	Subject   string `json:"subject"`
}

// ChargedTo is the seat and identity a job's cost is charged to.
type ChargedTo struct {
	Seat    Seat   `json:"seat"`
	Subject string `json:"subject"`
}

// Result is the server-composed typed result ($defs.result).
type Result struct {
	JobID      string          `json:"job_id"`
	Kind       Kind            `json:"kind"`
	Requester  Requester       `json:"requester"`
	Subject    Subject         `json:"subject"`
	Verdict    Verdict         `json:"verdict"`
	Reason     string          `json:"reason,omitempty"`
	Evidence   json.RawMessage `json:"evidence,omitempty"`
	ChargedTo  ChargedTo       `json:"charged_to"`
	DurationMS int64           `json:"duration_ms"`
}

// ProbeEvidence is one probe observation.
type ProbeEvidence struct {
	ID       string `json:"id"`
	Observed string `json:"observed"`
	Expected string `json:"expected"`
	Matched  bool   `json:"matched"`
	Reason   string `json:"reason,omitempty"`
}

// MutationEvidence is one counterfactual observation (E85.3 vocabulary).
type MutationEvidence struct {
	ControlPath    string `json:"control_path"`
	Observed       string `json:"observed"`
	Baseline       string `json:"baseline"`
	Reason         string `json:"reason,omitempty"`
	MutationSHA256 string `json:"mutation_sha256"`
}

// BatteryEvidence is the counterfactual_battery evidence.
type BatteryEvidence struct {
	Mutations []MutationEvidence `json:"mutations"`
	Probes    []ProbeEvidence    `json:"probes"`
}

// OverlapRunEvidence is one run's changed-file set.
type OverlapRunEvidence struct {
	RunID        string   `json:"run_id"`
	Resolved     bool     `json:"resolved"`
	ChangedFiles []string `json:"changed_files"`
	Truncated    bool     `json:"truncated"`
}

// OverlapPair is one pairwise intersection.
type OverlapPair struct {
	RunA      string   `json:"run_a"`
	RunB      string   `json:"run_b"`
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// WatchHit is one watch-path glob hit.
type WatchHit struct {
	RunID     string   `json:"run_id"`
	WatchPath string   `json:"watch_path"`
	Paths     []string `json:"paths"`
}

// OverlapEvidence is the overlap_scan evidence.
type OverlapEvidence struct {
	Runs      []OverlapRunEvidence `json:"runs"`
	Overlaps  []OverlapPair        `json:"overlaps"`
	WatchHits []WatchHit           `json:"watch_hits"`
}

// FaninSliceEvidence is one slice merge outcome.
type FaninSliceEvidence struct {
	Name          string   `json:"name"`
	HeadSHA       string   `json:"head_sha"`
	Outcome       string   `json:"outcome"`
	ConflictPaths []string `json:"conflict_paths"`
}

// FaninPackageEvidence is one package test outcome.
type FaninPackageEvidence struct {
	Package string `json:"package"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// FaninEvidence is the fanin_build evidence.
type FaninEvidence struct {
	Slices   []FaninSliceEvidence   `json:"slices"`
	Packages []FaninPackageEvidence `json:"packages"`
}

// FocusFileEvidence is one focus-file observation.
type FocusFileEvidence struct {
	Path      string   `json:"path"`
	Expect    string   `json:"expect"`
	Observed  string   `json:"observed"`
	Matched   bool     `json:"matched"`
	TouchedBy []string `json:"touched_by"`
}

// CompareRunEvidence is one compare run's overlap with the focus files.
type CompareRunEvidence struct {
	RunID        string   `json:"run_id"`
	Resolved     bool     `json:"resolved"`
	OverlapPaths []string `json:"overlap_paths"`
}

// CrosscheckEvidence is the plan_crosscheck evidence.
type CrosscheckEvidence struct {
	Questions   []ProbeEvidence      `json:"questions"`
	FocusFiles  []FocusFileEvidence  `json:"focus_files"`
	CompareRuns []CompareRunEvidence `json:"compare_runs"`
}

// InventoryEvidence is the inventory evidence.
type InventoryEvidence struct {
	Command   string   `json:"command"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
	ExitCode  int      `json:"exit_code"`
}

// DraftEvidence is the draft evidence.
type DraftEvidence struct {
	Text string `json:"text"`
}

// compiled holds the three $def validators, compiled once at package init.
// A malformed embedded schema panics at process start rather than serving a
// wrong verdict on the first call (the crewmessage idiom).
var compiled = mustCompile()

type compiledDefs struct {
	request, completion, result *jsonschema.Schema
}

func mustCompile() compiledDefs {
	data, err := schemaFS.ReadFile(embeddedSchemaPath)
	if err != nil {
		panic(fmt.Sprintf("specialistjob: read embedded schema %s: %v", embeddedSchemaPath, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("specialistjob: parse embedded schema %s: %v", embeddedSchemaPath, err))
	}
	c := jsonschema.NewCompiler()
	// format is an annotation by default under 2020-12; assert it so the Go
	// layer is never weaker than check-jsonschema on the uuid members.
	c.AssertFormat()
	if err := c.AddResource(schemaID, raw); err != nil {
		panic(fmt.Sprintf("specialistjob: register embedded schema %s: %v", embeddedSchemaPath, err))
	}
	compile := func(def string) *jsonschema.Schema {
		s, err := c.Compile(schemaID + "#/$defs/" + def)
		if err != nil {
			panic(fmt.Sprintf("specialistjob: compile %s: %v", def, err))
		}
		return s
	}
	return compiledDefs{request: compile("request"), completion: compile("completion"), result: compile("result")}
}

// EmbeddedSchema returns the raw bytes of the embedded canonical mirror.
func EmbeddedSchema() []byte {
	data, err := schemaFS.ReadFile(embeddedSchemaPath)
	if err != nil {
		panic(fmt.Sprintf("specialistjob: read embedded schema %s: %v", embeddedSchemaPath, err))
	}
	return data
}

// ParseRequest validates a request body and returns the typed Request. Order:
// the duplicate-member walk, the schema ($defs.request), a strict typed
// decode (DisallowUnknownFields), then the Go semantic rules. It returns
// *ParseError, *SchemaError or an error wrapping one of the sentinels.
func ParseRequest(data []byte) (*Request, error) {
	if err := validateAgainst(compiled.request, data); err != nil {
		return nil, err
	}
	var req Request
	if err := decodeStrict(data, &req); err != nil {
		return nil, err
	}
	if err := decodeInputs(&req); err != nil {
		return nil, err
	}
	if err := checkRequestSemantics(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ParseCompletion validates a runner completion against the stored job's
// kind and returns it. The evidence is strictly decoded into its typed form
// (DecodeEvidence) so a json-tag drift surfaces here. A completion whose kind
// differs from jobKind is refused with ErrKindMismatch even when its evidence
// is schema-valid for its own kind.
func ParseCompletion(data []byte, jobKind Kind) (*Completion, error) {
	if err := validateAgainst(compiled.completion, data); err != nil {
		return nil, err
	}
	var c Completion
	if err := decodeStrict(data, &c); err != nil {
		return nil, err
	}
	if c.Kind != jobKind {
		return nil, fmt.Errorf("%w: completion kind %q, job kind %q", ErrKindMismatch, c.Kind, jobKind)
	}
	if len(c.Evidence) > 0 {
		if _, err := DecodeEvidence(c.Kind, c.Evidence); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// ValidateResult checks a composed Result document against $defs.result.
// fishhawkd calls it on what it composes, so a composition bug fails loudly
// instead of persisting an off-contract record.
func ValidateResult(data []byte) error {
	return validateAgainst(compiled.result, data)
}

// DecodeEvidence strictly decodes raw evidence into the kind's typed struct
// (*BatteryEvidence, *OverlapEvidence, *FaninEvidence, *CrosscheckEvidence,
// *InventoryEvidence or *DraftEvidence).
func DecodeEvidence(k Kind, raw json.RawMessage) (any, error) {
	var target any
	switch k {
	case KindCounterfactualBattery:
		target = &BatteryEvidence{}
	case KindOverlapScan:
		target = &OverlapEvidence{}
	case KindFaninBuild:
		target = &FaninEvidence{}
	case KindPlanCrosscheck:
		target = &CrosscheckEvidence{}
	case KindInventory:
		target = &InventoryEvidence{}
	case KindDraft:
		target = &DraftEvidence{}
	default:
		return nil, &ParseError{Msg: fmt.Sprintf("unknown kind %q", k)}
	}
	if err := decodeStrict(raw, target); err != nil {
		return nil, err
	}
	return target, nil
}

func validateAgainst(s *jsonschema.Schema, data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &ParseError{Msg: "empty document"}
	}
	// Ahead of BOTH decodes: a duplicate member is the one construct the
	// generic and typed decoders read differently.
	if err := rejectDuplicateMembers(data); err != nil {
		return err
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	if err := s.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return schemaErrorFrom(verr)
		}
		return &SchemaError{Path: "/", Message: err.Error()}
	}
	return nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &ParseError{Msg: err.Error(), Cause: err}
	}
	return nil
}

func decodeInputs(req *Request) error {
	var target any
	switch req.Kind {
	case KindCounterfactualBattery:
		req.Battery = &BatteryInputs{}
		target = req.Battery
	case KindPlanCrosscheck:
		req.Crosscheck = &CrosscheckInputs{}
		target = req.Crosscheck
	case KindOverlapScan:
		req.Overlap = &OverlapInputs{}
		target = req.Overlap
	case KindFaninBuild:
		req.Fanin = &FaninInputs{}
		target = req.Fanin
	case KindInventory:
		req.Inventory = &InventoryInputs{}
		target = req.Inventory
	case KindDraft:
		req.Draft = &DraftInputs{}
		target = req.Draft
	default:
		return &ParseError{Msg: fmt.Sprintf("unknown kind %q", req.Kind)}
	}
	return decodeStrict(req.Inputs, target)
}

// rejectDuplicateMembers refuses any object that repeats a member name, at
// any depth. Only ErrDuplicateMember escapes; a malformed document is left to
// json.Unmarshal, and an over-deep one to encoding/json's own depth limit.
func rejectDuplicateMembers(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(dec, 0); err != nil && errors.Is(err, ErrDuplicateMember) {
		return err
	}
	return nil
}

const maxDuplicateScanDepth = 64

var errScanTooDeep = errors.New("specialistjob: document nested too deeply to scan")

func scanValue(dec *json.Decoder, depth int) error {
	if depth > maxDuplicateScanDepth {
		return errScanTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			nameTok, err := dec.Token()
			if err != nil {
				return err
			}
			name, _ := nameTok.(string)
			if seen[name] {
				return fmt.Errorf("%w: %q", ErrDuplicateMember, name)
			}
			seen[name] = true
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return nil
}

// schemaErrorFrom reports the deepest failing instance location (ties go to
// the first in tree order) with the full causal tree as the message. An
// additionalProperties failure names the unknown member in the path.
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	leaf := deepestCause(verr)
	loc := "/" + strings.Join(leaf.InstanceLocation, "/")
	if ap, ok := leaf.ErrorKind.(*kind.AdditionalProperties); ok && len(ap.Properties) > 0 {
		props := append([]string(nil), ap.Properties...)
		sort.Strings(props)
		loc = strings.TrimSuffix(loc, "/") + "/" + props[0]
	}
	return &SchemaError{Path: loc, Message: verr.Error()}
}

func deepestCause(v *jsonschema.ValidationError) *jsonschema.ValidationError {
	if len(v.Causes) == 0 {
		return v
	}
	var best *jsonschema.ValidationError
	for _, c := range v.Causes {
		if d := deepestCause(c); best == nil || len(d.InstanceLocation) > len(best.InstanceLocation) {
			best = d
		}
	}
	return best
}

// checkRequestSemantics applies the Go-only rules.
func checkRequestSemantics(req *Request) error {
	if req.Battery != nil {
		for i, m := range req.Battery.Mutations {
			at := fmt.Sprintf("/inputs/mutations/%d", i)
			if msg := controlPathProblem(m.ControlPath); msg != "" {
				return fmt.Errorf("%w at %s/control_path: %s", ErrInvalidControlPath, at, msg)
			}
			if msg := mutationFormProblem(m.Mutation); msg != "" {
				return fmt.Errorf("%w at %s/mutation: %s", ErrInvalidMutationForm, at, msg)
			}
			if err := checkPackage(at+"/tests/package", m.Tests.Package); err != nil {
				return err
			}
		}
		if err := checkProbes("/inputs/probes", req.Battery.Probes); err != nil {
			return err
		}
	}
	if req.Crosscheck != nil {
		if err := checkProbes("/inputs/questions", req.Crosscheck.Questions); err != nil {
			return err
		}
		for i, f := range req.Crosscheck.FocusFiles {
			if err := checkRepoPath(fmt.Sprintf("/inputs/focus_files/%d/path", i), f.Path); err != nil {
				return err
			}
		}
	}
	if req.Overlap != nil {
		for i, w := range req.Overlap.WatchPaths {
			if err := checkRepoPath(fmt.Sprintf("/inputs/watch_paths/%d", i), w); err != nil {
				return err
			}
		}
	}
	if req.Fanin != nil {
		for i, p := range req.Fanin.Packages {
			if err := checkPackage(fmt.Sprintf("/inputs/packages/%d", i), p); err != nil {
				return err
			}
		}
	}
	if req.Inventory != nil {
		a := req.Inventory.Args
		for i, p := range a.Paths {
			if err := checkRepoPath(fmt.Sprintf("/inputs/args/paths/%d", i), p); err != nil {
				return err
			}
		}
		for i, p := range a.Pathspec {
			if err := checkRepoPath(fmt.Sprintf("/inputs/args/pathspec/%d", i), p); err != nil {
				return err
			}
		}
		for i, p := range a.Packages {
			if err := checkPackage(fmt.Sprintf("/inputs/args/packages/%d", i), p); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkProbes(at string, qs []ProbeQuestion) error {
	for i, q := range qs {
		pat := fmt.Sprintf("%s/%d/probe", at, i)
		switch {
		case q.Probe.PathExists != nil:
			if err := checkRepoPath(pat+"/path_exists/path", q.Probe.PathExists.Path); err != nil {
				return err
			}
		case q.Probe.FileContains != nil:
			if err := checkRepoPath(pat+"/file_contains/path", q.Probe.FileContains.Path); err != nil {
				return err
			}
		case q.Probe.GoTest != nil:
			if err := checkPackage(pat+"/go_test/tests/package", q.Probe.GoTest.Tests.Package); err != nil {
				return err
			}
		}
	}
	return nil
}

// controlPathProblem mirrors backend/internal/plan's
// counterfactualControlPathProblem: the path is trimmed before every check.
func controlPathProblem(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "control_path must be a non-blank repo-relative path"
	case strings.HasPrefix(p, "/"):
		return fmt.Sprintf("control_path %q is absolute", p)
	case strings.Contains(p, `\`):
		return fmt.Sprintf("control_path %q contains a backslash", p)
	case hasDotDotSegment(p):
		return fmt.Sprintf("control_path %q contains a `..` segment", p)
	case strings.HasSuffix(p, "_test.go"):
		return fmt.Sprintf("control_path %q names a test file; name the production control the tests pin", p)
	}
	return ""
}

// mutationFormProblem mirrors backend/internal/plan's
// counterfactualMutationProblem.
func mutationFormProblem(m MutationForm) string {
	hasReplaceForm := m.Find != "" || m.Replace != nil
	switch {
	case m.Patch != "" && hasReplaceForm:
		return "mutation declares both find/replace and patch; declare exactly one form"
	case m.Patch == "" && !hasReplaceForm:
		return "mutation declares no form; declare find + replace, or patch"
	case m.Patch != "":
		return ""
	case m.Replace == nil:
		return "mutation.find without mutation.replace"
	case m.Find == "":
		return "mutation.replace without mutation.find"
	case *m.Replace == m.Find:
		return "mutation.replace equals mutation.find, so the mutation changes nothing"
	}
	return ""
}

func checkPackage(at, p string) error {
	switch {
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w at %s: %q is absolute", ErrInvalidPackagePath, at, p)
	case hasDotDotSegment(p):
		return fmt.Errorf("%w at %s: %q contains a `..` segment", ErrInvalidPackagePath, at, p)
	case strings.HasPrefix(strings.TrimSpace(p), "-"):
		return fmt.Errorf("%w at %s: %q starts with `-`", ErrInvalidPackagePath, at, p)
	}
	return nil
}

func checkRepoPath(at, p string) error {
	switch {
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%w at %s: %q is absolute", ErrInvalidRepoPath, at, p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("%w at %s: %q contains a backslash", ErrInvalidRepoPath, at, p)
	case hasDotDotSegment(p):
		return fmt.Errorf("%w at %s: %q contains a `..` segment", ErrInvalidRepoPath, at, p)
	case strings.HasPrefix(strings.TrimSpace(p), "-"):
		return fmt.Errorf("%w at %s: %q starts with `-`", ErrInvalidRepoPath, at, p)
	}
	return nil
}

func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Canonical draft sections per document kind.
var (
	adrSections   = []string{"Context", "Options", "Recommendation", "Decision", "Consequences"}
	issueSections = []string{"Summary", "Scope", "Done means"}
)

// RenderDraft renders draft inputs into a deterministic markdown skeleton:
// `# title`, the brief, each outline section in order, then every canonical
// section of the document kind the outline did not name (matched
// case-insensitively on the trimmed heading) as a `_TBD_` placeholder. It is
// pure and model-free; the output is bounded by MaxDraftBytes
// (ErrDraftTooLarge above it).
func RenderDraft(in DraftInputs) (string, error) {
	var canonical []string
	switch in.DocumentKind {
	case "adr":
		canonical = adrSections
	case "issue":
		canonical = issueSections
	default:
		return "", fmt.Errorf("specialistjob: unknown draft document_kind %q", in.DocumentKind)
	}
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(strings.TrimSpace(in.Title))
	b.WriteString("\n")
	if brief := strings.TrimSpace(in.Brief); brief != "" {
		b.WriteString("\n")
		b.WriteString(brief)
		b.WriteString("\n")
	}
	named := map[string]bool{}
	for _, s := range in.Outline {
		h := strings.TrimSpace(s.Heading)
		named[strings.ToLower(h)] = true
		b.WriteString("\n## ")
		b.WriteString(h)
		b.WriteString("\n")
		if body := strings.TrimSpace(s.Body); body != "" {
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	for _, c := range canonical {
		if named[strings.ToLower(c)] {
			continue
		}
		b.WriteString("\n## ")
		b.WriteString(c)
		b.WriteString("\n\n_TBD_\n")
	}
	out := b.String()
	if len(out) > MaxDraftBytes {
		return "", fmt.Errorf("%w: %d bytes rendered, at most %d allowed", ErrDraftTooLarge, len(out), MaxDraftBytes)
	}
	return out, nil
}
