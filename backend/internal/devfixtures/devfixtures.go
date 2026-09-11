// Package devfixtures holds the named fixture scenarios a seeded
// acceptance preview materializes into its target database (E72.2 /
// #3326, folding in #1874).
//
// A scenario is an embedded YAML document declaring runs, stages,
// artifacts, approvals and audit rows by HANDLE (a scenario-local key)
// rather than by id, so every Apply mints fresh rows. This file owns the
// FORMAT: the typed scenario, the embedded set, Load/Parse and the
// fail-closed Validate. Materialization (Apply) lives in apply.go.
//
// The scenario NAME SET is owned by the leaf subpackage
// devfixtures/catalog (stdlib-only) so a pure classifier can consume it;
// TestNames_MatchesEmbeddedScenarioSet pins the two together in both
// directions.
//
// Every refusal in Validate names the offending handle, because the
// only consumer of these errors is a developer editing a YAML file.
package devfixtures

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures/catalog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// scenarioFS carries every shipped scenario. The glob, not a hand-kept
// list, is the embedded set: adding a YAML here is what adds a scenario,
// and the two-way catalog test refuses one the catalog does not name.
//
//go:embed scenarios/*.yaml
var scenarioFS embed.FS

// ErrUnknownScenario is returned by Load for a name outside the catalog.
// The wrapped error message names the known set so a caller (or the
// 404 body the dev route renders from it) tells the reader what IS valid.
var ErrUnknownScenario = errors.New("unknown fixture scenario")

// Scenario is one seeded fixture: the rows Apply materializes, keyed by
// scenario-local handles. Field names mirror the YAML keys.
type Scenario struct {
	// Name is the catalog name; set by Load from the file name, not
	// declared inside the document (a document cannot disagree with the
	// file it lives in).
	Name      string     `yaml:"-"`
	Runs      []Run      `yaml:"runs"`
	Stages    []Stage    `yaml:"stages"`
	Artifacts []Artifact `yaml:"artifacts"`
	Approvals []Approval `yaml:"approvals"`
	Audit     []AuditRow `yaml:"audit"`
}

// Run declares one run row.
type Run struct {
	Key        string `yaml:"key"`
	Repo       string `yaml:"repo"`
	WorkflowID string `yaml:"workflow_id"`
	// WorkflowSpec is the INLINE workflow document the run is pinned to.
	// Validate parses it with the spec package so a scenario cannot ship a
	// spec the product would refuse, and refuses a WorkflowID the spec does
	// not declare.
	WorkflowSpec string `yaml:"workflow_spec"`
	TriggerRef   string `yaml:"trigger_ref"`
	State        string `yaml:"state"`
}

// Stage declares one stage row on a run.
type Stage struct {
	Key              string `yaml:"key"`
	Run              string `yaml:"run"`
	Sequence         int    `yaml:"sequence"`
	Type             string `yaml:"type"`
	ExecutorKind     string `yaml:"executor_kind"`
	ExecutorRef      string `yaml:"executor_ref"`
	RequiresApproval bool   `yaml:"requires_approval"`
	// State is the DECLARED terminal position on the canonical walk
	// pending → dispatched → running → {succeeded | awaiting_approval |
	// failed}; Apply walks TransitionStage to it. Any other stage state is
	// refused because the walk cannot reach it.
	State           string `yaml:"state"`
	FailureCategory string `yaml:"failure_category"`
	FailureReason   string `yaml:"failure_reason"`
}

// Artifact declares one artifact row on a stage. Content is inline JSON
// text; Validate requires it to parse.
type Artifact struct {
	Key           string `yaml:"key"`
	Stage         string `yaml:"stage"`
	Kind          string `yaml:"kind"`
	SchemaVersion string `yaml:"schema_version"`
	Content       string `yaml:"content"`
}

// Approval declares one approval submission on a stage.
type Approval struct {
	Stage           string `yaml:"stage"`
	ApproverSubject string `yaml:"approver_subject"`
	Decision        string `yaml:"decision"`
	Comment         string `yaml:"comment"`
	Surface         string `yaml:"surface"`
}

// AuditRow declares one audit-log entry. Payload is inline JSON text.
// Age is an OPTIONAL Go duration string ("1h5m"); Apply subtracts it from
// its clock so the row's Timestamp is backdated — this is what lets
// trace-upload-target seed a spend baseline in PRIOR hour buckets.
type AuditRow struct {
	Run          string `yaml:"run"`
	Stage        string `yaml:"stage"`
	Category     string `yaml:"category"`
	ActorKind    string `yaml:"actor_kind"`
	ActorSubject string `yaml:"actor_subject"`
	Payload      string `yaml:"payload"`
	Age          string `yaml:"age"`
}

// AgeDuration parses Age. Zero for an absent Age; Validate has already
// refused an unparseable one, so Apply can ignore the error.
func (a AuditRow) AgeDuration() (time.Duration, error) {
	if a.Age == "" {
		return 0, nil
	}
	return time.ParseDuration(a.Age)
}

// Names returns the catalog's sorted scenario name set.
func Names() []string { return catalog.Names() }

// EmbeddedNames lists the scenario names present in the embedded set
// (file stems), sorted. It is the OTHER half of the two-way catalog
// binding: Names() is what the catalog claims, EmbeddedNames() is what
// the binary actually carries.
func EmbeddedNames() ([]string, error) {
	entries, err := fs.ReadDir(scenarioFS, "scenarios")
	if err != nil {
		return nil, fmt.Errorf("read embedded scenarios: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), path.Ext(e.Name())))
	}
	sort.Strings(names)
	return names, nil
}

// Load returns the validated scenario for a catalog name. An unknown name
// wraps ErrUnknownScenario and lists the known set.
func Load(name string) (*Scenario, error) {
	if !catalog.Known(name) {
		return nil, fmt.Errorf("%w %q: known scenarios are %s",
			ErrUnknownScenario, name, strings.Join(catalog.Names(), ", "))
	}
	data, err := scenarioFS.ReadFile("scenarios/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("scenario %q: %w", name, err)
	}
	s.Name = name
	return s, nil
}

// Parse decodes a scenario document (unknown YAML keys refused) and runs
// Validate on it.
func Parse(data []byte) (*Scenario, error) {
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// reachableStageStates is the set of declared stage states Apply's
// canonical walk (pending → dispatched → running → terminal-or-park) can
// reach. Every other run.StageState is refused at validation: a scenario
// must not declare a shape Apply would fail to materialize.
var reachableStageStates = map[run.StageState]struct{}{
	run.StageStatePending:          {},
	run.StageStateDispatched:       {},
	run.StageStateRunning:          {},
	run.StageStateSucceeded:        {},
	run.StageStateAwaitingApproval: {},
	run.StageStateFailed:           {},
}

var knownRunStates = map[run.State]struct{}{
	run.StatePending:   {},
	run.StateRunning:   {},
	run.StateSucceeded: {},
	run.StateFailed:    {},
	run.StateCancelled: {},
}

var knownStageTypes = map[run.StageType]struct{}{
	run.StageTypePlan:       {},
	run.StageTypeImplement:  {},
	run.StageTypeReview:     {},
	run.StageTypeDeploy:     {},
	run.StageTypeAcceptance: {},
}

var knownArtifactKinds = map[artifact.Kind]struct{}{
	artifact.KindPlan:           {},
	artifact.KindPullRequest:    {},
	artifact.KindDeployment:     {},
	artifact.KindAcceptance:     {},
	artifact.KindReleaseNotes:   {},
	artifact.KindGroomingReport: {},
}

var knownActorKinds = map[audit.ActorKind]struct{}{
	audit.ActorAgent:  {},
	audit.ActorUser:   {},
	audit.ActorSystem: {},
}

// Validate fails closed on every shape Apply could not materialize or the
// product would refuse: an unknown run state, stage type, executor kind,
// artifact kind, approval decision/surface or actor kind; a stage state
// the canonical walk cannot reach; a failed stage without a valid
// category; an unknown or duplicate handle; an audit category
// audit.IsKnownCategory rejects; an unparseable age; invalid inline JSON;
// and a workflow_spec the spec package refuses or that does not declare
// the run's workflow_id. Every error names the offending handle.
func (s *Scenario) Validate() error {
	if len(s.Runs) == 0 {
		return errors.New("scenario declares no runs")
	}
	runs := make(map[string]struct{}, len(s.Runs))
	for i, r := range s.Runs {
		if r.Key == "" {
			return fmt.Errorf("runs[%d]: key required", i)
		}
		if _, dup := runs[r.Key]; dup {
			return fmt.Errorf("run %q: duplicate key", r.Key)
		}
		runs[r.Key] = struct{}{}
		if _, ok := knownRunStates[run.State(r.State)]; !ok {
			return fmt.Errorf("run %q: unknown run state %q", r.Key, r.State)
		}
		parsed, err := spec.ParseBytes([]byte(r.WorkflowSpec))
		if err != nil {
			return fmt.Errorf("run %q: workflow_spec: %w", r.Key, err)
		}
		if _, ok := parsed.Workflows[r.WorkflowID]; !ok {
			return fmt.Errorf("run %q: workflow_spec does not declare workflow_id %q", r.Key, r.WorkflowID)
		}
	}

	stages := make(map[string]struct{}, len(s.Stages))
	for i, st := range s.Stages {
		if st.Key == "" {
			return fmt.Errorf("stages[%d]: key required", i)
		}
		if _, dup := stages[st.Key]; dup {
			return fmt.Errorf("stage %q: duplicate key", st.Key)
		}
		stages[st.Key] = struct{}{}
		if _, ok := runs[st.Run]; !ok {
			return fmt.Errorf("stage %q: unknown run handle %q", st.Key, st.Run)
		}
		if _, ok := knownStageTypes[run.StageType(st.Type)]; !ok {
			return fmt.Errorf("stage %q: unknown stage type %q", st.Key, st.Type)
		}
		switch run.ExecutorKind(st.ExecutorKind) {
		case run.ExecutorAgent, run.ExecutorHuman:
		default:
			return fmt.Errorf("stage %q: unknown executor kind %q", st.Key, st.ExecutorKind)
		}
		state := run.StageState(st.State)
		if _, ok := reachableStageStates[state]; !ok {
			return fmt.Errorf("stage %q: stage state %q is not reachable by the canonical walk (pending → dispatched → running → succeeded|awaiting_approval|failed)", st.Key, st.State)
		}
		if state == run.StageStateFailed && !run.FailureCategory(st.FailureCategory).Valid() {
			return fmt.Errorf("stage %q: failed stage requires a failure_category in A–D, got %q", st.Key, st.FailureCategory)
		}
	}

	artifacts := make(map[string]struct{}, len(s.Artifacts))
	for i, a := range s.Artifacts {
		if a.Key == "" {
			return fmt.Errorf("artifacts[%d]: key required", i)
		}
		if _, dup := artifacts[a.Key]; dup {
			return fmt.Errorf("artifact %q: duplicate key", a.Key)
		}
		artifacts[a.Key] = struct{}{}
		if _, ok := stages[a.Stage]; !ok {
			return fmt.Errorf("artifact %q: unknown stage handle %q", a.Key, a.Stage)
		}
		if _, ok := knownArtifactKinds[artifact.Kind(a.Kind)]; !ok {
			return fmt.Errorf("artifact %q: unknown artifact kind %q", a.Key, a.Kind)
		}
		if !json.Valid([]byte(a.Content)) {
			return fmt.Errorf("artifact %q: content is not valid JSON", a.Key)
		}
	}

	for i, ap := range s.Approvals {
		if _, ok := stages[ap.Stage]; !ok {
			return fmt.Errorf("approvals[%d]: unknown stage handle %q", i, ap.Stage)
		}
		if !approval.Decision(ap.Decision).Valid() {
			return fmt.Errorf("approvals[%d] on stage %q: unknown decision %q", i, ap.Stage, ap.Decision)
		}
		if !approval.Surface(ap.Surface).Valid() {
			return fmt.Errorf("approvals[%d] on stage %q: unknown surface %q", i, ap.Stage, ap.Surface)
		}
	}

	for i, row := range s.Audit {
		if _, ok := runs[row.Run]; !ok {
			return fmt.Errorf("audit[%d]: unknown run handle %q", i, row.Run)
		}
		if row.Stage != "" {
			if _, ok := stages[row.Stage]; !ok {
				return fmt.Errorf("audit[%d]: unknown stage handle %q", i, row.Stage)
			}
		}
		if !audit.IsKnownCategory(row.Category) {
			return fmt.Errorf("audit[%d]: unknown audit category %q", i, row.Category)
		}
		if _, ok := knownActorKinds[audit.ActorKind(row.ActorKind)]; !ok {
			return fmt.Errorf("audit[%d]: unknown actor kind %q", i, row.ActorKind)
		}
		if !json.Valid([]byte(row.Payload)) {
			return fmt.Errorf("audit[%d] (%s): payload is not valid JSON", i, row.Category)
		}
		if _, err := row.AgeDuration(); err != nil {
			return fmt.Errorf("audit[%d] (%s): age %q: %w", i, row.Category, row.Age, err)
		}
	}
	return nil
}
