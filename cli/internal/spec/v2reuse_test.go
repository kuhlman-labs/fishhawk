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

// TestValidateV2_VersionGate_ReuseKeysRejectedBelowMajor2 is the version
// gate. This repo's own .fishhawk/workflows.yaml and all three shipped
// presets are v0.x / v1.x, so a resolver leaking below the gate would touch
// every spec that actually runs today. Both lower majors are asserted.
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
    extends: base
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

// TestResolveV2Reuse_MalformedShapesAreSkippedNotCrashes is the CLI twin of
// the backend's shape-skip table. Same contract, same reason: the resolver
// runs before schema validation, so every unrecognized shape must be SKIPPED
// — never panicked on, never reported — and left in place for the schema.
func TestResolveV2Reuse_MalformedShapesAreSkippedNotCrashes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{name: "root is a scalar", doc: `just-a-string`},
		{name: "root is a list", doc: "- a\n- b\n"},
		{name: "workflows is a scalar", doc: "version: \"2\"\nworkflows: nope\n"},
		{name: "workflows is a list", doc: "version: \"2\"\nworkflows: [a, b]\n"},
		{name: "a workflow is a scalar", doc: "version: \"2\"\nworkflows:\n  wf: nope\n"},
		{name: "extends names a non-object workflow", doc: "version: \"2\"\nworkflows:\n  base: nope\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: plan\n"},
		{name: "stages is a scalar", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages: nope\n"},
		{name: "a stage is a scalar", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages: [nope, 7]\n"},
		{name: "a stage id is not a string", doc: "version: \"2\"\nworkflows:\n  wf:\n    stages:\n      - id: 7\n        type: plan\n"},
		{name: "a base stage is a scalar", doc: "version: \"2\"\nworkflows:\n  base:\n    stages: [nope]\n  derived:\n    extends: base\n    stages:\n      - id: a\n        type: plan\n"},
		{name: "a deriving stage is a scalar", doc: "version: \"2\"\nworkflows:\n  base:\n    stages:\n      - id: a\n        type: plan\n  derived:\n    extends: base\n    stages: [nope]\n"},
		{name: "defaults blocks are scalars", doc: "version: \"2\"\ndefaults: 7\nworkflows:\n  wf:\n    defaults: 7\n    stages:\n      - id: a\n        type: plan\n"},
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
			if raw == nil {
				t.Fatal("resolver dropped the document; it must leave an unrecognized shape in place for the schema to report")
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
