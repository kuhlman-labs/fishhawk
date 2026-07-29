package spec

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Tests for the CLI's workflow-v2 same-document reuse resolver (E52.4 /
// #2216). The backend carries a byte-parity twin in
// backend/internal/spec/v2reuse_test.go; the two modules are deliberately
// separate (see this package's doc comment), so lockstep is held by the
// SHARED GOLDEN FIXTURE below and by message-content assertions on both
// sides, never by a shared constant.

// reuseFixtureRelPath / reuseGoldenRelPath are the AC7 cross-validator
// fixture and its golden, read over the module wall from the same paths the
// backend's twin reads. cli/internal/spec sits at the same depth from the
// repo root as backend/internal/spec.
const (
	reuseFixtureRelPath = "../../../docs/spec/examples/workflow-v2-reuse.yaml"
	reuseGoldenRelPath  = "../../../docs/spec/examples/workflow-v2-reuse.resolved.json"
)

// TestV2Reuse_SharedFixtureResolvesIdenticallyAcrossValidators is the AC7
// cross-module test, CLI side. It runs THIS module's resolution over the
// shared fixture and asserts the result canonicalizes byte-for-byte to the
// third-party golden — never to the backend's output directly. A
// backend-only change to the algorithm regenerates the golden and fails
// HERE; a CLI-only change fails the backend's twin.
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
	// one key deleted before comparing — the backend's twin does the same.
	if _, ok := golden["$comment"]; !ok {
		t.Errorf("%s lost its $comment header; the golden is generated and must document that", reuseGoldenRelPath)
	}
	delete(golden, "$comment")
	want, err := json.Marshal(golden)
	if err != nil {
		t.Fatalf("re-marshal golden: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("CLI resolution diverged from the golden\n got: %s\nwant: %s\nthe backend mirror changed without this one, or vice versa", got, want)
	}

	// The published fixture must also pass the CLI's own gate end to end.
	if err := ValidateBytes(data); err != nil {
		t.Errorf("ValidateBytes(%s) = %v, want nil", reuseFixtureRelPath, err)
	}
}

// TestValidateV2_ExecutorBranchRule_HumanAndDelegateStages pins both
// non-agent branches of the executor rule on the CLI side: a `human: true`
// gate stage and a DELEGATING deploy stage, each under a file-level
// defaults.executor.agent, must resolve VALID with their authored branch
// intact. Without the rule the merged executor carries two branch keys and
// $defs/executor's oneOf rejects a document whose author wrote nothing wrong.
func TestValidateV2_ExecutorBranchRule_HumanAndDelegateStages(t *testing.T) {
	const doc = `
version: "2"
defaults:
  executor:
    agent: claude-code
    timeout: 30m
workflows:
  wf:
    stages:
      - id: apply
        type: implement
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
	if err := ValidateBytes([]byte(doc)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil — the branch rule must drop the agent default wholesale", err)
	}

	var raw any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatal(err)
	}
	if err := resolveV2Reuse(raw); err != nil {
		t.Fatalf("resolveV2Reuse: %v", err)
	}
	stages := raw.(map[string]any)["workflows"].(map[string]any)["wf"].(map[string]any)["stages"].([]any)

	gate := stages[1].(map[string]any)["executor"].(map[string]any)
	if _, grafted := gate["agent"]; grafted {
		t.Errorf("gate.executor = %v, want NO agent key grafted onto the human branch", gate)
	}
	if len(gate) != 1 {
		t.Errorf("gate.executor = %v, want the agent default dropped WHOLESALE (timeout included)", gate)
	}
	ship := stages[2].(map[string]any)["executor"].(map[string]any)
	if _, grafted := ship["agent"]; grafted {
		t.Errorf("ship.executor = %v, want NO agent key grafted onto a DELEGATING deploy stage", ship)
	}
	if _, ok := ship["delegate"]; !ok {
		t.Errorf("ship.executor = %v, want the delegate branch intact", ship)
	}
	if len(ship) != 1 {
		t.Errorf("ship.executor = %v, want the agent default dropped wholesale", ship)
	}
	// The sibling agent stage still receives the defaults, so the drop is
	// scoped to the branch conflict rather than disabling defaults outright.
	apply := stages[0].(map[string]any)["executor"].(map[string]any)
	if apply["agent"] != "claude-code" {
		t.Errorf("apply.executor = %v, want the file default applied", apply)
	}
}

// TestResolveV2Reuse_DottedVersionDeterministicOffender pins the CLI's
// pre-schema version gate, the byte-parity twin of the backend's: a dotted
// minor form ("2.0") routes to v2 by MAJOR but is invalid against the
// single-token enum, so resolveV2Reuse reports it as a *ValidationError at
// /version BEFORE schema.Validate. A document that ALSO trips another
// top-level rule (an empty `workflows`) therefore surfaces the /version
// offender deterministically instead of flipping with the schema's unordered
// sibling causes.
func TestResolveV2Reuse_DottedVersionDeterministicOffender(t *testing.T) {
	for _, doc := range []string{
		"version: \"2.0\"\nworkflows: {}\n",
		"version: \"2.1\"\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n",
	} {
		var raw any
		if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
			t.Fatal(err)
		}
		err := resolveV2Reuse(raw)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("resolveV2Reuse(%q) = %v, want a *ValidationError", doc, err)
		}
		if len(ve.Errors) != 1 || ve.Errors[0].Path != "/version" {
			t.Errorf("errors = %+v, want a single /version entry", ve.Errors)
		}
	}

	// The valid single token "2" is untouched: the gate does not fire.
	var raw any
	if err := yaml.Unmarshal([]byte("version: \"2\"\nworkflows:\n  wf:\n    stages:\n      - id: a\n        type: plan\n"), &raw); err != nil {
		t.Fatal(err)
	}
	if err := resolveV2Reuse(raw); err != nil {
		t.Fatalf("resolveV2Reuse(version 2) = %v, want nil", err)
	}
}

// TestValidateV2_MalformedReuseKeysAreSchemaErrors proves the CLI resolver
// SKIPS an unvalidated shape rather than panicking or reporting: the schema,
// which runs next, owns the structural message. Same fail-closed bound as the
// backend's twin.
func TestValidateV2_MalformedReuseKeysAreSchemaErrors(t *testing.T) {
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
		// be replaced by the resolved base stages and validate as though the
		// key had been omitted — the schema bypassed rather than deferred to.
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
		// Byte-parity with the backend twin.
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
			err := ValidateBytes([]byte(tc.doc))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v (%T), want *ValidationError — the resolver must skip, not crash", err, err)
			}
			var found bool
			for _, e := range ve.Errors {
				if strings.HasPrefix(e.Path, tc.path) {
					found = true
				}
			}
			if !found {
				t.Errorf("errors = %+v, want one at %s", ve.Errors, tc.path)
			}
		})
	}
}

// TestValidateV2_AbsentKeyStillInheritsDefaults is the POSITIVE control the
// present-null work turns on (E52.14 / #2331, Condition 3), CLI side: over-
// broadening the preserve from PRESENT-null to key-ABSENT would silently
// disable defaults inheritance for every v2 document, and every rejection test
// above would still pass while doing it. A stage OMITTING each of reviewers,
// executor and budget must still inherit the file-level defaults block and
// validate clean, with the resolved values checked — so absent is proven NOT
// to have been turned into present-null.
func TestValidateV2_AbsentKeyStillInheritsDefaults(t *testing.T) {
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
	if err := ValidateBytes([]byte(doc)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil — an omitted key must still inherit the file defaults", err)
	}
	ex := resolveStageBlock(t, doc, "wf", 0, "executor")
	if ex["agent"] != "claude-code" || ex["timeout"] != "30m" {
		t.Errorf("executor = %v, want the inherited file default {claude-code, 30m}", ex)
	}
	rev := resolveStageBlock(t, doc, "wf", 0, "reviewers")
	if rev["human"] != 1 {
		t.Errorf("reviewers = %v, want the inherited file default human:1", rev)
	}
	agents, _ := rev["agents"].([]any)
	if len(agents) != 1 {
		t.Errorf("reviewers.agents = %v, want the inherited one entry", agents)
	}
	bud := resolveStageBlock(t, doc, "wf", 0, "budget")
	if bud["max_runtime"] != "45m" {
		t.Errorf("budget = %v, want the inherited file default max_runtime:45m", bud)
	}
}

// TestValidateV2_VersionGate_ReuseKeysRejectedBelowMajor2 is the version
// gate. This repo's own .fishhawk/workflows.yaml and all three shipped
// presets are v0.x / v1.x, so a resolver leaking below the gate would touch
// every spec that actually runs today. Both lower majors are asserted.
//
// The document declares `extends: nope` — an UNKNOWN base — deliberately. A
// document that merely carries well-formed reuse keys cannot distinguish "the
// resolver was gated off" from "the resolver ran and the old schema rejected
// the keys afterwards": both end at the same additional-properties error, and
// the CLI wraps both outcomes in *ValidationError, so the type alone says
// nothing. resolveV2Reuse runs BEFORE schema.Validate and rejects an unknown
// base with its own single-entry error, so had it been invoked for a lower
// major that message would have PREEMPTED the schema's leaf set. Getting the
// schema's message instead is only possible if the resolver never ran.
func TestValidateV2_VersionGate_ReuseKeysRejectedBelowMajor2(t *testing.T) {
	for _, version := range []string{"0.7", "1.6"} {
		t.Run(version, func(t *testing.T) {
			doc := `
version: "` + version + `"
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
			err := ValidateBytes([]byte(doc))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v (%T), want a schema *ValidationError from the v%s schema", err, err, version)
			}
			for _, e := range ve.Errors {
				if strings.Contains(e.Message, "extends names workflow") {
					t.Fatalf("errors = %+v, want the v%s schema's rejection — the resolver's unknown-base message proves resolveV2Reuse ran below major 2", ve.Errors, version)
				}
			}
			// And the same document DOES trip the resolver when it is run, so
			// the assertion above discriminates rather than passing because
			// the resolver has nothing to say about this input.
			var probe any
			if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
				t.Fatal(err)
			}
			if err := resolveV2Reuse(probe); err == nil {
				t.Error("resolveV2Reuse = nil on the gate document; it must reject the unknown base, or the gate assertion above proves nothing")
			}
			// And the routing layer reports a major below 2, which is what
			// gates the resolver off in ValidateBytes.
			var raw any
			if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
				t.Fatal(err)
			}
			if _, major, err := schemaForVersion(raw); err != nil || major >= 2 {
				t.Errorf("routed major = %d (err %v), want < 2 so the resolver never runs", major, err)
			}
		})
	}
}

// TestValidateV2_ExecutorBranchRule_BranchlessDefaultDroppedOnClosedBranches
// is the SECOND half of the branch rule, CLI side, and the case the first half
// cannot see: a BRANCHLESS file-level default — the bare {timeout: 30m} the
// schema's own defaults.executor description advertises — against `human:
// true` and `delegate` stages.
//
// The different-branch half fires only when BOTH fragments declare a branch,
// so a branchless default slips past it and merges key-wise into {human:
// true}. The human oneOf arm declares ONLY `human` and $defs/executor sets
// unevaluatedProperties: false, so the merged fragment fails the oneOf — the
// same "author wrote nothing wrong" rejection the branch rule exists to
// prevent, one case over, on a shape essentially every real workflow has.
func TestValidateV2_ExecutorBranchRule_BranchlessDefaultDroppedOnClosedBranches(t *testing.T) {
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
	if err := ValidateBytes([]byte(doc)); err != nil {
		t.Fatalf("ValidateBytes = %v, want nil — the closed-branch half must drop the branchless default", err)
	}

	// The agent stage is the whole point of a bare timeout default: it still
	// receives it, so the drop is scoped to the closed branches rather than
	// disabling branchless defaults outright.
	if apply := resolveStageBlock(t, doc, "wf", 0, "executor"); apply["timeout"] != "30m" {
		t.Errorf("apply.executor = %v, want the branchless file default applied", apply)
	}
	gate := resolveStageBlock(t, doc, "wf", 1, "executor")
	if len(gate) != 1 || gate["human"] != true {
		t.Errorf("gate.executor = %v, want exactly {human: true} — the human arm declares no other property", gate)
	}
	ship := resolveStageBlock(t, doc, "wf", 2, "executor")
	if len(ship) != 1 {
		t.Errorf("ship.executor = %v, want the branchless default dropped on the delegate branch too", ship)
	}
	if _, ok := ship["delegate"]; !ok {
		t.Errorf("ship.executor = %v, want the delegate branch intact", ship)
	}
}

// TestValidateV2_InvalidAgentVersionFromDefaultsRejected pins the error path
// resolution CREATES. agent_version is a plain string to the JSON Schema, so
// a malformed comparator range survives validation and is caught by
// validateAgentVersions — which at major >= 2 sweeps the RESOLVED document.
// A bad range declared only in a `defaults` block therefore has no authored
// position of its own: it is reported at each stage it was folded into. That
// path exists only because defaults exist, and nothing else covers it.
func TestValidateV2_InvalidAgentVersionFromDefaultsRejected(t *testing.T) {
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
	err := ValidateBytes([]byte(doc))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *ValidationError — a malformed range inherited from defaults must still be rejected", err, err)
	}
	const want = "/workflows/wf/stages/0/executor/agent_version"
	var found bool
	for _, e := range ve.Errors {
		if e.Path == want && strings.Contains(e.Message, "agent_version") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %+v, want the comparator-range error at %s — the resolved stage position, since the defaults block has none of its own", ve.Errors, want)
	}
}

// TestResolveV2Reuse_MalformedShapesAreSkippedNotCrashes is the CLI twin of
// the backend's shape-skip table. Same contract, same reason: the resolver
// runs before schema validation, so every unrecognized shape must be SKIPPED
// — never panicked on, never reported — and LEFT IN PLACE for the schema.
//
// That last clause is the load-bearing one: skipping a shape it cannot read
// while OVERWRITING that same key would bypass validation outright rather
// than defer to it, which is exactly what `stages` under `extends` did. So
// every case carries `preserved`, a JSON fragment of the malformed shape that
// must still be present AFTER resolution. A `raw == nil` guard is not that
// assertion — `raw` is an interface value passed by value, so no code path in
// resolveV2Reuse could ever nil the caller's variable.
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
		// authored path, so the resolver must leave the null in place. The
		// backend twin carries the byte-identical table.
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
			var raw any
			if err := yaml.Unmarshal([]byte(tc.doc), &raw); err != nil {
				t.Fatalf("yaml: %v", err)
			}
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
		})
	}
}

// TestResolveV2Reuse_DefaultsRungCombinations pins the two rung-combination
// paths the shared fixture does not reach: a workflow-level `defaults` with
// NO file-level block, and `reviewers` declared at BOTH rungs — where the
// workflow's block is taken WHOLE rather than blended with the file's.
func TestResolveV2Reuse_DefaultsRungCombinations(t *testing.T) {
	t.Run("workflow defaults with no file defaults", func(t *testing.T) {
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
		if err := ValidateBytes([]byte(doc)); err != nil {
			t.Fatalf("ValidateBytes = %v, want nil", err)
		}
		ex := resolveStageBlock(t, doc, "wf", 0, "executor")
		if ex["agent"] != "codex" || ex["timeout"] != "15m" {
			t.Errorf("executor = %v, want the workflow-only defaults {codex, 15m}", ex)
		}
	})

	t.Run("reviewers at both rungs takes the workflow block whole", func(t *testing.T) {
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
		if err := ValidateBytes([]byte(doc)); err != nil {
			t.Fatalf("ValidateBytes = %v, want nil", err)
		}
		rev := resolveStageBlock(t, doc, "wf", 0, "reviewers")
		if _, blended := rev["human"]; blended {
			t.Errorf("reviewers = %v, want NO human blended in from the file rung", rev)
		}
		agents, _ := rev["agents"].([]any)
		if len(agents) != 1 {
			t.Fatalf("reviewers.agents = %v, want exactly the workflow rung's one entry", agents)
		}
	})
}

// resolveStageBlock resolves a document and returns one stage's named block.
func resolveStageBlock(t *testing.T, doc, wf string, idx int, key string) map[string]any {
	t.Helper()
	var raw any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if err := resolveV2Reuse(raw); err != nil {
		t.Fatalf("resolveV2Reuse: %v", err)
	}
	stages := raw.(map[string]any)["workflows"].(map[string]any)[wf].(map[string]any)["stages"].([]any)
	block, ok := stages[idx].(map[string]any)[key].(map[string]any)
	if !ok {
		t.Fatalf("stage %d has no %s block: %v", idx, key, stages[idx])
	}
	return block
}
