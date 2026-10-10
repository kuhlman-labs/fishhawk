package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// An ESCALATION-attached reviewer persona that cannot run blocks a GATING
// review round (#3913, operator option (b)). A persona a fired escalation's
// require.reviewers selected — whether or not the stage also attaches it
// statically — that never invoked (persona_remit_unavailable, reviewer_unavailable,
// or the escalation_unevaluable pseudo invocation) fails the reviewed stage
// category-B with a named escalation_persona_unavailable reason under the
// existing *_review_rejected prefix. Static-only personas, the
// persona_stage_unresolvable pseudo invocation and advisory rounds keep the
// degrade. Shared fixtures: personaSpec / newPersonaPlanRun
// (reviewer_persona_test.go), escalatedSpec / planBodyWithScope
// (escalation_persona_test.go), newLoopRun (persona_hardening_test.go).

// escalationGateBlockSuffix is the fixed tail escalationPersonaGateBlock
// appends after the blocked entries.
const escalationGateBlockSuffix = " could not run under gating authority; a fired escalation requires them for this change, so the round cannot settle on the standard reviewers alone"

// failedTransitions returns the TransitionStage calls p's repo saw into failed.
func failedTransitions(p *personaPlanRun) []promptTransitionStageCall {
	var out []promptTransitionStageCall
	for _, c := range p.rr.transitionStageCalls {
		if c.To == run.StageStateFailed {
			out = append(out, c)
		}
	}
	return out
}

// rawSkipPayloads returns the raw JSON payloads of every entry of category.
func rawSkipPayloads(au *auditFake, category string) [][]byte {
	var out [][]byte
	for _, e := range auditFakeEntries(au, category) {
		out = append(out, e.Payload)
	}
	return out
}

// withOptionalPersona marks the `security` persona's agent optional:true —
// which must NOT exempt an escalation-attached persona (an escalation may only
// raise).
func withOptionalPersona(t *testing.T, spec []byte) []byte {
	t.Helper()
	old := "      model: " + personaAgentModel + "\n"
	if !bytes.Contains(spec, []byte(old)) {
		t.Fatalf("spec carries no persona agent model line %q", old)
	}
	return bytes.Replace(spec, []byte(old), []byte(old+"      optional: true\n"), 1)
}

// (T1) Gating plan review: an escalation-attached persona that cannot run
// fails the plan stage category-B with a named reason, while the approving
// standard reviewer still runs and the round settles at configured_agents.
// One subtest per mode: the remit is missing (escalation-only attachment), the
// persona's provider is not runnable (reviewer_unavailable, also with
// optional:true — the Captain ruling), and the persona attached BOTH
// statically and by the fired escalation (membership tagging).
//
// Counterfactuals (run): escalationPersonaGateBlock returning "" — every
// subtest returns false with no failed transition: RED (C1). Tagging only the
// escalation-only personas appended after the static set — (c) is untagged and
// does not block: RED (C3). Dropping escalationPersonaGateBlockEntry's
// resolveErr branch — (b) renders persona_remit_unavailable, not
// reviewer_unavailable: RED (C6). Deleting the EscalationAttached stamp in
// emitPersonaDegraded (a, c) / emitReviewerUnavailable (b): RED (C7).
func TestPlanReview_EscalationPersonaUnavailable_GatingFailsStageWithNamedReason(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spec        func(t *testing.T) []byte
		personaLive bool // the persona's provider resolves
		wantReason  string
		wantDetail  string
		wantEntry   string
	}{
		{"escalation-only remit missing", func(*testing.T) []byte { return escalatedSpec("") }, true,
			planreview.ReasonPersonaRemitUnavailable, personaDetailRemitMissing,
			`persona "security" (persona_remit_unavailable: remit_missing)`},
		{"escalation-only reviewer unavailable", func(*testing.T) []byte { return escalatedSpec("") }, false,
			planreview.ReasonReviewerUnavailable, "",
			`persona "security" (reviewer_unavailable)`},
		{"escalation-only reviewer unavailable optional", func(t *testing.T) []byte { return withOptionalPersona(t, escalatedSpec("")) }, false,
			planreview.ReasonReviewerUnavailable, "",
			`persona "security" (reviewer_unavailable)`},
		{"static and escalation remit missing", func(*testing.T) []byte { return escalatedSpec("plan") }, true,
			planreview.ReasonPersonaRemitUnavailable, personaDetailRemitMissing,
			`persona "security" (persona_remit_unavailable: remit_missing)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			std := approvingFake()
			var persona PlanReviewer
			if tc.personaLive {
				persona = approvingFake()
			}
			p := newPersonaPlanRun(t, tc.spec(t), std, persona)
			p.planBody = planBodyWithScope(t, escalatedPath)
			delete(p.fetcher.files, personaRemitPath)

			if !p.review(t) {
				t.Fatal("runPlanReviews = false, want true — an escalation-attached persona that cannot run must block a gating round")
			}
			failed := failedTransitions(p)
			if len(failed) != 1 {
				t.Fatalf("failed transitions = %d, want exactly 1", len(failed))
			}
			c := failed[0].Completion
			if c == nil || c.FailureCategory == nil || *c.FailureCategory != run.FailureB || c.FailureReason == nil {
				t.Fatalf("completion = %+v, want category B with a reason", c)
			}
			want := "plan_review_rejected: escalation_persona_unavailable: " + tc.wantEntry + escalationGateBlockSuffix
			if *c.FailureReason != want {
				t.Errorf("failure reason = %q\nwant %q", *c.FailureReason, want)
			}
			if n := len(reviewerCalls(std)); n != 1 {
				t.Errorf("standard reviewer calls = %d, want 1 — the standard reviewers still run", n)
			}
			skips := decodeSkipped(t, p.au, "plan_review_skipped")
			if len(skips) != 1 {
				t.Fatalf("plan_review_skipped = %+v, want 1", skips)
			}
			sk := skips[0]
			if sk.Reason != tc.wantReason || sk.Persona != personaTestName || sk.Detail != tc.wantDetail || !sk.EscalationAttached || sk.Authority != planreview.AuthorityGating {
				t.Errorf("skip = %+v, want {reason %s persona %s detail %q escalation_attached true authority gating}", sk, tc.wantReason, personaTestName, tc.wantDetail)
			}
			if tc.wantReason == planreview.ReasonPersonaRemitUnavailable && !strings.Contains(p.logs.String(), "escalation-attached persona cannot run under gating authority; the stage is failed") {
				t.Errorf("WARN log does not say the stage is failed:\n%s", p.logs.String())
			}
			started := decodeStarted(t, p.au, "plan_review_started")
			if started.ConfiguredAgents != 2 || terminalCount(p.au, "plan") != started.ConfiguredAgents {
				t.Errorf("round settles at %d/%d terminal entries, want 2/2", terminalCount(p.au, "plan"), started.ConfiguredAgents)
			}
		})
	}
}

// (T2) Static-only persona with an unavailable remit under GATING authority
// keeps the advisory degrade: no block, no failed transition, and the skip
// carries NO escalation_attached key at all (decided on the raw JSON).
//
// Counterfactual (run): escalationPersonaGateBlock testing cannotRun() alone
// (ignoring escalationAttached) — the static persona fails the stage: RED (C4).
func TestPlanReview_StaticPersonaUnavailable_GatingStillDegrades(t *testing.T) {
	std := approvingFake()
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), std, approvingFake())
	p.planBody = planBodyWithScope(t, escalatedPath)
	delete(p.fetcher.files, personaRemitPath)
	if p.review(t) {
		t.Fatal("runPlanReviews = true, want false — a static-only persona degrade never blocks")
	}
	if n := len(failedTransitions(p)); n != 0 {
		t.Errorf("failed transitions = %d, want 0", n)
	}
	raws := rawSkipPayloads(p.au, "plan_review_skipped")
	if len(raws) != 1 {
		t.Fatalf("plan_review_skipped = %d, want 1", len(raws))
	}
	if bytes.Contains(raws[0], []byte(`"escalation_attached"`)) {
		t.Errorf("static-only skip = %s, want no escalation_attached key", raws[0])
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Errorf("standard reviewer calls = %d, want 1", n)
	}
}

// (T3) ADVISORY authority is unaffected: the escalation-attached persona's
// skip is still tagged, but the stage is never failed.
//
// Counterfactual (run): hoisting the block evaluation and transition above
// runPlanReviews' advisory return — human:1 fails the stage: RED (C9).
func TestPlanReview_EscalationPersonaUnavailable_AdvisoryDoesNotBlock(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{human: 1, escalatePaths: []string{escalatedGlob}}), approvingFake(), approvingFake())
	p.planBody = planBodyWithScope(t, escalatedPath)
	delete(p.fetcher.files, personaRemitPath)
	if p.review(t) {
		t.Fatal("runPlanReviews = true under advisory authority, want false")
	}
	if n := len(failedTransitions(p)); n != 0 {
		t.Errorf("failed transitions = %d, want 0 — advisory never blocks", n)
	}
	skips := decodeSkipped(t, p.au, "plan_review_skipped")
	if len(skips) != 1 || !skips[0].EscalationAttached || skips[0].Authority != planreview.AuthorityAdvisory {
		t.Errorf("skips = %+v, want one escalation_attached advisory skip", skips)
	}
	if !strings.Contains(p.logs.String(), "persona skipped, standard reviewers unaffected") || strings.Contains(p.logs.String(), "the stage is failed") {
		t.Errorf("advisory WARN log must keep the degrade text:\n%s", p.logs.String())
	}
}

// (T4) CROSS-LAYER, implement path: a raw trace upload whose diff touches the
// escalated path, a gating spec, an approving standard reviewer and NO
// document resolver (the remit is unreadable: document_resolver_unconfigured)
// crosses bundle diff → escalation matching → persona resolution → remit
// degrade → the implement loop → the audit payload → the persisted stage
// failure. The reason keeps implementReviewGatingRejectPrefix, the key
// handleShipPullRequest's dangling-PR close matches (#877).
//
// Counterfactuals (run): escalationPersonaGateBlock returning "" (C1) or
// runImplementReviewsForTree's gating tail dropping the block (C2b) — the
// stage advances: RED. The trace caller failing with the constant
// implementReviewGatingRejectReason (C2a) — RED on reason equality. The block
// branch prefixed "implement_review_blocked" (C8) — RED on HasPrefix.
func TestShipTrace_ImplementReview_EscalationPersonaUnavailable_GatingFailsStageWithNamedReason(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	s, sf, au, _, runRow, implStage := newImplementReviewServerWithSet(t, personaReviewerSet{def: std, byKey: map[string]PlanReviewer{
		"anthropic/" + personaStdModel: std,
		"codex/" + personaAgentModel:   persona,
	}}, escalatedSpec(""))
	runRow.DocumentBaseCommit = strPtr(personaBaseCommit)
	s.cfg.DocumentResolver = nil
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
		{"path": escalatedPath, "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if implStage.State != run.StageStateFailed {
		t.Fatalf("implement stage state = %q, want failed", implStage.State)
	}
	if implStage.FailureCategory == nil || *implStage.FailureCategory != run.FailureB {
		t.Errorf("failure category = %v, want B", implStage.FailureCategory)
	}
	want := implementReviewGatingRejectPrefix + ": escalation_persona_unavailable: " +
		`persona "security" (persona_remit_unavailable: document_resolver_unconfigured)` + escalationGateBlockSuffix
	if implStage.FailureReason == nil {
		t.Fatal("failure reason = nil, want the named escalation_persona_unavailable reason")
	}
	if *implStage.FailureReason != want {
		t.Fatalf("failure reason = %q\nwant %q", *implStage.FailureReason, want)
	}
	if !strings.HasPrefix(*implStage.FailureReason, implementReviewGatingRejectPrefix) {
		t.Errorf("failure reason %q lacks the #877 PR-close prefix %q", *implStage.FailureReason, implementReviewGatingRejectPrefix)
	}
	skips := decodeSkipped(t, au, "implement_review_skipped")
	if len(skips) != 1 || skips[0].Persona != personaTestName || !skips[0].EscalationAttached || skips[0].Detail != personaDetailResolverUnconfigured {
		t.Errorf("implement_review_skipped = %+v, want one escalation_attached security skip (document_resolver_unconfigured)", skips)
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Errorf("standard reviewer calls = %d, want 1", n)
	}
	if n := len(reviewerCalls(persona)); n != 0 {
		t.Errorf("persona calls = %d, want 0 — it cannot run without its remit", n)
	}
}

// (T5) The implement-path sibling: a STATIC-only persona with an unresolvable
// remit under gating authority degrades — the stage is NOT failed.
//
// Counterfactual (run): escalationPersonaGateBlock ignoring
// escalationAttached() — the stage fails: RED (C4).
func TestShipTrace_ImplementReview_StaticPersonaUnavailable_GatingAdvances(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	s, sf, au, _, runRow, implStage := newImplementReviewServerWithSet(t, personaReviewerSet{def: std, byKey: map[string]PlanReviewer{
		"anthropic/" + personaStdModel: std,
		"codex/" + personaAgentModel:   persona,
	}}, personaSpec(personaSpecOpts{attachOn: "implement"}))
	runRow.DocumentBaseCommit = strPtr(personaBaseCommit)
	s.cfg.DocumentResolver = nil
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundle(t, []map[string]string{
		{"path": "backend/internal/foo/foo.go", "status": "M"},
		{"path": escalatedPath, "status": "M"},
	})
	w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if implStage.State == run.StageStateFailed {
		reason := "<nil>"
		if implStage.FailureReason != nil {
			reason = *implStage.FailureReason
		}
		t.Fatalf("implement stage failed (%s) — a static-only persona degrade must not block", reason)
	}
	raws := rawSkipPayloads(au, "implement_review_skipped")
	if len(raws) != 1 || bytes.Contains(raws[0], []byte(`"escalation_attached"`)) {
		t.Errorf("implement_review_skipped = %q, want one skip without escalation_attached", raws)
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Errorf("standard reviewer calls = %d, want 1", n)
	}
}

// resolveAndBuild is the dispatch site's persona resolution + prompt build
// over l and parsed (malformed = an unevaluable escalation glob).
func (l *loopRun) resolveAndBuild(t *testing.T, malformed bool) []reviewerInvocation {
	t.Helper()
	ctx := t.Context()
	paths := reviewPaths{diff: []string{escalatedPath}, source: escalationPathSourceApprovedAndDiff, headSHA: "head-gate"}
	invs := l.s.resolveParsedReviewPersonaInvocations(ctx, l.runRow, l.parsed(t, malformed), l.implStage.ID, "implement_review", paths)
	l.s.buildPersonaPrompts(ctx, l.runRow, l.implStage.ID, "implement_review", prompt.Trigger{Repo: l.runRow.Repo}, nil, "", invs)
	return invs
}

// (T6) The block's SOURCES, over the hardening fixtures (the
// escalation_unevaluable producers are refused by ParseBytes, so the degraded
// spec is built by construction, as in persona_hardening_test.go):
//
//	(i)   an unevaluable escalation glob — the escalation_unevaluable pseudo
//	      invocation is escalation-attached and blocks;
//	(ii)  the static source degraded (persona_stage_unresolvable) while the
//	      escalated persona builds a prompt — static-source pseudo by
//	      construction, never blocks; the escalated persona is tagged by
//	      membership but can run;
//	(iii) no escalation declared, a static persona with no remit — "".
//
// Counterfactual (run): remove the pseudo-invocation tagging line in
// resolveParsedReviewPersonaInvocations — (i) yields "": RED (C5).
func TestEscalationPersonaGateBlock_Sources(t *testing.T) {
	t.Run("escalation unevaluable blocks", func(t *testing.T) {
		l := newLoopRun(t, "")
		invs := l.resolveAndBuild(t, true)
		block := escalationPersonaGateBlock(invs)
		want := escalationPersonaGateBlockSlug + ": escalation source (persona_attachment_unresolvable: escalation_unevaluable)" + escalationGateBlockSuffix
		if block != want {
			t.Errorf("block = %q\nwant %q", block, want)
		}
		if len(invs) != 1 || !invs[0].escalationAttached() || !invs[0].cannotRun() {
			t.Errorf("invocations = %+v, want the one escalation-attached pseudo invocation", invs)
		}
	})
	t.Run("static source degraded does not block", func(t *testing.T) {
		l := newLoopRun(t, "implement")
		l.s.cfg.RunRepo.(*orchestratorRepo).stagesByRunID[l.runRow.ID] = nil
		invs := l.resolveAndBuild(t, false)
		block := escalationPersonaGateBlock(invs)
		if block != "" {
			t.Errorf("block = %q, want \"\" — persona_stage_unresolvable is static-source", block)
		}
		var stagePseudo, escalated bool
		for _, inv := range invs {
			switch {
			case inv.personaName() == personaTestName:
				escalated = inv.escalationAttached() && !inv.cannotRun()
			case inv.persona != nil && inv.persona.degraded == personaDetailStageUnresolvable:
				stagePseudo = !inv.escalationAttached() && inv.cannotRun()
			}
		}
		if !stagePseudo || !escalated {
			t.Errorf("stage pseudo untagged+cannotRun = %v, escalated security tagged+runnable = %v; want both true", stagePseudo, escalated)
		}
	})
	t.Run("static persona without remit does not block", func(t *testing.T) {
		std := approvingFake()
		s, _, _, _, runRow, implStage := newImplementReviewServerWithSet(t, hardeningPersonaSet(std, approvingFake()),
			hardeningSpec(hardeningSpecOpts{attachOn: "implement"}))
		runRow.DocumentBaseCommit = strPtr(admCommitA)
		s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: newRCFetcher()} // serves no remit
		l := &loopRun{s: s, runRow: runRow, implStage: implStage}
		invs := l.resolveAndBuild(t, false)
		block := escalationPersonaGateBlock(invs)
		if block != "" {
			t.Errorf("block = %q, want \"\" — a static-only persona never blocks", block)
		}
		if len(invs) != 1 || invs[0].escalationAttached() || !invs[0].cannotRun() {
			t.Errorf("invocations = %+v, want one untagged, degraded static persona", invs)
		}
	})
}

// (T7) Reason precedence and the unchanged reject literals, byte-exact: the
// block wins over a reject (a fix-up or replan cannot clear it), a reject alone
// keeps the pre-#3913 literal, and neither yields "".
//
// Counterfactuals (run): swapping the block and reject branches — (t,b) RED
// (C10); the implement block branch prefixed "implement_review_blocked" — RED
// (C8).
func TestGatingFailureReasonPrecedence(t *testing.T) {
	const b = "escalation_persona_unavailable: X"
	for _, tc := range []struct {
		rejected        bool
		block           string
		wantPlan, wantI string
	}{
		{false, "", "", ""},
		{true, "", "plan_review_rejected: agent review verdict reject under gating authority",
			"implement_review_rejected: agent review verdict reject under gating authority"},
		{false, b, "plan_review_rejected: " + b, "implement_review_rejected: " + b},
		{true, b, "plan_review_rejected: " + b, "implement_review_rejected: " + b},
	} {
		if got := planReviewGatingFailureReason(tc.rejected, tc.block); got != tc.wantPlan {
			t.Errorf("planReviewGatingFailureReason(%v, %q) = %q, want %q", tc.rejected, tc.block, got, tc.wantPlan)
		}
		if got := implementReviewGatingFailureReason(tc.rejected, tc.block); got != tc.wantI {
			t.Errorf("implementReviewGatingFailureReason(%v, %q) = %q, want %q", tc.rejected, tc.block, got, tc.wantI)
		}
	}
	if implementReviewGatingRejectReason != "implement_review_rejected: agent review verdict reject under gating authority" {
		t.Errorf("implementReviewGatingRejectReason drifted: %q", implementReviewGatingRejectReason)
	}
}
