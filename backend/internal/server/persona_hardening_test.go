package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Reviewer-persona hardening (E55.10 / #3755, carrying review items from
// #3753, #2244 and #3754). Three families, each driven through the REAL
// review loops with fakes:
//
//	(a) a persona prompt KEEPS the stage's standard documents — the declared
//	    stage-injected document and the selected review convention — alongside
//	    its remit, standard set before remit, on the plan AND implement paths;
//	(b) held-ingest DURABILITY — a conventions-file-modifying implement round
//	    whose second reviewer fails (and a variant whose caller context is
//	    cancelled after the first reviewer returns) still lands the first
//	    reviewer's verdict, its concerns and the synthesized
//	    conventions_file_modified concern, and settles;
//	(c) the escalation_unevaluable pseudo invocation through the implement
//	    invocation loop, and the two MIXED degrades (static source degraded
//	    while the escalation-attached persona runs, and vice versa).

// hardeningSpecOpts shapes hardeningSpec.
type hardeningSpecOpts struct {
	// attachOn is the stage TYPE whose reviewers block attaches `security`
	// statically ("" = no static attachment).
	attachOn string
	// convention declares the `backend` review convention (rcConvPath, no cap,
	// no applies_to) and selects it on both stages.
	convention bool
	// escalatePaths, when non-empty, declares one escalation requiring the
	// `security` persona on those paths.
	escalatePaths []string
}

// hardeningSpec renders a workflow-v2 document: the `security` persona
// (codex/persona-model, remit personaRemitPath) is ALWAYS declared; a plan and
// an implement stage each run one GATING anthropic/std-model agent reviewer.
func hardeningSpec(o hardeningSpecOpts) []byte {
	var b strings.Builder
	b.WriteString("version: \"2\"\n")
	if o.convention {
		b.WriteString("review_conventions:\n  backend:\n    path: " + rcConvPath + "\n")
	}
	fmt.Fprintf(&b, "reviewer_personas:\n  security:\n    agent:\n      provider: codex\n      model: %s\n    remit:\n      path: %s\n", personaAgentModel, personaRemitPath)
	b.WriteString("workflows:\n  feature_change:\n")
	if len(o.escalatePaths) > 0 {
		fmt.Fprintf(&b, "    escalations:\n      - match:\n          paths: [%s]\n        require:\n          reviewers: [security]\n", quotedList(o.escalatePaths))
	}
	b.WriteString("    stages:\n")
	stage := func(id, typ, produces string) {
		fmt.Fprintf(&b, "      - id: %s\n        type: %s\n        executor:\n          agent: claude-code\n        produces:\n%s", id, typ, produces)
		fmt.Fprintf(&b, "        reviewers:\n          human: 0\n          agents:\n            - provider: anthropic\n              model: %s\n", personaStdModel)
		if o.attachOn == typ {
			b.WriteString("          personas: [security]\n")
		}
		if o.convention {
			b.WriteString("          conventions: [backend]\n")
		}
	}
	stage("plan", "plan", "          - artifact: plan\n            schema: standard_v1\n")
	stage("implement", "implement", "          - artifact: pull_request\n")
	return []byte(b.String())
}

// hardeningFetcher serves the review convention and the declared stage
// document (rcFetcher's two files) PLUS the persona remit at the run's
// admission commit admCommitA.
func hardeningFetcher() *rcFetcher {
	f := newRCFetcher()
	f.files[personaRemitPath+"@"+admCommitA] = personaRemitBody
	return f
}

// wireHardeningDocuments configures the declaration seam (seamDecl — injPath
// resolved at injPinnedCommit) and a resolver over hardeningFetcher, and
// records admCommitA as the run's admission commit.
func wireHardeningDocuments(s *Server, runRow *run.Run) {
	wireReviewInjection(s, seamDecl())
	s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: hardeningFetcher(), Commits: &injCommits{sha: injPinnedCommit}}
	runRow.DocumentBaseCommit = strPtr(admCommitA)
}

// hardeningPersonaSet resolves the standard reviewer at anthropic/std-model
// and the persona at codex/persona-model.
func hardeningPersonaSet(std, persona PlanReviewer) personaReviewerSet {
	return personaReviewerSet{def: std, byKey: map[string]PlanReviewer{
		"anthropic/" + personaStdModel: std,
		"codex/" + personaAgentModel:   persona,
	}}
}

// injectedContentHash returns the content_hash the document_injected entry for
// path recorded.
func injectedContentHash(t *testing.T, au *auditFake, path string) string {
	t.Helper()
	for _, e := range auditFakeEntries(au, "document_injected") {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode document_injected: %v", err)
		}
		if p["path"] == path {
			h, _ := p["content_hash"].(string)
			if h == "" {
				t.Fatalf("document_injected for %s carries no content_hash: %v", path, p)
			}
			return h
		}
	}
	t.Fatalf("no document_injected entry for %s", path)
	return ""
}

// standardDocQuotes are the two persona concerns quoting the STANDARD
// documents (never the remit): the declared stage-injected document and the
// selected review convention, each with document_ref = that document's path.
func standardDocQuotes() []planreview.Concern {
	return []planreview.Concern{
		quotedConcern("quotes the injected document", "reject an unattributed injection", injPath),
		quotedConcern("quotes the review convention", "every exported func carries a doc comment", rcConvPath),
	}
}

// assertPersonaPromptKeepsStandardDocuments asserts the persona prompt carries
// the declared injected document, the selected review convention AND the
// remit, with the standard injected set rendered BEFORE the remit; that the
// standard prompt carries both standard documents and never the remit; and
// that each persona concern quoting a standard document verified with THAT
// document's document_injected content_hash at its original severity.
func assertPersonaPromptKeepsStandardDocuments(t *testing.T, stdPrompt, personaPrompt string, au *auditFake, personaConcerns []planreview.Concern) {
	t.Helper()
	for _, want := range []string{injBaseContent, rcConvContent, personaRemitBody} {
		if !strings.Contains(personaPrompt, want) {
			t.Errorf("persona prompt lacks %q — a persona must keep the stage's standard documents and its remit", want)
		}
	}
	if ii, ri := strings.Index(personaPrompt, injBaseContent), strings.Index(personaPrompt, personaRemitBody); ii < 0 || ri < 0 || ii > ri {
		t.Errorf("persona prompt order: injected document @%d, remit @%d — want the standard set BEFORE the remit", ii, ri)
	}
	for _, want := range []string{injBaseContent, rcConvContent} {
		if !strings.Contains(stdPrompt, want) {
			t.Errorf("standard prompt lacks %q", want)
		}
	}
	if strings.Contains(stdPrompt, personaRemitBody) {
		t.Error("standard prompt carries the persona remit")
	}
	for _, tc := range []struct{ note, path string }{
		{"quotes the injected document", injPath},
		{"quotes the review convention", rcConvPath},
	} {
		c := payloadConcernByNote(t, personaConcerns, tc.note)
		if c.QuoteUnverified || c.Severity != planreview.SeverityHigh || c.QuoteVerifiedContentHash != injectedContentHash(t, au, tc.path) {
			t.Errorf("%s: concern = %+v, want verified at high with %s's content_hash", tc.note, c, tc.path)
		}
	}
}

// (a, plan path) The persona prompt keeps the stage's standard documents.
// Fixture: the plan stage declares a stage-injected document (the seam) and
// selects the `backend` review convention, and attaches `security`; the
// persona quotes a passage of each STANDARD document.
//
// Counterfactuals (run): (1) build the persona's InjectedDocuments from the
// remit alone (drop the slices.Clone(standardInjected) prefix in
// buildPersonaPrompt) — the persona prompt loses injBaseContent and the
// injected-document quote is document_unknown and demotes: RED; (2) clear
// ptrig.ReviewConventions in buildPersonaPrompt — the persona prompt loses
// rcConvContent and the convention quote demotes: RED.
func TestPersonaHardening_Plan_PersonaPromptKeepsStandardDocuments(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	std := approvingFake()
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel, standardDocQuotes()...)
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s, _, _, au, rr := newPlanServerWithReviewerSet(t, runID, stageID, hardeningPersonaSet(std, persona),
		hardeningSpec(hardeningSpecOpts{attachOn: "plan", convention: true}), logger)
	st := rr.getStages[stageID]
	st.Type = run.StageTypePlan
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {st}}
	wireHardeningDocuments(s, rr.getRuns[runID])

	s.runPlanReviews(t.Context(), runID, stageID, validPlanBytes(t), nil, nil, nil, nil, nil)
	s.waitBackgroundReviews()

	stdCalls, perCalls := reviewerCalls(std), reviewerCalls(persona)
	if len(stdCalls) != 1 || len(perCalls) != 1 {
		t.Fatalf("calls: standard %d persona %d, want 1 and 1", len(stdCalls), len(perCalls))
	}
	assertPersonaPromptKeepsStandardDocuments(t, stdCalls[0], perCalls[0], au, personaPlanVerdict(t, au).Concerns)
}

// (a, implement path) The same contract at the implement-review dispatch site,
// which resolves the documents inside reviewDispatchMu and builds the persona
// prompt from a value copy of the implement trigger.
//
// Counterfactuals: as the plan path — both mutations live in the one shared
// buildPersonaPrompt, so each reddens this case too (run).
func TestPersonaHardening_Implement_PersonaPromptKeepsStandardDocuments(t *testing.T) {
	std := approvingFake()
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel, standardDocQuotes()...)
	s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, hardeningPersonaSet(std, persona),
		hardeningSpec(hardeningSpecOpts{attachOn: "implement", convention: true}))
	wireHardeningDocuments(s, runRow)
	s.cfg.ConcernRepo = newFakeConcernRepo()

	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, reviewInjectionDiff(), nil, "head-keeps-docs", nil)

	stdCalls, perCalls := reviewerCalls(std), reviewerCalls(persona)
	if len(stdCalls) != 1 || len(perCalls) != 1 {
		t.Fatalf("calls: standard %d persona %d, want 1 and 1", len(stdCalls), len(perCalls))
	}
	var personaConcerns []planreview.Concern
	for _, v := range decodeImplementReviewed(t, au) {
		if v.Persona == personaTestName {
			personaConcerns = v.Concerns
		}
	}
	assertPersonaPromptKeepsStandardDocuments(t, stdCalls[0], perCalls[0], au, personaConcerns)
}

// cancelRefusingAudit models a durable audit store that REFUSES an append on
// a cancelled context (as a database driver does), so a held ingest running on
// a cancellable context loses its writes observably.
type cancelRefusingAudit struct{ *auditFake }

func (a cancelRefusingAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cancelRefusingAudit: append %s on a cancelled context: %w", p.Category, err)
	}
	return a.auditFake.AppendChained(ctx, p)
}

// cancelRefusingConcerns is cancelRefusingAudit's concern-store twin.
type cancelRefusingConcerns struct{ *fakeConcernRepo }

func (c cancelRefusingConcerns) InsertRaised(ctx context.Context, p concern.InsertRaisedParams) ([]*concern.Concern, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cancelRefusingConcerns: insert on a cancelled context: %w", err)
	}
	return c.fakeConcernRepo.InsertRaised(ctx, p)
}

// cancellingReviewer returns its verdict AFTER cancelling the caller's
// context — "the round context is cancelled after reviewer 1 returns".
type cancellingReviewer struct {
	*fakePlanReviewer
	cancel context.CancelFunc
}

func (r cancellingReviewer) Review(ctx context.Context, promptText string) (*planreview.ReviewVerdict, string, error) {
	v, m, err := r.fakePlanReviewer.Review(ctx, promptText)
	r.cancel()
	return v, m, err
}

// (b) HELD-INGEST DURABILITY. The diff modifies the declared conventions file,
// so the implement round is HELD: every invocation runs before any is
// ingested. Reviewer 1 (standard) returns approve_with_concerns with its own
// concern; reviewer 2 (the `security` persona) FAILS. The held verdict of
// reviewer 1 must not be lost to reviewer 2's failure: its implement_reviewed
// lands carrying the synthesized conventions_file_modified concern, both of
// its concerns persist (role standard), reviewer 2 records
// implement_review_failed (reason prefixed with the persona), and the round
// settles at configured_agents.
//
// The "cancelled" variant also cancels the CALLER's context the moment
// reviewer 1 returns, over audit and concern stores that refuse a write on a
// cancelled context. The round runs on context.WithoutCancel, so every write
// still lands.
//
// Counterfactuals (run): (1) in the held branch of
// runImplementReviewInvocationsWithConventions, ingest only results[0] when a
// later result failed (drop the failed tail) — reviewer 2's
// implement_review_failed is missing and the round does not settle: RED on
// both variants; (2) replace `reviewCtx := context.WithoutCancel(ctx)` with
// `reviewCtx := ctx` in runImplementReviewsForTree — the cancelled variant's
// held writes are refused, implement_reviewed and the concern rows are
// missing: RED (the plain variant stays green — the cancellation is what
// isolates that control).
func TestPersonaHardening_HeldIngestDurability(t *testing.T) {
	stdConcern := planreview.Concern{Severity: planreview.SeverityMedium, Category: "correctness", Note: "standard reviewer's own concern"}
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{"second reviewer fails", false},
		{"second reviewer fails and the caller context is cancelled after the first returns", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			std := verdictFake(planreview.VerdictApproveWithConcerns, personaStdModel, stdConcern)
			var stdReviewer PlanReviewer = std
			if tc.cancel {
				stdReviewer = cancellingReviewer{fakePlanReviewer: std, cancel: cancel}
			}
			persona := &fakePlanReviewer{err: errors.New("persona backend exploded")}
			s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, hardeningPersonaSet(stdReviewer, persona),
				hardeningSpec(hardeningSpecOpts{attachOn: "implement", convention: true}))
			wireHardeningDocuments(s, runRow)
			cr := newFakeConcernRepo()
			s.cfg.ConcernRepo = cr
			if tc.cancel {
				s.cfg.AuditRepo = cancelRefusingAudit{au}
				s.cfg.ConcernRepo = cancelRefusingConcerns{cr}
			}

			if s.runImplementReviews(ctx, runRow.ID, implStage.ID, ircModifiedDiff(), nil, "head-held", nil) {
				t.Error("runImplementReviews = true: an approve_with_concerns plus a failed reviewer must not gate")
			}
			if tc.cancel && ctx.Err() == nil {
				t.Fatal("precondition: the caller context was never cancelled")
			}
			if n := len(reviewerCalls(persona)); n != 1 {
				t.Fatalf("persona calls = %d, want 1 (the round must still invoke reviewer 2)", n)
			}

			vs := decodeImplementReviewed(t, au)
			if len(vs) != 1 || vs[0].Persona != "" {
				t.Fatalf("implement_reviewed = %+v, want exactly reviewer 1's (standard) verdict", vs)
			}
			if !vs[0].ConventionsFileModifiedSynthesized || countConcernCategory(vs[0].Concerns, planreview.ConventionsFileModifiedConcernCategory) != 1 {
				t.Errorf("reviewer 1's payload = %+v, want it to carry the one synthesized conventions_file_modified concern", vs[0])
			}
			synth := concernRowsByCategory(cr, planreview.ConventionsFileModifiedConcernCategory)
			if len(synth) != 1 || synth[0].Severity != string(planreview.SeverityMedium) || synth[0].ReviewerRole != concern.ReviewerRoleStandard {
				t.Errorf("conventions_file_modified rows = %+v, want one medium row attributed standard", synth)
			}
			own := concernRowsByCategory(cr, "correctness")
			if len(own) != 1 || own[0].Note != stdConcern.Note || own[0].ReviewerRole != concern.ReviewerRoleStandard {
				t.Errorf("reviewer 1's own concern rows = %+v, want it persisted, attributed standard", own)
			}

			failed := auditFakeEntries(au, "implement_review_failed")
			if len(failed) != 1 {
				t.Fatalf("implement_review_failed = %d, want 1 (reviewer 2)", len(failed))
			}
			var fp map[string]any
			_ = json.Unmarshal(failed[0].Payload, &fp)
			if r, _ := fp["reason"].(string); !strings.HasPrefix(r, "persona "+personaTestName+": ") {
				t.Errorf("implement_review_failed reason = %q, want the persona prefix", r)
			}
			started := decodeStarted(t, au, "implement_review_started")
			if started.ConfiguredAgents != 2 || !planreview.Settled(started.ConfiguredAgents, terminalCount(au, "implement")) {
				t.Errorf("round does not settle: configured_agents %d, terminal entries %d", started.ConfiguredAgents, terminalCount(au, "implement"))
			}
		})
	}
}

// (b, #3915) the held-round CARRIER ROLE. The diff modifies the declared
// conventions file, so the round is HELD; reviewer 1 (standard) FAILS and
// reviewer 2 (the `security` persona) succeeds with a VERIFIED remit quote, so
// conventionsFileModifiedCarrier selects the persona: the synthesized
// conventions_file_modified concern rides the persona's verdict and is
// attributed to the persona on the implement_reviewed payload AND the row.
//
// Counterfactuals (run): (C) stamp concern.ReviewerRoleStandard instead of the
// carrier's role in synthesizeConventionsFileModified — the payload concern
// reads standard: RED (stampReviewerRole overwrites every differing concern,
// so the persona's own payload concern flips too; its ROW stays persona);
// (D) pass concern.ReviewerRoleStandard to persistReviewConcernsAs in
// ingestImplementReview — the synthesized row reads standard: RED.
func TestPersonaHardening_HeldRoundPersonaCarriesSynthesizedConcern(t *testing.T) {
	const ownNote = "persona quoted remit"
	std := &fakePlanReviewer{err: errors.New("standard backend exploded")}
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel, quotedConcern(ownNote, personaRemitExactQuote, personaRemitPath))
	s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, hardeningPersonaSet(std, persona),
		hardeningSpec(hardeningSpecOpts{attachOn: "implement", convention: true}))
	wireHardeningDocuments(s, runRow)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr

	if s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, ircModifiedDiff(), nil, "head-held-carrier", nil) {
		t.Error("runImplementReviews = true: an approve_with_concerns plus a failed reviewer must not gate")
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Fatalf("standard calls = %d, want 1", n)
	}

	vs := decodeImplementReviewed(t, au)
	if len(vs) != 1 || vs[0].Persona != personaTestName || !vs[0].ConventionsFileModifiedSynthesized {
		t.Fatalf("implement_reviewed = %+v, want exactly the persona's verdict, carrying the synthesized concern", vs)
	}
	var synthPayload []planreview.Concern
	for _, c := range vs[0].Concerns {
		if c.Category == planreview.ConventionsFileModifiedConcernCategory {
			synthPayload = append(synthPayload, c)
		}
	}
	if len(synthPayload) != 1 || synthPayload[0].ReviewerRole != personaTestName {
		t.Errorf("payload conventions_file_modified concerns = %+v, want one attributed %s (its carrier)", synthPayload, personaTestName)
	}
	synth := concernRowsByCategory(cr, planreview.ConventionsFileModifiedConcernCategory)
	if len(synth) != 1 || synth[0].Severity != string(planreview.SeverityMedium) || synth[0].ReviewerRole != personaTestName {
		got := make([]string, len(synth))
		for i, r := range synth {
			got[i] = r.Severity + "/" + r.ReviewerRole
		}
		t.Errorf("conventions_file_modified rows (severity/role) = %v, want one medium row attributed %s", got, personaTestName)
	} else if len(synthPayload) == 1 && synth[0].ReviewerRole != synthPayload[0].ReviewerRole {
		t.Errorf("synthesized row role %q != payload role %q", synth[0].ReviewerRole, synthPayload[0].ReviewerRole)
	}
	if c := payloadConcernByNote(t, vs[0].Concerns, ownNote); c.Severity != planreview.SeverityHigh || c.QuoteUnverified || c.ReviewerRole != personaTestName {
		t.Errorf("persona's own quoted payload concern = %+v, want high, verified, attributed %s", c, personaTestName)
	}
	own := rowsByNote(t, cr, runRow.ID)[ownNote]
	if own == nil || own.Severity != string(planreview.SeverityHigh) || own.QuoteUnverified || own.ReviewerRole != personaTestName {
		t.Errorf("persona's own quoted concern row = %+v, want high, verified, attributed %s", own, personaTestName)
	}

	failed := auditFakeEntries(au, "implement_review_failed")
	if len(failed) != 1 {
		t.Fatalf("implement_review_failed = %d, want 1 (the standard reviewer)", len(failed))
	}
	var fp map[string]any
	_ = json.Unmarshal(failed[0].Payload, &fp)
	if r, _ := fp["reason"].(string); r == "" || strings.HasPrefix(r, "persona ") {
		t.Errorf("implement_review_failed reason = %q, want the standard reviewer's failure with no persona prefix", r)
	}
	started := decodeStarted(t, au, "implement_review_started")
	if started.ConfiguredAgents != 2 || !planreview.Settled(started.ConfiguredAgents, terminalCount(au, "implement")) {
		t.Errorf("round does not settle: configured_agents %d, terminal entries %d", started.ConfiguredAgents, terminalCount(au, "implement"))
	}
}

// loopRun drives the IMPLEMENT invocation loop (runImplementReviewInvocations-
// WithConventions) over the standard reviewer plus the persona invocations
// resolveParsedReviewPersonaInvocations derives from parsed, with each
// persona's own prompt built by buildPersonaPrompts. It is the seam for the
// escalation_unevaluable cases: both producers of that detail (a Match error, a
// SelectNamedReviewerPersonas refusal) are rejected by ParseBytes, so the
// dispatch site can never see one from a stored spec — the degraded spec is
// therefore built by construction and handed to the same resolution the
// dispatch site calls, then to the same loop.
type loopRun struct {
	s         *Server
	au        *auditFake
	runRow    *run.Run
	implStage *run.Stage
	std       *fakePlanReviewer
	persona   *fakePlanReviewer
	logs      *bytes.Buffer
}

func newLoopRun(t *testing.T, attachOn string) *loopRun {
	t.Helper()
	std := approvingFake()
	persona := verdictFake(planreview.VerdictApprove, personaAgentModel)
	s, _, au, _, runRow, implStage := newImplementReviewServerWithSet(t, hardeningPersonaSet(std, persona),
		hardeningSpec(hardeningSpecOpts{attachOn: attachOn, escalatePaths: []string{escalatedGlob}}))
	runRow.DocumentBaseCommit = strPtr(admCommitA)
	s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: hardeningFetcher()}
	logs := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &loopRun{s: s, au: au, runRow: runRow, implStage: implStage, std: std, persona: persona, logs: logs}
}

// parsed returns runRow's spec parsed, with the escalation's glob replaced by
// a malformed one when malformed is set (a Match error — escalation_unevaluable).
func (l *loopRun) parsed(t *testing.T, malformed bool) *spec.Spec {
	t.Helper()
	parsed, err := spec.ParseBytes(l.runRow.WorkflowSpec)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	if malformed {
		parsed.Workflows["feature_change"].Escalations[0].Match.Paths[0] = "backend/["
	}
	return parsed
}

// review resolves the persona invocations over parsed and runs the gating
// implement loop; it returns the loop's hasRejection and the invocation count.
func (l *loopRun) review(t *testing.T, parsed *spec.Spec) (bool, int) {
	t.Helper()
	ctx := t.Context()
	paths := reviewPaths{diff: []string{escalatedPath}, source: escalationPathSourceApprovedAndDiff, headSHA: "head-loop"}
	personaInvs := l.s.resolveParsedReviewPersonaInvocations(ctx, l.runRow, parsed, l.implStage.ID, "implement_review", paths)
	trig := prompt.Trigger{Repo: l.runRow.Repo}
	l.s.buildPersonaPrompts(ctx, l.runRow, l.implStage.ID, "implement_review", trig, nil, "", personaInvs)
	invs := append([]reviewerInvocation{{reviewer: l.std, provider: "anthropic", specModel: personaStdModel}}, personaInvs...)
	got := l.s.runImplementReviewInvocationsWithConventions(ctx, l.runRow.ID, l.implStage.ID, invs, planreview.AuthorityGating,
		"standard implement review prompt", "author-model", "", "head-loop", planreview.ReviewBudget{Floor: time.Minute}, "", 0, reviewConventionRound{})
	return got, len(invs)
}

// skipsWith returns the implement_review_skipped payloads carrying detail.
func skipsWith(t *testing.T, au *auditFake, detail string) []planreview.ReviewSkippedPayload {
	t.Helper()
	var out []planreview.ReviewSkippedPayload
	for _, sk := range decodeSkipped(t, au, "implement_review_skipped") {
		if sk.Detail == detail {
			out = append(out, sk)
		}
	}
	return out
}

// assertSettledWith asserts the loop wrote exactly configured terminal entries
// so planreview.Settled holds at configured.
func assertSettledWith(t *testing.T, au *auditFake, configured int) {
	t.Helper()
	if n := terminalCount(au, "implement"); n != configured || !planreview.Settled(configured, n) {
		t.Errorf("terminal entries = %d, want %d (Settled at configured_agents)", n, configured)
	}
}

// (c1) escalation_unevaluable THROUGH THE IMPLEMENT LOOP. The escalation's
// glob is malformed, so the escalation source cannot be evaluated: the round
// carries ONE pseudo invocation that is counted (configured_agents 2 on its
// skip), emits ONE terminal implement_review_skipped with reason
// persona_attachment_unresolvable and detail escalation_unevaluable, never
// sets the LOOP's verdict accumulator, and the round settles; the standard
// reviewer still runs. The skip is escalation_attached: under gating authority
// the DISPATCH SITE fails the stage on it (#3913, escalationPersonaGateBlock —
// TestEscalationPersonaGateBlock_Sources), not the loop.
//
// Counterfactual (run): drop the `for _, detail := range unresolvable` loop in
// resolvePersonaInvocations — no pseudo invocation, no skip, the standard
// verdict is the only terminal entry and the round "settles" at 1 having
// silently run no escalation: RED on the skip and the configured count.
func TestPersonaHardening_EscalationUnevaluable_ThroughImplementLoop(t *testing.T) {
	l := newLoopRun(t, "")
	rejected, configured := l.review(t, l.parsed(t, true))
	if rejected {
		t.Error("hasRejection = true: the loop's verdict accumulator must stay verdict-only — the escalation_unevaluable block is decided at the dispatch site (#3913)")
	}
	if configured != 2 {
		t.Fatalf("invocations = %d, want 2 (standard + the escalation_unevaluable pseudo invocation)", configured)
	}
	sk := skipsWith(t, l.au, personaDetailEscalationUnevaluable)
	if len(sk) != 1 || sk[0].Reason != planreview.ReasonPersonaAttachmentUnresolvable || sk[0].ConfiguredAgents != 2 || sk[0].Persona != "" {
		t.Fatalf("escalation_unevaluable skips = %+v, want one persona_attachment_unresolvable skip counted at configured_agents 2", sk)
	}
	if !sk[0].EscalationAttached {
		t.Error("escalation_unevaluable skip escalation_attached = false, want true (#3913)")
	}
	if n := len(reviewerCalls(l.persona)); n != 0 {
		t.Errorf("persona calls = %d, want 0 — nothing fired that could be evaluated", n)
	}
	if n := len(reviewerCalls(l.std)); n != 1 {
		t.Errorf("standard calls = %d, want 1", n)
	}
	if n := countAuditCategory(l.au, CategoryEscalationPersonaAttached); n != 0 {
		t.Errorf("%s entries = %d, want 0 — nothing attached", CategoryEscalationPersonaAttached, n)
	}
	assertSettledWith(t, l.au, configured)
}

// (c2) MIXED: the static source DEGRADES while the escalation-attached persona
// still runs. The implement stage attaches `security` statically, but the
// reviewed stage is missing from the run's stage list, so the static set is
// unknown (persona_stage_unresolvable); the escalation fires on the diff and
// attaches `security`, which runs. Four invocations would double-run; three
// is correct: standard + security + the stage pseudo invocation.
//
// Counterfactual (run): append nothing to degraded on a static failure in
// resolveParsedReviewPersonaInvocations — no persona_stage_unresolvable skip,
// the round reads as if the static set were known: RED.
func TestPersonaHardening_Mixed_StaticDegradedEscalationRuns(t *testing.T) {
	l := newLoopRun(t, "implement")
	l.s.cfg.RunRepo.(*orchestratorRepo).stagesByRunID[l.runRow.ID] = nil // the static source degrades
	rejected, configured := l.review(t, l.parsed(t, false))
	if rejected {
		t.Error("hasRejection = true, want false")
	}
	if configured != 3 {
		t.Fatalf("invocations = %d, want 3 (standard + escalated security + the stage pseudo invocation)", configured)
	}
	if n := len(reviewerCalls(l.persona)); n != 1 {
		t.Errorf("persona calls = %d, want 1 — the escalation still attaches it", n)
	}
	var personaVerdicts int
	for _, v := range decodeImplementReviewed(t, l.au) {
		if v.Persona == personaTestName {
			personaVerdicts++
		}
	}
	if personaVerdicts != 1 {
		t.Errorf("persona implement_reviewed = %d, want 1", personaVerdicts)
	}
	if sk := skipsWith(t, l.au, personaDetailStageUnresolvable); len(sk) != 1 || sk[0].ConfiguredAgents != 3 {
		t.Errorf("persona_stage_unresolvable skips = %+v, want one counted at configured_agents 3", sk)
	} else if sk[0].EscalationAttached {
		t.Error("persona_stage_unresolvable skip escalation_attached = true, want false — it is static-source by construction (#3913)")
	}
	if sk := skipsWith(t, l.au, personaDetailEscalationUnevaluable); len(sk) != 0 {
		t.Errorf("escalation_unevaluable skips = %+v, want none — the escalation source resolved", sk)
	}
	assertSettledWith(t, l.au, configured)
}

// (c3) MIXED, the other way: the ESCALATION source is unevaluable while the
// statically attached persona still runs. Standard + static security + the
// escalation pseudo invocation; the static persona's verdict lands and the
// round settles at three. The loop's verdict accumulator stays false; the
// escalation_attached skip blocks a gating round at the dispatch site (#3913).
//
// Counterfactual (run): return early from resolveParsedReviewPersonaInvocations
// when the escalation source degrades (dropping the static personas) — the
// static persona never runs: RED.
func TestPersonaHardening_Mixed_EscalationUnevaluableStaticRuns(t *testing.T) {
	l := newLoopRun(t, "implement")
	rejected, configured := l.review(t, l.parsed(t, true))
	if rejected {
		t.Error("hasRejection = true, want false — the loop stays verdict-only; the block is decided at the dispatch site (#3913)")
	}
	if configured != 3 {
		t.Fatalf("invocations = %d, want 3 (standard + static security + the escalation pseudo invocation)", configured)
	}
	if n := len(reviewerCalls(l.persona)); n != 1 {
		t.Errorf("persona calls = %d, want 1 — the static attachment still runs", n)
	}
	if sk := skipsWith(t, l.au, personaDetailEscalationUnevaluable); len(sk) != 1 || sk[0].ConfiguredAgents != 3 {
		t.Errorf("escalation_unevaluable skips = %+v, want one counted at configured_agents 3", sk)
	} else if !sk[0].EscalationAttached {
		t.Error("escalation_unevaluable skip escalation_attached = false, want true (#3913)")
	}
	if sk := skipsWith(t, l.au, personaDetailStageUnresolvable); len(sk) != 0 {
		t.Errorf("persona_stage_unresolvable skips = %+v, want none — the static source resolved", sk)
	}
	assertSettledWith(t, l.au, configured)
}
