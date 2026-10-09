package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- neutralizeClosingReferences (E83.52 / #4085) ---

// TestNeutralizeClosingReferences pins the rewrite table: every active closing
// keyword form aimed at #7 becomes `Refs`, while non-closing mentions, other
// issue numbers, and text inside inline code or fenced blocks are
// byte-preserved. The fenced and inline rows are each the fixture's ONLY
// closing directive, so treating code as active text reddens them.
func TestNeutralizeClosingReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		n    int
	}{
		{"closes", "Closes #7", "Refs #7", 1},
		{"lowercase", "closes #7", "Refs #7", 1},
		{"closed upper", "CLOSED #7", "Refs #7", 1},
		{"fixes colon", "Fixes: #7", "Refs #7", 1},
		{"resolve", "resolve #7", "Refs #7", 1},
		{"fix space colon", "Fix : #7", "Refs #7", 1},
		{"mid sentence", "This PR closes #7 partially.", "This PR Refs #7 partially.", 1},
		{"two references", "Closes #7\n\nFixes #7", "Refs #7\n\nRefs #7", 2},
		{"bare keyword split across lines keeps the newline", "Closes\n#7", "Refs\n#7", 1},
		{"other number", "Closes #71", "Closes #71", 0},
		{"suffix word", "Closes #7foo", "Closes #7foo", 0},
		{"other issue", "Closes #8", "Closes #8", 0},
		{"see mention", "See #7", "See #7", 0},
		{"refs already", "Refs #7", "Refs #7", 0},
		{"inline code", "Write `Closes #7` to close it.", "Write `Closes #7` to close it.", 0},
		{"keyword then code span", "Closes `x` #7", "Closes `x` #7", 0},
		{"backtick fence", "```\nCloses #7\n```", "```\nCloses #7\n```", 0},
		{"tilde fence", "~~~\nCloses #7\n~~~", "~~~\nCloses #7\n~~~", 0},
		{"inner shorter fence does not close the block", "````\n```\nCloses #7\n````", "````\n```\nCloses #7\n````", 0},
		{"info string line is not a closer", "```\n```go\nCloses #7\n```", "```\n```go\nCloses #7\n```", 0},
		{"active after a closed fence", "```\nCloses #7\n```\nCloses #7", "```\nCloses #7\n```\nRefs #7", 1},
		{"unterminated fence swallows the rest", "```\nCloses #7", "```\nCloses #7", 0},
		{"no issue number", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := neutralizeClosingReferences(tc.in, 7)
			if got != tc.want || n != tc.n {
				t.Errorf("neutralizeClosingReferences(%q, 7) = (%q, %d), want (%q, %d)", tc.in, got, n, tc.want, tc.n)
			}
			if hasClosingReference(got, 7) {
				t.Errorf("result %q still carries an active closing reference to #7", got)
			}
		})
	}
}

// TestNeutralizeClosingReferences_NeverLeavesAClosingReference pins the
// invariant against hasClosingReference's OWN reading of a body, including the
// shapes where its whitespace run crosses an elided fence (a keyword and its
// `#7` separated by a code block) or where the keyword-to-colon span crosses
// masked text — there only the keyword may be replaced, so the fence bytes
// survive.
func TestNeutralizeClosingReferences_NeverLeavesAClosingReference(t *testing.T) {
	for _, in := range []string{
		"Closes\n```\nnote\n```\n#7",
		"Fixes\n```\n```\n: #7",
		"closes #7, fixes #7 and resolves: #7",
		"Closes #7\r\nmore",
		"  * Closes #7",
		"`code` Closes #7 `more`",
		"Closes #7`",
		"close Closes #7",
		"Closes\t#7",
	} {
		got, n := neutralizeClosingReferences(in, 7)
		if !hasClosingReference(in, 7) {
			t.Fatalf("fixture %q carries no closing reference; the row tests nothing", in)
		}
		if n == 0 || hasClosingReference(got, 7) {
			t.Errorf("neutralizeClosingReferences(%q) = (%q, %d); want a rewrite leaving no closing reference", in, got, n)
		}
	}
	got, _ := neutralizeClosingReferences("Fixes\n```\n```\n: #7", 7)
	if got != "Refs\n```\n```\n: #7" {
		t.Errorf("a keyword-to-colon span crossing a fence must replace only the keyword; got %q", got)
	}
	if got, n := neutralizeClosingReferences("Closes #7", 0); got != "Closes #7" || n != 0 {
		t.Errorf("issue number 0 must be a no-op; got (%q, %d)", got, n)
	}
}

// --- ship-time guard (neutralizePartialDeliveryClosingRef) ---

// countingArtifactRepo counts plan-artifact lookups so a test can prove a guard
// returned BEFORE the plan was loaded.
type countingArtifactRepo struct {
	*fakeArtifactRepo
	mu    sync.Mutex
	lists int
}

func (c *countingArtifactRepo) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.fakeArtifactRepo.ListForStage(ctx, stageID)
}

func (c *countingArtifactRepo) listCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}

// partialDeliveryFixture is a run with a triggering issue (#7), an installation,
// a plan stage holding a standard_v1 artifact with the given delivery, and an
// implement stage, behind a GitHub stub that serves the PR body on GET and
// captures the PATCH.
type partialDeliveryFixture struct {
	s       *Server
	sf      *signingFake
	rr      *orchestratorRepo
	ar      *countingArtifactRepo
	au      *auditFake
	stub    *stampGitHub
	runRow  *run.Run
	implStg *run.Stage
}

const partialDeliveryRemainingScope = "the merge-time remaining-scope comment lands in a later run"

func newPartialDeliveryFixture(t *testing.T, delivery string, prBody string) *partialDeliveryFixture {
	t.Helper()
	f := &partialDeliveryFixture{
		sf:   newSigningFake(),
		rr:   newOrchestratorRepo(),
		ar:   &countingArtifactRepo{fakeArtifactRepo: newFakeArtifactRepo()},
		au:   newAuditFake(),
		stub: &stampGitHub{getBody: prBody},
	}
	f.runRow = f.rr.seedRun()
	inst := int64(99)
	f.runRow.InstallationID = &inst
	f.runRow.IssueContext = &run.IssueContext{Number: 7, Title: "t"}
	planStage := f.rr.seedStage(f.runRow.ID, 0, run.StageStateSucceeded)
	p := plan.Plan{Summary: "Ship one slice.", Delivery: delivery}
	if delivery == plan.DeliveryPartial {
		p.RemainingScope = partialDeliveryRemainingScope
	}
	content, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	f.ar.all = append(f.ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: content,
	})
	f.implStg = f.rr.seedStage(f.runRow.ID, 1, run.StageStateRunning)
	f.implStg.Type = run.StageTypeImplement
	f.implStg.RequiresApproval = true
	f.s = New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  f.sf,
		ArtifactRepo: f.ar,
		AuditRepo:    f.au,
		RunRepo:      f.rr,
		Orchestrator: &orchestrator.Orchestrator{Runs: f.rr},
		GitHub:       newStampGitHubClient(t, f.stub),
	})
	return f
}

func (f *partialDeliveryFixture) neutralizedRows() []audit.ChainAppendParams {
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range f.au.appended {
		if p.Category == categoryPartialDeliveryClosingReferenceNeutralized {
			out = append(out, p)
		}
	}
	return out
}

func (f *partialDeliveryFixture) github() (getCalled, patchCalled bool, patchBody string) {
	f.stub.mu.Lock()
	defer f.stub.mu.Unlock()
	return f.stub.getCalled, f.stub.patchCalled, f.stub.patchBody
}

// shipPartialDelivery drives the REAL ship handler with an agent PR body.
func (f *partialDeliveryFixture) ship(t *testing.T) int {
	t.Helper()
	priv, _ := f.sf.issue(t, f.runRow.ID)
	w := shipPRRequest(t, f.s, f.runRow.ID, f.implStg.ID, priv, validPRBytes(t), "")
	return w.Code
}

const agentClosingBody = "## Summary\n\n- ship one slice\n\nCloses #7\n"

// TestShipPullRequest_PartialDelivery_RewritesClosingReference is the guard's
// success path through the REAL ship handler: a partial plan's PR body that
// still carries `Closes #7` is PATCHed to `Refs #7`, and exactly one system-actor
// audit row records the rewrite. Deleting the call site in handleShipPullRequest
// leaves the PATCH uncalled and reddens this.
func TestShipPullRequest_PartialDelivery_RewritesClosingReference(t *testing.T) {
	f := newPartialDeliveryFixture(t, plan.DeliveryPartial, agentClosingBody)
	if code := f.ship(t); code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201", code)
	}
	_, patched, body := f.github()
	if !patched {
		t.Fatal("EditPullRequest was not called; the partial plan's PR still closes #7")
	}
	if hasClosingReference(body, 7) || !strings.Contains(body, "Refs #7") {
		t.Errorf("rewritten body must reference #7 without closing it:\n%s", body)
	}
	rows := f.neutralizedRows()
	if len(rows) != 1 {
		t.Fatalf("neutralized audit rows = %d, want 1", len(rows))
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["issue_number"] != float64(7) || payload["pr_number"] != float64(42) || payload["rewritten"] != float64(1) {
		t.Errorf("audit payload = %v, want issue_number 7, pr_number 42, rewritten 1", payload)
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorSystem {
		t.Errorf("audit actor = %v, want system", rows[0].ActorKind)
	}
	if rows[0].StageID == nil || *rows[0].StageID != f.implStg.ID {
		t.Errorf("audit stage = %v, want the implement stage", rows[0].StageID)
	}
}

// TestShipPullRequest_PartialDelivery_AlreadyAdvancedStage pins approval
// condition C1: the guard sits OUTSIDE the `implement && running` terminal
// drive, so the non-gated flow — whose implement stage the trace handler already
// advanced to succeeded before the PR upload arrives — is guarded too. With the
// call moved inside that conditional, this stage shape never reaches it.
func TestShipPullRequest_PartialDelivery_AlreadyAdvancedStage(t *testing.T) {
	f := newPartialDeliveryFixture(t, plan.DeliveryPartial, agentClosingBody)
	f.implStg.State = run.StageStateSucceeded
	f.implStg.RequiresApproval = false
	if code := f.ship(t); code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201", code)
	}
	_, patched, body := f.github()
	if !patched || hasClosingReference(body, 7) {
		t.Fatalf("already-advanced implement stage was not guarded (patched=%v):\n%s", patched, body)
	}
	if n := len(f.neutralizedRows()); n != 1 {
		t.Errorf("neutralized audit rows = %d, want 1", n)
	}
}

// TestShipPullRequest_PartialDelivery_EditErrorRecordsNothing pins that the
// audit row is appended ONLY after EditPullRequest succeeds — the check reads
// the COMMITTED chain after the call, so an append moved above the edit reddens
// it — and that a forge failure never unwinds the ship response.
func TestShipPullRequest_PartialDelivery_EditErrorRecordsNothing(t *testing.T) {
	f := newPartialDeliveryFixture(t, plan.DeliveryPartial, agentClosingBody)
	f.stub.patchStatus = http.StatusInternalServerError
	if code := f.ship(t); code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201 despite the edit failure", code)
	}
	if _, patched, _ := f.github(); !patched {
		t.Fatal("EditPullRequest was not attempted; the fixture does not reach the edit")
	}
	if n := len(f.neutralizedRows()); n != 0 {
		t.Errorf("neutralized audit rows = %d after a failed edit, want 0", n)
	}
}

// TestNeutralizePartialDeliveryClosingRef_SkipBranches pins every early return
// on the guard itself: each row leaves NO forge read, NO edit and NO audit row.
func TestNeutralizePartialDeliveryClosingRef_SkipBranches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery string
		body     string
		mutate   func(f *partialDeliveryFixture) (runID uuid.UUID, prNumber int)
		// wantGet reports whether the guard is expected to READ the PR before
		// deciding to do nothing (only the live-body rows reach the forge).
		wantGet bool
	}{
		{name: "full delivery", delivery: plan.DeliveryFull, body: agentClosingBody},
		{name: "absent delivery", delivery: "", body: agentClosingBody},
		{name: "run load fails", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(*partialDeliveryFixture) (uuid.UUID, int) { return uuid.New(), 42 }},
		{name: "plan load fails", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) {
				f.ar.listErr = errors.New("artifact store down")
				return f.runRow.ID, 42
			}},
		{name: "no installation", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) {
				f.runRow.InstallationID = nil
				return f.runRow.ID, 42
			}},
		{name: "no github client", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) {
				f.s.cfg.GitHub = nil
				return f.runRow.ID, 42
			}},
		// No dedicated guard: GetPullRequest refuses a non-positive number before
		// any request (so no GET reaches the stub), landing on the get-error skip.
		{name: "no pr number", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) { return f.runRow.ID, 0 }},
		{name: "unparseable repo", delivery: plan.DeliveryPartial, body: agentClosingBody,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) {
				f.runRow.Repo = "not-a-repo"
				return f.runRow.ID, 42
			}},
		{name: "get pr fails", delivery: plan.DeliveryPartial, body: agentClosingBody, wantGet: true,
			mutate: func(f *partialDeliveryFixture) (uuid.UUID, int) {
				f.stub.getStatus = http.StatusInternalServerError
				return f.runRow.ID, 42
			}},
		{name: "live body has no closing reference", delivery: plan.DeliveryPartial, wantGet: true,
			body: "## Summary\n\nRefs #7\n\n`Closes #7` is documented here only.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPartialDeliveryFixture(t, tc.delivery, tc.body)
			runID, prNumber := f.runRow.ID, 42
			if tc.mutate != nil {
				runID, prNumber = tc.mutate(f)
			}
			f.s.neutralizePartialDeliveryClosingRef(context.Background(), runID, f.implStg.ID, prNumber)
			got, patched, _ := f.github()
			if got != tc.wantGet {
				t.Errorf("GetPullRequest called = %v, want %v", got, tc.wantGet)
			}
			if patched {
				t.Error("EditPullRequest called on a skip branch")
			}
			if n := len(f.neutralizedRows()); n != 0 {
				t.Errorf("neutralized audit rows = %d on a skip branch, want 0", n)
			}
		})
	}
}

// TestNeutralizePartialDeliveryClosingRef_NoIssueSkipsBeforePlanLoad pins the
// ordering the plan names: a run without a triggering issue returns before the
// plan is loaded and before any forge call, even when its plan is partial.
func TestNeutralizePartialDeliveryClosingRef_NoIssueSkipsBeforePlanLoad(t *testing.T) {
	for _, ic := range []*run.IssueContext{nil, {Number: 0}} {
		f := newPartialDeliveryFixture(t, plan.DeliveryPartial, agentClosingBody)
		f.runRow.IssueContext = ic
		f.s.neutralizePartialDeliveryClosingRef(context.Background(), f.runRow.ID, f.implStg.ID, 42)
		if n := f.ar.listCount(); n != 0 {
			t.Errorf("issue context %+v: plan artifacts listed %d time(s), want 0", ic, n)
		}
		if got, patched, _ := f.github(); got || patched {
			t.Errorf("issue context %+v: forge called (get=%v patch=%v), want none", ic, got, patched)
		}
		if n := len(f.neutralizedRows()); n != 0 {
			t.Errorf("issue context %+v: neutralized audit rows = %d, want 0", ic, n)
		}
	}
}

// TestNeutralizePartialDeliveryClosingRef_AuditAppendFailure pins the last
// best-effort branch: the forge rewrite stands (the PATCH landed) even though
// the audit append failed, and nothing panics or retries.
func TestNeutralizePartialDeliveryClosingRef_AuditAppendFailure(t *testing.T) {
	f := newPartialDeliveryFixture(t, plan.DeliveryPartial, agentClosingBody)
	f.au.appendErrCategory = categoryPartialDeliveryClosingReferenceNeutralized
	f.s.neutralizePartialDeliveryClosingRef(context.Background(), f.runRow.ID, f.implStg.ID, 42)
	_, patched, body := f.github()
	if !patched || hasClosingReference(body, 7) {
		t.Fatalf("the PR body rewrite must stand when only the audit append fails (patched=%v):\n%s", patched, body)
	}
	if n := len(f.neutralizedRows()); n != 0 {
		t.Errorf("neutralized audit rows = %d with the append failing, want 0", n)
	}
}

// TestPartialDeliveryPlan reports the shared helper's three outcomes: partial,
// full, and no loadable plan (no ArtifactRepo → nil plan → false).
func TestPartialDeliveryPlan(t *testing.T) {
	partial := newPartialDeliveryFixture(t, plan.DeliveryPartial, "")
	if p, ok := partial.s.partialDeliveryPlan(context.Background(), partial.runRow.ID); !ok || p == nil || p.RemainingScope != partialDeliveryRemainingScope {
		t.Errorf("partial plan: got (%v, %v), want the partial plan and true", p, ok)
	}
	full := newPartialDeliveryFixture(t, plan.DeliveryFull, "")
	if _, ok := full.s.partialDeliveryPlan(context.Background(), full.runRow.ID); ok {
		t.Error("full plan reported partial")
	}
	full.s.cfg.ArtifactRepo = nil
	if p, ok := full.s.partialDeliveryPlan(context.Background(), full.runRow.ID); ok || p != nil {
		t.Errorf("no artifact repo: got (%v, %v), want (nil, false)", p, ok)
	}
}

// --- plan-upload delivery rule (E83.52 / #4085 acceptance failure) ---

// deliveryShipBody returns a schema-shaped plan body (one drivable criterion)
// with the top-level delivery / remaining_scope keys set; an empty value omits
// the key. Unlike withDelivery it validates nothing, so it can build the
// schema-invalid partial-without-remaining_scope shape too.
func deliveryShipBody(t *testing.T, delivery, remainingScope string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(acceptancePlanBody(t, []map[string]any{drivableCriterion("c1")}, nil), &m); err != nil {
		t.Fatalf("decode plan body: %v", err)
	}
	if delivery != "" {
		m["delivery"] = delivery
	}
	if remainingScope != "" {
		m["remaining_scope"] = remainingScope
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal plan body: %v", err)
	}
	return out
}

// TestShipPlan_DeliveryRule_RefusedAtUpload drives the REAL
// POST /v0/runs/{id}/plan route with each delivery / remaining_scope shape the
// schema description and plan.Parse call invalid, and reads COMMITTED state
// after the call: 400 plan_invalid with details.error naming the rule, and no
// artifact row (assertNoSideEffects). handleShipPlan never reaches plan.Parse,
// so before the rule moved into plan.Validate the full+remaining_scope,
// remaining_scope-only and whitespace-only shapes shipped 201 (the acceptance
// failure at 1628eded).
//
// Counterfactual (run): make plan.Validate skip checkDeliveryBytes → the
// "full with remaining_scope", "remaining_scope only" and "partial with
// whitespace" rows go RED (201, artifact stored); the schema row stays green.
func TestShipPlan_DeliveryRule_RefusedAtUpload(t *testing.T) {
	for _, tc := range []struct {
		name, delivery, remaining, wantInError string
	}{
		{"partial without remaining_scope", plan.DeliveryPartial, "", "remaining_scope"},
		{"full with remaining_scope", plan.DeliveryFull, "x", "only valid with delivery: partial"},
		{"remaining_scope only", "", "the rest", "only valid with delivery: partial"},
		{"partial with whitespace remaining_scope", plan.DeliveryPartial, "   ", "non-blank"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSurfaceRefusalHarness(t)
			code, resp := h.ship(t, deliveryShipBody(t, tc.delivery, tc.remaining))
			if code != http.StatusBadRequest {
				t.Errorf("plan status = %d, want 400:\n%s", code, resp)
			} else {
				env := decodePlanInvalid(t, resp)
				if env.Error.Code != "plan_invalid" {
					t.Errorf("error.code = %q, want plan_invalid", env.Error.Code)
				}
				if !strings.Contains(env.Error.Details.Error, tc.wantInError) {
					t.Errorf("details.error = %q, want it to name the rule (%q)", env.Error.Details.Error, tc.wantInError)
				}
			}
			h.assertNoSideEffects(t)
		})
	}
}

// TestShipPlan_DeliveryRule_ValidPartialAdmitted is the narrowness control: a
// partial plan carrying a non-blank remaining_scope ships 201, its artifact is
// stored and the plan stage reaches awaiting_approval.
func TestShipPlan_DeliveryRule_ValidPartialAdmitted(t *testing.T) {
	h := newSurfaceRefusalHarness(t)
	code, resp := h.ship(t, deliveryShipBody(t, plan.DeliveryPartial, "the merge-time comment lands in a later run"))
	if code != http.StatusCreated {
		t.Fatalf("plan status = %d, want 201:\n%s", code, resp)
	}
	arts, err := h.art.ListForStage(context.Background(), h.plan.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	if len(arts) != 1 {
		t.Errorf("artifact rows for the plan stage = %d, want 1", len(arts))
	}
	h.rr.mu.Lock()
	got := h.rr.stagesByID[h.plan.ID]
	h.rr.mu.Unlock()
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("plan stage state = %q, want awaiting_approval", got.State)
	}
}
