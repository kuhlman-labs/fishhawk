package spec

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Tests for workflow-v2 same-document reuse (E52.4 / #2216).
//
// Every rule is pinned by a BEHAVIORAL assertion on the SHIPPED resolved
// document or the SHIPPED rejection message, never a presence check — a
// comment-only or no-op edit to the schema or the resolver fails these.
//
// This file is an INTERNAL test (package spec) because the golden-fixture
// comparison calls resolveV2Reuse directly, to capture the document at the
// one point in the pipeline both modules reach identically. The one case that
// needs planreview.ResolveAuthority — condition 1's
// TestParseV2_DeclaredReviewersBlockTakenWhole — lives in spec_test.go
// instead: planreview imports spec, so only the EXTERNAL test package can
// import it back without an import cycle.

// reuseFixtureRelPath / reuseGoldenRelPath are the AC7 cross-validator
// fixture and its golden, read from disk over the module wall so both modules
// exercise the SHIPPED files rather than copies that can drift from them (the
// established groomingExampleRelPath pattern).
const (
	reuseFixtureRelPath = "../../../docs/spec/examples/workflow-v2-reuse.yaml"
	reuseGoldenRelPath  = "../../../docs/spec/examples/workflow-v2-reuse.resolved.json"
)

// --- AC7: the cross-module / cross-boundary integration test ---------------

// TestV2Reuse_SharedFixtureResolvesIdenticallyAcrossValidators is the
// load-bearing test. Scope spans docs/spec (schema + fixture),
// backend/internal/spec and cli/internal/spec, so per-layer units are not
// enough: this module runs its OWN resolution over the shared fixture and
// asserts the result canonicalizes byte-for-byte to the third-party golden.
// cli/internal/spec carries a test of the same name doing the same thing, so
// a backend-only or CLI-only change to the algorithm fails the OTHER module's
// test rather than silently diverging.
//
// It also drives the fixture through the FULL chain — YAML -> version routing
// -> the v2removed sweep -> resolution -> schema validation -> v2shape
// normalization -> the strip -> the typed decode -> semantic Validate — so
// the seam between the schema, the resolver and the graph-shape checks is
// exercised end to end, not just each layer alone.
func TestV2Reuse_SharedFixtureResolvesIdenticallyAcrossValidators(t *testing.T) {
	data, err := os.ReadFile(reuseFixtureRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", reuseFixtureRelPath, err)
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("yaml %s: %v", reuseFixtureRelPath, err)
	}
	if err := resolveV2Reuse(raw); err != nil {
		t.Fatalf("resolveV2Reuse(%s): %v", reuseFixtureRelPath, err)
	}
	got, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal resolved: %v", err)
	}

	goldenBytes, err := os.ReadFile(reuseGoldenRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", reuseGoldenRelPath, err)
	}
	var golden map[string]any
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("decode %s: %v", reuseGoldenRelPath, err)
	}
	// $comment is the golden's file header (JSON has no comments) and is the
	// one key deleted before comparing.
	if _, ok := golden["$comment"]; !ok {
		t.Errorf("%s lost its $comment header; the golden is generated and must document that", reuseGoldenRelPath)
	}
	delete(golden, "$comment")
	want, err := json.Marshal(golden)
	if err != nil {
		t.Fatalf("re-marshal golden: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("resolution diverged from the golden\n got: %s\nwant: %s\nregenerate %s if the change is intentional", got, want, reuseGoldenRelPath)
	}

	// The full chain, on the same shipped fixture.
	s, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", reuseFixtureRelPath, err)
	}
	// AC5 + the done-means: a RESOLVED document validates under every
	// existing semantic rule, including the from_stage graph-shape check
	// against a stage that exists only in the inherited base.
	wf, ok := s.Workflows["hotfix_change"]
	if !ok {
		t.Fatalf("workflows = %v, want hotfix_change", s.Workflows)
	}
	gotIDs := make([]string, 0, len(wf.Stages))
	for _, st := range wf.Stages {
		gotIDs = append(gotIDs, st.ID)
	}
	wantIDs := []string{"propose", "apply", "gate", "verify", "ship"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Errorf("hotfix_change stage ids = %v, want the transitively inherited %v", gotIDs, wantIDs)
	}
}

// --- AC1..AC5: one behavioral case per acceptance criterion ----------------

// TestParseV2_FileDefaultsReviewers_AppliedOnlyWhenStageDeclaresNone is AC1,
// both directions in one table: a stage declaring no reviewers inherits the
// file default; a stage declaring its own keeps it verbatim and is NOT
// supplemented from the default.
func TestParseV2_FileDefaultsReviewers_AppliedOnlyWhenStageDeclaresNone(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
  reviewers:
    human: 1
    agents:
      - provider: claudecode
workflows:
  wf:
    stages:
      - id: inherits
        type: plan
      - id: declares
        type: review
        reviewers:
          human: 1
`
	s := mustParseV2(t, doc)
	stages := s.Workflows["wf"].Stages

	if got := stages[0].Reviewers; got == nil || got.Human != 1 || got.AgentCount() != 1 {
		t.Errorf("inherits.reviewers = %+v, want the file default {human:1, agents:[claudecode]}", got)
	}
	// The declaring stage keeps ONLY what it wrote: no agents supplemented.
	if got := stages[1].Reviewers; got == nil || got.Human != 1 || got.AgentCount() != 0 {
		t.Errorf("declares.reviewers = %+v, want exactly the authored {human:1} with NO agents supplemented", got)
	}
}

// TestParseV2_WorkflowDefaultsOverrideFileDefaults is AC2 and pins the
// ratified precedence ladder rung by rung: file defaults -> extends base ->
// workflow defaults -> the stage's own declaration.
func TestParseV2_WorkflowDefaultsOverrideFileDefaults(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
  budget:
    max_runtime: 45m
workflows:
  base:
    stages:
      - id: apply
        type: implement
  derived:
    extends: base
    defaults:
      executor:
        agent: codex
      budget:
        max_runtime: 60m
    stages:
      - id: own
        type: review
        budget:
          max_runtime: 90m
`
	s := mustParseV2(t, doc)
	stages := s.Workflows["derived"].Stages

	// Rung 3 over rung 2: the workflow default overrides the value the BASE
	// stage resolved to, while the branchless file-default timeout survives.
	if got := stages[0].Executor.Agent; got != "codex" {
		t.Errorf("inherited apply.executor.agent = %q, want the workflow default codex", got)
	}
	if got := stages[0].Executor.Timeout.Duration; got != 30*time.Minute {
		t.Errorf("inherited apply.executor.timeout = %v, want the file default 30m", got)
	}
	if got := stages[0].Budget; got == nil || got.MaxRuntime.Duration != 60*time.Minute {
		t.Errorf("inherited apply.budget = %+v, want the workflow default 60m over the file default 45m", got)
	}
	// Rung 4 over rung 3: the stage's own declaration still wins.
	if got := stages[1].Budget; got == nil || got.MaxRuntime.Duration != 90*time.Minute {
		t.Errorf("own.budget = %+v, want the stage's own 90m over the workflow default 60m", got)
	}
	// And rung 1 still reaches a workflow that declares no defaults of its own.
	if got := s.Workflows["base"].Stages[0].Executor.Agent; got != "claude-code" {
		t.Errorf("base apply.executor.agent = %q, want the file default claude-code", got)
	}
}

// TestParseV2_ExtendsInheritsStagesAndOverridesByID is AC3: an inherited
// stage is present, a named stage's field is overridden IN THE BASE'S
// POSITION, a new stage is appended, and the base's order is preserved.
func TestParseV2_ExtendsInheritsStagesAndOverridesByID(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: first
        type: plan
      - id: second
        type: implement
        executor:
          timeout: 10m
      - id: third
        type: review
        executor:
          human: true
  derived:
    extends: base
    stages:
      - id: second
        type: implement
        executor:
          timeout: 99m
      - id: fourth
        type: review
        executor:
          human: true
`
	s := mustParseV2(t, doc)
	stages := s.Workflows["derived"].Stages

	got := make([]string, 0, len(stages))
	for _, st := range stages {
		got = append(got, st.ID)
	}
	if want := "first,second,third,fourth"; strings.Join(got, ",") != want {
		t.Fatalf("stage ids = %v, want %q — base order preserved, new ids appended", got, want)
	}
	if v := stages[1].Executor.Timeout.Duration; v != 99*time.Minute {
		t.Errorf("second.executor.timeout = %v, want the deriving override 99m in the base's position", v)
	}
	if v := stages[1].Executor.Agent; v != "claude-code" {
		t.Errorf("second.executor.agent = %q, want the file default to survive the key-wise merge", v)
	}
}

// TestParseV2_ArraysReplaceNeverConcatenate is AC4: a deriving stage
// declaring ONE agent reviewer where the base declared TWO resolves to
// exactly one, and it is the DERIVING one. A governance file never
// accumulates a reviewer its author did not write.
func TestParseV2_ArraysReplaceNeverConcatenate(t *testing.T) {
	const doc = `
version: "2"
workflows:
  base:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: claudecode
            - provider: anthropic
  derived:
    extends: base
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: codex
`
	s := mustParseV2(t, doc)
	rev := s.Workflows["derived"].Stages[0].Reviewers
	if rev == nil {
		t.Fatal("derived apply declares no reviewers after resolution")
	}
	if got := rev.AgentCount(); got != 1 {
		t.Fatalf("agent reviewers = %d (%+v), want exactly 1 — arrays REPLACE, never concatenate", got, rev.Agents)
	}
	if got := rev.Agents[0].Provider; string(got) != "codex" {
		t.Errorf("surviving reviewer = %q, want the deriving codex", got)
	}
}

// TestParseV2_FromStageResolvesIntoInheritedBaseStage is AC5: a stage's
// inputs[].from_stage may reference a stage that exists only in the inherited
// base, because the graph-shape checks evaluate POST-resolution.
func TestParseV2_FromStageResolvesIntoInheritedBaseStage(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: apply
        type: implement
        produces:
          - artifact: pull_request
  derived:
    extends: base
    stages:
      - id: verify
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: apply
`
	s := mustParseV2(t, doc)
	stages := s.Workflows["derived"].Stages
	if len(stages) != 2 {
		t.Fatalf("stages = %d, want the inherited apply plus verify", len(stages))
	}
	in := stages[1].Inputs
	if len(in) != 1 || in[0].FromStage != "apply" {
		t.Errorf("verify.inputs = %+v, want one entry wired from the base-only stage apply", in)
	}
}

// --- Named failure / fail-closed modes -------------------------------------

// TestParseV2_ExtendsUnknownBaseRejected pins the message CONTENT: it names
// the missing workflow AND lists the ones the document does define, sorted.
func TestParseV2_ExtendsUnknownBaseRejected(t *testing.T) {
	const doc = `
version: "2"
workflows:
  zebra:
    extends: nope
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  alpha:
    stages:
      - id: b
        type: plan
        executor:
          agent: claude-code
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	if ve.Path != "/workflows/zebra/extends" {
		t.Errorf("Path = %q, want /workflows/zebra/extends", ve.Path)
	}
	if want := msgExtendsUnknownBase; ve.Message != want {
		t.Errorf("Message = %q, want %q", ve.Message, want)
	}
}

// msgExtendsUnknownBase / msgExtendsCycle / msgExtendsSelfCycle /
// msgDuplicateStageID are the exact rejection strings this module ships. The
// CLI mirror asserts the byte-identical strings; the two modules are
// deliberately separate, so parity is held by test CONTENT, never by a shared
// constant.
const (
	msgExtendsUnknownBase = `extends names workflow "nope", which this document does not define; defined workflows: alpha, zebra`
	msgExtendsCycle       = `extends forms a cycle: alpha -> beta -> alpha; a workflow cannot inherit from itself, directly or transitively`
	msgExtendsSelfCycle   = `extends forms a cycle: solo -> solo; a workflow cannot inherit from itself, directly or transitively`
	msgDuplicateStageID   = `duplicate stage id "apply" declared at positions 0 and 1; two entries targeting one stage have no defined merge order under extends, so the document is rejected rather than resolved`
)

// TestParseV2_ExtendsCycleRejected pins the two-workflow cycle: the message
// names the full chain in declaration order.
func TestParseV2_ExtendsCycleRejected(t *testing.T) {
	const doc = `
version: "2"
workflows:
  alpha:
    extends: beta
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  beta:
    extends: alpha
    stages:
      - id: b
        type: plan
        executor:
          agent: claude-code
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	if ve.Path != "/workflows/alpha/extends" {
		t.Errorf("Path = %q, want /workflows/alpha/extends", ve.Path)
	}
	if ve.Message != msgExtendsCycle {
		t.Errorf("Message = %q, want %q", ve.Message, msgExtendsCycle)
	}
}

// TestParseV2_ExtendsSelfCycleRejected pins the self-reference case, which
// falls out of the same detector rather than a special branch.
func TestParseV2_ExtendsSelfCycleRejected(t *testing.T) {
	const doc = `
version: "2"
workflows:
  solo:
    extends: solo
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	if ve.Message != msgExtendsSelfCycle {
		t.Errorf("Message = %q, want %q", ve.Message, msgExtendsSelfCycle)
	}
}

// TestResolveV2Reuse_DuplicateStageIDs pins the STANDALONE duplicate case:
// one workflow declaring the same id twice, rejected before any merge, with
// the id, both positions and the deriving workflow's stages path.
func TestResolveV2Reuse_DuplicateStageIDs(t *testing.T) {
	const doc = `
version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
      - id: apply
        type: implement
        executor:
          agent: codex
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError", err, err)
	}
	if ve.Path != "/workflows/wf/stages" {
		t.Errorf("Path = %q, want /workflows/wf/stages", ve.Path)
	}
	if ve.Message != msgDuplicateStageID {
		t.Errorf("Message = %q, want %q", ve.Message, msgDuplicateStageID)
	}
}

// TestParseV2_DuplicateStageIDInDerivingWorkflowRejected is the DERIVING
// case, and it is the one the condition exists for: a standalone duplicate
// never reaches the keyed merge, so it cannot exercise the COLLAPSE. Here two
// deriving stages both match the one base stage `apply`; a merge would fold
// both onto it and produce a single resolved stage, so validate.go's
// post-resolution uniqueness check would see one id and PASS. The resolver
// refuses instead — two entries targeting one base stage have no defensible
// merge order, so it does not invent one.
func TestParseV2_DuplicateStageIDInDerivingWorkflowRejected(t *testing.T) {
	const doc = `
version: "2"
workflows:
  base:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
  derived:
    extends: base
    stages:
      - id: apply
        type: implement
        executor:
          agent: codex
      - id: apply
        type: implement
        executor:
          agent: claude-code
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want *ValidationError — the keyed merge must not collapse two deriving stages onto one base stage", err, err)
	}
	if ve.Path != "/workflows/derived/stages" {
		t.Errorf("Path = %q, want /workflows/derived/stages", ve.Path)
	}
	if !strings.Contains(ve.Message, `duplicate stage id "apply"`) {
		t.Errorf("Message = %q, want it to name the duplicated id", ve.Message)
	}
	if !strings.Contains(ve.Message, "positions 0 and 1") {
		t.Errorf("Message = %q, want it to name both positions", ve.Message)
	}
}

// TestParseV2_StageWithNoExecutorAnywhereIsSchemaError is the fail-closed
// case: with no executor on the stage and none in any defaults rung, the
// RESOLVED stage still fails $defs/stage's required list. Fail closed, never
// a panic and never a silent pass.
func TestParseV2_StageWithNoExecutorAnywhereIsSchemaError(t *testing.T) {
	const doc = `
version: "2"
workflows:
  wf:
    stages:
      - id: a
        type: plan
`
	err := parseV2Err(t, doc)
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *SchemaError", err, err)
	}
	if !strings.Contains(se.Message, "executor") {
		t.Errorf("Message = %q, want the missing-executor required-property error", se.Message)
	}
}

// TestParseV2_MalformedReuseKeysAreSchemaErrors proves the resolver SKIPS
// every unvalidated shape rather than panicking or reporting: each case
// reaches the schema, which owns the structural message. This is the bound on
// the before-validation ordering trade.
func TestParseV2_MalformedReuseKeysAreSchemaErrors(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		path string
	}{
		{
			name: "non-object defaults",
			doc: `
version: "2"
defaults: "oops"
workflows:
  wf:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
`,
			path: "/defaults",
		},
		{
			name: "non-string extends",
			doc: `
version: "2"
workflows:
  wf:
    extends: [not, a, string]
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
`,
			path: "/workflows/wf/extends",
		},
		// The two cases below are the ones where skipping a malformed shape
		// could have meant OVERWRITING it. `stages` is the ONE key resolution
		// writes, so a deriving workflow's invalid declaration would otherwise
		// be replaced by the resolved base stages and parse as though the key
		// had been omitted — an invalid document turned into an executable
		// inherited workflow, the schema bypassed rather than deferred to.
		{
			name: "extends with a scalar stages list",
			doc: `
version: "2"
workflows:
  base:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  derived:
    extends: base
    stages: nope
`,
			path: "/workflows/derived/stages",
		},
		{
			name: "extends with a null stages list",
			doc: `
version: "2"
workflows:
  base:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  derived:
    extends: base
    stages:
`,
			path: "/workflows/derived/stages",
		},
		// E52.14 / #2331: a stage block written with NO value is PRESENT-but-
		// null, is PRESERVED by the resolver rather than overwritten by a
		// defaults block or an inherited base value, and the schema rejects it
		// at the AUTHORED path — the whole point of the change is that this
		// document no longer validates by having its null silently replaced.
		{
			name: "present-null reviewers rejected at the stage's own path",
			doc: `
version: "2"
defaults:
  reviewers:
    human: 1
workflows:
  wf:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
        reviewers:
`,
			path: "/workflows/wf/stages/0/reviewers",
		},
		{
			name: "present-null executor rejected at the stage's own path",
			doc: `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  wf:
    stages:
      - id: a
        type: plan
        executor:
`,
			path: "/workflows/wf/stages/0/executor",
		},
		{
			name: "deriving present-null type rejected at the stage's own path",
			doc: `
version: "2"
workflows:
  base:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  derived:
    extends: base
    stages:
      - id: a
        type:
`,
			path: "/workflows/derived/stages/0/type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseV2Err(t, tc.doc)
			var se *SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want *SchemaError — the resolver must skip, not crash or report", err, err)
			}
			if se.Path != tc.path {
				t.Errorf("Path = %q, want %q", se.Path, tc.path)
			}
		})
	}
}

// TestParseV2_AbsentKeyStillInheritsDefaults is the POSITIVE control the
// present-null work turns on (E52.14 / #2331, Condition 3): over-broadening the
// preserve from PRESENT-null to key-ABSENT would silently disable defaults
// inheritance for every v2 document, and every rejection test above would
// still pass while doing it. A stage OMITTING each of reviewers, executor and
// budget must still inherit the file-level defaults block and parse clean, with
// the resolved values checked — so absent is proven NOT to have been turned
// into present-null.
func TestParseV2_AbsentKeyStillInheritsDefaults(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
  reviewers:
    human: 1
    agents:
      - provider: claudecode
  budget:
    max_runtime: 45m
workflows:
  wf:
    stages:
      - id: a
        type: plan
`
	s := mustParseV2(t, doc)
	st := s.Workflows["wf"].Stages[0]
	if st.Executor.Agent != "claude-code" || st.Executor.Timeout.Duration != 30*time.Minute {
		t.Errorf("executor = %+v, want the inherited file default {claude-code, 30m}", st.Executor)
	}
	if st.Reviewers == nil || st.Reviewers.Human != 1 || st.Reviewers.AgentCount() != 1 {
		t.Errorf("reviewers = %+v, want the inherited file default {human:1, agents:[claudecode]}", st.Reviewers)
	}
	if st.Budget == nil || st.Budget.MaxRuntime.Duration != 45*time.Minute {
		t.Errorf("budget = %+v, want the inherited file default {max_runtime:45m}", st.Budget)
	}
}

// --- Rule pins the acceptance criteria do not name -------------------------

// TestParseV2_ExecutorBranchRule_HumanStageUnderAgentDefault pins the human
// side of the branch rule: a file-level defaults.executor.agent against a
// `human: true` stage resolves VALID with the human executor untouched.
// Without the rule the merged executor carries both keys and fails
// $defs/executor's oneOf, so this test fails on a regression.
func TestParseV2_ExecutorBranchRule_HumanStageUnderAgentDefault(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
workflows:
  wf:
    stages:
      - id: gate
        type: review
        executor:
          human: true
`
	s := mustParseV2(t, doc)
	ex := s.Workflows["wf"].Stages[0].Executor
	if !ex.Human {
		t.Errorf("gate.executor.human = %v, want the authored human branch preserved", ex.Human)
	}
	if ex.Agent != "" {
		t.Errorf("gate.executor.agent = %q, want NO agent key grafted from the default", ex.Agent)
	}
	if ex.Timeout.Duration != 0 {
		t.Errorf("gate.executor.timeout = %v, want the default dropped WHOLESALE (the human branch sets unevaluatedProperties:false)", ex.Timeout.Duration)
	}
}

// TestParseV2_ExecutorBranchRule_DelegateStageUnderAgentDefault pins the
// DELEGATE side — the deploy stage, the highest-consequence branch in the
// spec, and the one where a grafted `agent` key would both break the oneOf
// and misrepresent who executes a deploy. A resolver special-casing only
// `human` passes every other branch-rule test and fails this one.
func TestParseV2_ExecutorBranchRule_DelegateStageUnderAgentDefault(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
workflows:
  wf:
    defaults:
      executor:
        agent: codex
    stages:
      - id: apply
        type: implement
      - id: ship
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
`
	s := mustParseV2(t, doc)
	ship := s.Workflows["wf"].Stages[1]
	if ship.Executor.Delegate == nil {
		t.Fatal("ship.executor.delegate = nil, want the delegate branch intact")
	}
	if got := ship.Executor.Delegate.WorkflowRef; got != "deploy.yml" {
		t.Errorf("ship.executor.delegate.workflow_ref = %q, want deploy.yml", got)
	}
	if ship.Executor.Agent != "" {
		t.Errorf("ship.executor.agent = %q, want NO agent key grafted onto a DELEGATING deploy stage", ship.Executor.Agent)
	}
	if ship.Executor.Human {
		t.Error("ship.executor.human = true, want the delegate branch alone")
	}
	if ship.Executor.Timeout.Duration != 0 {
		t.Errorf("ship.executor.timeout = %v, want the agent default dropped wholesale", ship.Executor.Timeout.Duration)
	}
	// The sibling agent stage still receives the defaults, so the drop is
	// scoped to the branch conflict rather than disabling defaults outright.
	if got := s.Workflows["wf"].Stages[0].Executor.Agent; got != "codex" {
		t.Errorf("apply.executor.agent = %q, want the workflow default codex", got)
	}
}

// TestParseV2_TransitiveExtendsChain pins a three-deep chain resolving
// transitively, with each rung's contribution visible in the result.
func TestParseV2_TransitiveExtendsChain(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  a:
    stages:
      - id: one
        type: plan
  b:
    extends: a
    stages:
      - id: two
        type: implement
  c:
    extends: b
    stages:
      - id: three
        type: review
        executor:
          human: true
`
	s := mustParseV2(t, doc)
	got := make([]string, 0, 3)
	for _, st := range s.Workflows["c"].Stages {
		got = append(got, st.ID)
	}
	if want := "one,two,three"; strings.Join(got, ",") != want {
		t.Errorf("c stage ids = %v, want %q from the transitive a <- b <- c chain", got, want)
	}
}

// TestParseV2_ExtendsOnlyWorkflowNeedsNoStages proves a deriving workflow may
// omit `stages` ENTIRELY and still satisfy $defs/workflow's required [stages]
// — which only holds because resolution runs BEFORE schema validation.
func TestParseV2_ExtendsOnlyWorkflowNeedsNoStages(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: only
        type: plan
  clone:
    extends: base
`
	s := mustParseV2(t, doc)
	if got := len(s.Workflows["clone"].Stages); got != 1 {
		t.Fatalf("clone stages = %d, want the one inherited stage", got)
	}
	if got := s.Workflows["clone"].Stages[0].ID; got != "only" {
		t.Errorf("clone stage id = %q, want only", got)
	}
}

// TestParseV2_NestedObjectMergesKeyWise pins the recursive key-wise merge on
// a NESTED object: the base's executor.verify.command survives while the
// deriving side's timeout wins, inside the same nested block.
func TestParseV2_NestedObjectMergesKeyWise(t *testing.T) {
	const doc = `
version: "2"
workflows:
  base:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
          verify:
            command: scripts/test verify
            timeout: 10m
  derived:
    extends: base
    stages:
      - id: apply
        type: implement
        executor:
          verify:
            timeout: 25m
`
	s := mustParseV2(t, doc)
	ex := s.Workflows["derived"].Stages[0].Executor
	if ex.Verify == nil {
		t.Fatal("derived apply.executor.verify = nil, want the base's block merged key-wise")
	}
	if got := ex.Verify.Command; got != "scripts/test verify" {
		t.Errorf("verify.command = %q, want the base's command to survive the nested merge", got)
	}
	if got := ex.Verify.Timeout.Duration; got != 25*time.Minute {
		t.Errorf("verify.timeout = %v, want the deriving 25m", got)
	}
}

// TestParseV2_ReuseKeysStrippedBeforeTypedDecode is the strip pin: a document
// declaring `defaults` and `extends` parses cleanly under
// DisallowUnknownFields, proving both keys are gone before the typed decode.
// No Spec/Workflow/Stage struct field was added for them.
func TestParseV2_ReuseKeysStrippedBeforeTypedDecode(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: a
        type: plan
  derived:
    extends: base
    defaults:
      budget:
        max_runtime: 5m
    stages:
      - id: b
        type: review
        executor:
          human: true
`
	// A leaked key surfaces as an "internal: decode to Spec: ... unknown
	// field" error, so a clean parse IS the assertion.
	s := mustParseV2(t, doc)
	if len(s.Workflows["derived"].Stages) != 2 {
		t.Errorf("derived stages = %d, want 2", len(s.Workflows["derived"].Stages))
	}
}

// TestParseV2_ResolutionPrecedesNormalization is the ordering pin: a BASE
// stage carrying the v2 `constraints` OBJECT and the `needs:` shorthand is
// inherited verbatim and normalized once, afterwards, in its resolved
// position. If normalization ran first the inherited copy would keep the
// un-normalized object and the typed decode would fail.
func TestParseV2_ResolutionPrecedesNormalization(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: propose
        type: plan
      - id: apply
        type: implement
        produces:
          - artifact: pull_request
        needs: [propose]
        constraints:
          max_files_changed: 40
  derived:
    extends: base
`
	s := mustParseV2(t, doc)
	apply := s.Workflows["derived"].Stages[1]
	if len(apply.Constraints) != 1 || apply.Constraints[0].MaxFilesChanged != 40 {
		t.Errorf("inherited apply.constraints = %+v, want the v2 object normalized to a one-element list in its resolved position", apply.Constraints)
	}
	if len(apply.Inputs) != 1 || apply.Inputs[0].FromStage != "propose" || string(apply.Inputs[0].Artifact) != "plan" {
		t.Errorf("inherited apply.inputs = %+v, want the needs: shorthand expanded post-resolution", apply.Inputs)
	}
}

// TestParseV2_VersionGate_ReuseKeysRejectedBelowMajor2 is the version gate.
// This repo's own .fishhawk/workflows.yaml and all three shipped presets are
// v0.x / v1.x, so a resolver leaking below the major gate would touch every
// spec that actually runs today. Both lower majors are asserted: the reuse
// keys are rejected by their own schema and are NEVER resolved.
//
// The document declares `extends: nope` — an UNKNOWN base — deliberately. A
// document that merely carries well-formed reuse keys cannot distinguish "the
// resolver was gated off" from "the resolver ran and the old schema rejected
// the keys afterwards", because both end at the same additional-properties
// error. resolveV2Reuse runs BEFORE schema.Validate and rejects an unknown
// base with its OWN *ValidationError, so had it been invoked for a lower
// major that error would have PREEMPTED the schema's. Reaching the schema's
// message instead is only possible if the resolver never ran.
func TestParseV2_VersionGate_ReuseKeysRejectedBelowMajor2(t *testing.T) {
	cases := []struct {
		name    string
		version string
	}{
		{name: "v0", version: "0.7"},
		{name: "v1", version: "1.6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `
version: "` + tc.version + `"
defaults:
  executor:
    agent: claude-code
workflows:
  base:
    stages:
      - id: a
        type: plan
        executor:
          agent: claude-code
  derived:
    extends: nope
    stages:
      - id: b
        type: review
        executor:
          human: true
`
			err := parseV2Err(t, doc)
			var ve *ValidationError
			if errors.As(err, &ve) {
				t.Fatalf("err = %v, want the v%s schema's rejection — a resolver *ValidationError proves resolveV2Reuse ran below major 2", ve, tc.version)
			}
			var se *SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want a *SchemaError from the v%s schema", err, err, tc.version)
			}
			// The reuse keys are undeclared properties of an
			// additionalProperties:false schema below major 2.
			if !strings.Contains(se.Message, "additional") {
				t.Errorf("Message = %q, want the additional-properties rejection", se.Message)
			}
			// And the same document DOES trip the resolver when it is run,
			// so the assertion above discriminates rather than passing
			// because the resolver has nothing to say about this input.
			if err := resolveV2Reuse(mustDecodeYAML(t, doc)); err == nil {
				t.Error("resolveV2Reuse = nil on the gate document; it must reject the unknown base, or the gate assertion above proves nothing")
			}
			// Proof the resolver never ran: `derived` declares its own
			// stage and no executor default could have been folded in.
			raw := mustDecodeYAML(t, doc)
			if err := resolveNothingBelowMajor2(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestParseV2_ExecutorBranchRule_BranchlessDefaultDroppedOnClosedBranches is
// the SECOND half of the branch rule, and the case the first half cannot see:
// a BRANCHLESS file-level default — the bare {timeout: 30m} the schema's own
// defaults.executor description advertises — against `human: true` and
// `delegate` stages.
//
// The different-branch half fires only when BOTH fragments declare a branch,
// so a branchless default slips past it and merges key-wise, producing
// {human: true, timeout: 30m}. The human oneOf arm declares ONLY `human` and
// $defs/executor sets unevaluatedProperties: false, so that fails the oneOf —
// the same "author wrote nothing wrong" rejection the branch rule exists to
// prevent, one case over. Essentially every real workflow carries a human gate
// stage, so this rejects nearly every realistic document.
func TestParseV2_ExecutorBranchRule_BranchlessDefaultDroppedOnClosedBranches(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    timeout: 30m
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
      - id: gate
        type: review
        executor:
          human: true
      - id: ship
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
`
	s := mustParseV2(t, doc)
	stages := s.Workflows["wf"].Stages

	// The agent stage is the whole point of a bare timeout default: it still
	// receives it, so the drop is scoped to the closed branches rather than
	// disabling branchless defaults outright.
	if got := stages[0].Executor.Timeout.Duration; got != 30*time.Minute {
		t.Errorf("apply.executor.timeout = %v, want the branchless file default 30m", got)
	}
	if !stages[1].Executor.Human {
		t.Errorf("gate.executor.human = %v, want the authored human branch preserved", stages[1].Executor.Human)
	}
	if got := stages[1].Executor.Timeout.Duration; got != 0 {
		t.Errorf("gate.executor.timeout = %v, want the branchless default dropped WHOLESALE (the human arm declares only `human`)", got)
	}
	if stages[2].Executor.Delegate == nil {
		t.Fatal("ship.executor.delegate = nil, want the delegate branch intact")
	}
	if got := stages[2].Executor.Timeout.Duration; got != 0 {
		t.Errorf("ship.executor.timeout = %v, want the branchless default dropped wholesale on the delegate branch too", got)
	}
}

// TestParseV2_InvalidAgentVersionFromDefaultsRejected pins the error path
// resolution CREATES. agent_version is a plain string to the JSON Schema, so
// a malformed comparator range survives validation and is caught by the
// semantic validator — which reads the TYPED stage, i.e. the RESOLVED one.
// A bad range declared only in a `defaults` block therefore has no authored
// position of its own: it is reported at each stage it was folded into. That
// path exists only because defaults exist, and nothing else covers it.
func TestParseV2_InvalidAgentVersionFromDefaultsRejected(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    agent_version: ">=abc"
workflows:
  wf:
    stages:
      - id: a
        type: plan
`
	err := parseV2Err(t, doc)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError — a malformed range inherited from defaults must still be rejected", err, err)
	}
	if want := "/workflows/wf/stages/0/executor/agent_version"; ve.Path != want {
		t.Errorf("Path = %q, want %q — the resolved stage position, since the defaults block has none of its own", ve.Path, want)
	}
	if !strings.Contains(ve.Message, "agent_version") {
		t.Errorf("Message = %q, want the comparator-range error", ve.Message)
	}
}

// resolveNothingBelowMajor2 asserts the routing layer reports a major below
// 2 for the document, which is what gates the resolver off in ParseBytes.
func resolveNothingBelowMajor2(raw any) error {
	_, major, err := schemaForVersion(raw)
	if err != nil {
		return err
	}
	if major >= 2 {
		return errors.New("routed major >= 2; the version gate would have run the resolver")
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func parseV2Err(t *testing.T, doc string) error {
	t.Helper()
	if _, err := ParseBytes([]byte(doc)); err != nil {
		return err
	}
	t.Fatal("ParseBytes succeeded, want an error")
	return nil
}

func mustDecodeYAML(t *testing.T, doc string) any {
	t.Helper()
	var raw any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return raw
}

// TestResolveV2Reuse_MalformedShapesAreSkippedNotCrashes exercises every
// SHAPE-MISMATCH SKIP branch in the resolver directly, on decoded documents
// that never reach the schema.
//
// These branches are the BOUND on running before schema.Validate: the pass
// sees an unvalidated document, so it must never panic and never invent an
// error the schema owns — and, load-bearingly, it must LEAVE THE MALFORMED
// VALUE IN PLACE for the schema to report. Skipping a shape it cannot read
// while OVERWRITING that same key would bypass validation outright rather
// than defer to it, which is exactly what `stages` under `extends` did.
//
// So every case carries `preserved`: a JSON fragment of the malformed shape
// that must still be present in the document AFTER resolution. A checked-in
// `raw == nil` guard is not that assertion — `raw` is an interface value
// passed by value, so no code path in resolveV2Reuse could ever nil it.
func TestResolveV2Reuse_MalformedShapesAreSkippedNotCrashes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		// preserved is a JSON fragment of the malformed shape that must
		// survive resolution verbatim.
		preserved string
		// absent is a JSON fragment that must NOT appear after resolution —
		// the negative-assertion form the null-INSIDE-defaults and
		// workflow-rung-null cases need, since they prove a fragment was NOT
		// fabricated rather than that one survived.
		absent string
	}{
		{name: "root is a scalar", doc: `just-a-string`, preserved: `"just-a-string"`},
		{name: "root is a list", doc: "- a\n- b\n", preserved: `["a","b"]`},
		{name: "workflows is a scalar", doc: "version: \"2\"\nworkflows: nope\n", preserved: `"workflows":"nope"`},
		{name: "workflows is a list", doc: "version: \"2\"\nworkflows: [a, b]\n", preserved: `"workflows":["a","b"]`},
		{name: "a workflow is a scalar", doc: "version: \"2\"\nworkflows:\n  wf: nope\n", preserved: `"wf":"nope"`},
		{name: "extends names a non-object workflow", doc: "version: \"2\"\nworkflows:\n  base: nope\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: plan\n", preserved: `"base":"nope"`},
		{name: "stages is a scalar", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages: nope\n", preserved: `"stages":"nope"`},
		{name: "a stage is a scalar", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages: [nope, 7]\n", preserved: `"stages":["nope",7]`},
		{name: "a stage id is not a string", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages:\n      - id: 7\n        type: plan\n", preserved: `"id":7`},
		{name: "a base stage is a scalar", doc: "version: \"2\"\nworkflows:\n  base:\n    stages: [nope]\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: plan\n", preserved: `"base":{"stages":["nope"]}`},
		{name: "a deriving stage is a scalar", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: plan\n  derived:\n    extends: base\n    stages: [nope]\n", preserved: `"stages":[{"id":"a","type":"plan"},"nope"]`},
		{name: "defaults blocks are scalars", doc: "version: \"2\"\ndefaults: 7\nworkflows:\n  wf:\n    defaults: 7\n    stages:\n      - id: a\n        type: plan\n", preserved: `"defaults":7`},
		// The two write-not-just-read cases: `extends` supplies stages, so a
		// malformed `stages` on the deriving workflow is the one shape whose
		// skip could have silently REPLACED it with a valid inherited list.
		{name: "extends with a scalar stages list", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: plan\n  derived:\n    extends: base\n    stages: nope\n", preserved: `"derived":{"extends":"base","stages":"nope"}`},
		{name: "extends with a null stages list", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: plan\n  derived:\n    extends: base\n    stages:\n", preserved: `"derived":{"extends":"base","stages":null}`},
		// E52.14 / #2331: a key written with NO value is PRESENT-but-null and
		// must be PRESERVED, never overwritten by a defaults block or an
		// inherited base value, and never FABRICATED onto a stage from a null
		// INSIDE a defaults block. Same principle as the malformed-`stages`
		// cases above, one level down — the schema owns the rejection at the
		// authored path, so the resolver must leave the null in place.
		//
		// (a) present-null reviewers under a file-level defaults.reviewers.
		{name: "present-null reviewers preserved under file defaults", doc: "version: \"2\"\ndefaults:\n  reviewers:\n    human: 1\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n        reviewers:\n", preserved: `"reviewers":null`},
		// (b) present-null executor under a file-level defaults.executor.
		{name: "present-null executor preserved under file defaults", doc: "version: \"2\"\ndefaults:\n  executor:\n    agent: claude-code\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n        executor:\n", preserved: `"executor":null`},
		// (c) present-null budget under a file-level defaults.budget.
		{name: "present-null budget preserved under file defaults", doc: "version: \"2\"\ndefaults:\n  budget:\n    tokens: 5\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n        budget:\n", preserved: `"budget":null`},
		// (d) a deriving stage matched by id declaring type: null against a
		//     base declaring type: plan — the deriving present-null WINS.
		{name: "deriving present-null type wins over base type", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: plan\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type:\n", preserved: `"type":null`},
		// (e) a deriving stage's executor: null and reviewers: null over a base
		//     that declares them — both present-nulls win and are preserved.
		{name: "deriving present-null executor and reviewers win over base", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: implement\n        executor:\n          agent: claude-code\n        reviewers:\n          human: 1\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: implement\n        executor:\n        reviewers:\n", preserved: `"executor":null,"id":"a","reviewers":null`},
		// (f) nested null: a deriving budget: {tokens: null} over a base
		//     budget: {tokens: 5} — the nested null wins in mergeKeyWise.
		{name: "nested present-null wins in mergeKeyWise", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: implement\n        budget:\n          tokens: 5\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: implement\n        budget:\n          tokens:\n", preserved: `"tokens":null`},
		// (g) NEGATIVE: a null INSIDE a defaults block is NOT fabricated onto a
		//     stage that declares no executor. The stage keeps exactly the keys
		//     its author wrote; no "executor":null is grafted before its id.
		{name: "null inside defaults not fabricated onto stage", doc: "version: \"2\"\ndefaults:\n  executor:\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n", preserved: `"stages":[{"id":"a","type":"plan"}]`, absent: `"executor":null,"id":"a"`},
		// (h) NEGATIVE: a workflow-rung null defaults.reviewers suppresses the
		//     file-rung fallback — the file block {human:1} is NOT applied to
		//     the stage, and the null survives at the workflow defaults path.
		{name: "workflow-rung null suppresses file-rung fallback", doc: "version: \"2\"\ndefaults:\n  reviewers:\n    human: 1\nworkflows:\n  wf:\n    defaults:\n      reviewers:\n    stages:\n      - id: a\n        type: plan\n", preserved: `"defaults":{"reviewers":null}`, absent: `"reviewers":{"human":1},"type"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustDecodeYAML(t, tc.doc)
			if err := resolveV2Reuse(raw); err != nil {
				t.Fatalf("resolveV2Reuse = %v, want nil — an unvalidated shape is the SCHEMA's error, never this pass's", err)
			}
			resolved, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("marshal resolved: %v", err)
			}
			if tc.preserved != "" && !strings.Contains(string(resolved), tc.preserved) {
				t.Errorf("resolved document = %s\nwant it to still contain %s — an authored value must be LEFT IN PLACE for the schema to report, never overwritten", resolved, tc.preserved)
			}
			if tc.absent != "" && strings.Contains(string(resolved), tc.absent) {
				t.Errorf("resolved document = %s\nwant it to NOT contain %s — a null must never be FABRICATED onto a stage the author did not write it on", resolved, tc.absent)
			}
			// The strip must tolerate the identical shapes.
			stripV2Reuse(raw)
		})
	}
}

// TestResolveV2Reuse_DottedVersionDeterministicOffender pins the pre-schema
// version gate: a dotted minor form ("2.0") routes to v2 by MAJOR but is
// invalid against the single-token enum. resolveV2Reuse reports it as a
// *SchemaError at /version BEFORE schema.Validate, so a document that ALSO
// trips another top-level rule — here an empty `workflows` — surfaces the
// /version offender deterministically. schema.Validate would emit /version and
// /workflows as unordered sibling causes, flipping the reported first offender
// run to run and intermittently dropping the /version path a stale
// fishhawk-mcp self-diagnoses on (#1422).
func TestResolveV2Reuse_DottedVersionDeterministicOffender(t *testing.T) {
	for _, doc := range []string{
		// The flaky /version-vs-/workflows combo, plus a dotted form that
		// resolves cleanly apart from the version.
		"version: \"2.0\"\nworkflows: {}\n",
		"version: \"2.1\"\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n",
	} {
		raw := mustDecodeYAML(t, doc)
		err := resolveV2Reuse(raw)
		var se *SchemaError
		if !errors.As(err, &se) {
			t.Fatalf("resolveV2Reuse(%q) = %v, want a *SchemaError", doc, err)
		}
		if se.Path != "/version" {
			t.Errorf("SchemaError.Path = %q, want /version", se.Path)
		}
	}

	// The valid single token "2" is untouched: the gate does not fire, and the
	// document resolves as before.
	raw := mustDecodeYAML(t, "version: \"2\"\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n")
	if err := resolveV2Reuse(raw); err != nil {
		t.Fatalf("resolveV2Reuse(version 2) = %v, want nil", err)
	}
}

// TestResolveV2Reuse_WorkflowDefaultsWithNoFileDefaults pins the rung-3-only
// path: a workflow declaring `defaults` in a document with NO file-level
// block still folds them into its stages.
func TestResolveV2Reuse_WorkflowDefaultsWithNoFileDefaults(t *testing.T) {
	const doc = `
version: "2"
workflows:
  wf:
    defaults:
      executor:
        agent: codex
        timeout: 15m
    stages:
      - id: a
        type: plan
`
	s := mustParseV2(t, doc)
	ex := s.Workflows["wf"].Stages[0].Executor
	if ex.Agent != "codex" || ex.Timeout.Duration != 15*time.Minute {
		t.Errorf("executor = %+v, want the workflow-only defaults {codex, 15m}", ex)
	}
}

// TestResolveV2Reuse_ReviewersAtBothDefaultsRungsTakesWorkflowWhole is the
// block-level rule BETWEEN the two defaults rungs: when file and workflow
// levels both declare `reviewers`, the workflow's block is taken WHOLE — the
// file's keys are not blended in, for the same authority reason a stage's own
// block is never blended.
func TestResolveV2Reuse_ReviewersAtBothDefaultsRungsTakesWorkflowWhole(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
  reviewers:
    human: 1
    agents:
      - provider: claudecode
workflows:
  wf:
    defaults:
      reviewers:
        agents:
          - provider: codex
    stages:
      - id: a
        type: plan
`
	s := mustParseV2(t, doc)
	rev := s.Workflows["wf"].Stages[0].Reviewers
	if rev == nil {
		t.Fatal("reviewers = nil, want the workflow-level defaults block")
	}
	if rev.Human != 0 {
		t.Errorf("reviewers.human = %d, want 0 — the file rung's human must NOT blend into the workflow rung's block", rev.Human)
	}
	if got := rev.AgentCount(); got != 1 || string(rev.Agents[0].Provider) != "codex" {
		t.Errorf("reviewers.agents = %+v, want exactly the workflow rung's [codex]", rev.Agents)
	}
}
