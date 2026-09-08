package spec_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// Deterministic cross-workflow error ordering (#2944 / E60.11). Both
// validateGraphShape (graphshape.go) and validateAgentVersions (validate.go)
// map-ranged the decoded `workflows` map before this change, so a document
// defective in several workflows reported its entries in Go's randomized map
// order — different on every run over the identical bytes. Both sites now
// sweep sortedKeys(workflows) (v2reuse.go:742, the package's existing
// determinism helper), so ValidateBytes reports the SAME entry sequence
// every time. This file drives the WHOLE shipped path (spec.ValidateBytes
// over raw bytes), the Done-means behavioral test for a change whose
// correctness compilation cannot enforce.
//
// The workflow names are chosen so their DECLARATION order (the order
// written below: zeta, alpha, mike, bravo, yankee) differs from their
// SORTED order (alpha, bravo, mike, yankee, zeta) — a document whose
// declaration happened to already be sorted could pass a broken
// implementation by accident.
//
// Five workflows is load-bearing for the counterfactual: a pre-fix run
// matches sorted order with probability 1/120 (5!), and 25 repeated
// ValidateBytes calls over the same bytes drive the escape probability of an
// accidental all-sorted, all-stable run below 1e-40 — a two-workflow "build
// twice" shape would only be a coin flip and could produce a false GREEN
// that reads as "the control is unnecessary".
//
// Counterfactual A (graphshape.go), observed empirically per approval
// condition 3 — one control reverted at a time, restored immediately after:
// reverted graphshape.go's `for _, wfName := range sortedKeys(workflows) {
// wfRaw := workflows[wfName]` back to the bare `for wfName, wfRaw := range
// workflows {`, then ran
// `go test -run TestValidateBytes_GraphShapeEntryOrderIsSortedAndStable ./internal/spec/...`
// from cli/. Observed RED:
//
//	=== RUN   TestValidateBytes_GraphShapeEntryOrderIsSortedAndStable
//	    ordering_test.go:188: run 0: entry[0].Path = "/workflows/yankee/stages/0/inputs/0/from_stage", want "/workflows/alpha/stages/0/inputs/0/from_stage" (want sorted-name order)
//	--- FAIL: TestValidateBytes_GraphShapeEntryOrderIsSortedAndStable (0.00s)
//	FAIL
//
// Restored graphshape.go byte-identical to the pre-deletion version
// immediately after (confirmed via `git diff` showing only the intended
// change).
//
// Counterfactual B (validate.go), observed empirically per approval
// condition 3 — one control reverted at a time, restored immediately after:
// reverted validate.go's `for _, wfName := range sortedKeys(workflows) {
// wfRaw := workflows[wfName]` back to the bare `for wfName, wfRaw := range
// workflows {`, then ran
// `go test -run TestValidateBytes_AgentVersionEntryOrderIsSortedAndStable ./internal/spec/...`
// from cli/. Observed RED:
//
//	=== RUN   TestValidateBytes_AgentVersionEntryOrderIsSortedAndStable
//	    ordering_test.go:241: run 0: entry[0].Path = "/workflows/mike/stages/0/executor/agent_version", want "/workflows/alpha/stages/0/executor/agent_version" (want sorted-name order)
//	--- FAIL: TestValidateBytes_AgentVersionEntryOrderIsSortedAndStable (0.00s)
//	FAIL
//
// Restored validate.go byte-identical to the pre-deletion version
// immediately after (confirmed via `git diff` showing only the intended
// change).
//
// Per approval condition 3: these are the ACTUAL observed failure
// transcripts from running each counterfactual, not a permutation-probability
// argument standing in for evidence.

// sortedWorkflowNames is the fixed five-name set both ordering tests use,
// already in sorted order — the order every assertion below expects.
var sortedWorkflowNames = []string{"alpha", "bravo", "mike", "yankee", "zeta"}

// declarationOrderNames is the same set written in a DIFFERENT order than
// sortedWorkflowNames, so the fixture documents below cannot pass a broken
// (map-range) implementation merely because declaration order already
// matched sorted order.
var declarationOrderNames = []string{"zeta", "alpha", "mike", "bravo", "yankee"}

// buildGraphShapeOrderingDoc builds a workflow-v2 document declaring the five
// workflows in declarationOrderNames, each with exactly one graph-shape
// defect: a single implement stage whose inputs[0].from_stage names a
// nonexistent stage id. Each workflow therefore yields exactly one
// validateGraphShape entry at PathFmtFromStage(name, 0, 0) with message
// MsgFmtFromStageUnknown("nope", name) — mirroring the shape
// TestValidateBytes_GraphShape_UnknownFromStage already exercises.
func buildGraphShapeOrderingDoc() string {
	var b strings.Builder
	b.WriteString("version: \"2\"\nworkflows:\n")
	for _, name := range declarationOrderNames {
		fmt.Fprintf(&b, `  %s:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: nope
        produces:
          - artifact: pull_request
`, name)
	}
	return b.String()
}

// buildAgentVersionOrderingDoc builds a workflow-v2 document declaring the
// five workflows in declarationOrderNames, each with exactly one
// validateAgentVersions defect: a single implement stage whose
// executor.agent_version is ">=abc" — a plain non-empty string, so it PASSES
// the schema tier (the field's only schema constraint is
// type:string,minLength:1; TestValidateBytes_AgentVersion_ExecutorMalformed
// pins the same fixture shape) but is REJECTED by ValidAgentVersionRange
// ("abc" is not a 1-to-3-part dotted numeric version), giving each workflow
// exactly one entry at /workflows/<name>/stages/0/executor/agent_version.
func buildAgentVersionOrderingDoc() string {
	var b strings.Builder
	b.WriteString("version: \"2\"\nworkflows:\n")
	for _, name := range declarationOrderNames {
		fmt.Fprintf(&b, `  %s:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          agent_version: ">=abc"
`, name)
	}
	return b.String()
}

// validateBytesEntries runs spec.ValidateBytes over doc and returns the
// entries of the resulting *ValidationError, failing the test if the call
// unexpectedly succeeds or returns a different error type.
func validateBytesEntries(t *testing.T, doc string) []spec.ValidationErrorEntry {
	t.Helper()
	err := spec.ValidateBytes([]byte(doc))
	if err == nil {
		t.Fatalf("ValidateBytes: want an error, got nil")
	}
	ve, ok := err.(*spec.ValidationError)
	if !ok {
		t.Fatalf("error = %T (%v), want *ValidationError", err, err)
	}
	return ve.Errors
}

// entryPaths projects a slice of entries onto their Path values, for the
// sorted-order assertion below.
func entryPaths(entries []spec.ValidationErrorEntry) []string {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	return paths
}

// TestValidateBytes_GraphShapeEntryOrderIsSortedAndStable is the DONE-MEANS
// test for the graphshape.go half of #2944: a document with a from_stage
// defect in several workflows reports the SAME entry sequence, in sorted
// workflow-name order, on every run.
func TestValidateBytes_GraphShapeEntryOrderIsSortedAndStable(t *testing.T) {
	doc := buildGraphShapeOrderingDoc()

	wantPaths := make([]string, len(sortedWorkflowNames))
	wantMessages := make([]string, len(sortedWorkflowNames))
	for i, name := range sortedWorkflowNames {
		wantPaths[i] = fmt.Sprintf(spec.PathFmtFromStage, name, 0, 0)
		wantMessages[i] = fmt.Sprintf(spec.MsgFmtFromStageUnknown, "nope", name)
	}

	const runs = 25
	var firstPaths, firstMessages []string
	for run := 0; run < runs; run++ {
		entries := validateBytesEntries(t, doc)
		if len(entries) != len(sortedWorkflowNames) {
			t.Fatalf("run %d: len(entries) = %d, want %d", run, len(entries), len(sortedWorkflowNames))
		}
		gotPaths := entryPaths(entries)
		gotMessages := make([]string, len(entries))
		for i, e := range entries {
			gotMessages[i] = e.Message
		}

		if run == 0 {
			// (a) exact expected entry sequence equals sorted workflow-name order.
			for i := range wantPaths {
				if gotPaths[i] != wantPaths[i] {
					t.Fatalf("run %d: entry[%d].Path = %q, want %q (want sorted-name order)", run, i, gotPaths[i], wantPaths[i])
				}
				if gotMessages[i] != wantMessages[i] {
					t.Fatalf("run %d: entry[%d].Message = %q, want %q", run, i, gotMessages[i], wantMessages[i])
				}
			}
			firstPaths, firstMessages = gotPaths, gotMessages
			continue
		}

		// (b) stability: every subsequent run's (path, message) sequence
		// equals the first run's.
		for i := range firstPaths {
			if gotPaths[i] != firstPaths[i] || gotMessages[i] != firstMessages[i] {
				t.Fatalf("run %d: entry[%d] = (%q, %q), want (%q, %q) (unstable across repeated ValidateBytes calls)",
					run, i, gotPaths[i], gotMessages[i], firstPaths[i], firstMessages[i])
			}
		}
	}
}

// TestValidateBytes_AgentVersionEntryOrderIsSortedAndStable is the sibling
// DONE-MEANS test for the validate.go half of #2944 (validateAgentVersions):
// a document with a malformed executor.agent_version in several workflows
// reports the same PATH sequence, in sorted workflow-name order, on every
// run. Per approach step 5, the message text is read off the first run
// rather than hardcoded — agentversion_test.go already owns the exact
// message-text contract — so this test pins ordering only.
func TestValidateBytes_AgentVersionEntryOrderIsSortedAndStable(t *testing.T) {
	doc := buildAgentVersionOrderingDoc()

	wantPaths := make([]string, len(sortedWorkflowNames))
	for i, name := range sortedWorkflowNames {
		wantPaths[i] = fmt.Sprintf("/workflows/%s/stages/0/executor/agent_version", name)
	}

	const runs = 25
	var firstPaths, firstMessages []string
	for run := 0; run < runs; run++ {
		entries := validateBytesEntries(t, doc)
		if len(entries) != len(sortedWorkflowNames) {
			t.Fatalf("run %d: len(entries) = %d, want %d", run, len(entries), len(sortedWorkflowNames))
		}
		gotPaths := entryPaths(entries)
		gotMessages := make([]string, len(entries))
		for i, e := range entries {
			gotMessages[i] = e.Message
		}

		if run == 0 {
			// exact expected PATH sequence equals sorted workflow-name order.
			for i := range wantPaths {
				if gotPaths[i] != wantPaths[i] {
					t.Fatalf("run %d: entry[%d].Path = %q, want %q (want sorted-name order)", run, i, gotPaths[i], wantPaths[i])
				}
				if !strings.Contains(gotMessages[i], "agent_version") {
					t.Fatalf("run %d: entry[%d].Message = %q, want it to mention agent_version", run, i, gotMessages[i])
				}
			}
			firstPaths, firstMessages = gotPaths, gotMessages
			continue
		}

		// stability: every subsequent run's (path, message) sequence equals
		// the first run's.
		for i := range firstPaths {
			if gotPaths[i] != firstPaths[i] || gotMessages[i] != firstMessages[i] {
				t.Fatalf("run %d: entry[%d] = (%q, %q), want (%q, %q) (unstable across repeated ValidateBytes calls)",
					run, i, gotPaths[i], gotMessages[i], firstPaths[i], firstMessages[i])
			}
		}
	}
}
