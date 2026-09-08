package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// chUnrelatedDocumentDeclarations is a declaration seam that resolves SOMETHING
// — just not the charter. It is the M7 / condition-H2 bad state, seeded by
// construction: L1 succeeds, so only L2 can refuse.
func chUnrelatedDocumentDeclarations(context.Context, *run.Run, *run.Stage) ([]repodoc.Declaration, string, error) {
	return []repodoc.Declaration{{
		Path:            chOtherPath,
		DeclarationSite: "some other consumer",
		Framing:         repodoc.Framing{Heading: "Unrelated document"},
	}}, chDefaultBranch, nil
}

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
		requires        *bool  // the row's persisted determination; nil is the LEGACY row
		stageIsPlan     bool
		wantUndecidable bool // stageRequiresCharter returns an error → handler refuses
	}{
		// Rows carrying a persisted determination: the FACT decides, and the
		// cached spec is never read — corrupt or not.
		{"persisted grooming on a plan stage", chGroomingSpec, "", chTrue(), true, false},
		{"persisted non-grooming on a plan stage", chPlainSpec, "", chFalse(), true, false},
		{"persisted grooming on a non-plan stage", chGroomingSpec, "", chTrue(), false, false},
		{"persisted non-grooming, nil workflow spec", "", "", chFalse(), true, false},
		{"persisted grooming, unparseable spec", chCorruptGroomingSpec, "", chTrue(), true, false},
		{"persisted grooming, spec token destroyed", chGroomingSpecTokenDestroyed, "", chTrue(), true, false},
		{"persisted non-grooming, corrupt spec with incidental token", chCorruptPlainSpecIncidentalToken, "", chFalse(), true, false},
		// Legacy rows (no persisted determination): derived from the cached
		// spec, refused when undecidable.
		{"legacy grooming spec on a plan stage", chGroomingSpec, "", nil, true, false},
		{"legacy plain spec on a plan stage", chPlainSpec, "", nil, true, false},
		{"legacy grooming spec on a non-plan stage", chGroomingSpec, "", nil, false, false},
		{"legacy nil workflow spec", "", "", nil, true, true},
		{"legacy unparseable grooming spec", chCorruptGroomingSpec, "", nil, true, true},
		{"legacy unparseable plain spec", chCorruptPlainSpec, "", nil, true, true},
		{"legacy workflow absent", chGroomingSpec, "not_declared", nil, true, true},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			installConventions(t, chConventions(chCharterPath), nil)
			s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
				specYAML:        tc.spec,
				resolver:        &repodoc.Resolver{Fetcher: newCHFetcher(), Commits: &chCommits{sha: chPinnedCommit}},
				baseRef:         chDefaultBaseRef,
				useCharter:      true,
				stageIsPlan:     tc.stageIsPlan,
				requiresCharter: tc.requires,
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

// TestGroomingPrompt_PreviewMatchesServed is E54.12 / #2804's criterion 3 and
// the operator's binding condition 3 (exact refusal parity). It drives BOTH
// real HTTP handlers — handleGetStagePromptRender and handleGetStagePrompt —
// for the SAME stage on ONE server, and COMPARES the two responses rather than
// asserting each in isolation:
//
//   - success rows: identical prompt TEXT, identical prompt_hash, and agreement
//     on whether the injected charter block is present.
//   - refusal rows: identical HTTP status, identical error.code and identical
//     error.details.reason.
//
// PARITY IS NOT THE ONLY ASSERTION. Every row also declares its own EXPECTED
// outcome — wantStatus, plus wantCharter on a 200 row — and the success-vs-
// refusal branch is selected by that declared outcome, NOT by the status the
// served endpoint happened to return. Selecting off the observed status makes
// the table vacuous in both directions: a refusal row where both endpoints
// regressed to 200 would take the success branch and pass (they would agree on
// the then-absent charter block too), and a success row where both regressed to
// 500 would take the refusal branch and pass. A shared regression is exactly
// the class an agreement test is otherwise blind to.
//
// Per-layer units would pass while the seam between them diverged, which is
// exactly the defect this issue names: before the change the preview resolved
// no documents, wired neither charter layer and set no grooming context, so it
// rendered a groom stage as an ordinary unanchored plan and refused nothing.
//
// The PREVIEW is fetched FIRST and the signed prompt SECOND, so the served
// path's markStageRunningOnPromptFetch side effect cannot perturb the
// comparison. The fixture stages carry no dispatched state today, so the flip
// no-ops; the ordering makes the test robust if that changes.
//
// Counterfactuals: deleting the preview's previewInjectedDocuments call reddens
// the success rows (the preview loses the charter block, so the hashes differ);
// deleting its assertCharterInjected call reddens the refusal rows (preview 200
// against a served 500).
func TestGroomingPrompt_PreviewMatchesServed(t *testing.T) {
	charterBlockMarker := "### " + charterFraming().Heading

	rows := []struct {
		name        string
		spec        string
		workflowID  string // override when non-empty (the workflow-absent rows)
		requires    *bool  // the row's persisted determination; nil is the LEGACY row
		charterPath string // conventions charter path; chCharterPath unless overridden
		noCharter   bool   // conventions declare NO charter block at all
		baseRef     func(ctx context.Context, repo forge.RepoRef) (string, error)
		decls       func(context.Context, *run.Run, *run.Stage) ([]repodoc.Declaration, string, error)
		noSeam      bool // wire NO declaration seam and no resolver at all
		stageIsPlan bool
		// wantStatus is the EXPECTED HTTP status, asserted on BOTH endpoints. It
		// is MANDATORY on every row and is what makes a row enforce its own
		// INTENT rather than reading that intent off the served response. Keyed
		// on the served status alone, a refusal row where BOTH endpoints
		// regressed to 200 would take the success-parity branch and pass (they
		// would also agree on the then-absent charter block), and a success row
		// where both regressed to 500 would take the refusal branch and pass.
		wantStatus int
		// wantRefusal is the expected error.details.reason on a 500 row. It is
		// legitimately EMPTY on a refusal produced by a *repodoc.ResolveError,
		// which carries path / declaration_site but NO `reason` key; on such a
		// row wantStatus is what pins that a refusal occurred at all and the
		// assertions below degrade to code + reason PARITY only.
		wantRefusal string
		// wantCharter is the expected charter-block presence on a 200 row,
		// asserted against BOTH responses. Parity alone is satisfied by both
		// endpoints LOSING the block.
		wantCharter bool
	}{
		// Success rows: both endpoints must agree byte-for-byte, and each row
		// pins whether the charter block is expected in those bytes.
		{name: "grooming spec on a plan stage", spec: chGroomingSpec, requires: chTrue(), stageIsPlan: true,
			wantStatus: http.StatusOK, wantCharter: true},
		{name: "plain spec on a plan stage", spec: chPlainSpec, requires: chFalse(), stageIsPlan: true,
			wantStatus: http.StatusOK, wantCharter: false},
		{name: "grooming spec on a non-plan stage", spec: chGroomingSpec, requires: chTrue(), stageIsPlan: false,
			wantStatus: http.StatusOK, wantCharter: false},
		{name: "persisted non-grooming, nil workflow spec", spec: "", requires: chFalse(), stageIsPlan: true,
			wantStatus: http.StatusOK, wantCharter: false},
		{name: "persisted non-grooming, corrupt spec with incidental token", spec: chCorruptPlainSpecIncidentalToken,
			requires: chFalse(), stageIsPlan: true, wantStatus: http.StatusOK, wantCharter: false},
		{name: "persisted grooming, unparseable spec", spec: chCorruptGroomingSpec, requires: chTrue(),
			stageIsPlan: true, wantStatus: http.StatusOK, wantCharter: true},
		{name: "legacy grooming spec on a plan stage", spec: chGroomingSpec, stageIsPlan: true,
			wantStatus: http.StatusOK, wantCharter: true},
		{name: "legacy plain spec on a plan stage", spec: chPlainSpec, stageIsPlan: true,
			wantStatus: http.StatusOK, wantCharter: false},

		// Refusal rows: both endpoints must refuse, and refuse identically.
		{name: "legacy row, unparseable spec", spec: chCorruptGroomingSpec,
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonGroomingSpecUnreadable},
		{name: "legacy row, nil workflow spec", spec: "",
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonGroomingSpecUnreadable},
		{name: "legacy row, workflow absent", spec: chGroomingSpec, workflowID: "not_declared",
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonGroomingSpecUnreadable},
		// A *repodoc.ResolveError refusal: no `reason` key exists to compare, so
		// wantStatus carries the "must refuse" half and the reason assertion
		// degrades to parity.
		{name: "declared charter does not resolve", spec: chGroomingSpec, requires: chTrue(), charterPath: "docs/no-such-charter.md",
			stageIsPlan: true, wantStatus: http.StatusInternalServerError, wantRefusal: ""},
		{name: "charter path empty", spec: chGroomingSpec, requires: chTrue(), charterPath: "   ",
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonCharterPathEmpty},
		{name: "charter absent from conventions", spec: chGroomingSpec, requires: chTrue(), noCharter: true,
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonCharterAbsent},
		{name: "base ref unresolved", spec: chGroomingSpec, requires: chTrue(), stageIsPlan: true,
			baseRef:     func(context.Context, forge.RepoRef) (string, error) { return "", nil },
			wantStatus:  http.StatusInternalServerError,
			wantRefusal: reasonCharterBaseRefUnresolved},

		// The two L2-ONLY refusals. Every row above is refused by L1 as well
		// (charterDeclarations calls stageRequiresCharter and charterDeclaredPath
		// itself), so these two are what discriminate the preview's
		// assertCharterInjected call specifically: L1 either cannot run at all or
		// resolves a document that is NOT the charter.
		{name: "M6 declaration seam entirely unwired", spec: chGroomingSpec, requires: chTrue(), noSeam: true,
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonCharterNotInjected},
		{name: "M7 an unrelated document injected, no charter", spec: chGroomingSpec, requires: chTrue(),
			decls:       chUnrelatedDocumentDeclarations,
			stageIsPlan: true, wantStatus: http.StatusInternalServerError,
			wantRefusal: reasonCharterNotInjected},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			conv := chConventions(chCharterPath)
			switch {
			case tc.noCharter:
				conv = chConventionsWithoutCharter()
			case tc.charterPath != "":
				conv = chConventions(tc.charterPath)
			}
			installConventions(t, conv, nil)

			baseRef := chDefaultBaseRef
			if tc.baseRef != nil {
				baseRef = tc.baseRef
			}
			ff := newCHFetcher()
			ff.extra[chOtherPath] = chOtherContent
			opts := chServerOpts{
				specYAML:        tc.spec,
				resolver:        &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
				baseRef:         baseRef,
				useCharter:      true,
				stageIsPlan:     tc.stageIsPlan,
				requiresCharter: tc.requires,
			}
			switch {
			case tc.noSeam:
				// The no-forge deployment: no resolver, no base ref, no seam.
				opts.resolver, opts.baseRef, opts.useCharter = nil, nil, false
			case tc.decls != nil:
				opts.decls, opts.useCharter = tc.decls, false
			}
			s, runID, stageID, priv, _ := newCharterServer(t, opts)
			rr := s.cfg.RunRepo.(*promptRunRepo)
			if tc.workflowID != "" {
				rr.getRuns[runID].WorkflowID = tc.workflowID
			}
			if tc.spec == "" {
				rr.getRuns[runID].WorkflowSpec = nil
			}

			// PREVIEW FIRST, then the signed prompt.
			pw := promptRenderRequest(t, s, stageID)
			sw := promptRequest(t, s, runID, stageID, priv, "")

			// (1) Status parity, on every row.
			if pw.Code != sw.Code {
				t.Fatalf("status divergence: preview = %d, served = %d\npreview body: %s\nserved body: %s",
					pw.Code, sw.Code, pw.Body.String(), sw.Body.String())
			}
			// (1b) EXPECTED outcome, on every row. Parity alone is vacuous:
			// both endpoints regressing the SAME way agrees with itself. The
			// branch below is selected by tc.wantStatus, never by the observed
			// status, so a refusal row cannot be greened by an agreeing 200 and
			// a success row cannot be greened by an agreeing 500.
			if sw.Code != tc.wantStatus {
				t.Fatalf("both endpoints answered %d, want %d — the row's expected outcome did not happen"+
					"\npreview body: %s\nserved body: %s", sw.Code, tc.wantStatus, pw.Body.String(), sw.Body.String())
			}

			if tc.wantStatus != http.StatusOK {
				// (2) Refusal parity: same code, same reason.
				pCode, pReason := chErrCodeReason(t, pw.Body.Bytes())
				sCode, sReason := chErrCodeReason(t, sw.Body.Bytes())
				if pCode != sCode {
					t.Errorf("error.code divergence: preview = %q, served = %q", pCode, sCode)
				}
				if pReason != sReason {
					t.Errorf("error.details.reason divergence: preview = %q, served = %q", pReason, sReason)
				}
				if sCode != "document_injection_failed" {
					t.Errorf("error.code = %q, want document_injection_failed\n%s", sCode, sw.Body.String())
				}
				if tc.wantRefusal != "" && sReason != tc.wantRefusal {
					t.Errorf("refusal reason = %q, want %q — a DIFFERENT control refused\n%s",
						sReason, tc.wantRefusal, sw.Body.String())
				}
				return
			}

			// (3) Success parity: identical bytes, identical hash, agreeing on
			// whether the charter block is present.
			var pResp, sResp promptResponse
			if err := json.Unmarshal(pw.Body.Bytes(), &pResp); err != nil {
				t.Fatalf("decode preview: %v\n%s", err, pw.Body.String())
			}
			if err := json.Unmarshal(sw.Body.Bytes(), &sResp); err != nil {
				t.Fatalf("decode served: %v\n%s", err, sw.Body.String())
			}
			if pResp.Prompt != sResp.Prompt {
				t.Errorf("preview and served prompt TEXT diverge\n--- preview ---\n%s\n--- served ---\n%s",
					pResp.Prompt, sResp.Prompt)
			}
			if pResp.PromptHash != sResp.PromptHash {
				t.Errorf("prompt_hash divergence: preview = %q, served = %q", pResp.PromptHash, sResp.PromptHash)
			}
			pCharter := strings.Contains(pResp.Prompt, charterBlockMarker)
			sCharter := strings.Contains(sResp.Prompt, charterBlockMarker)
			if pCharter != sCharter {
				t.Errorf("charter block present: preview = %v, served = %v — the endpoints disagree on the "+
					"security-relevant block", pCharter, sCharter)
			}
			// EXPECTED presence, asserted against BOTH responses: agreement is
			// also satisfied by both endpoints losing the block, so the row
			// pins which answer is correct rather than only that they match.
			if pCharter != tc.wantCharter {
				t.Errorf("preview charter block present = %v, want %v", pCharter, tc.wantCharter)
			}
			if sCharter != tc.wantCharter {
				t.Errorf("served charter block present = %v, want %v", sCharter, tc.wantCharter)
			}
		})
	}
}

// chErrCodeReason digs the error code and the details.reason out of an error
// envelope ({"error": {"code", "message", "details"}}).
func chErrCodeReason(t *testing.T, body []byte) (code, reason string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, string(body))
	}
	env, _ := decoded["error"].(map[string]any)
	if env == nil {
		t.Fatalf("body carries no error envelope: %s", string(body))
	}
	code, _ = env["code"].(string)
	if details, ok := env["details"].(map[string]any); ok {
		reason, _ = details["reason"].(string)
	}
	return code, reason
}
