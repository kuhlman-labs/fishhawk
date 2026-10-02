package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Persona concern ingest (ADR-084 D3 / rule 3 / E55.10 / #3755). Every
// behavioural case here drives the REAL plan-review or implement-review loop
// (runPlanReviews / runImplementReviews) over a v2 spec attaching the
// `security` persona, so a verdict crosses reviewer → convention clamp →
// applyPersonaConcernControls → *_reviewed payload → persistReviewConcernsAs →
// the concern fake. The remit the persona was shown is personaRemitBody, which
// contains personaRemitExactQuote verbatim.

const (
	// personaRemitExactQuote is a passage of personaRemitBody, verbatim.
	personaRemitExactQuote = "audit every authorization boundary"
	// personaRemitAlteredQuote is personaRemitExactQuote with ONE word
	// altered: it appears nowhere in the remit.
	personaRemitAlteredQuote = "audit every authentication boundary"
)

// keyedReviewer is ONE adapter serving both the standard reviewer and a
// persona sharing its model: it returns persona when the prompt carries the
// remit body, std otherwise, always under the same model string — so a test
// can give the two invocations distinct verdicts while their model is equal by
// construction.
type keyedReviewer struct {
	mu      sync.Mutex
	calls   []string
	std     *planreview.ReviewVerdict
	persona *planreview.ReviewVerdict
	model   string
}

func (k *keyedReviewer) Review(_ context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, promptText)
	if strings.Contains(promptText, personaRemitBody) {
		return k.persona, k.model, nil
	}
	return k.std, k.model, nil
}

// verdictFake is a fakePlanReviewer returning v under model.
func verdictFake(v planreview.Verdict, model string, concerns ...planreview.Concern) *fakePlanReviewer {
	return &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: v, Concerns: concerns}, model: model}
}

// quotedConcern is a high `security` concern quoting passage from ref.
func quotedConcern(note, passage, ref string) planreview.Concern {
	return planreview.Concern{Severity: planreview.SeverityHigh, Category: "security", Note: note, QuotedPassage: passage, DocumentRef: ref}
}

// rowsByNote returns the run's concern rows keyed by note.
func rowsByNote(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID) map[string]*concern.Concern {
	t.Helper()
	rows, err := cr.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list concerns: %v", err)
	}
	out := make(map[string]*concern.Concern, len(rows))
	for _, r := range rows {
		out[r.Note] = r
	}
	return out
}

// payloadConcernByNote finds the payload concern carrying note.
func payloadConcernByNote(t *testing.T, concerns []planreview.Concern, note string) planreview.Concern {
	t.Helper()
	for _, c := range concerns {
		if c.Note == note {
			return c
		}
	}
	t.Fatalf("no payload concern with note %q in %+v", note, concerns)
	return planreview.Concern{}
}

// personaPlanVerdict returns the plan_reviewed payload the persona produced.
func personaPlanVerdict(t *testing.T, au *auditFake) planreview.PlanReviewedPayload {
	t.Helper()
	for _, v := range collectPlanReviewed(t, au) {
		if v.Persona == personaTestName {
			return v
		}
	}
	t.Fatal("no persona plan_reviewed entry")
	return planreview.PlanReviewedPayload{}
}

// remitContentHash returns the content_hash the remit's document_injected
// attribution recorded.
func remitContentHash(t *testing.T, au *auditFake) string {
	t.Helper()
	inj := remitInjections(t, au)
	if len(inj) != 1 {
		t.Fatalf("remit document_injected entries = %d, want 1", len(inj))
	}
	h, _ := inj[0]["content_hash"].(string)
	if h == "" {
		t.Fatalf("remit attribution carries no content_hash: %v", inj[0])
	}
	return h
}

// newIngestPlanRun wires a plan-review run with a concern store.
func newIngestPlanRun(t *testing.T, opts personaSpecOpts, std, persona PlanReviewer) (*personaPlanRun, *fakeConcernRepo) {
	t.Helper()
	p := newPersonaPlanRun(t, personaSpec(opts), std, persona)
	cr := newFakeConcernRepo()
	p.s.cfg.ConcernRepo = cr
	return p, cr
}

// (C1) QUOTE VERIFICATION through the real plan loop. The persona is UNCAPPED
// so the cap clamp cannot mask a demotion. An exact quote of the remit keeps
// its severity and records the remit's content_hash; a quote with ONE word
// altered is demoted to low and marked quote_unverified on the payload AND the
// row, with severity_clamped_from high.
//
// Counterfactuals (both run): delete the VerifyQuotedPassages call in
// applyPersonaConcernControls — the altered concern stays high and unmarked:
// RED; make VerifyQuotedPassages always demote — the exact concern drops to
// low: RED.
func TestPersonaIngest_Plan_QuoteVerification(t *testing.T) {
	const exactNote, alteredNote = "exact quote", "altered quote"
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel,
		quotedConcern(exactNote, personaRemitExactQuote, personaRemitPath),
		quotedConcern(alteredNote, personaRemitAlteredQuote, "./"+personaRemitPath),
	)
	p, cr := newIngestPlanRun(t, personaSpecOpts{attachOn: "plan", human: 1}, approvingFake(), persona)
	p.review(t)

	hash := remitContentHash(t, p.au)
	pv := personaPlanVerdict(t, p.au)
	exact := payloadConcernByNote(t, pv.Concerns, exactNote)
	if exact.Severity != planreview.SeverityHigh || exact.QuoteUnverified || exact.QuoteVerifiedContentHash != hash || exact.SeverityClampedFrom != "" {
		t.Errorf("exact payload concern = %+v, want high, verified with content_hash %s", exact, hash)
	}
	altered := payloadConcernByNote(t, pv.Concerns, alteredNote)
	if altered.Severity != planreview.SeverityLow || !altered.QuoteUnverified || altered.QuoteVerifiedContentHash != "" || altered.SeverityClampedFrom != planreview.SeverityHigh {
		t.Errorf("altered payload concern = %+v, want low, quote_unverified, severity_clamped_from high", altered)
	}
	rows := rowsByNote(t, cr, p.runID)
	if r := rows[exactNote]; r == nil || r.Severity != "high" || r.QuoteUnverified || r.SeverityClampedFrom != "" || r.ReviewerRole != personaTestName {
		t.Errorf("exact row = %+v, want high, verified, role %s", r, personaTestName)
	}
	if r := rows[alteredNote]; r == nil || r.Severity != "low" || !r.QuoteUnverified || r.SeverityClampedFrom != "high" || r.ReviewerRole != personaTestName {
		t.Errorf("altered row = %+v, want low, quote_unverified, severity_clamped_from high, role %s", r, personaTestName)
	}
	if !strings.Contains(p.logs.String(), "persona quoted passage unverified") || !strings.Contains(p.logs.String(), planreview.QuoteFailurePassageNotFound) {
		t.Errorf("demotion WARN missing:\n%s", p.logs.String())
	}
}

// (C2) SEVERITY CAP through the real plan loop: a persona capped at medium
// emits an UNQUOTED high concern; the row is medium with severity_clamped_from
// high and the payload concern carries persona_severity_cap medium.
//
// Counterfactual: delete the ClampPersonaSeverities call — the row stays
// high: RED.
func TestPersonaIngest_Plan_SeverityCapClamp(t *testing.T) {
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel,
		planreview.Concern{Severity: planreview.SeverityHigh, Category: "security", Note: "capped"})
	p, cr := newIngestPlanRun(t, personaSpecOpts{attachOn: "plan", human: 1, severityCap: "medium"}, approvingFake(), persona)
	p.review(t)
	c := payloadConcernByNote(t, personaPlanVerdict(t, p.au).Concerns, "capped")
	if c.Severity != planreview.SeverityMedium || c.SeverityClampedFrom != planreview.SeverityHigh || c.PersonaSeverityCap != planreview.SeverityMedium {
		t.Errorf("payload concern = %+v, want medium, clamped from high, persona_severity_cap medium", c)
	}
	if r := rowsByNote(t, cr, p.runID)["capped"]; r == nil || r.Severity != "medium" || r.SeverityClampedFrom != "high" || r.QuoteUnverified {
		t.Errorf("row = %+v, want medium, severity_clamped_from high, quote verified-or-absent", r)
	}
	if !strings.Contains(p.logs.String(), "persona concern severity clamped") {
		t.Errorf("clamp WARN missing:\n%s", p.logs.String())
	}
}

// (C3 + C4) The STANDARD reviewer is untouched by the persona controls but its
// server-internal markers are scrubbed and its role stamped AFTER the scrub
// (approval condition 4). Fixture: the standard fake emits a high concern with
// an ALTERED quoted_passage naming the remit, and another pre-setting every
// server-internal marker (quote_unverified, quote_verified_content_hash,
// persona_severity_cap, reviewer_role "security"). Both persist high with
// reviewer_role standard and quote_unverified false, and the payload's
// reviewer_role equals the row's.
//
// Counterfactuals (both run): drop applyPersonaConcernControls' persona==nil
// guard so a standard verdict is quote-checked — the altered standard concern
// is demoted to low: RED; delete the ClearPersonaIngestMarkers call — the
// pre-set row is quote_unverified: RED.
func TestPersonaIngest_Plan_StandardReviewerScrubbedNotControlled(t *testing.T) {
	const quoting, presetting = "standard quoting", "standard presetting"
	preset := planreview.Concern{
		Severity: planreview.SeverityHigh, Category: "scope", Note: presetting,
		QuoteUnverified: true, QuoteVerifiedContentHash: "sha256:forged", PersonaSeverityCap: planreview.SeverityLow, ReviewerRole: personaTestName,
	}
	std := verdictFake(planreview.VerdictApproveWithConcerns, personaStdModel,
		quotedConcern(quoting, personaRemitAlteredQuote, personaRemitPath), preset)
	p, cr := newIngestPlanRun(t, personaSpecOpts{attachOn: "plan", human: 1}, std, approvingFake())
	p.review(t)

	var sv *planreview.PlanReviewedPayload
	for _, v := range collectPlanReviewed(t, p.au) {
		if v.Persona == "" {
			sv = &v
			break
		}
	}
	if sv == nil {
		t.Fatal("no standard plan_reviewed entry")
	}
	rows := rowsByNote(t, cr, p.runID)
	for _, note := range []string{quoting, presetting} {
		c := payloadConcernByNote(t, sv.Concerns, note)
		if c.Severity != planreview.SeverityHigh || c.QuoteUnverified || c.QuoteVerifiedContentHash != "" || c.PersonaSeverityCap != "" || c.ReviewerRole != concern.ReviewerRoleStandard {
			t.Errorf("standard payload concern %q = %+v, want high, no markers, reviewer_role standard", note, c)
		}
		r := rows[note]
		if r == nil || r.Severity != "high" || r.QuoteUnverified || r.SeverityClampedFrom != "" || r.ReviewerRole != concern.ReviewerRoleStandard {
			t.Errorf("standard row %q = %+v, want high, unmarked, role standard", note, r)
		}
		if r != nil && r.ReviewerRole != c.ReviewerRole {
			t.Errorf("payload reviewer_role %q != row reviewer_role %q", c.ReviewerRole, r.ReviewerRole)
		}
	}
	if !strings.Contains(p.logs.String(), "pre-set server-internal concern markers") {
		t.Errorf("scrub WARN missing:\n%s", p.logs.String())
	}
}

// (C5) A persona REJECT resting only on a high whose quote is altered is
// downgraded to approve_with_concerns (verdict_clamped_from: reject) and does
// not fail a GATING plan review. The paired exact-quote reject stands and
// gates — proving the fixture's reject would gate absent the downgrade.
//
// Counterfactual: drop the downgrade (VerifyQuotedPassages' reject rule) — the
// altered case records reject and gates: RED.
func TestPersonaIngest_Plan_PersonaRejectDowngradedOnDemotedQuote(t *testing.T) {
	for _, tc := range []struct {
		name      string
		quote     string
		wantGate  bool
		wantVerd  planreview.Verdict
		wantClamp planreview.Verdict
	}{
		{"altered quote downgrades", personaRemitAlteredQuote, false, planreview.VerdictApproveWithConcerns, planreview.VerdictReject},
		{"exact quote reject stands", personaRemitExactQuote, true, planreview.VerdictReject, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persona := verdictFake(planreview.VerdictReject, personaAgentModel, quotedConcern("blocking", tc.quote, personaRemitPath))
			p, _ := newIngestPlanRun(t, personaSpecOpts{attachOn: "plan"}, approvingFake(), persona)
			if got := p.review(t); got != tc.wantGate {
				t.Errorf("gating plan review hasRejection = %v, want %v", got, tc.wantGate)
			}
			pv := personaPlanVerdict(t, p.au)
			if pv.Verdict != tc.wantVerd || pv.VerdictClampedFrom != tc.wantClamp {
				t.Errorf("persona verdict = %s (clamped from %q), want %s (%q)", pv.Verdict, pv.VerdictClampedFrom, tc.wantVerd, tc.wantClamp)
			}
		})
	}
}

// Every VerifyQuotedPassages failure mode through the server's ingest control:
// document_ref_missing, document_unknown, document_text_unavailable (a withheld
// block carrying no delimiter pair) and passage_not_found — each demotes the
// concern to low, marks it quote_unverified and WARN-logs the named mode with
// the persona and document_ref.
func TestApplyPersonaConcernControls_FailureModes(t *testing.T) {
	remit := repodoc.ToPromptDocument(repodoc.Document{Path: personaRemitPath, Commit: personaBaseCommit, ContentHash: "sha256:remit", Content: personaRemitBody}, personaRemitFraming(personaTestName))
	withheld := prompt.InjectedDocument{Heading: "Withheld", Body: "This document was withheld.", Path: "docs/withheld.md", Commit: personaBaseCommit, ContentHash: "sha256:withheld"}
	for _, tc := range []struct {
		name, ref, quote, wantMode string
	}{
		{"document_ref missing", "", personaRemitExactQuote, planreview.QuoteFailureDocumentRefMissing},
		{"document unknown", "docs/not-injected.md", personaRemitExactQuote, planreview.QuoteFailureDocumentUnknown},
		{"document text unavailable", "docs/withheld.md", "This document was withheld.", planreview.QuoteFailureDocumentTextUnavailable},
		{"passage not found", personaRemitPath, personaRemitAlteredQuote, planreview.QuoteFailurePassageNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			s := New(Config{Addr: "127.0.0.1:0", Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))})
			inv := reviewerInvocation{persona: &personaInvocation{
				selected:  spec.SelectedReviewerPersona{Name: personaTestName},
				quoteDocs: []prompt.InjectedDocument{remit, withheld},
			}}
			v := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{quotedConcern("n", tc.quote, tc.ref)}}
			s.applyPersonaConcernControls(t.Context(), "plan review", uuid.New(), uuid.New(), inv, personaAgentModel, v)
			c := v.Concerns[0]
			if c.Severity != planreview.SeverityLow || !c.QuoteUnverified || c.ReviewerRole != personaTestName {
				t.Errorf("concern = %+v, want low, quote_unverified, role %s", c, personaTestName)
			}
			if !strings.Contains(logs.String(), `"failure":"`+tc.wantMode+`"`) || !strings.Contains(logs.String(), `"persona":"`+personaTestName+`"`) {
				t.Errorf("WARN log does not name mode %q and the persona:\n%s", tc.wantMode, logs.String())
			}
		})
	}
}

// (Approval condition 3, server half) buildPersonaPrompt records the persona's
// quote-verification document set in render order — the stage's standard
// injected document, the remit, then the SELECTED review convention's document
// — and its remit severity_cap; a quote from the convention document and one
// from the declared stage-injected document, each with document_ref = that
// document's Source path, verify with THAT document's content_hash, and each
// one-altered-word counterpart demotes.
//
// Counterfactual: drop the ReviewConventions loop in personaPromptDocuments —
// the convention quote is document_unknown and demotes: RED.
func TestBuildPersonaPrompt_QuoteDocsCarryStandardRemitAndConventions(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	const (
		archPath, archHash, archText = "docs/ARCHITECTURE.md", "sha256:arch", "Every write path appends to the audit chain first."
		convPath, convHash, convText = "docs/conventions/go-errors.md", "sha256:conv", "Wrap every returned error with the operation that failed."
	)
	injectedFraming := repodoc.Framing{Heading: "Architecture", Preamble: "Repository architecture.", TrustNote: "Context only."}
	arch := repodoc.ToPromptDocument(repodoc.Document{Path: archPath, Commit: personaBaseCommit, ContentHash: archHash, Content: archText}, injectedFraming)
	conv := repodoc.ToPromptDocument(repodoc.Document{Path: convPath, Commit: personaBaseCommit, ContentHash: convHash, Content: convText}, injectedFraming)
	trig := prompt.Trigger{
		Repo:              "kuhlman-labs/example",
		InjectedDocuments: []prompt.InjectedDocument{arch},
		ReviewConventions: []prompt.ReviewConvention{{Name: "go-errors", Document: conv}},
	}
	inv := reviewerInvocation{reviewer: approvingFake(), persona: &personaInvocation{selected: spec.SelectedReviewerPersona{
		Name: personaTestName, RemitPath: personaRemitPath, DeclarationSite: personaDeclSite, SeverityCap: "medium",
	}}}
	if detail := p.s.buildPersonaPrompt(t.Context(), p.rr.getRuns[p.runID], p.stageID, "plan_review", trig, trig.InjectedDocuments, "", inv); detail != "" {
		t.Fatalf("buildPersonaPrompt degraded: %s", detail)
	}
	got := inv.persona.quoteDocs
	if len(got) != 3 || got[0].Path != archPath || got[1].Path != personaRemitPath || got[2].Path != convPath {
		paths := make([]string, len(got))
		for i, d := range got {
			paths[i] = d.Path
		}
		t.Fatalf("quoteDocs paths = %v, want [%s %s %s]", paths, archPath, personaRemitPath, convPath)
	}
	if inv.persona.severityCap != "medium" {
		t.Errorf("severityCap = %q, want medium", inv.persona.severityCap)
	}
	for _, tc := range []struct {
		name, ref, quote, wantHash string
		wantVerified               bool
	}{
		{"convention exact", convPath, "with the operation that failed", convHash, true},
		{"convention altered", convPath, "with the operation that succeeded", "", false},
		{"injected exact", archPath, "appends to the audit chain first", archHash, true},
		{"injected altered", archPath, "appends to the audit chain last", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{
				{Severity: planreview.SeverityMedium, Category: "repo_convention", Note: "n", QuotedPassage: tc.quote, DocumentRef: tc.ref},
			}}
			p.s.applyPersonaConcernControls(t.Context(), "plan review", p.runID, p.stageID, inv, personaAgentModel, v)
			c := v.Concerns[0]
			if tc.wantVerified {
				if c.QuoteUnverified || c.QuoteVerifiedContentHash != tc.wantHash || c.Severity != planreview.SeverityMedium {
					t.Errorf("concern = %+v, want verified with %s at medium", c, tc.wantHash)
				}
				return
			}
			if !c.QuoteUnverified || c.Severity != planreview.SeverityLow || c.SeverityClampedFrom != planreview.SeverityMedium {
				t.Errorf("concern = %+v, want demoted to low from medium, quote_unverified", c)
			}
		})
	}
}

// (C6, #3753 item 1) IMPLEMENT-ROUND VETO IDENTITY is model + reviewer_role.
// Fixture: an implement concern raised by model M with reviewer_role standard,
// in addressed_pending; a round where the standard reviewer (model M) REJECTS
// and the persona — on the SAME model string, by construction one adapter —
// CONFIRMS it. The confirm is vetoed (raiser rejected same round): the concern
// stays open, and the concern_resolution_vetoed entry names both roles.
//
// Counterfactual: key reviewerIdentity on model only (newReviewerIdentity
// dropping the role) — raiser == confirmer, the confirm applies and the
// concern reaches addressed: RED.
func TestPersonaIngest_Implement_VetoIdentityIsModelAndRole(t *testing.T) {
	const sharedModel = "shared-model"
	shared := &keyedReviewer{model: sharedModel}
	s, au, runRow, implStage, _ := personaImplRun(t, personaSpec(personaSpecOpts{attachOn: "implement", personaProvider: "anthropic"}),
		shared, shared, "anthropic/"+personaAgentModel)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	seeded, err := cr.InsertRaised(t.Context(), concern.InsertRaisedParams{
		RunID: runRow.ID, StageID: implStage.ID, StageKind: concern.StageKindImplement,
		ReviewerModel: sharedModel, ReviewerRole: concern.ReviewerRoleStandard, OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: "correctness", Note: "seeded"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := seeded[0]
	if err := cr.MarkAddressedPending(t.Context(), []uuid.UUID{row.ID}, "routed"); err != nil {
		t.Fatal(err)
	}
	shared.std = &planreview.ReviewVerdict{Verdict: planreview.VerdictReject, Concerns: []planreview.Concern{
		{Severity: planreview.SeverityHigh, Category: "correctness", Note: "still broken"},
	}}
	shared.persona = &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove, ConcernResolutions: []planreview.ConcernResolution{
		{ID: row.ID.String(), Resolution: "confirmed", Note: "looks fixed to me"},
	}}
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, reviewInjectionDiff(), nil, "head-veto", nil)

	if len(shared.calls) != 2 {
		t.Fatalf("shared adapter calls = %d, want 2", len(shared.calls))
	}
	got, _ := cr.GetByIDs(t.Context(), []uuid.UUID{row.ID})
	if got[0].State != concern.StateAddressedPending {
		t.Errorf("seeded concern state = %s, want addressed_pending — a persona on the raiser's model is not the raiser", got[0].State)
	}
	vetoes := auditFakeEntries(au, concernResolutionVetoedCategory)
	if len(vetoes) != 1 {
		t.Fatalf("%s entries = %d, want 1", concernResolutionVetoedCategory, len(vetoes))
	}
	var vp concernResolutionVetoedPayload
	if err := json.Unmarshal(vetoes[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if vp.VetoReason != vetoRaiserRejectedSameRound || vp.RaisingReviewerRole != concern.ReviewerRoleStandard || vp.ConfirmingReviewerRole != personaTestName ||
		vp.RaisingReviewerModel != sharedModel || vp.ConfirmingReviewerModel != sharedModel {
		t.Errorf("veto payload = %+v, want %s raised by standard/%s, confirmed by %s/%s", vp, vetoRaiserRejectedSameRound, sharedModel, personaTestName, sharedModel)
	}
}

// (Approval condition 5) reviewerIdentity normalizes the role through
// concern.NormalizedReviewerRole: an unattributed legacy row (empty role) is the
// standard reviewer, a persona name is itself.
func TestReviewerIdentity_NormalizesRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b reviewerIdentity
		same bool
	}{
		{"legacy empty role reads as standard", newReviewerIdentity("m", ""), newReviewerIdentity("m", concern.ReviewerRoleStandard), true},
		{"whitespace role reads as standard", newReviewerIdentity("m", "  "), newReviewerIdentity("m", concern.ReviewerRoleStandard), true},
		{"persona differs from standard on one model", newReviewerIdentity("m", personaTestName), newReviewerIdentity("m", concern.ReviewerRoleStandard), false},
		{"same role, different model", newReviewerIdentity("m1", personaTestName), newReviewerIdentity("m2", personaTestName), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.a == tc.b) != tc.same {
				t.Errorf("%+v == %+v is %v, want %v", tc.a, tc.b, tc.a == tc.b, tc.same)
			}
		})
	}
	if got := (reviewerInvocation{}).reviewerRole(); got != concern.ReviewerRoleStandard {
		t.Errorf("standard reviewerRole = %q, want %q", got, concern.ReviewerRoleStandard)
	}
	if got := (reviewerInvocation{persona: &personaInvocation{selected: spec.SelectedReviewerPersona{Name: personaTestName}}}).reviewerRole(); got != personaTestName {
		t.Errorf("persona reviewerRole = %q, want %q", got, personaTestName)
	}
}

// (C1, implement) the altered-quote demotion through the real implement loop:
// payload and row agree on low + quote_unverified + reviewer_role.
//
// Counterfactual: delete the applyPersonaConcernControls call in
// clampImplementReviewVerdict — the persona concern stays high: RED.
func TestPersonaIngest_Implement_AlteredQuoteDemoted(t *testing.T) {
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel,
		quotedConcern("altered", personaRemitAlteredQuote, personaRemitPath))
	s, au, runRow, implStage, _ := personaImplRun(t, personaSpec(personaSpecOpts{attachOn: "implement"}), approvingFake(), persona, "codex/"+personaAgentModel)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, reviewInjectionDiff(), nil, "head-quote", nil)
	var found bool
	for _, v := range decodeImplementReviewed(t, au) {
		if v.Persona != personaTestName {
			continue
		}
		found = true
		c := payloadConcernByNote(t, v.Concerns, "altered")
		if c.Severity != planreview.SeverityLow || !c.QuoteUnverified || c.ReviewerRole != personaTestName {
			t.Errorf("payload concern = %+v, want low, quote_unverified, role %s", c, personaTestName)
		}
	}
	if !found {
		t.Fatal("no persona implement_reviewed entry")
	}
	r := rowsByNote(t, cr, runRow.ID)["altered"]
	if r == nil || r.Severity != "low" || !r.QuoteUnverified || r.SeverityClampedFrom != "high" || r.ReviewerRole != personaTestName {
		t.Errorf("row = %+v, want low, quote_unverified, clamped from high, role %s", r, personaTestName)
	}
}
