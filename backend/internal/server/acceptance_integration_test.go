package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// This file is the E31.10 capstone cross-boundary seam test (#1538, per the
// #618→#624 lesson): the per-slice unit tests (acceptance_precheck_test.go,
// acceptance_test.go, acceptance_stats_test.go) each prove ONE layer against an
// isolated fake and none crosses a slice boundary, so a payload-shape or
// spec-shape DRIFT between the writers (precheck, orchestrator dispatch, ship
// handler + inline triage) and the readers (the living-anchor renderer, the
// triage classifier's plan-provenance join) would pass every unit test yet
// break in production.
//
// It drives the WHOLE acceptance seam over ONE shared in-memory audit store —
// wired into every writer AND every reader — starting from the COMMITTED
// workflow-v1 example (../../../docs/spec/examples/workflow-v1-acceptance.yaml),
// which it spec.Parse()s and builds the run from. That path welds the example's
// schema+semantic validity to this suite (there is no standalone
// example-validation harness): a schema-invalid or drifted example fails the
// test, satisfying binding condition (b) — the example and the seam cannot
// silently drift apart.

const acceptanceExampleRelPath = "../../../docs/spec/examples/workflow-v1-acceptance.yaml"

// readAcceptanceExampleSpec reads and parses the committed workflow-v1
// acceptance example. spec.ParseBytes runs the semantic validator (the
// type↔executor↔constraint↔artifact↔egress bindings), so a call that returns no
// error is proof the committed stanza the operator hand-applies is valid.
func readAcceptanceExampleSpec(t *testing.T) ([]byte, *spec.Spec) {
	t.Helper()
	raw, err := os.ReadFile(acceptanceExampleRelPath)
	if err != nil {
		t.Fatalf("read committed acceptance example %s: %v", acceptanceExampleRelPath, err)
	}
	parsed, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("spec.ParseBytes(committed example): %v", err)
	}
	return raw, parsed
}

// exampleAcceptanceSeam bundles the fakes + ids of an example-derived run whose
// plan/implement/review stages are succeeded, its approved plan artifact
// (ac-create explicit, ac-list inferred) is seeded, and its acceptance stage is
// in acceptanceState. It is the shared scaffolding for the happy-path and
// failure-mode legs — ONE promptRunRepo (permissive transitions, the fake the
// triage tests drive) and ONE auditFake threaded through every leg.
type exampleAcceptanceSeam struct {
	s                                                  *Server
	rr                                                 *promptRunRepo
	ar                                                 *fakeArtifactRepo
	au                                                 *auditFake
	sf                                                 *signingFake
	runID, planID, implementID, reviewID, acceptanceID uuid.UUID
	priv                                               ed25519.PrivateKey
}

// buildExampleAcceptanceSeam wires the run from the COMMITTED example spec: the
// run's WorkflowSpec is the example bytes, so runAcceptancePrecheck's
// resolveAcceptanceStage and the whole dispatch path see the genuine parsed
// spec, not a hand-rolled fixture.
func buildExampleAcceptanceSeam(t *testing.T, exampleBytes []byte, acceptanceState run.StageState) *exampleAcceptanceSeam {
	t.Helper()
	runID := uuid.New()
	planID, implementID, reviewID, acceptanceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	rr := newPromptRunRepo()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()

	rr.getRuns[runID] = &run.Run{
		ID: runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		State: run.StateRunning, WorkflowSpec: exampleBytes,
	}
	planStage := &run.Stage{ID: planID, RunID: runID, Sequence: 1, Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded}
	implementStage := &run.Stage{ID: implementID, RunID: runID, Sequence: 2, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, State: run.StageStateSucceeded}
	reviewStage := &run.Stage{ID: reviewID, RunID: runID, Sequence: 3, Type: run.StageTypeReview, ExecutorKind: run.ExecutorHuman, State: run.StageStateSucceeded}
	acceptanceStage := &run.Stage{ID: acceptanceID, RunID: runID, Sequence: 4, Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, State: acceptanceState}
	for _, st := range []*run.Stage{planStage, implementStage, reviewStage, acceptanceStage} {
		rr.getStages[st.ID] = st
	}
	// Share the SAME pointers between getStages and stagesByRunID so a
	// TransitionStage mutation is visible to the orchestrator's/triage's
	// ListStagesForRun walk.
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {planStage, implementStage, reviewStage, acceptanceStage}}

	v := "standard_v1"
	ar.all = append(ar.all, &artifact.Artifact{
		ID: uuid.New(), StageID: planID, Kind: artifact.KindPlan,
		SchemaVersion: &v, Content: acceptancePlanArtifactContent(t), CreatedAt: time.Now().UTC(),
	})

	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, SigningRepo: sf, ArtifactRepo: ar, AuditRepo: au})
	priv, _ := sf.issue(t, runID)
	return &exampleAcceptanceSeam{s: s, rr: rr, ar: ar, au: au, sf: sf, runID: runID, planID: planID, implementID: implementID, reviewID: reviewID, acceptanceID: acceptanceID, priv: priv}
}

// TestAcceptanceExample_ParsesAndCarriesEveryMinor welds the committed example
// to the suite: it must parse (schema + semantic validator) AND exercise every
// v1 acceptance minor — the acceptance stage type (v1.1), the acceptance
// produces artifact (v1.2), and the egress allowance (v1.3) — so a future
// example edit that drops one, or a schema change that invalidates the stanza,
// fails here.
func TestAcceptanceExample_ParsesAndCarriesEveryMinor(t *testing.T) {
	_, parsed := readAcceptanceExampleSpec(t)

	if parsed.Version != "1.3" {
		t.Errorf("example version = %q, want 1.3 (must exercise the v1.3 egress minor)", parsed.Version)
	}
	wf, ok := parsed.Workflows["feature_change"]
	if !ok {
		t.Fatalf("example missing the feature_change workflow; got %v", keysOfWorkflows(parsed))
	}
	var acc *spec.Stage
	for i := range wf.Stages {
		if wf.Stages[i].Type == spec.StageTypeAcceptance {
			acc = &wf.Stages[i]
			break
		}
	}
	if acc == nil {
		t.Fatal("example has no acceptance stage — the operator companion stanza is missing")
	}
	// v1.1: acceptance rides the agent executor branch (never delegate).
	if acc.Executor.Agent == "" {
		t.Errorf("acceptance stage must use an agent executor; got %+v", acc.Executor)
	}
	// v1.3: the egress allowance (the single customer-declared allow-list slot).
	if acc.Egress == nil || len(acc.Egress.TargetHosts) == 0 {
		t.Errorf("acceptance stage must declare egress.target_hosts (v1.3); got %+v", acc.Egress)
	}
	// v1.2: the acceptance produces artifact.
	hasAcceptanceArtifact := false
	for _, p := range acc.Produces {
		if string(p.Artifact) == "acceptance" {
			hasAcceptanceArtifact = true
		}
	}
	if !hasAcceptanceArtifact {
		t.Errorf("acceptance stage must declare produces: acceptance (v1.2); got %+v", acc.Produces)
	}
}

func keysOfWorkflows(s *spec.Spec) []string {
	out := make([]string, 0, len(s.Workflows))
	for k := range s.Workflows {
		out = append(out, k)
	}
	return out
}

// TestAcceptanceSeam_ExampleDrivenHappyPath drives the full pass path over one
// shared audit store from the committed example: spec parse → plan
// acceptance-criteria pre-check → orchestrator dispatch emit → runner-shaped
// signed passed verdict → outcome ingest → living-anchor render.
func TestAcceptanceSeam_ExampleDrivenHappyPath(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStatePending)
	ctx := context.Background()

	// 1) Plan pre-check off the example-declared acceptance stage. The plan
	//    carries two well-formed blocking criteria (ac-create explicit /
	//    ac-list inferred), so the entry is checked-and-clean (empty findings).
	if got := seam.s.runAcceptancePrecheck(ctx, seam.runID, seam.planID, acceptancePlanArtifactContent(t)); got == nil {
		t.Fatal("precheck returned nil despite an example-declared acceptance stage")
	}
	pre := lastAcceptancePrecheckEntry(t, seam.au)
	if pre.CriteriaCount != 2 || pre.BlockingCount != 2 {
		t.Errorf("precheck counts = criteria %d / blocking %d, want 2 / 2", pre.CriteriaCount, pre.BlockingCount)
	}
	if len(pre.Findings) != 0 {
		t.Errorf("precheck findings = %+v, want empty (checked-and-clean)", pre.Findings)
	}

	// 2) Orchestrator advance dispatches the acceptance stage (nil GitHub → the
	//    workflow_dispatch is skipped but the transition + emit still happen).
	o := &orchestrator.Orchestrator{Runs: seam.rr, Audit: seam.au}
	if _, err := o.Advance(ctx, seam.runID); err != nil {
		t.Fatalf("orchestrator advance to acceptance: %v", err)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceDispatched); n != 1 {
		t.Fatalf("acceptance_dispatched entries = %d, want 1", n)
	}
	if got := seam.rr.getStages[seam.acceptanceID].State; got != run.StageStateDispatched {
		t.Errorf("acceptance stage state after advance = %q, want dispatched", got)
	}

	// 3) The runner ran; ship a signed, passed, runner-wire-shape verdict.
	seam.rr.getStages[seam.acceptanceID].State = run.StageStateSucceeded
	body, err := json.Marshal(acceptanceBody{
		Verdict: "passed",
		Criteria: []acceptanceCriterionResult{
			{ID: "ac-create", Result: "passed", Observed: "201 returned"},
			{ID: "ac-list", Result: "passed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship passed verdict status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(seam.ar.all) != 2 { // plan artifact + acceptance artifact
		t.Fatalf("artifacts = %d, want 2 (plan + acceptance)", len(seam.ar.all))
	}
	if got := seam.ar.all[1].Kind; got != artifact.KindAcceptance {
		t.Errorf("shipped artifact Kind = %q, want acceptance", got)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
	// A passed verdict triages nothing.
	if n := countAppendedByCategory(seam.au, CategoryAcceptanceTriageDecided); n != 0 {
		t.Errorf("acceptance_triage_decided entries = %d, want 0 on a passed verdict", n)
	}

	// 4) Living-anchor render reads the SAME store and surfaces the outcome.
	entries, _ := seam.au.ListForRun(ctx, seam.runID)
	rendered := issuecomment.RenderStatusBody(seam.rr.getRuns[seam.runID], seam.rr.stagesByRunID[seam.runID], entries, "https://x", time.Now())
	if !strings.Contains(rendered, "Acceptance recorded — accepted (2/2 criteria passed)") {
		t.Errorf("status body missing acceptance outcome line:\n%s", rendered)
	}
}

// TestAcceptanceSeam_NotesCrossesSeam is a7 (#1567): a notes-carrying verdict
// crosses the full wire→domain→persist seam — HTTP ship → validate → artifact
// persist → acceptance_outcome_recorded audit — with the free-text overflow
// field stored verbatim rather than failing closed at the DisallowUnknownFields
// decode.
func TestAcceptanceSeam_NotesCrossesSeam(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	const notes = "preview took ~40s to boot; all criteria passed once up"
	body, err := json.Marshal(acceptanceBody{
		Verdict: "passed",
		Criteria: []acceptanceCriterionResult{
			{ID: "ac-create", Result: "passed", Observed: "201 returned"},
			{ID: "ac-list", Result: "passed"},
		},
		Notes: notes,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship notes-carrying verdict status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(seam.ar.all) != 2 { // plan artifact + acceptance artifact
		t.Fatalf("artifacts = %d, want 2 (plan + acceptance)", len(seam.ar.all))
	}
	if stored := string(seam.ar.all[1].Content); !strings.Contains(stored, `"notes":"`+notes+`"`) {
		t.Errorf("persisted acceptance artifact missing notes verbatim:\n%s", stored)
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
}

// TestAcceptanceSeam_LegacyShapesCoerceAcrossSeam is the #1574-class capstone:
// a verdict carrying BOTH historical field-shape variants — a string-valued
// object-map evidence_hashes AND a schemeless host:port target_url — crosses
// the full HTTP ship → validate/coerce → artifact persist → outcome-audit
// seam and returns 201 (rather than failing closed at the decode as it did for
// 7 of 10 historical acceptance runs). The single acceptance_outcome_recorded
// entry records the coerced http:// target_url and the sorted evidence_hashes
// slice — proof the coercion is what durably lands, not the raw legacy shape.
func TestAcceptanceSeam_LegacyShapesCoerceAcrossSeam(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)
	ctx := context.Background()

	// Both legacy shapes in one body: object-map evidence_hashes + schemeless
	// target_url. Built as raw bytes (not via json.Marshal of acceptanceBody)
	// so the object-map shape survives to the decoder.
	body := []byte(`{"verdict":"passed",` +
		`"criteria":[{"id":"ac-create","result":"passed","observed":"201 returned"},{"id":"ac-list","result":"passed"}],` +
		`"target_url":"localhost:8090",` +
		`"evidence_hashes":{"shot":"sha256:bb","log":"sha256:aa","trace":"sha256:cc"}}`)

	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship legacy-shape verdict status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}

	// The recorded outcome carries the coerced shapes.
	var payload []byte
	seam.au.mu.Lock()
	for _, e := range seam.au.appended {
		if e.Category == CategoryAcceptanceOutcomeRecorded {
			payload = e.Payload
		}
	}
	seam.au.mu.Unlock()
	if payload == nil {
		t.Fatal("no acceptance_outcome_recorded payload found")
	}
	var got struct {
		TargetURL      string   `json:"target_url"`
		EvidenceHashes []string `json:"evidence_hashes"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode outcome payload: %v", err)
	}
	if got.TargetURL != "http://localhost:8090" {
		t.Errorf("recorded target_url = %q, want the coerced http://localhost:8090", got.TargetURL)
	}
	if want := []string{"sha256:aa", "sha256:bb", "sha256:cc"}; !reflectDeepEqualStrings(got.EvidenceHashes, want) {
		t.Errorf("recorded evidence_hashes = %v, want the sorted slice %v", got.EvidenceHashes, want)
	}
	_ = ctx
}

// reflectDeepEqualStrings is a tiny local slice-equality helper (the
// integration file avoids a reflect import for one assertion).
func reflectDeepEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAcceptanceSeam_Class1Error_FixupAndAnchor: a failed{error} verdict
// re-opens implement+review+acceptance to pending (class 1 → fixup_dispatched)
// and the triage decision anchor-renders from the shared store.
func TestAcceptanceSeam_Class1Error_FixupAndAnchor(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	body := failedAcceptanceBytes(t, "error", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "failed", Observed: "500 returned", Expected: "201 returned"},
	})
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	for name, id := range map[string]uuid.UUID{"implement": seam.implementID, "review": seam.reviewID, "acceptance": seam.acceptanceID} {
		if got := seam.rr.getStages[id].State; got != run.StageStatePending {
			t.Errorf("%s state = %q, want pending (class-1 fix-up reopen)", name, got)
		}
	}
	payload := triagePayload(t, seam.au)
	for _, want := range []string{`"class":"1"`, `"disposition":"fixup_dispatched"`, `"failure_mode":"error"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("triage payload missing %s:\n%s", want, payload)
		}
	}
	if n := countAppendedByCategory(seam.au, CategoryStageFixupTriggered); n != 1 {
		t.Errorf("stage_fixup_triggered entries = %d, want 1", n)
	}

	// The triage decision renders on the living anchor.
	entries, _ := seam.au.ListForRun(context.Background(), seam.runID)
	rendered := issuecomment.RenderStatusBody(seam.rr.getRuns[seam.runID], seam.rr.stagesByRunID[seam.runID], entries, "https://x", time.Now())
	if !strings.Contains(rendered, "Acceptance triage — class-1: fixup_dispatched") {
		t.Errorf("status body missing acceptance triage line:\n%s", rendered)
	}
}

// TestAcceptanceSeam_Class2Skip_ReopensAcceptance: a no-failed-but-skipped
// (flake) verdict REOPENS the succeeded acceptance stage (class 2 →
// retry_dispatched) without touching implement/review.
func TestAcceptanceSeam_Class2Skip_ReopensAcceptance(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	body := failedAcceptanceBytes(t, "assertion_fail", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "passed"},
		{ID: "ac-list", Result: "skipped"},
	})
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	if got := seam.rr.getStages[seam.acceptanceID].State; got != run.StageStatePending {
		t.Errorf("acceptance state = %q, want pending (class-2 reopen)", got)
	}
	if got := seam.rr.getStages[seam.implementID].State; got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", got)
	}
	if got := seam.rr.getStages[seam.reviewID].State; got != run.StageStateSucceeded {
		t.Errorf("review state = %q, want unchanged (succeeded)", got)
	}
	if n := countAppendedByCategory(seam.au, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("stage_fixup_triggered entries = %d, want 0 (class-2 is a reopen, not a fixup)", n)
	}
	payload := triagePayload(t, seam.au)
	for _, want := range []string{`"class":"2"`, `"disposition":"retry_dispatched"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("triage payload missing %s:\n%s", want, payload)
		}
	}
}

// TestAcceptanceSeam_Class3Inferred_PagedWithPlanReviewMiss: a failed
// inferred-source criterion (ac-list) pages the human (class 3) with no state
// transition and joins the plan criterion's provenance into a plan_review_miss
// record — the plan→verdict provenance seam.
func TestAcceptanceSeam_Class3Inferred_PagedWithPlanReviewMiss(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	body := failedAcceptanceBytes(t, "assertion_fail", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "passed"},
		{ID: "ac-list", Result: "failed", Observed: "returned an unpaginated array",
			Expected: "a widget list", StepsTaken: "GET /widgets", ReproHandle: "curl $TARGET/widgets"},
	})
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// No stage transitioned (paged is a human hand-off, not auto-routing).
	for name, id := range map[string]uuid.UUID{"implement": seam.implementID, "review": seam.reviewID, "acceptance": seam.acceptanceID} {
		if got := seam.rr.getStages[id].State; got != run.StageStateSucceeded {
			t.Errorf("%s state = %q, want unchanged (succeeded) on a paged class-3", name, got)
		}
	}
	payload := triagePayload(t, seam.au)
	for _, want := range []string{`"class":"3"`, `"disposition":"paged"`, `"ac-list"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("triage payload missing %s:\n%s", want, payload)
		}
	}
	misses := decodePlanReviewMisses(t, payload)
	if len(misses) != 1 {
		t.Fatalf("plan_review_miss entries = %d, want 1", len(misses))
	}
	m := misses[0]
	// Plan-side provenance joined from the seeded plan artifact's ac-list.
	if m.CriterionID != "ac-list" || m.Source != "inferred" || m.Statement != "GET /widgets lists widgets" || m.Rationale != "listing implied" {
		t.Errorf("plan provenance not joined into the miss: %+v", m)
	}
	// Verdict-side observed behavior joined from the shipped criterion.
	if m.Observed != "returned an unpaginated array" || m.Result != "failed" {
		t.Errorf("verdict evidence not joined into the miss: %+v", m)
	}
}

// TestAcceptanceSeam_CriteriaLessBehavioralPlan_PrecheckFlags is the issue's
// "precheck blocks a criteria-less behavioral plan" done-means: a behavioral
// plan shipped with NO acceptance_criteria and empty out_of_scope surfaces a
// no_blocking_criterion finding first-and-high at the plan gate — driven off
// the SAME example-declared acceptance stage.
func TestAcceptanceSeam_CriteriaLessBehavioralPlan_PrecheckFlags(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStatePending)

	body := acceptancePlanBody(t, nil, nil) // no criteria, no out_of_scope
	got := seam.s.runAcceptancePrecheck(context.Background(), seam.runID, seam.planID, body)
	if got == nil {
		t.Fatal("precheck returned nil despite an example-declared acceptance stage")
	}
	entry := lastAcceptancePrecheckEntry(t, seam.au)
	if hasAcceptanceFinding(entry, acceptanceRuleNoBlockingCriterion) == nil {
		t.Fatalf("want a no_blocking_criterion finding for a criteria-less behavioral plan; got %+v", entry.Findings)
	}
}
