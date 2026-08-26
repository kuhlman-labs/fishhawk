package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// TestGroomingPrompt_CrossLayerAgreement is criterion 3 (approval note "Step 11
// is the test the issue actually asks for"). It drives the REAL prompt handler
// over a table of workflow-spec fixtures — including the whole M8 family — and
// for each row asserts a THREE-WAY agreement between:
//
//	(i)   stageRequiresCharter's verdict (computed directly here),
//	(ii)  whether the served prompt carries the grooming artifact contract, and
//	(iii) whether the injected charter block is present in that same served
//	      prompt (or the request was refused as undecidable).
//
// (ii) and (iii) come entirely from the independent handler path
// (handleGetStagePrompt → resolveInjectedDocuments → assertCharterInjected →
// prompt.Build), so a row where the prompt says "grooming" but stageRequiresCharter
// says "plan" — or vice versa — fails. This crosses the server/prompt boundary
// #2834 records as broken. It is a real counterfactual, not a restatement: a
// future edit that re-derives the determination from anything other than
// stageRequiresCharter (a workflow-NAME check, a bare bytes.Contains, or the
// deleted Build dispatch fork) reddens on exactly the rows where the layers
// would drift apart.
func TestGroomingPrompt_CrossLayerAgreement(t *testing.T) {
	// groomContractMarker appears ONLY in buildGroomingPropose's output.
	const groomContractMarker = "You are producing a backlog grooming report"
	// charterBlockMarker is the injected charter's rendered heading.
	charterBlockMarker := "### " + charterFraming().Heading

	rows := []struct {
		name            string
		spec            string
		workflowID      string // override when non-empty (the workflow-absent rows)
		stageIsPlan     bool
		wantUndecidable bool // stageRequiresCharter returns an error → handler refuses
	}{
		{"grooming spec on a plan stage", chGroomingSpec, "", true, false},
		{"plain spec on a plan stage", chPlainSpec, "", true, false},
		{"grooming spec on a non-plan stage", chGroomingSpec, "", false, false},
		{"nil workflow spec", "", "", true, false},
		// The M8 family.
		{"M8a unparseable grooming-attributable", chCorruptGroomingSpec, "", true, true},
		{"M8b unparseable and plain", chCorruptPlainSpec, "", true, false},
		{"M8c workflow absent but grooming-shaped", chGroomingSpec, "not_declared", true, true},
		{"token in a comment", chSchemaInvalidTokenInComment, "", true, false},
		{"token in an unrelated scalar", chSchemaInvalidTokenInScalar, "", true, false},
		{"token in another workflow", chSchemaInvalidOtherWorkflowGrooms, "", true, false},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			installConventions(t, chConventions(chCharterPath), nil)
			s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
				specYAML:    tc.spec,
				resolver:    &repodoc.Resolver{Fetcher: newCHFetcher(), Commits: &chCommits{sha: chPinnedCommit}},
				baseRef:     chDefaultBaseRef,
				useCharter:  true,
				stageIsPlan: tc.stageIsPlan,
			})
			rr := s.cfg.RunRepo.(*promptRunRepo)
			if tc.workflowID != "" {
				rr.getRuns[runID].WorkflowID = tc.workflowID
			}
			// An empty spec must be a genuine nil-spec legacy row.
			if tc.spec == "" {
				rr.getRuns[runID].WorkflowSpec = nil
			}
			runRow := rr.getRuns[runID]
			stage := rr.getStages[stageID]

			// (i) the layer-1 determination, computed directly.
			required, sErr := stageRequiresCharter(runRow, stage)

			w := promptRequest(t, s, runID, stageID, priv, "")

			if tc.wantUndecidable {
				if sErr == nil {
					t.Fatalf("expected stageRequiresCharter to be undecidable (error), got required=%v err=nil", required)
				}
				if w.Code == http.StatusOK {
					t.Fatalf("an undecidable grooming spec served a 200 prompt instead of refusing:\n%s", w.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
				}
				chAssertReason(t, body, reasonGroomingSpecUnreadable)
				return
			}

			if sErr != nil {
				t.Fatalf("stageRequiresCharter unexpectedly errored: %v", sErr)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			var resp promptResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode prompt response: %v\n%s", err, w.Body.String())
			}

			gotGrooming := strings.Contains(resp.Prompt, groomContractMarker)
			gotCharter := strings.Contains(resp.Prompt, charterBlockMarker)

			// (ii) prompt grooming-ness must match stageRequiresCharter.
			if gotGrooming != required {
				t.Errorf("served-prompt grooming contract present=%v but stageRequiresCharter=%v — the two layers disagree:\n%s",
					gotGrooming, required, resp.Prompt)
			}
			// (iii) charter presence must match too.
			if gotCharter != required {
				t.Errorf("served-prompt charter block present=%v but stageRequiresCharter=%v — the two layers disagree:\n%s",
					gotCharter, required, resp.Prompt)
			}
		})
	}
}
