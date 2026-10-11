package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptance_fixup_deferral_test.go — E72.58 / #4075: a class-1 acceptance
// fix-up waits for an in-flight implement review round and then routes ONE
// pass carrying both the acceptance obligation and the round's concerns.

// deferralDelegatedSpec delegates route_fixup with an ADVISORY implement
// review (agent + human), so an agent reject is arbitrable and
// convergent_concerns can be met once the round settles.
var deferralDelegatedSpec = strings.Replace(autoDriveSpecYAML,
	"        reviewers:\n          agent: 2\n        produces:\n          - artifact: pull_request",
	"        reviewers:\n          agent: 2\n          human: 1\n        produces:\n          - artifact: pull_request", 1)

// deferralUndelegatedSpec is the same workflow without may_route_fixup.
var deferralUndelegatedSpec = strings.Replace(deferralDelegatedSpec, "      may_route_fixup: convergent_concerns\n", "", 1)

// deferralAudit wraps seqAuditRepo with error injection and an observer on
// the audit-complete recompute's first read.
type deferralAudit struct {
	*seqAuditRepo
	listErrCategory string
	// onAppend runs after each append (outside the lock).
	onAppend func(p audit.ChainAppendParams)
	mu       sync.Mutex
	// recomputeFixups records, at each audit-complete recompute, how many
	// stage_fixup_triggered entries existed.
	recomputeFixups []int
}

func (d *deferralAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := d.seqAuditRepo.AppendChained(ctx, p)
	if err == nil && d.onAppend != nil {
		d.onAppend(p)
	}
	return e, err
}

func (d *deferralAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if d.listErrCategory != "" && category == d.listErrCategory {
		return nil, errors.New("injected list error for " + category)
	}
	if category == "acceptance_skipped_out_of_scope" {
		n := 0
		for _, e := range d.entriesFor(runID) {
			if e.Category == CategoryStageFixupTriggered {
				n++
			}
		}
		d.mu.Lock()
		d.recomputeFixups = append(d.recomputeFixups, n)
		d.mu.Unlock()
	}
	return d.seqAuditRepo.ListForRunByCategory(ctx, runID, category)
}

type listErrConcernRepo struct{ *fakeConcernRepo }

func (r *listErrConcernRepo) ListByRun(context.Context, uuid.UUID) ([]*concern.Concern, error) {
	return nil, errors.New("injected concern list error")
}

type deferralFixture struct {
	s                              *Server
	rr                             *promptRunRepo
	da                             *deferralAudit
	cr                             *fakeConcernRepo
	runID, implID, reviewID, accID uuid.UUID
	priv                           ed25519.PrivateKey
	startedSeq                     int64
}

// newDeferralFixture: implement succeeded, review awaiting_approval,
// acceptance succeeded, plus (when inFlight) a current-process
// implement_review_started entry with ConfiguredAgents=2 and none landed.
func newDeferralFixture(t *testing.T, specYAML string, inFlight bool) *deferralFixture {
	t.Helper()
	_, rr, ar, au, sf, runID, implID, reviewID, accID, priv := newAcceptanceTriageServer(t)
	rr.getRuns[runID].WorkflowSpec = []byte(specYAML)
	rr.getStages[reviewID].State = run.StageStateAwaitingApproval
	f := &deferralFixture{rr: rr, da: &deferralAudit{seqAuditRepo: newSeqAuditRepo(au)}, cr: newFakeConcernRepo(),
		runID: runID, implID: implID, reviewID: reviewID, accID: accID, priv: priv}
	if inFlight {
		f.startedSeq = 500
		f.seedStarted(au, time.Now().UTC().Add(time.Minute))
	}
	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: ar, AuditRepo: f.da, ConcernRepo: f.cr})
	return f
}

func (f *deferralFixture) seedStarted(au *auditFake, ts time.Time) {
	rid, sid := f.runID, f.implID
	payload, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory})
	au.seeded = append(au.seeded, &audit.Entry{RunID: &rid, StageID: &sid, Sequence: f.startedSeq,
		Category: "implement_review_started", Payload: payload, Timestamp: ts})
}

func (f *deferralFixture) seed(category string, stageID uuid.UUID, seq int64, payload string) {
	rid, sid := f.runID, stageID
	f.da.auditFake.mu.Lock()
	f.da.seeded = append(f.da.seeded, &audit.Entry{RunID: &rid, StageID: &sid, Sequence: seq,
		Category: category, Payload: []byte(payload), Timestamp: time.Now().UTC()})
	f.da.auditFake.mu.Unlock()
}

func (f *deferralFixture) ship(t *testing.T) {
	t.Helper()
	body := failedAcceptanceBytes(t, "error", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "failed", Observed: "500 returned", Expected: "201 returned", StepsTaken: "POST /widgets", ExpectationBasis: "criterion ac-create", ReproHandle: "curl -XPOST /widgets"},
	})
	if w := shipAcceptanceRequest(t, f.s, f.runID, f.accID, f.priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

func (f *deferralFixture) byCategory(category string) []*audit.Entry {
	var out []*audit.Entry
	for _, e := range f.da.entriesFor(f.runID) {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out
}

// newestTriage returns the newest acceptance_triage_decided payload + entry.
func (f *deferralFixture) newestTriage(t *testing.T) (map[string]any, *audit.Entry) {
	t.Helper()
	var newest *audit.Entry
	for _, e := range f.byCategory(CategoryAcceptanceTriageDecided) {
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
		}
	}
	if newest == nil {
		t.Fatal("no acceptance_triage_decided entry")
	}
	var m map[string]any
	if err := json.Unmarshal(newest.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m, newest
}

func (f *deferralFixture) triggerConcerns(t *testing.T) (notes []string, provenance []string, ids []string, forced bool) {
	t.Helper()
	trig := f.byCategory(CategoryStageFixupTriggered)
	if len(trig) != 1 {
		t.Fatalf("stage_fixup_triggered entries = %d, want exactly 1", len(trig))
	}
	var p struct {
		Concerns   []planreview.Concern `json:"concerns"`
		ConcernIDs []string             `json:"concern_ids"`
		Forced     bool                 `json:"forced"`
	}
	if err := json.Unmarshal(trig[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Concerns {
		notes = append(notes, c.Note)
		provenance = append(provenance, string(c.Provenance))
	}
	return notes, provenance, p.ConcernIDs, p.Forced
}

// runRealRound drives the REAL implement-review loop: a reject raising a HIGH
// concern and an approve_with_concerns raising sev.
func (f *deferralFixture) runRealRound(sev planreview.ConcernSeverity, rejectFirst bool) {
	first := planreview.VerdictReject
	if !rejectFirst {
		first = planreview.VerdictApproveWithConcerns
	}
	firstSev := planreview.SeverityHigh
	if !rejectFirst {
		firstSev = sev
	}
	a := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: first,
		Concerns: []planreview.Concern{{Severity: firstSev, Category: "correctness", Note: "review-note-one"}}}, model: "m-a"}
	b := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: sev, Category: "tests", Note: "review-note-two"}}}, model: "m-b"}
	f.s.runImplementReviewInvocations(context.Background(), f.runID, f.implID,
		[]reviewerInvocation{{reviewer: a}, {reviewer: b}},
		planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", f.startedSeq)
}

func deferralContainsAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	joined := strings.Join(got, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
}

// TestAcceptanceFixupDeferral_InFlightRound_OnePassCarriesBoth is the done-means
// test: ship -> triage defers -> the REAL review loop settles -> ONE pass
// carrying the acceptance evidence AND both review concerns.
func TestAcceptanceFixupDeferral_InFlightRound_OnePassCarriesBoth(t *testing.T) {
	f := newDeferralFixture(t, deferralDelegatedSpec, true)
	f.ship(t)

	m, deferEntry := f.newestTriage(t)
	if m["disposition"] != acceptanceDispositionFixupDeferred {
		t.Fatalf("disposition = %v, want %s", m["disposition"], acceptanceDispositionFixupDeferred)
	}
	if n := len(f.byCategory(CategoryStageFixupTriggered)); n != 0 {
		t.Fatalf("stage_fixup_triggered at ship = %d, want 0", n)
	}
	if got := f.rr.getStages[f.implID].State; got != run.StageStateSucceeded {
		t.Fatalf("implement = %q, want succeeded while deferred", got)
	}
	if got := f.rr.getStages[f.accID].State; got != run.StageStateSucceeded {
		t.Fatalf("acceptance = %q, want succeeded while deferred", got)
	}

	f.runRealRound(planreview.SeverityMedium, true)

	notes, prov, ids, forced := f.triggerConcerns(t)
	deferralContainsAll(t, notes, "500 returned", "review-note-one", "review-note-two")
	if prov[0] != string(planreview.ConcernProvenanceAcceptance) {
		t.Errorf("acceptance concern provenance = %q, want acceptance", prov[0])
	}
	if forced {
		t.Error("forced = true, want false")
	}
	if len(ids) != 2 {
		t.Fatalf("concern_ids = %v, want the two minted review rows", ids)
	}
	for _, c := range f.cr.rows {
		if c.State != concern.StateAddressedPending {
			t.Errorf("concern %s state = %s, want addressed_pending", c.Note, c.State)
		}
	}
	for name, id := range map[string]uuid.UUID{"implement": f.implID, "review": f.reviewID, "acceptance": f.accID} {
		if got := f.rr.getStages[id].State; got != run.StageStatePending {
			t.Errorf("%s = %q, want pending", name, got)
		}
	}
	final, _ := f.newestTriage(t)
	if final["disposition"] != acceptanceDispositionFixupDispatched {
		t.Errorf("final disposition = %v, want fixup_dispatched", final["disposition"])
	}
	if got, _ := final["released_deferral_sequence"].(float64); int64(got) != deferEntry.Sequence {
		t.Errorf("released_deferral_sequence = %v, want %d", final["released_deferral_sequence"], deferEntry.Sequence)
	}

	rendered := f.s.resolveFixupConcerns(context.Background(), f.runID, f.implID)
	var accDerived, review int
	for _, r := range rendered {
		if r.AcceptanceDerived {
			accDerived++
		} else if strings.Contains(r.Text, "review-note-") {
			review++
		}
	}
	if accDerived != 1 || review != 2 {
		t.Errorf("rendered: acceptance-derived=%d review=%d, want 1 and 2 (%+v)", accDerived, review, rendered)
	}

	// Condition 3: the audit-complete recompute fires AFTER the trigger.
	f.da.mu.Lock()
	obs := append([]int(nil), f.da.recomputeFixups...)
	f.da.mu.Unlock()
	if len(obs) == 0 || obs[len(obs)-1] != 1 {
		t.Errorf("audit-complete recompute observations (fixup triggers seen) = %v, want the last to see the routed trigger", obs)
	}
}

// TestAcceptanceFixupDeferral_ImmediateRouteModes: every case where triage
// must NOT defer routes (or pages) at ship time, exactly as before.
func TestAcceptanceFixupDeferral_ImmediateRouteModes(t *testing.T) {
	cases := []struct {
		name  string
		spec  string
		setup func(f *deferralFixture, au *auditFake)
		want  string
	}{
		{name: "settled round", spec: deferralDelegatedSpec, want: acceptanceDispositionFixupDispatched,
			setup: func(f *deferralFixture, _ *auditFake) {
				f.seed("implement_reviewed", f.implID, 501, `{}`)
				f.seed("implement_reviewed", f.implID, 502, `{}`)
			}},
		{name: "prior-process orphan", spec: deferralDelegatedSpec, want: acceptanceDispositionFixupDispatched,
			setup: func(f *deferralFixture, au *auditFake) {
				au.seeded = nil
				f.seedStarted(au, time.Now().UTC().Add(-time.Hour))
			}},
		{name: "budget spent", spec: deferralDelegatedSpec, want: acceptanceDispositionFixupUnavailable,
			setup: func(f *deferralFixture, _ *auditFake) { f.seed(CategoryStageFixupTriggered, f.implID, 400, `{}`) }},
		{name: "in-flight read error", spec: deferralDelegatedSpec, want: acceptanceDispositionFixupDispatched,
			setup: func(f *deferralFixture, _ *auditFake) { f.da.listErrCategory = "implement_review_started" }},
		{name: "route_fixup not delegated (condition 1)", spec: deferralUndelegatedSpec, want: acceptanceDispositionFixupDispatched},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferralFixture(t, tc.spec, true)
			if tc.setup != nil {
				tc.setup(f, f.da.auditFake)
			}
			f.ship(t)
			m, _ := f.newestTriage(t)
			if m["disposition"] != tc.want {
				t.Fatalf("disposition = %v (reason %v), want %s", m["disposition"], m["reason"], tc.want)
			}
			if tc.want == acceptanceDispositionFixupDispatched {
				notes, _, ids, _ := f.triggerConcerns(t)
				if len(ids) != 0 || len(notes) != 1 {
					t.Errorf("immediate route must be acceptance-only: notes=%v ids=%v", notes, ids)
				}
			}
		})
	}
}

func TestAcceptanceFixupDeferral_DeferredNeverPagesOrCounts(t *testing.T) {
	if acceptanceDispositionPages(acceptanceDispositionFixupDeferred) {
		t.Error("deferred disposition must not page")
	}
	f := newDeferralFixture(t, deferralDelegatedSpec, true)
	f.seed(CategoryAcceptanceTriageDecided, f.accID, 450, `{"disposition":"`+acceptanceDispositionFixupDeferred+`"}`)
	n, err := f.s.countAcceptanceTriageRoutes(context.Background(), f.runID)
	if err != nil || n != 0 {
		t.Errorf("countAcceptanceTriageRoutes = %d, %v; want 0 (a deferral consumes no re-run)", n, err)
	}
}

// seedDeferral writes a pending deferral entry at seq on the acceptance stage.
func (f *deferralFixture) seedDeferral(t *testing.T, seq int64) {
	t.Helper()
	fields := acceptanceTriageFields(f.runID, f.accID, "art-1", acceptanceClass1, acceptanceDispositionFixupDeferred,
		[]string{"ac-create"}, "error", 0, "deferred", nil, nil)
	fields["fixup_deferral"] = acceptanceFixupDeferral{ImplementStageID: f.implID.String(), ReviewRoundSequence: f.startedSeq,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityHigh, Category: "acceptance", Note: "acceptance-evidence", Provenance: planreview.ConcernProvenanceAcceptance}}}
	b, _ := json.Marshal(fields)
	f.seed(CategoryAcceptanceTriageDecided, f.accID, seq, string(b))
}

func (f *deferralFixture) settle() {
	f.seed("implement_reviewed", f.implID, 601, `{}`)
	f.seed("implement_reviewed", f.implID, 602, `{}`)
}

// TestAcceptanceFixupDeferral_ReleaseModes pins each release branch.
func TestAcceptanceFixupDeferral_ReleaseModes(t *testing.T) {
	type outcome struct {
		triggers    int
		disposition string // newest triage disposition after release ("" = no new entry)
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *deferralFixture)
		want  outcome
		check func(t *testing.T, f *deferralFixture, m map[string]any)
	}{
		{name: "no pending deferral", want: outcome{0, ""},
			setup: func(t *testing.T, f *deferralFixture) {
				f.seed(CategoryAcceptanceTriageDecided, f.accID, 550, `{"disposition":"fixup_dispatched"}`)
				f.settle()
			}},
		{name: "still in flight", want: outcome{0, ""},
			setup: func(t *testing.T, f *deferralFixture) { f.seedDeferral(t, 550) },
			check: func(t *testing.T, f *deferralFixture, _ map[string]any) {
				if got := f.rr.getStages[f.implID].State; got != run.StageStateSucceeded {
					t.Errorf("implement = %q, want succeeded", got)
				}
			}},
		{name: "superseded", want: outcome{1, acceptanceDispositionFixupUnavailable},
			setup: func(t *testing.T, f *deferralFixture) {
				f.seedDeferral(t, 550)
				f.settle()
				f.seed(CategoryStageFixupTriggered, f.implID, 700, `{}`)
			},
			check: func(t *testing.T, _ *deferralFixture, m map[string]any) {
				if got, _ := m["superseded_by_fixup_sequence"].(float64); got != 700 {
					t.Errorf("superseded_by_fixup_sequence = %v, want 700", m["superseded_by_fixup_sequence"])
				}
			}},
		{name: "terminal run", want: outcome{0, ""},
			setup: func(t *testing.T, f *deferralFixture) {
				f.seedDeferral(t, 550)
				f.settle()
				f.rr.getRuns[f.runID].State = run.StateCancelled
			}},
		{name: "fixup refused", want: outcome{0, acceptanceDispositionFixupUnavailable},
			setup: func(t *testing.T, f *deferralFixture) {
				f.seedDeferral(t, 550)
				f.settle()
				f.rr.getStages[f.implID].State = run.StageStateFailed
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeferralFixture(t, deferralDelegatedSpec, true)
			tc.setup(t, f)
			before := len(f.byCategory(CategoryAcceptanceTriageDecided))
			f.s.releaseDeferredAcceptanceFixup(context.Background(), f.runID)
			triage := f.byCategory(CategoryAcceptanceTriageDecided)
			if got := len(f.byCategory(CategoryStageFixupTriggered)); got != tc.want.triggers {
				t.Errorf("stage_fixup_triggered = %d, want %d", got, tc.want.triggers)
			}
			if tc.want.disposition == "" {
				if len(triage) != before {
					t.Fatalf("triage entries %d -> %d, want no new entry", before, len(triage))
				}
				if tc.check != nil {
					tc.check(t, f, nil)
				}
				return
			}
			m, _ := f.newestTriage(t)
			if m["disposition"] != tc.want.disposition {
				t.Fatalf("disposition = %v (reason %v), want %s", m["disposition"], m["reason"], tc.want.disposition)
			}
			if tc.check != nil {
				tc.check(t, f, m)
			}
		})
	}
}

// TestAcceptanceFixupDeferral_Release_Idempotent: a second release after a
// successful one appends nothing.
func TestAcceptanceFixupDeferral_Release_Idempotent(t *testing.T) {
	f := newDeferralFixture(t, deferralDelegatedSpec, true)
	f.seedDeferral(t, 550)
	f.settle()
	f.s.releaseDeferredAcceptanceFixup(context.Background(), f.runID)
	n := len(f.byCategory(CategoryAcceptanceTriageDecided))
	f.s.releaseDeferredAcceptanceFixup(context.Background(), f.runID)
	if got := len(f.byCategory(CategoryAcceptanceTriageDecided)); got != n {
		t.Errorf("second release appended: %d -> %d", n, got)
	}
	if got := len(f.byCategory(CategoryStageFixupTriggered)); got != 1 {
		t.Errorf("triggers = %d, want 1", got)
	}
}

// TestAcceptanceFixupDeferral_Release_FoldFilters: requirement and
// conventions_override_attempt rows are held; an approve-only round whose
// open concerns sit below route_fixup_min_severity leaves convergent_concerns
// UNMET, so nothing review-side is folded (condition 1's fold gate).
func TestAcceptanceFixupDeferral_Release_FoldFilters(t *testing.T) {
	t.Run("holds requirement and conventions override", func(t *testing.T) {
		g := newDeferralFixture(t, deferralDelegatedSpec, true)
		g.ship(t)
		mk := func(cat, note string) *concern.Concern {
			c := &concern.Concern{ID: uuid.New(), RunID: g.runID, StageID: g.implID, StageKind: concern.StageKindImplement,
				Severity: "high", Category: cat, Note: note, State: concern.StateRaised}
			g.cr.rows = append(g.cr.rows, c)
			return c
		}
		req := mk("requirement", "req-note")
		conv := mk(planreview.ConventionsOverrideAttemptConcernCategory, "conv-note")
		g.runRealRound(planreview.SeverityMedium, true)
		notes, _, _, _ := g.triggerConcerns(t)
		joined := strings.Join(notes, "|")
		if strings.Contains(joined, "req-note") || strings.Contains(joined, "conv-note") {
			t.Errorf("held concerns routed: %v", notes)
		}
		m, _ := g.newestTriage(t)
		held, _ := m["held_review_concern_ids"].([]any)
		heldS := []string{}
		for _, h := range held {
			heldS = append(heldS, h.(string))
		}
		deferralContainsAll(t, heldS, req.ID.String(), conv.ID.String())
	})
	t.Run("fold requires delegation met", func(t *testing.T) {
		f := newDeferralFixture(t, deferralDelegatedSpec, true)
		f.ship(t)
		f.runRealRound(planreview.SeverityLow, false)
		notes, _, ids, _ := f.triggerConcerns(t)
		if len(ids) != 0 || len(notes) != 1 {
			t.Errorf("unmet delegation must route acceptance only: notes=%v ids=%v", notes, ids)
		}
		m, _ := f.newestTriage(t)
		if s, _ := m["review_concern_fold_unmet"].(string); !strings.Contains(s, "route_fixup_min_severity") {
			t.Errorf("review_concern_fold_unmet = %v, want the threshold reason", m["review_concern_fold_unmet"])
		}
	})
}

// TestAcceptanceFixupDeferral_RoundSettlesDuringDeferralWrite: the round's
// terminals land at the moment the deferral is appended; triage's post-write
// release routes it.
func TestAcceptanceFixupDeferral_RoundSettlesDuringDeferralWrite(t *testing.T) {
	f := newDeferralFixture(t, deferralDelegatedSpec, true)
	f.da.onAppend = func(p audit.ChainAppendParams) {
		if p.Category == CategoryAcceptanceTriageDecided && strings.Contains(string(p.Payload), acceptanceDispositionFixupDeferred) {
			f.settle()
		}
	}
	f.ship(t)
	if got := len(f.byCategory(CategoryStageFixupTriggered)); got != 1 {
		t.Fatalf("triggers = %d, want 1", got)
	}
	if m, _ := f.newestTriage(t); m["disposition"] != acceptanceDispositionFixupDispatched {
		t.Errorf("disposition = %v, want fixup_dispatched", m["disposition"])
	}
}

// TestAcceptanceFixupDeferral_BootReconcileReleasesStrandedDeferral: a
// prior-process orphaned round (ineligible for re-dispatch) plus a pending
// deferral. The terminate-only verb releases nothing; the boot sweep closes
// the round and releases ONE pass.
func TestAcceptanceFixupDeferral_BootReconcileReleasesStrandedDeferral(t *testing.T) {
	f := newDeferralFixture(t, deferralDelegatedSpec, false)
	f.startedSeq = 500
	f.seedStarted(f.da.auditFake, time.Now().UTC().Add(-time.Hour))
	f.seedDeferral(t, 550)

	if _, err := f.s.reconcileRunOrphanedReviews(context.Background(), f.runID); err != nil {
		t.Fatal(err)
	}
	if got := len(f.byCategory(CategoryStageFixupTriggered)); got != 0 {
		t.Fatalf("terminate-only reconcile routed %d triggers, want 0", got)
	}
	// Fresh fixture for the boot arm (the terminate-only pass already closed
	// the round).
	g := newDeferralFixture(t, deferralDelegatedSpec, false)
	g.startedSeq = 500
	g.seedStarted(g.da.auditFake, time.Now().UTC().Add(-time.Hour))
	g.seedDeferral(t, 550)
	if _, err := g.s.reconcileRunOrphanedReviewsForBoot(context.Background(), g.runID); err != nil {
		t.Fatal(err)
	}
	if got := len(g.byCategory(CategoryStageFixupTriggered)); got != 1 {
		t.Fatalf("boot reconcile triggers = %d, want 1", got)
	}
}

// TestAutoFixup_SkipsPendingAcceptanceDeferral (condition 2): autoFixup does
// not route while the newest triage disposition is a pending deferral.
func TestAutoFixup_SkipsPendingAcceptanceDeferral(t *testing.T) {
	for _, pending := range []bool{true, false} {
		f := newDeferralFixture(t, deferralDelegatedSpec, false)
		if pending {
			f.seedDeferral(t, 550)
		}
		open := []*concern.Concern{{ID: uuid.New(), RunID: f.runID, StageID: f.implID, StageKind: concern.StageKindImplement,
			Severity: "high", Category: "correctness", Note: "n", State: concern.StateRaised}}
		f.cr.rows = open
		dispatched, err := f.s.autoFixup(context.Background(), campaignOperatorIdentity(), f.rr.getRuns[f.runID],
			f.rr.stagesByRunID[f.runID], open, "convergent_concerns")
		if err != nil {
			t.Fatal(err)
		}
		if dispatched == pending {
			t.Errorf("pending=%v: dispatched=%v, want %v", pending, dispatched, !pending)
		}
	}
}

// TestAcceptanceFixupDeferral_Release_ConcernListErrorRoutesAcceptanceOnly: a
// review-concern read failure on release still routes the acceptance
// obligation (never stranded) and records review_concerns_unavailable.
func TestAcceptanceFixupDeferral_Release_ConcernListErrorRoutesAcceptanceOnly(t *testing.T) {
	f := newDeferralFixture(t, deferralDelegatedSpec, true)
	f.ship(t)
	f.s.cfg.ConcernRepo = &listErrConcernRepo{f.cr}
	f.runRealRound(planreview.SeverityMedium, true)
	notes, _, ids, _ := f.triggerConcerns(t)
	if len(ids) != 0 || len(notes) != 1 {
		t.Errorf("want acceptance-only pass, notes=%v ids=%v", notes, ids)
	}
	m, _ := f.newestTriage(t)
	if m["disposition"] != acceptanceDispositionFixupDispatched || m["review_concerns_unavailable"] != true {
		t.Errorf("disposition=%v review_concerns_unavailable=%v, want fixup_dispatched + true", m["disposition"], m["review_concerns_unavailable"])
	}
}
