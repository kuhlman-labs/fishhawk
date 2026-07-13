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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
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
		Criteria: critRaw(
			acceptanceCriterionResult{ID: "ac-create", Result: "passed", Observed: "201 returned"},
			acceptanceCriterionResult{ID: "ac-list", Result: "passed"},
		),
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
		Criteria: critRaw(
			acceptanceCriterionResult{ID: "ac-create", Result: "passed", Observed: "201 returned"},
			acceptanceCriterionResult{ID: "ac-list", Result: "passed"},
		),
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

// TestAcceptanceSeam_ObjectKeyedCriteriaCoerceAcrossSeam is the E38.1 binding-
// condition e2e (#1655, the third #1574-class shape variant): a verdict whose
// `criteria` field is a JSON OBJECT keyed by criterion id crosses the full HTTP
// ship → decode → validate/coerce → handler → outcome-record + triage seam and
// is ACCEPTED (201, not a category-B 400). It proves the object-keyed shape
// survives end-to-end exactly as #1646's target_url/evidence_hashes coercions
// did:
//   - the acceptance_outcome_recorded outcome reflects the coerced FLAT-ARRAY
//     form — its criteria tally counts BOTH folded criteria; and
//   - each object key folded into an element id, deterministically SORTED — the
//     class-1 triage decision's criterion_ids echo the two folded ids in sorted
//     order (a before b) even though the body lists them b-before-a. (Folding is
//     load-bearing: an unfolded element would carry an empty id and validate()
//     would 400 it category-B, so reaching 201 with both ids present is itself
//     proof each key folded into a valid element.)
func TestAcceptanceSeam_ObjectKeyedCriteriaCoerceAcrossSeam(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	// Object-keyed criteria with the distinctive ids, keys deliberately out of
	// sorted order (b before a) so the sort is observable. failure_mode=error
	// routes to class-1 triage, whose criterion_ids echo the folded, sorted ids.
	body := []byte(`{"verdict":"failed","failure_mode":"error","criteria":{` +
		`"e2e-obj-crit-b":{"result":"failed","observed":"500 on b"},` +
		`"e2e-obj-crit-a":{"result":"failed","observed":"500 on a"}}}`)

	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship object-keyed criteria status = %d, want 201 (accepted, not category-B):\n%s", w.Code, w.Body.String())
	}

	// (2a) The acceptance_outcome_recorded outcome reflects the coerced flat-array
	//      form — the tally counts BOTH folded criteria.
	if n := countByCategory(seam.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
	outcome := findAppendedByCategory(t, seam.au, CategoryAcceptanceOutcomeRecorded)
	for _, want := range []string{`"verdict":"failed"`, `"outcome":"rejected"`, `"criteria_failed":2`, `"criteria_total":2`} {
		if !strings.Contains(string(outcome.Payload), want) {
			t.Errorf("outcome payload missing %s:\n%s", want, outcome.Payload)
		}
	}

	// (2b) Each object key folded into an element id AND the flat array is
	//      deterministically sorted: the class-1 triage decision records the two
	//      folded ids in sorted order (a before b, despite b-before-a in the body).
	payload := triagePayload(t, seam.au)
	if !strings.Contains(payload, `"criterion_ids":["e2e-obj-crit-a","e2e-obj-crit-b"]`) {
		t.Errorf("triage criterion_ids must be the folded, deterministically sorted ids [a,b]:\n%s", payload)
	}
	for _, want := range []string{`"class":"1"`, `"disposition":"fixup_dispatched"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("triage payload missing %s:\n%s", want, payload)
		}
	}
}

// TestAcceptanceSeam_OutOfScopeZeroCriteria_AutoTerminal is the E38.3 (#1657)
// cross-boundary capstone: it crosses the orchestrator state machine → the audit
// store → the audit-completeness rule engine — the seam the per-layer units
// (plan predicate, orchestrator advance, auditcomplete rule, next_actions) cannot
// cover alone. The orchestrator auto-terminates an out_of_scope / zero-criteria
// acceptance stage and emits the skip marker; auditcomplete.Compute reads that
// marker from the SAME store and exempts the traceless acceptance stage, reaching
// StatePass. Both legs carry a negative control proving the behavior is gated
// (criteria present → still dispatched; no marker → still trace_missing).
//
// It uses the functional orchestratorRepo (real TransitionStage/TransitionRun so
// the run can reach succeeded) + the hash-chaining auditCompleteAuditFake (so
// verifyChain accepts the store — the promptRunRepo/auditFake seam used by the
// happy-path legs above cannot reach StatePass) + a fakeArtifactRepo holding the
// approved plan.
func TestAcceptanceSeam_OutOfScopeZeroCriteria_AutoTerminal(t *testing.T) {
	ctx := context.Background()

	// build seeds a plan(succ)+implement(succ)+review(succ)+acceptance(pending)
	// run with the approved plan artifact under the plan stage.
	build := func(t *testing.T, planJSON string) (runID, implID, accID uuid.UUID, rr *orchestratorRepo, ar *fakeArtifactRepo, au *auditCompleteAuditFake) {
		t.Helper()
		rr = newOrchestratorRepo()
		r := rr.seedRun()
		planS := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
		planS.Type = run.StageTypePlan
		impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
		impl.Type = run.StageTypeImplement
		rev := rr.seedStage(r.ID, 2, run.StageStateSucceeded)
		rev.Type = run.StageTypeReview
		acc := rr.seedStage(r.ID, 3, run.StageStatePending)
		acc.Type = run.StageTypeAcceptance

		ar = newFakeArtifactRepo()
		v := "standard_v1"
		ar.all = append(ar.all, &artifact.Artifact{
			ID: uuid.New(), StageID: planS.ID, Kind: artifact.KindPlan,
			SchemaVersion: &v, Content: json.RawMessage(planJSON), CreatedAt: time.Now().UTC(),
		})
		au = newAuditCompleteAuditFake()
		return r.ID, impl.ID, acc.ID, rr, ar, au
	}
	countCat := func(t *testing.T, au *auditCompleteAuditFake, runID uuid.UUID, cat string) int {
		t.Helper()
		es, err := au.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			t.Fatalf("ListForRunByCategory: %v", err)
		}
		return len(es)
	}
	seedTraceEvidence := func(t *testing.T, au *auditCompleteAuditFake, ar *fakeArtifactRepo, runID, planID, implID uuid.UUID) {
		t.Helper()
		au.appendTrace(t, runID, planID, "raw")
		au.appendTrace(t, runID, planID, "redacted")
		au.appendTrace(t, runID, implID, "raw")
		au.appendTrace(t, runID, implID, "redacted")
		ar.all = append(ar.all, &artifact.Artifact{
			ID: uuid.New(), StageID: implID, Kind: artifact.KindPullRequest,
			Content: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(),
		})
	}

	const skipPlan = `{"plan_version":"standard_v1","summary":"x","verification":{"test_strategy":"unit","rollback_plan":"revert","out_of_scope":["deletion deferred to a follow-up"]}}`
	const criteriaPlan = `{"plan_version":"standard_v1","summary":"x","verification":{"test_strategy":"unit","rollback_plan":"revert","out_of_scope":["deletion deferred"],"acceptance_criteria":[{"id":"ac-1","statement":"POST returns 201","source":"explicit","source_ref":"#1","blocking":true}]}}`

	// --- Orchestrator leg: the skip path drives acceptance → succeeded and the
	//     run → succeeded, emitting the skip marker (and NO acceptance_dispatched).
	runID, implID, accID, rr, ar, au := build(t, skipPlan)
	o := &orchestrator.Orchestrator{Runs: rr, Audit: au, Artifacts: ar}
	out, err := o.Advance(ctx, runID)
	if err != nil {
		t.Fatalf("advance (skip): %v", err)
	}
	if out != orchestrator.OutcomeRunCompleted {
		t.Errorf("advance outcome = %q, want run_completed", out)
	}
	if got := rr.stagesByID[accID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance stage state = %q, want succeeded (auto-terminated, NOT dispatched)", got)
	}
	if got := rr.runs[runID].State; got != run.StateSucceeded {
		t.Errorf("run state = %q, want succeeded", got)
	}
	if n := countCat(t, au, runID, CategoryAcceptanceSkippedOutOfScope); n != 1 {
		t.Fatalf("acceptance_skipped_out_of_scope entries = %d, want 1", n)
	}
	if n := countCat(t, au, runID, CategoryAcceptanceDispatched); n != 0 {
		t.Errorf("acceptance_dispatched entries = %d, want 0 on the skip path", n)
	}

	// --- Server-gate leg (E38.3 / #1877): acceptanceGateState classifies the
	//     REAL orchestrator-written skip marker as merge-eligible, crossing the
	//     orchestrator-write -> server-gate-read boundary the unit tables fake.
	//     A synthetic run row carries the acceptance-declaring spec so
	//     resolveAcceptanceStageSpec's off-switch is satisfied; the gate reads the
	//     marker from the SAME audit store the orchestrator wrote to (au).
	gateSrv := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	gateRun := &run.Run{ID: runID, WorkflowID: "feature_change", WorkflowSpec: specWithAcceptanceStage, State: run.StateSucceeded}
	gateStages := []*run.Stage{{ID: accID, RunID: runID, Type: run.StageTypeAcceptance, State: run.StageStateSucceeded}}
	gotGate, gerr := gateSrv.acceptanceGateState(ctx, gateRun, gateStages)
	if gerr != nil {
		t.Fatalf("acceptanceGateState (skip marker): %v", gerr)
	}
	if gotGate != acceptanceGateSkippedOutOfScope {
		t.Errorf("acceptanceGateState = %q, want %q (the real orchestrator skip marker is merge-eligible)", gotGate, acceptanceGateSkippedOutOfScope)
	}

	// --- Orchestrator control: a plan with a blocking criterion still DISPATCHES
	//     the acceptance stage (predicate false → operator-dispatched path).
	cRunID, _, cAccID, cRR, cAR, cAU := build(t, criteriaPlan)
	cO := &orchestrator.Orchestrator{Runs: cRR, Audit: cAU, Artifacts: cAR}
	if _, err := cO.Advance(ctx, cRunID); err != nil {
		t.Fatalf("advance (control): %v", err)
	}
	if got := cRR.stagesByID[cAccID].State; got != run.StageStateDispatched {
		t.Errorf("control acceptance stage state = %q, want dispatched (a criterion is present)", got)
	}
	if n := countCat(t, cAU, cRunID, CategoryAcceptanceDispatched); n != 1 {
		t.Errorf("control acceptance_dispatched entries = %d, want 1", n)
	}
	if n := countCat(t, cAU, cRunID, CategoryAcceptanceSkippedOutOfScope); n != 0 {
		t.Errorf("control acceptance_skipped_out_of_scope entries = %d, want 0", n)
	}

	// --- auditcomplete leg: seed the plan+implement trace evidence + the PR
	//     artifact, then Compute over the SAME store the orchestrator wrote the
	//     skip marker to. StatePass despite the acceptance stage having no trace.
	planStageID := rr.stagesByRunID[runID][0].ID
	seedTraceEvidence(t, au, ar, runID, planStageID, implID)
	state, missing, err := auditcomplete.Compute(ctx, runID, auditcomplete.Deps{Runs: rr, Artifacts: ar, Audit: au})
	if err != nil {
		t.Fatalf("auditcomplete.Compute (skip): %v", err)
	}
	if state != stagecheck.StatePass {
		t.Fatalf("auditcomplete state = %s, want pass (the skip marker exempts the traceless acceptance stage); missing=%+v", state, missing)
	}

	// --- auditcomplete control: the identical run WITHOUT the skip marker still
	//     fails trace_missing for the traceless acceptance stage (marker-gated,
	//     not blanket).
	ctrlRunID, ctrlImplID, ctrlAccID, ctrlRR, ctrlAR, ctrlAU := build(t, skipPlan)
	ctrlRR.stagesByID[ctrlAccID].State = run.StageStateSucceeded // succeeded, but NO skip marker emitted
	ctrlPlanID := ctrlRR.stagesByRunID[ctrlRunID][0].ID
	seedTraceEvidence(t, ctrlAU, ctrlAR, ctrlRunID, ctrlPlanID, ctrlImplID)
	cState, cMissing, err := auditcomplete.Compute(ctx, ctrlRunID, auditcomplete.Deps{Runs: ctrlRR, Artifacts: ctrlAR, Audit: ctrlAU})
	if err != nil {
		t.Fatalf("auditcomplete.Compute (control): %v", err)
	}
	if cState != stagecheck.StateFail {
		t.Fatalf("auditcomplete control state = %s, want fail (an unmarked acceptance stage still needs its trace); missing=%+v", cState, cMissing)
	}
	sawAcceptanceTraceMiss := false
	for _, m := range cMissing {
		if m.Kind == auditcomplete.MissingTrace && strings.Contains(m.Detail, "acceptance") {
			sawAcceptanceTraceMiss = true
		}
	}
	if !sawAcceptanceTraceMiss {
		t.Errorf("want a trace_missing item naming the acceptance stage; got %+v", cMissing)
	}
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

// TestAcceptanceSeam_Class5_ExternallyUnvalidatable_Terminal is the #1671
// end-to-end done-means, crossing ship→classify→route→stage-state: an all-skip
// verdict whose every skip carries expectation_basis leaves the acceptance
// stage terminal/succeeded (no re-open, no re-dispatch) and writes an
// acceptance_triage_decided entry with disposition externally_unvalidatable_paged
// and class "5" — proving the merge gate is no longer wedged by a futile
// class-2 retry loop and fishhawk_audit_complete can clear.
func TestAcceptanceSeam_Class5_ExternallyUnvalidatable_Terminal(t *testing.T) {
	exampleBytes, _ := readAcceptanceExampleSpec(t)
	seam := buildExampleAcceptanceSeam(t, exampleBytes, run.StageStateSucceeded)

	body := failedAcceptanceBytes(t, "assertion_fail", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "skipped", ExpectationBasis: "closing the issue needs GitHub; the egress sandbox is default-deny"},
		{ID: "ac-list", Result: "skipped", ExpectationBasis: "webhook trigger unreachable from the localhost preview"},
	})
	w := shipAcceptanceRequest(t, seam.s, seam.runID, seam.acceptanceID, seam.priv, body, "")
	if w.Code != 201 {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// The stage stays terminal/succeeded — NOT re-opened to pending. This is
	// the anti-wedge regression: fishhawk_audit_complete can clear.
	if got := seam.rr.getStages[seam.acceptanceID].State; got != run.StageStateSucceeded {
		t.Errorf("acceptance state = %q, want unchanged (succeeded) — class 5 must NOT re-open the stage", got)
	}
	for name, id := range map[string]uuid.UUID{"implement": seam.implementID, "review": seam.reviewID} {
		if got := seam.rr.getStages[id].State; got != run.StageStateSucceeded {
			t.Errorf("%s state = %q, want unchanged (succeeded)", name, got)
		}
	}
	if n := countAppendedByCategory(seam.au, CategoryStageFixupTriggered); n != 0 {
		t.Errorf("stage_fixup_triggered entries = %d, want 0 (no re-dispatch on class-5)", n)
	}
	payload := triagePayload(t, seam.au)
	for _, want := range []string{`"class":"5"`, `"disposition":"externally_unvalidatable_paged"`} {
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
