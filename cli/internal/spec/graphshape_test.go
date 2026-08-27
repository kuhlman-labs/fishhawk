package spec_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// Stage-reference resolution (E52.13 / #2323). Every case drives the WHOLE
// ValidateBytes chain — raw decode → schema → (v2) needs expansion →
// graph-shape → error rendering — and asserts the SHIPPED message AND the
// SHIPPED JSON-pointer path, because C1's promise is that the CLI and backend
// agree on both. Expected values are built from the exported constants so the
// per-mode assertions and the parity guard (TestGraphShapeMessageParity) pin
// the same bytes from two angles.

// graphErrors runs ValidateBytes over doc, requires a *ValidationError, and
// returns its entries.
func graphErrors(t *testing.T, doc string) []spec.ValidationErrorEntry {
	t.Helper()
	err := spec.ValidateBytes([]byte(doc))
	if err == nil {
		t.Fatalf("ValidateBytes: want an error, got nil")
	}
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T (%v), want *ValidationError", err, err)
	}
	return ve.Errors
}

// requireEntry fails unless some entry matches path AND message EXACTLY. The
// exact match makes each test a genuine counterfactual vehicle: deleting the
// control removes the entry and no other entry can accidentally satisfy it.
func requireEntry(t *testing.T, entries []spec.ValidationErrorEntry, path, message string) {
	t.Helper()
	for _, e := range entries {
		if e.Path == path && e.Message == message {
			return
		}
	}
	t.Fatalf("no entry with\n  path=%q\n  msg=%q\ngot entries: %#v", path, message, entries)
}

func TestValidateBytes_GraphShape_UnknownFromStage(t *testing.T) {
	// A longhand inputs[].from_stage naming a nonexistent stage, exercised at
	// every routed major. v1 is included (C3): the acceptance criteria name v1
	// documents, and the rule is version-agnostic, so v1 must be EXERCISED, not
	// asserted-by-equivalence.
	for _, version := range []string{"0.3", "1.0", "2"} {
		t.Run("v"+version, func(t *testing.T) {
			doc := fmt.Sprintf(`version: "%s"
workflows:
  feature_change:
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
`, version)
			entries := graphErrors(t, doc)
			requireEntry(t, entries,
				fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 0, 0),
				fmt.Sprintf(spec.MsgFmtFromStageUnknown, "nope", "feature_change"))
		})
	}
}

func TestValidateBytes_GraphShape_SelfFromStage(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: pull_request
            from_stage: implement
        produces:
          - artifact: pull_request
`
	entries := graphErrors(t, doc)
	requireEntry(t, entries,
		fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 0, 0),
		fmt.Sprintf(spec.MsgFmtFromStageNotEarlier, "implement", 0, 0))
}

func TestValidateBytes_GraphShape_LaterFromStage(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        inputs:
          - artifact: plan
            from_stage: review
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
`
	entries := graphErrors(t, doc)
	requireEntry(t, entries,
		fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 0, 0),
		fmt.Sprintf(spec.MsgFmtFromStageNotEarlier, "review", 1, 0))
}

func TestValidateBytes_GraphShape_NeedsUnknownReferent(t *testing.T) {
	// M4: a `needs` naming a nonexistent stage is expanded to an inputs entry
	// with an empty artifact and routed to the UNCHANGED from_stage-unknown
	// rule at the POST-EXPANSION inputs index — not a competing needs-specific
	// error. That distinction is the docs' promise, so assert both the message
	// identity and the absence of any /needs/ error.
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [nope]
        produces:
          - artifact: pull_request
`
	entries := graphErrors(t, doc)
	requireEntry(t, entries,
		fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 0, 0),
		fmt.Sprintf(spec.MsgFmtFromStageUnknown, "nope", "feature_change"))
	for _, e := range entries {
		if strings.Contains(e.Path, "/needs/") {
			t.Errorf("unknown `needs` referent must route to the from_stage rule, not a needs-specific error; got %q", e.Path)
		}
	}
}

func TestValidateBytes_GraphShape_NeedsArtifactlessReferent(t *testing.T) {
	// M5: a `needs` naming a stage whose type has no default input artifact is
	// rejected at the /needs/<j> index (distinct from M4's post-expansion
	// inputs index) with the no-default message. One sub-case per artifactless
	// type: review, deploy, acceptance.
	cases := []struct {
		name     string
		refType  string
		refStage string // the schema-valid stage block declaring the referent
	}{
		{
			name:    "review",
			refType: "review",
			refStage: `      - id: ref
        type: review
        executor:
          human: true
`,
		},
		{
			name:    "deploy",
			refType: "deploy",
			refStage: `      - id: ref
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        produces:
          - artifact: deployment
`,
		},
		{
			name:    "acceptance",
			refType: "acceptance",
			refStage: `      - id: ref
        type: acceptance
        executor:
          agent: claude-code
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `version: "2"
workflows:
  feature_change:
    stages:
` + tc.refStage + `      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [ref]
        produces:
          - artifact: pull_request
`
			entries := graphErrors(t, doc)
			requireEntry(t, entries,
				fmt.Sprintf(spec.PathFmtNeeds, "feature_change", 1, 0),
				fmt.Sprintf(spec.MsgFmtNeedsNoDefaultArtifact, "ref", tc.refType, "ref"))
		})
	}
}

func TestValidateBytes_GraphShape_DuplicateStageID(t *testing.T) {
	// M6a: at v0 the graph-shape duplicate-id rule fires with its own message
	// and path.
	t.Run("v0", func(t *testing.T) {
		doc := `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: implement
        type: review
        executor:
          human: true
`
		entries := graphErrors(t, doc)
		requireEntry(t, entries,
			fmt.Sprintf(spec.PathFmtStageID, "feature_change", 1),
			fmt.Sprintf(spec.MsgFmtDuplicateStageID, "implement", "feature_change", 0))
	})

	// M6b: at v2 the PRE-SCHEMA v2reuse duplicate check fires first, with its
	// OWN message — so ValidateBytes never reaches the graph-shape rule. Pin
	// the precedence: the reported message is the v2reuse one, NOT the
	// graph-shape "(also at …)" one.
	t.Run("v2_v2reuse_wins", func(t *testing.T) {
		doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: implement
        type: review
        executor:
          human: true
`
		entries := graphErrors(t, doc)
		var sawV2Reuse bool
		for _, e := range entries {
			if strings.Contains(e.Message, `duplicate stage id "implement" declared at positions 0 and 1`) {
				sawV2Reuse = true
			}
			if strings.Contains(e.Message, "(also at") {
				t.Errorf("at v2 the graph-shape duplicate message must not win; got %q", e.Message)
			}
		}
		if !sawV2Reuse {
			t.Errorf("expected the pre-schema v2reuse duplicate message, got entries: %#v", entries)
		}
	})
}

func TestValidateBytes_GraphShape_NeedsDedupesAgainstLonghand(t *testing.T) {
	// A `needs` entry whose derived (artifact, from_stage) pair is already
	// declared longhand is DROPPED, so `needs` + longhand resolve to the
	// identical input set however the author spelled it.
	t.Run("clean", func(t *testing.T) {
		// The deduped set carries exactly one plan input, so the document validates.
		doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan]
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
`
		if err := spec.ValidateBytes([]byte(doc)); err != nil {
			t.Fatalf("ValidateBytes: want nil, got %v", err)
		}
	})

	t.Run("dedup_observed_via_index", func(t *testing.T) {
		// LOAD-BEARING observation of the resolved set (the fix for the formerly
		// vacuous nil-only assertion): `needs: [plan, nope]` alongside a longhand
		// plan input. The `plan` derivation duplicates the declared input and is
		// DROPPED, so the surviving unknown `nope` derivation lands at
		// post-expansion index len(declared) = 1 and is reported at
		// /inputs/1/from_stage. WITHOUT dedup the un-dropped `plan` duplicate
		// would push `nope` to index 2, so this assertion fails when
		// deduplication is disabled — the reported index observes the resolved
		// input set's cardinality, which a nil-only check never does.
		doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan, nope]
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
`
		entries := graphErrors(t, doc)
		requireEntry(t, entries,
			fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 1, 1),
			fmt.Sprintf(spec.MsgFmtFromStageUnknown, "nope", "feature_change"))
	})
}

func TestValidateBytes_GraphShape_NeedsMixesValidAndArtifactless(t *testing.T) {
	// Concern-2 mixing branch (C2(a)): a stage whose `needs` combines a VALID
	// referent (`plan`, which derives cleanly) with an artifactless one (`ref`,
	// a review stage). expandNeeds processes entries in order and RETURNS on the
	// first no-default referent before applying any derived entries, mirroring
	// the backend's first-error posture — so the no-default rejection is reported
	// at the referent's /needs/<j> index (here j=1, NOT 0), pinning that the loop
	// advances past the valid entry and reports at the offending index. The
	// existing M5 case only exercises a SINGLE artifactless referent at index 0.
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: ref
        type: review
        executor:
          human: true
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan, ref]
        produces:
          - artifact: pull_request
`
	entries := graphErrors(t, doc)
	requireEntry(t, entries,
		fmt.Sprintf(spec.PathFmtNeeds, "feature_change", 2, 1),
		fmt.Sprintf(spec.MsgFmtNeedsNoDefaultArtifact, "ref", "review", "ref"))
}

func TestValidateBytes_GraphShape_NeedsAppendsAfterNonMatchingLonghand(t *testing.T) {
	// Concern-2 index branch (C2(a)): ONE declared longhand input that is NOT a
	// verbatim match for the derived entry, plus an unknown `needs` referent. The
	// derived entry is appended at post-expansion index len(declared) = 1, so the
	// unknown referent is reported at /inputs/1/from_stage. This pins the
	// append-AFTER-declared order the M4 path promise depends on: reversing the
	// append order, or dropping the declared input, moves the reported index and
	// reddens this assertion. The only other M4 case has zero declared inputs, so
	// its index 0 is trivially right and cannot catch the arithmetic.
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [nope]
        inputs:
          - artifact: plan
            from_stage: plan
        produces:
          - artifact: pull_request
`
	entries := graphErrors(t, doc)
	requireEntry(t, entries,
		fmt.Sprintf(spec.PathFmtFromStage, "feature_change", 1, 1),
		fmt.Sprintf(spec.MsgFmtFromStageUnknown, "nope", "feature_change"))
}

func TestValidateBytes_GraphShape_WellFormedWorkflowIsClean(t *testing.T) {
	doc := `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        needs: [plan]
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
`
	if err := spec.ValidateBytes([]byte(doc)); err != nil {
		t.Fatalf("ValidateBytes: want nil, got %v", err)
	}
}
