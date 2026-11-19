package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// attnT0 anchors every seeded timestamp so the end-to-end golden is byte-stable.
var attnT0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// attnID mints a fixed, readable UUID from a small integer so the golden
// carries no random identifiers.
func attnID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
}

// attnRunRepo is the run fake with a stage table: fakeRepo's ListStagesForRun
// is unimplemented, and the aggregator's every derivation starts from stages.
type attnRunRepo struct {
	*fakeRepo
	smu      sync.Mutex
	stages   map[uuid.UUID][]*run.Stage
	stageErr map[uuid.UUID]error
}

func newAttnRunRepo() *attnRunRepo {
	return &attnRunRepo{fakeRepo: newFakeRepo(), stages: map[uuid.UUID][]*run.Stage{}, stageErr: map[uuid.UUID]error{}}
}

func (r *attnRunRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.smu.Lock()
	defer r.smu.Unlock()
	if err := r.stageErr[runID]; err != nil {
		return nil, err
	}
	return r.stages[runID], nil
}

// seed inserts a run with fixed identity and timestamps.
func (r *attnRunRepo) seed(id uuid.UUID, repo string, state run.State, created time.Time, title string) *run.Run {
	ru := &run.Run{
		ID: id, Repo: repo, WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: run.TriggerCLI, State: state, CreatedAt: created, UpdatedAt: created,
	}
	if title != "" {
		ru.IssueContext = &run.IssueContext{Title: title}
	}
	r.mu.Lock()
	r.runs[id] = ru
	r.mu.Unlock()
	return ru
}

func (r *attnRunRepo) addStage(runID, stageID uuid.UUID, typ run.StageType, state run.StageState, ended time.Time) *run.Stage {
	st := &run.Stage{ID: stageID, RunID: runID, Type: typ, State: state, EndedAt: &ended}
	r.smu.Lock()
	r.stages[runID] = append(r.stages[runID], st)
	r.smu.Unlock()
	return st
}

// attnFixture bundles the fakes one attention server reads.
type attnFixture struct {
	runs      *attnRunRepo
	audit     *auditFake
	concerns  *fakeConcernRepo
	amends    *fakeScopeAmendmentRepo
	campaigns *fakeCampaignRepo
	artifacts *fakeArtifactRepo
}

func newAttnFixture() *attnFixture {
	return &attnFixture{
		runs: newAttnRunRepo(), audit: newAuditFake(), concerns: newFakeConcernRepo(),
		amends: newFakeScopeAmendmentRepo(), campaigns: newFakeCampaignRepo(), artifacts: newFakeArtifactRepo(),
	}
}

func (f *attnFixture) config() Config {
	return Config{
		Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.audit, ConcernRepo: f.concerns,
		ScopeAmendmentRepo: f.amends, CampaignRepo: f.campaigns, ArtifactRepo: f.artifacts,
	}
}

// attnReader is an operator token carrying the read scope the endpoint requires.
func attnReader(accountID string) Identity {
	return Identity{Subject: "github:op", TokenID: "tok-op", Scopes: []string{scopeGateViewRead}, AccountID: accountID}
}

// getAttention drives the handler directly with an injected identity (the auth
// middleware would re-derive identity from the request).
func getAttention(t *testing.T, s *Server, id Identity, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/attention"+query, nil), id)
	rec := httptest.NewRecorder()
	s.handleListAttention(rec, req)
	return rec
}

func decodeAttention(t *testing.T, rec *httptest.ResponseRecorder) attentionResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got attentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return got
}

func attnKinds(items []attentionItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func attnHasDegraded(resp attentionResponse, reason, runID string) bool {
	for _, d := range resp.Degraded {
		if d.Reason == reason && (runID == "" || d.RunID == runID) {
			return true
		}
	}
	return false
}

// seedPlanGateRun seeds a run parked at its plan gate.
func (f *attnFixture) seedPlanGateRun(id, stageID uuid.UUID, repo string, state run.State, created time.Time) *run.Run {
	ru := f.runs.seed(id, repo, state, created, "plan gate run")
	f.runs.addStage(id, stageID, run.StageTypePlan, run.StageStateAwaitingApproval, created.Add(time.Minute))
	return ru
}

// seedConcernRun seeds a run parked at its implement gate with one open concern.
func (f *attnFixture) seedConcernRun(id, stageID, concernID uuid.UUID, repo string, created time.Time) {
	f.runs.seed(id, repo, run.StateRunning, created, "")
	f.runs.addStage(id, stageID, run.StageTypeImplement, run.StageStateAwaitingApproval, created.Add(time.Minute))
	f.concerns.rows = append(f.concerns.rows, &concern.Concern{
		ID: concernID, RunID: id, StageID: stageID, StageKind: concern.StageKindImplement,
		OriginReviewSequence: 1, Severity: "medium", Category: "correctness",
		Note: "note", State: concern.StateRaised, CreatedAt: created.Add(2 * time.Minute),
	})
}

const attnAcceptanceSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: accept
        type: acceptance
        executor: {agent: claude-code}
        egress:
          target_hosts: ["staging.example.com:8443"]
`

// seedAllSixKinds seeds exactly one item of every kind. Creation timestamps
// are deliberately INVERTED against priority (the lowest-priority kind is the
// oldest) so a sort by `since` alone would produce the reverse order.
func (f *attnFixture) seedAllSixKinds(t *testing.T) {
	t.Helper()
	// Campaign (priority 6) — oldest.
	camp := &campaign.Campaign{
		ID: attnID(60), Repo: "acme/app", EpicRef: "issue:900", State: campaign.StateAwaitingHuman,
		CreatedAt: attnT0, UpdatedAt: attnT0,
	}
	f.campaigns.campaigns[camp.ID] = camp
	f.campaigns.itemsByCmp[camp.ID] = []*campaign.Item{
		{ID: attnID(61), CampaignID: camp.ID, IssueRef: "issue:901", Autonomy: "low", State: campaign.ItemStatePending},
	}

	// Implement-gate run (priorities 4 and 5): one disputed, one plain concern.
	implRun := attnID(40)
	implStage := attnID(41)
	f.runs.seed(implRun, "acme/app", run.StateRunning, attnT0.Add(time.Hour), "Implement gate run")
	f.runs.addStage(implRun, implStage, run.StageTypeImplement, run.StageStateAwaitingApproval, attnT0.Add(time.Hour+time.Minute))
	model := "claude-opus"
	f.concerns.rows = append(f.concerns.rows,
		&concern.Concern{
			ID: attnID(42), RunID: implRun, StageID: implStage, StageKind: concern.StageKindImplement,
			OriginReviewSequence: 1, ReviewerModel: &model, Severity: "high", Category: "correctness",
			Note:  "The retry loop never re-reads the lease, so a stale holder is never displaced.",
			State: concern.StateRaised, CreatedAt: attnT0.Add(2 * time.Hour),
		},
		&concern.Concern{
			ID: attnID(43), RunID: implRun, StageID: implStage, StageKind: concern.StageKindImplement,
			OriginReviewSequence: 1, ReviewerModel: &model, Severity: "low", Category: "test-coverage",
			Note:  "The degraded branch has no test asserting its reason string.",
			State: concern.StateRaised, CreatedAt: attnT0.Add(90 * time.Minute),
		},
	)
	// A second-round review CONFIRMED concern 42 while it is still open: a split.
	payload, _ := json.Marshal(map[string]any{
		"verdict": "approve",
		"concern_resolutions": []map[string]string{
			{"id": attnID(42).String(), "resolution": "confirmed", "note": "fixed by the lease re-read"},
		},
	})
	f.audit.seeded = append(f.audit.seeded, &audit.Entry{
		ID: attnID(44), Sequence: 5, RunID: &implRun, StageID: &implStage, Timestamp: attnT0.Add(3 * time.Hour),
		Category: "implement_reviewed", Payload: payload,
	})

	// Acceptance run (priority 3): newest verdict failed, no arbitration.
	accRun := attnID(30)
	accStage := attnID(31)
	ar := f.runs.seed(accRun, "acme/app", run.StateRunning, attnT0.Add(4*time.Hour), "Acceptance run")
	ar.WorkflowSpec = []byte(attnAcceptanceSpec)
	f.runs.addStage(accRun, accStage, run.StageTypeAcceptance, run.StageStateSucceeded, attnT0.Add(4*time.Hour+time.Minute))
	// The outcome carries a transcript so the card names WHICH criteria failed
	// and each one's decision-relevant explanation (the failing request). A
	// skipped criterion is present to prove only failed criteria surface.
	outcome, _ := json.Marshal(map[string]any{
		"verdict": "failed", "criteria_failed": 2, "criteria_skipped": 1,
		"transcript": map[string]any{
			"artifact_id":  attnID(33).String(),
			"content_hash": "sha256:acc",
			"criteria": []map[string]any{
				{"id": "health-probe", "outcome": "failed", "request_count": 2,
					"failing_request": map[string]any{"method": "GET", "path": "/healthz", "status": 503}},
				{"id": "widget-create", "outcome": "failed", "request_count": 1,
					"failing_request": map[string]any{"method": "POST", "path": "/v0/widgets", "status": 422}},
				{"id": "flaky-metric", "outcome": "skipped", "request_count": 0, "failing_request": nil},
			},
		},
	})
	f.audit.seeded = append(f.audit.seeded, &audit.Entry{
		ID: attnID(32), Sequence: 7, RunID: &accRun, StageID: &accStage, Timestamp: attnT0.Add(5 * time.Hour),
		Category: CategoryAcceptanceOutcomeRecorded, Payload: outcome,
	})

	// Scope amendment (priority 2) on a running run with no parked gate.
	amRun := attnID(20)
	amStage := attnID(21)
	f.runs.seed(amRun, "acme/app", run.StateRunning, attnT0.Add(6*time.Hour), "Amendment run")
	f.runs.addStage(amRun, amStage, run.StageTypeImplement, run.StageStateRunning, attnT0.Add(6*time.Hour))
	f.amends.rows[attnID(22)] = &scopeamendment.Amendment{
		ID: attnID(22), RunID: amRun, StageID: amStage, Status: scopeamendment.StatusPending,
		Reason:      "the registry must list the new audit category",
		Paths:       []scopeamendment.PathEntry{{Path: "backend/internal/audit/categories.go", Operation: "modify"}},
		RequestedAt: attnT0.Add(7 * time.Hour),
	}
	f.amends.order = append(f.amends.order, attnID(22))
	// A decided amendment on the same run must NOT surface.
	f.amends.rows[attnID(23)] = &scopeamendment.Amendment{
		ID: attnID(23), RunID: amRun, StageID: amStage, Status: scopeamendment.StatusApproved,
		Reason: "already decided", RequestedAt: attnT0.Add(6 * time.Hour),
	}
	f.amends.order = append(f.amends.order, attnID(23))
	// A server-check concern (ADR-143 D1 / #5228) on the same run, which has NO
	// parked implement/review gate: it is listed directly as a paged_concern
	// carrying context.provenance=server_check, so the Web UI panel — the only
	// place it can be cleared — reaches it. The SPA's server-check marker tests
	// consume THIS golden item.
	f.concerns.rows = append(f.concerns.rows, &concern.Concern{
		ID: attnID(24), RunID: amRun, StageID: amStage, StageKind: concern.StageKindImplement,
		OriginReviewSequence: 3, Severity: "high", Category: "security",
		Note:       "The diff adds a string that matches a GitHub token pattern (backend/internal/x/fixture.go:12).",
		Provenance: concern.ProvenanceServerCheck,
		State:      concern.StateRaised, CreatedAt: attnT0.Add(6*time.Hour + 30*time.Minute),
	})
	// A pending agent waive proposal on that concern at the run's recorded
	// head (ADR-143 A1, #5229): the item carries context.waive_proposal, and
	// the SPA's decision-panel 'Proposed waive' tests consume THIS golden item.
	f.audit.seeded = append(f.audit.seeded, attnHeadEntry(amRun, 40, "pull_request_opened", attnProposalHead, attnT0.Add(6*time.Hour+10*time.Minute)))
	f.audit.seeded = append(f.audit.seeded, attnWaiveProposalEntry(amRun, amStage, attnID(24), 41, attnProposalHead, attnT0.Add(6*time.Hour+45*time.Minute)))

	// Plan gate (priority 1) — the NEWEST, on a PENDING run.
	planRun := attnID(10)
	planStage := attnID(11)
	f.runs.seed(planRun, "acme/app", run.StatePending, attnT0.Add(8*time.Hour), "Add the attention queue")
	f.runs.addStage(planRun, planStage, run.StageTypePlan, run.StageStateAwaitingApproval, attnT0.Add(9*time.Hour))
	sv := "standard_v1"
	f.artifacts.all = append(f.artifacts.all, &artifact.Artifact{
		ID: attnID(12), StageID: planStage, Kind: artifact.KindPlan, SchemaVersion: &sv,
		Content:   json.RawMessage(`{"summary":"Add GET /v0/attention and make it the SPA home page."}`),
		CreatedAt: attnT0.Add(9 * time.Hour),
	})
	review, _ := json.Marshal(map[string]any{
		"reviewer_kind": "agent", "reviewer_model": "claude-opus", "verdict": "approve",
		"concerns": []map[string]string{{"severity": "low", "note": "n"}},
	})
	f.audit.seeded = append(f.audit.seeded, &audit.Entry{
		ID: attnID(13), Sequence: 9, RunID: &planRun, StageID: &planStage, Timestamp: attnT0.Add(9 * time.Hour),
		Category: "plan_reviewed", Payload: review,
	})
}

// attnGoldenPath resolves the shared wire golden the SPA's tests also read.
func attnGoldenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "attention_list.json")
}

// TestAttention_EndToEnd_AllSixKinds drives the REAL mux (s.Handler(), bearer
// auth included) across all six kinds and compares the serialized body
// byte-for-byte (after indentation) against the shared wire golden
// testdata/wire/attention_list.json — the same file the SPA renders — so a
// field rename on the backend side fails here. Regenerate with
// FISHHAWK_UPDATE_WIRE_GOLDEN=1.
func TestAttention_EndToEnd_AllSixKinds(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	cfg := f.config()
	cfg.APITokenRepo = stubToken(scopeGateViewRead)
	s := New(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/attention", nil)
	req.Header.Set("Authorization", "Bearer fhk_test_export_matrix")
	s.Handler().ServeHTTP(rec, req)
	got := decodeAttention(t, rec)

	want := []string{
		attentionKindPlanGate, attentionKindScopeAmendment, attentionKindAcceptanceDisposition,
		attentionKindSplitVerdict, attentionKindPagedConcern, attentionKindPagedConcern, attentionKindAttendHumanLed,
	}
	if k := attnKinds(got.Items); strings.Join(k, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", k, want)
	}
	// The second paged_concern is the server-check row on the run with no
	// parked gate, carrying its provenance across the wire.
	if sc := got.Items[5]; sc.ConcernID != attnID(24).String() || sc.Context.Provenance != concern.ProvenanceServerCheck {
		t.Errorf("server-check item = %+v, want concern %s with provenance %q", sc, attnID(24), concern.ProvenanceServerCheck)
	}
	// Both the pending (plan gate) and running runs contributed: a scan of only
	// one state would drop the plan_gate or every running-run item.
	if got.ScannedRuns != 4 {
		t.Errorf("scanned_runs = %d, want 4 (pending + running scans merged)", got.ScannedRuns)
	}
	if len(got.Degraded) != 0 || got.Truncated {
		t.Errorf("degraded = %+v truncated = %v, want a complete page", got.Degraded, got.Truncated)
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, bytes.TrimSpace(rec.Body.Bytes()), "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := attnGoldenPath(t)
	if os.Getenv("FISHHAWK_UPDATE_WIRE_GOLDEN") == "1" {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wire golden %s: %v", path, err)
	}
	if !bytes.Equal(pretty.Bytes(), golden) {
		t.Fatalf("GET /v0/attention body drifted from the shared wire golden %s (the SPA renders that file — update both sides deliberately):\n--- got ---\n%s\n--- golden ---\n%s", path, pretty.String(), golden)
	}
}

// TestAttention_PriorityOrdering pins the rank table and the within-kind
// oldest-first tiebreak.
func TestAttention_PriorityOrdering(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	// A second, OLDER plan gate must sort ahead of the seeded one.
	f.seedPlanGateRun(attnID(70), attnID(71), "acme/app", run.StatePending, attnT0.Add(-time.Hour))
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))

	want := []string{
		attentionKindPlanGate, attentionKindPlanGate, attentionKindScopeAmendment,
		attentionKindAcceptanceDisposition, attentionKindSplitVerdict, attentionKindPagedConcern,
		attentionKindPagedConcern, attentionKindAttendHumanLed,
	}
	if k := attnKinds(got.Items); strings.Join(k, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", k, want)
	}
	if got.Items[0].RunID != attnID(70).String() {
		t.Errorf("first plan gate = %s, want the older run %s", got.Items[0].RunID, attnID(70))
	}
	for i, it := range got.Items {
		if it.Priority != attentionPriority[it.Kind] {
			t.Errorf("item %d priority = %d, want %d", i, it.Priority, attentionPriority[it.Kind])
		}
	}
}

func TestAttention_NilRunRepo_503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	rec := getAttention(t, s, attnReader(""), "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "run_repo_unconfigured")
	if strings.Contains(rec.Body.String(), `"items"`) {
		t.Errorf("503 body carries items: %s", rec.Body.String())
	}
}

func TestAttention_RequiresReadScope(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
	s := New(f.config())

	rec := getAttention(t, s, Identity{}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
	rec = getAttention(t, s, Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs"}}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scope-less token status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), attnID(1).String()) {
		t.Errorf("refused body leaks a run id: %s", rec.Body.String())
	}
}

func TestAttention_NilConcernRepo_DegradesVisibly(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	cfg := f.config()
	cfg.ConcernRepo = nil
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))
	for _, it := range got.Items {
		if it.Kind == attentionKindSplitVerdict || it.Kind == attentionKindPagedConcern {
			t.Errorf("concern item emitted with no concern store: %+v", it)
		}
	}
	if !attnHasDegraded(got, attentionDegradedConcernStoreUnconfigured, "") {
		t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedConcernStoreUnconfigured)
	}
	if len(got.Items) != 4 {
		t.Errorf("items = %v, want the other four kinds intact", attnKinds(got.Items))
	}
}

func TestAttention_NilAmendmentRepo_DegradesVisibly(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	cfg := f.config()
	cfg.ScopeAmendmentRepo = nil
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))
	for _, it := range got.Items {
		if it.Kind == attentionKindScopeAmendment {
			t.Errorf("amendment item emitted with no amendment store: %+v", it)
		}
	}
	if !attnHasDegraded(got, attentionDegradedAmendmentStoreUnconfigured, "") {
		t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedAmendmentStoreUnconfigured)
	}
	if len(got.Items) != 6 {
		t.Errorf("items = %v, want the other five kinds intact (six items: two paged concerns)", attnKinds(got.Items))
	}
}

func TestAttention_NilCampaignRepo_DegradesVisibly(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	cfg := f.config()
	cfg.CampaignRepo = nil
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))
	for _, it := range got.Items {
		if it.Kind == attentionKindAttendHumanLed {
			t.Errorf("campaign item emitted with no campaign store: %+v", it)
		}
	}
	if !attnHasDegraded(got, attentionDegradedCampaignStoreUnconfigured, "") {
		t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedCampaignStoreUnconfigured)
	}
	if len(got.Items) != 6 {
		t.Errorf("items = %v, want the other five kinds intact (six items: two paged concerns)", attnKinds(got.Items))
	}
}

func TestAttention_CampaignReadErrors_DegradeVisibly(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		f := newAttnFixture()
		f.seedAllSixKinds(t)
		f.campaigns.listErr = errors.New("campaign db down")
		got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
		if !attnHasDegraded(got, attentionDegradedCampaignReadFailed, "") {
			t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedCampaignReadFailed)
		}
		if len(got.Items) != 6 {
			t.Errorf("items = %v, want the five run kinds intact (six items: two paged concerns)", attnKinds(got.Items))
		}
	})
	t.Run("items error keeps the campaign item", func(t *testing.T) {
		f := newAttnFixture()
		f.seedAllSixKinds(t)
		f.campaigns.itemsErr = errors.New("items db down")
		got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
		var camp *attentionItem
		for i := range got.Items {
			if got.Items[i].Kind == attentionKindAttendHumanLed {
				camp = &got.Items[i]
			}
		}
		if camp == nil {
			t.Fatalf("campaign item dropped on an items read error: %v", attnKinds(got.Items))
		}
		if len(camp.Context.HumanLedRefs) != 0 {
			t.Errorf("human_led_refs = %v, want none (items unreadable)", camp.Context.HumanLedRefs)
		}
		found := false
		for _, d := range got.Degraded {
			if d.Reason == attentionDegradedCampaignItemsUnreadable && d.CampaignID == attnID(60).String() {
				found = true
			}
		}
		if !found {
			t.Errorf("degraded = %+v, want %s naming the campaign", got.Degraded, attentionDegradedCampaignItemsUnreadable)
		}
	})
}

// TestAttention_CampaignScanTruncates is binding condition 1's backend half:
// a bite of attentionCampaignScanLimit is VISIBLE.
func TestAttention_CampaignScanTruncates(t *testing.T) {
	f := newAttnFixture()
	for i := 0; i < attentionCampaignScanLimit+1; i++ {
		c := &campaign.Campaign{
			ID: attnID(1000 + i), Repo: "acme/app", EpicRef: fmt.Sprintf("issue:%d", 1000+i),
			State: campaign.StateAwaitingHuman, CreatedAt: attnT0.Add(time.Duration(i) * time.Minute),
			UpdatedAt: attnT0.Add(time.Duration(i) * time.Minute),
		}
		f.campaigns.campaigns[c.ID] = c
	}
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if !attnHasDegraded(got, attentionDegradedCampaignScanTruncated, "") {
		t.Fatalf("degraded = %+v, want %s", got.Degraded, attentionDegradedCampaignScanTruncated)
	}
	if len(got.Items) != attentionCampaignScanLimit {
		t.Errorf("items = %d, want %d (the cap)", len(got.Items), attentionCampaignScanLimit)
	}
}

func TestAttention_StageReadError_DegradesOneRun(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
	f.seedPlanGateRun(attnID(3), attnID(4), "acme/app", run.StatePending, attnT0.Add(time.Minute))
	f.runs.stageErr[attnID(3)] = errors.New("stages unreadable")
	// A pending amendment on the erroring run isolates the skip: it needs no
	// stages, so it surfaces unless the run is skipped whole.
	f.amends.rows[attnID(5)] = &scopeamendment.Amendment{
		ID: attnID(5), RunID: attnID(3), StageID: attnID(4), Status: scopeamendment.StatusPending,
		Reason: "r", RequestedAt: attnT0,
	}
	f.amends.order = append(f.amends.order, attnID(5))
	// An open server-check concern on the erroring run pins a NAMED residual
	// state (ADR-143 D1 / #5228): stage_read_failed skips the run before the
	// concern read, so the concern is not reachable from the panel this read.
	f.concerns.rows = append(f.concerns.rows, attnServerCheckRow(attnID(6), attnID(3), attnID(4), attnT0))
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() || got.Items[0].Kind != attentionKindPlanGate {
		t.Fatalf("items = %+v, want only the healthy run's plan_gate (the stage-unreadable run is skipped whole)", got.Items)
	}
	if !attnHasDegraded(got, attentionDegradedStageReadFailed, attnID(3).String()) {
		t.Errorf("degraded = %+v, want %s naming the erroring run", got.Degraded, attentionDegradedStageReadFailed)
	}
}

func TestAttention_AcceptanceGateStateError_DegradesOneRun(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	f.audit.listByCategoryErrCategory = CategoryAcceptanceOutcomeRecorded
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	for _, it := range got.Items {
		if it.Kind == attentionKindAcceptanceDisposition {
			t.Errorf("acceptance item emitted on an unreadable outcome: %+v", it)
		}
	}
	if !attnHasDegraded(got, attentionDegradedAcceptanceUnreadable, attnID(30).String()) {
		t.Errorf("degraded = %+v, want %s naming the acceptance run", got.Degraded, attentionDegradedAcceptanceUnreadable)
	}
	if len(got.Items) != 6 {
		t.Errorf("items = %v, want the other five kinds intact (six items: two paged concerns)", attnKinds(got.Items))
	}
}

// attnNthCategoryFailAudit fails the Nth ListForRunByCategory call for ONE
// category and serves every other read from the wrapped fake. It lets a test
// make the SECOND acceptance-outcome read (acceptanceItem's) fail while the
// FIRST (acceptanceGateState's, which parks the item at triage) succeeds — the
// two-consecutive-reads window concern (3) names.
type attnNthCategoryFailAudit struct {
	*auditFake
	cmu      sync.Mutex
	category string
	failCall int // 1-based ordinal of the matching call to fail
	seen     int
}

func (a *attnNthCategoryFailAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == a.category {
		a.cmu.Lock()
		a.seen++
		n := a.seen
		a.cmu.Unlock()
		if n == a.failCall {
			return nil, errors.New("attn: injected read failure #" + fmt.Sprint(n) + " for " + category)
		}
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestAttention_AcceptanceSecondReadError_ThinsAndDegrades pins concern (2/3):
// when acceptanceGateState reads the outcome successfully (parking the item at
// triage) but the acceptanceItem RE-READ fails, the item is still emitted with a
// thinned context (no verdict, no criteria, no failed_criteria) AND the thinning
// is named in degraded[] as acceptance_state_unreadable — never swallowed.
func TestAttention_AcceptanceSecondReadError_ThinsAndDegrades(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	cfg := f.config()
	cfg.AuditRepo = &attnNthCategoryFailAudit{auditFake: f.audit, category: CategoryAcceptanceOutcomeRecorded, failCall: 2}
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))

	var acc *attentionItem
	for i := range got.Items {
		if got.Items[i].Kind == attentionKindAcceptanceDisposition {
			acc = &got.Items[i]
		}
	}
	if acc == nil {
		t.Fatalf("acceptance item dropped; want it KEPT with thin context. kinds=%v", attnKinds(got.Items))
	}
	if acc.Context.Verdict != "" || acc.Context.CriteriaFailed != nil || len(acc.Context.FailedCriteria) != 0 {
		t.Errorf("context = %+v, want thinned (no verdict/criteria/failed_criteria) on the second-read failure", acc.Context)
	}
	if !attnHasDegraded(got, attentionDegradedAcceptanceUnreadable, attnID(30).String()) {
		t.Errorf("degraded = %+v, want %s naming the acceptance run's thinned context", got.Degraded, attentionDegradedAcceptanceUnreadable)
	}
}

// TestAttention_PlanGateNilAudit_DegradesReviews pins concern (3): with no audit
// store the plan-gate item cannot carry its review verdicts, so the missing
// contribution is named in degraded[] (plan_reviews_unreadable) rather than
// dropped silently — and the item is still shown with its summary.
func TestAttention_PlanGateNilAudit_DegradesReviews(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
	cfg := f.config()
	cfg.AuditRepo = nil
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))
	if len(got.Items) != 1 || got.Items[0].Kind != attentionKindPlanGate {
		t.Fatalf("items = %+v, want the plan gate kept", got.Items)
	}
	if len(got.Items[0].Context.ReviewVerdicts) != 0 {
		t.Errorf("review_verdicts = %+v, want none with no audit store", got.Items[0].Context.ReviewVerdicts)
	}
	if !attnHasDegraded(got, attentionDegradedPlanReviewsUnreadable, attnID(1).String()) {
		t.Errorf("degraded = %+v, want %s naming the plan-gate run", got.Degraded, attentionDegradedPlanReviewsUnreadable)
	}
}

func TestAttention_AmendmentAndConcernReadErrors_DegradeOneRun(t *testing.T) {
	t.Run("amendment list error", func(t *testing.T) {
		f := newAttnFixture()
		f.seedAllSixKinds(t)
		f.amends.failListOn = 1
		got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
		if !attnHasDegraded(got, attentionDegradedAmendmentReadFailed, "") {
			t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedAmendmentReadFailed)
		}
	})
	t.Run("concern list error", func(t *testing.T) {
		f := newAttnFixture()
		f.seedAllSixKinds(t)
		f.concerns.listErr = errors.New("concern db down")
		got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
		if !attnHasDegraded(got, attentionDegradedConcernReadFailed, attnID(40).String()) {
			t.Errorf("degraded = %+v, want %s naming the implement-gate run", got.Degraded, attentionDegradedConcernReadFailed)
		}
		if len(got.Items) != 4 {
			t.Errorf("items = %v, want the four non-concern kinds", attnKinds(got.Items))
		}
	})
}

func TestAttention_PlanContextReadErrors_KeepTheItem(t *testing.T) {
	t.Run("artifact store unconfigured", func(t *testing.T) {
		f := newAttnFixture()
		f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
		cfg := f.config()
		cfg.ArtifactRepo = nil
		got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), ""))
		if len(got.Items) != 1 || got.Items[0].Kind != attentionKindPlanGate {
			t.Fatalf("items = %+v, want the plan gate kept", got.Items)
		}
		if !attnHasDegraded(got, attentionDegradedPlanSummaryUnavailable, attnID(1).String()) {
			t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedPlanSummaryUnavailable)
		}
	})
	t.Run("plan review read error", func(t *testing.T) {
		f := newAttnFixture()
		f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
		f.audit.listByCategoryErrCategory = "plan_reviewed"
		got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
		if len(got.Items) != 1 || got.Items[0].Kind != attentionKindPlanGate {
			t.Fatalf("items = %+v, want the plan gate kept", got.Items)
		}
		if !attnHasDegraded(got, attentionDegradedPlanReviewsUnreadable, attnID(1).String()) {
			t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedPlanReviewsUnreadable)
		}
	})
}

func TestAttention_GateViewHistoryIncomplete_SetsDegraded(t *testing.T) {
	f := newAttnFixture()
	f.seedConcernRun(attnID(1), attnID(2), attnID(3), "acme/app", attnT0)
	f.audit.listByCategoryErrCategory = "fixup_pushed"
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if len(got.Items) != 1 || got.Items[0].Kind != attentionKindPagedConcern {
		t.Fatalf("items = %+v, want the concern kept", got.Items)
	}
	if !attnHasDegraded(got, attentionDegradedGateViewHistoryIncomplete, attnID(1).String()) {
		t.Errorf("degraded = %+v, want %s", got.Degraded, attentionDegradedGateViewHistoryIncomplete)
	}
}

func TestAttention_LimitInvalid_400(t *testing.T) {
	f := newAttnFixture()
	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=501"} {
		rec := getAttention(t, New(f.config()), attnReader(""), q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestAttention_LimitCutsAndFlagsTruncated(t *testing.T) {
	f := newAttnFixture()
	f.seedAllSixKinds(t)
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), "?limit=2"))
	if len(got.Items) != 2 || !got.Truncated {
		t.Fatalf("items = %v truncated = %v, want 2 + truncated", attnKinds(got.Items), got.Truncated)
	}
	if got.Items[0].Kind != attentionKindPlanGate {
		t.Errorf("first = %s, want the highest-priority item kept", got.Items[0].Kind)
	}
}

// fakeErrOnRepoVisibility allows `allowed` and errors on `failing`: the second
// row errors only AFTER the first already matched.
type fakeErrOnRepoVisibility struct {
	allowed, failing string
}

func (v fakeErrOnRepoVisibility) Visible(_ context.Context, _, _, repo string) (bool, error) {
	if repo == v.failing {
		return false, errors.New("mirror down")
	}
	return repo == v.allowed, nil
}
func (fakeErrOnRepoVisibility) InvalidateSubject(context.Context, string, string) error { return nil }

func attnMemberServer(f *attnFixture, vis RepoVisibility) *Server {
	cfg := f.config()
	cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
	cfg.RepoVisibility = vis
	return New(cfg)
}

// TestAttention_RepoFilterNarrowsItems — counterfactual (a): deleting the
// filter.allows call makes beta/two's item appear.
func TestAttention_RepoFilterNarrowsItems(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "alpha/one", run.StatePending, attnT0)
	f.seedPlanGateRun(attnID(3), attnID(4), "beta/two", run.StatePending, attnT0.Add(time.Minute))
	f.campaigns.campaigns[attnID(5)] = &campaign.Campaign{ID: attnID(5), Repo: "beta/two", State: campaign.StateAwaitingHuman}
	s := attnMemberServer(f, newFakeRepoVisibility(map[string]bool{"alpha/one": true}))
	got := decodeAttention(t, getAttention(t, s, memberIdentity(), ""))
	if len(got.Items) != 1 || got.Items[0].Repo != "alpha/one" {
		t.Fatalf("items = %+v, want exactly alpha/one's plan gate", got.Items)
	}
}

// TestAttention_AccountFilterNarrowsItems — counterfactual (b): dropping the
// caller's AccountID from the scan makes account B's run appear.
func TestAttention_AccountFilterNarrowsItems(t *testing.T) {
	f := newAttnFixture()
	acctA, acctB := uuid.NewString(), uuid.NewString()
	ra := f.seedPlanGateRun(attnID(1), attnID(2), "acme/app", run.StatePending, attnT0)
	ra.AccountID = acctA
	rb := f.seedPlanGateRun(attnID(3), attnID(4), "acme/app", run.StatePending, attnT0.Add(time.Minute))
	rb.AccountID = acctB
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(acctA), ""))
	if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() {
		t.Fatalf("items = %+v, want only account A's run", got.Items)
	}
	if f.runs.lastListFilter.AccountID != acctA {
		t.Errorf("ListRuns AccountID = %q, want %q", f.runs.lastListFilter.AccountID, acctA)
	}
}

// TestAttention_RepoFilterErrorFailsClosed — counterfactual (c): the mirror
// errors on the SECOND row after the first already matched; the whole request
// must 503 rather than emit the first row's item.
func TestAttention_RepoFilterErrorFailsClosed(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "alpha/one", run.StatePending, attnT0)
	f.seedPlanGateRun(attnID(3), attnID(4), "beta/two", run.StatePending, attnT0.Add(time.Minute))
	s := attnMemberServer(f, fakeErrOnRepoVisibility{allowed: "alpha/one", failing: "beta/two"})
	rec := getAttention(t, s, memberIdentity(), "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "service_unavailable")
	if strings.Contains(rec.Body.String(), attnID(1).String()) || strings.Contains(rec.Body.String(), `"items"`) {
		t.Errorf("fail-closed body leaks a partially-narrowed page: %s", rec.Body.String())
	}
}

// TestAttention_CampaignRepoFilterErrorFailsClosed: the campaign scan applies
// the same fail-closed filter.
func TestAttention_CampaignRepoFilterErrorFailsClosed(t *testing.T) {
	f := newAttnFixture()
	f.seedPlanGateRun(attnID(1), attnID(2), "alpha/one", run.StatePending, attnT0)
	f.campaigns.campaigns[attnID(5)] = &campaign.Campaign{ID: attnID(5), Repo: "beta/two", State: campaign.StateAwaitingHuman}
	s := attnMemberServer(f, fakeErrOnRepoVisibility{allowed: "alpha/one", failing: "beta/two"})
	rec := getAttention(t, s, memberIdentity(), "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), attnID(1).String()) {
		t.Errorf("fail-closed body leaks a partially-narrowed page: %s", rec.Body.String())
	}
}

// TestAttention_ScanLimitTruncates — counterfactual (d).
func TestAttention_ScanLimitTruncates(t *testing.T) {
	f := newAttnFixture()
	for i := 0; i < attentionRunScanLimit+5; i++ {
		f.seedPlanGateRun(attnID(2000+2*i), attnID(2001+2*i), "acme/app", run.StatePending, attnT0.Add(time.Duration(i)*time.Minute))
	}
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), "?limit=500"))
	if len(got.Items) != attentionRunScanLimit || !got.Truncated {
		t.Fatalf("items = %d truncated = %v, want %d + truncated", len(got.Items), got.Truncated, attentionRunScanLimit)
	}
	if got.ScannedRuns != attentionRunScanLimit {
		t.Errorf("scanned_runs = %d, want %d", got.ScannedRuns, attentionRunScanLimit)
	}
}

// TestAttention_GateViewBudgetTruncates — counterfactual (e).
func TestAttention_GateViewBudgetTruncates(t *testing.T) {
	f := newAttnFixture()
	for i := 0; i < attentionGateViewBudget+3; i++ {
		f.seedConcernRun(attnID(3000+3*i), attnID(3001+3*i), attnID(3002+3*i), "acme/app", attnT0.Add(time.Duration(i)*time.Minute))
	}
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if len(got.Items) != attentionGateViewBudget {
		t.Errorf("items = %d, want %d (the budget)", len(got.Items), attentionGateViewBudget)
	}
	if !attnHasDegraded(got, attentionDegradedGateViewBudgetExhausted, "") {
		t.Fatalf("degraded = %+v, want %s", got.Degraded, attentionDegradedGateViewBudgetExhausted)
	}
}

// TestAttention_RejectsNonGET — counterfactual (f): registering the route
// without the GET method prefix lets a POST reach the handler.
func TestAttention_RejectsNonGET(t *testing.T) {
	f := newAttnFixture()
	cfg := f.config()
	cfg.APITokenRepo = stubToken(scopeGateViewRead)
	s := New(cfg)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, "/v0/attention", nil)
		req.Header.Set("Authorization", "Bearer fhk_test_export_matrix")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", m, rec.Code)
		}
	}
}

// TestAttention_EmptyQueue pins the empty-but-complete shape: items and
// degraded are [] (never null) so a client can tell "nothing needs you" from a
// missing field.
func TestAttention_EmptyQueue(t *testing.T) {
	f := newAttnFixture()
	rec := getAttention(t, New(f.config()), attnReader(""), "")
	got := decodeAttention(t, rec)
	if len(got.Items) != 0 || len(got.Degraded) != 0 || got.Truncated {
		t.Fatalf("got %+v, want an empty complete queue", got)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) || !strings.Contains(rec.Body.String(), `"degraded":[]`) {
		t.Errorf("body = %s, want items/degraded as [] not null", rec.Body.String())
	}
}

// attnServerCheckRow is an open concern a deterministic server check raised
// (provenance server_check) — the class ADR-143 D1 lets only a Web UI session
// clear.
func attnServerCheckRow(id, runID, stageID uuid.UUID, created time.Time) *concern.Concern {
	return &concern.Concern{
		ID: id, RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
		OriginReviewSequence: 2, Severity: "high", Category: "security",
		Note: "server check: secret-shaped string in the diff", NewEvidence: "fixture.go:12",
		Provenance: concern.ProvenanceServerCheck, State: concern.StateRaised, CreatedAt: created,
	}
}

// attnProposalHead is the recorded run head the attention fixtures' waive
// proposals bind to.
const attnProposalHead = "5229a5229a5229a5229a5229a5229a5229a5229a"

// attnHeadEntry is a seeded head-recording chain row naming head.
func attnHeadEntry(runID uuid.UUID, seq int64, category, head string, at time.Time) *audit.Entry {
	rid := runID
	payload, _ := json.Marshal(map[string]any{"head_sha": head})
	return &audit.Entry{RunID: &rid, Sequence: seq, Timestamp: at, Category: category, Payload: payload}
}

// attnWaiveProposalEntry is a seeded concern_waive_proposed row for concernID
// at head, proposed by an operator agent (ADR-143 A1, #5229).
func attnWaiveProposalEntry(runID, stageID, concernID uuid.UUID, seq int64, head string, at time.Time) *audit.Entry {
	rid, sid := runID, stageID
	kind := audit.ActorAgent
	subject := "operator-agent/1.0"
	payload, _ := json.Marshal(map[string]any{
		"concern_id":    concernID.String(),
		"stage_id":      stageID.String(),
		"stage_kind":    concern.StageKindImplement,
		"provenance":    concern.ProvenanceServerCheck,
		"check_key":     "diff_secrets|github-pat-classic|backend/internal/x/fixture.go",
		"category":      "security",
		"severity":      "high",
		"reason":        "The matched string is the documented placeholder from the README, not a live token.",
		"judgement":     "tradeoff",
		"tradeoff_cost": "none: the fixture never reaches a forge",
		"evidence":      "fixture.go:12 assigns the README example token; the forge refuses it.",
		"head_sha":      head,
	})
	return &audit.Entry{
		RunID: &rid, StageID: &sid, Sequence: seq, Timestamp: at, Category: CategoryConcernWaiveProposed,
		ActorKind: &kind, ActorSubject: &subject, Payload: payload,
	}
}

// attnReviewerRow is an open reviewer-raised concern (no provenance).
func attnReviewerRow(id, runID, stageID uuid.UUID, created time.Time) *concern.Concern {
	model := "claude-opus"
	return &concern.Concern{
		ID: id, RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
		OriginReviewSequence: 1, ReviewerModel: &model, Severity: "medium", Category: "correctness",
		Note: "reviewer note", State: concern.StateRaised, CreatedAt: created,
	}
}

func attnFindConcernItem(items []attentionItem, concernID uuid.UUID) *attentionItem {
	for i := range items {
		if items[i].ConcernID == concernID.String() {
			return &items[i]
		}
	}
	return nil
}

// TestAttention_ServerCheckConcern_ListedWithoutParkedGate pins ADR-143 D1's
// reachability half (#5228): a server-check concern can be cleared only from
// the Web UI session, so it is listed on a scanned run with NO parked
// implement/review gate, while a reviewer-raised concern on the same run keeps
// the parked-gate rule and is NOT listed. A terminal run is a named residual
// state: the scan reads pending and running runs only.
//
// COUNTERFACTUAL (8), run by hand: deleting the collectServerCheckConcerns
// call at the end of collectRunAttention lists no item for the running run →
// RED ("items = [], want exactly the server-check concern").
func TestAttention_ServerCheckConcern_ListedWithoutParkedGate(t *testing.T) {
	f := newAttnFixture()
	runID, stageID := attnID(1), attnID(2)
	f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "Running run")
	f.runs.addStage(runID, stageID, run.StageTypeImplement, run.StageStateRunning, attnT0)
	f.concerns.rows = append(f.concerns.rows,
		attnServerCheckRow(attnID(3), runID, stageID, attnT0.Add(time.Minute)),
		attnReviewerRow(attnID(4), runID, stageID, attnT0.Add(2*time.Minute)),
	)
	// A terminal run's open server-check concern is out of the scan.
	doneID, doneStage := attnID(5), attnID(6)
	f.runs.seed(doneID, "acme/app", run.StateSucceeded, attnT0, "Done run")
	f.runs.addStage(doneID, doneStage, run.StageTypeImplement, run.StageStateSucceeded, attnT0)
	f.concerns.rows = append(f.concerns.rows, attnServerCheckRow(attnID(7), doneID, doneStage, attnT0))

	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want exactly the server-check concern (reviewer row and terminal run not listed)", got.Items)
	}
	it := got.Items[0]
	wantPath := "/runs/" + runID.String() + "/stages/" + stageID.String()
	if it.Kind != attentionKindPagedConcern || it.ConcernID != attnID(3).String() || it.ID != attnID(3).String() ||
		it.StageID != stageID.String() || it.DetailPath != wantPath || !it.Since.Equal(attnT0.Add(time.Minute)) {
		t.Errorf("item = %+v, want paged_concern %s on stage %s at %s", it, attnID(3), stageID, wantPath)
	}
	c := it.Context
	if c.Provenance != concern.ProvenanceServerCheck || c.StageKind != concern.StageKindImplement ||
		c.Severity != "high" || c.Category != "security" || c.Note == "" || c.NewEvidence != "fixture.go:12" {
		t.Errorf("context = %+v, want the row's stage_kind/severity/category/note/new_evidence and provenance server_check", c)
	}
	if attnFindConcernItem(got.Items, attnID(4)) != nil {
		t.Errorf("reviewer-raised concern listed without a parked gate: %+v", got.Items)
	}
	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %+v, want none", got.Degraded)
	}
}

// TestAttention_ServerCheckConcern_ListedPastGateViewBudget: a run parked at
// its implement gate but PAST the gate-view budget still lists its open
// server-check concern (no gate view), and the over-budget degraded count
// names only the runs that lost reviewer-raised concerns.
//
// COUNTERFACTUAL (9), run by hand: returning after *overBudget++ (no
// past-budget listing) drops the server-check row → RED ("server-check
// concern past the gate-view budget not listed").
func TestAttention_ServerCheckConcern_ListedPastGateViewBudget(t *testing.T) {
	f := newAttnFixture()
	for i := 0; i < attentionGateViewBudget+1; i++ {
		f.seedConcernRun(attnID(3000+3*i), attnID(3001+3*i), attnID(3002+3*i), "acme/app", attnT0.Add(time.Duration(i)*time.Minute))
	}
	scRun, scStage := attnID(9000), attnID(9001)
	f.runs.seed(scRun, "acme/app", run.StateRunning, attnT0.Add(time.Hour), "")
	f.runs.addStage(scRun, scStage, run.StageTypeImplement, run.StageStateAwaitingApproval, attnT0.Add(time.Hour))
	f.concerns.rows = append(f.concerns.rows, attnServerCheckRow(attnID(9002), scRun, scStage, attnT0.Add(time.Hour)))

	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	it := attnFindConcernItem(got.Items, attnID(9002))
	if it == nil {
		t.Fatalf("server-check concern past the gate-view budget not listed: %v", attnKinds(got.Items))
	}
	if it.Kind != attentionKindPagedConcern || it.Context.Provenance != concern.ProvenanceServerCheck || it.StageID != scStage.String() {
		t.Errorf("item = %+v, want a server_check paged_concern on stage %s", it, scStage)
	}
	if len(got.Items) != attentionGateViewBudget+1 {
		t.Errorf("items = %d, want %d (budget's reviewer concerns + the server-check one)", len(got.Items), attentionGateViewBudget+1)
	}
	var detail string
	for _, d := range got.Degraded {
		if d.Reason == attentionDegradedGateViewBudgetExhausted {
			detail = d.Detail
		}
	}
	if !strings.HasPrefix(detail, "1 run(s)") {
		t.Errorf("gate_view_budget_exhausted detail = %q, want it to count only the one reviewer-concern run past the budget", detail)
	}
}

// TestAttention_GateViewPath_StampsProvenance: on the parked-gate gate-view
// path each item's context carries the open row's provenance, so a
// server-check concern is marked and a reviewer one is not.
//
// COUNTERFACTUAL (10), run by hand: dropping `Provenance: provenance[c.ID]`
// from collectGateViewConcerns leaves context.provenance absent → RED.
func TestAttention_GateViewPath_StampsProvenance(t *testing.T) {
	f := newAttnFixture()
	runID, stageID := attnID(1), attnID(2)
	f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
	f.runs.addStage(runID, stageID, run.StageTypeImplement, run.StageStateAwaitingApproval, attnT0)
	f.concerns.rows = append(f.concerns.rows,
		attnServerCheckRow(attnID(3), runID, stageID, attnT0.Add(time.Minute)),
		attnReviewerRow(attnID(4), runID, stageID, attnT0.Add(2*time.Minute)),
	)
	got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
	if len(got.Items) != 2 {
		t.Fatalf("items = %+v, want both open concerns via the gate view", got.Items)
	}
	sc, rv := attnFindConcernItem(got.Items, attnID(3)), attnFindConcernItem(got.Items, attnID(4))
	if sc == nil || sc.Context.Provenance != concern.ProvenanceServerCheck {
		t.Errorf("server-check item = %+v, want provenance %q", sc, concern.ProvenanceServerCheck)
	}
	if rv == nil || rv.Context.Provenance != "" {
		t.Errorf("reviewer item = %+v, want no provenance", rv)
	}
}

// attnCountingConcernRepo counts ListOpenByRun calls.
type attnCountingConcernRepo struct {
	*fakeConcernRepo
	cmu   sync.Mutex
	calls int
}

func (r *attnCountingConcernRepo) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	r.cmu.Lock()
	r.calls++
	r.cmu.Unlock()
	return r.fakeConcernRepo.ListOpenByRun(ctx, runID)
}

// TestAttention_ScanCapsHoldWithPerRunConcernRead (#5228 approval condition
// C9): the concern read now runs for every scanned run, and both caps still
// bite — the run scan stops at attentionRunScanLimit (one ListOpenByRun per
// scanned run, none past the cap) and ?limit cuts the item list.
func TestAttention_ScanCapsHoldWithPerRunConcernRead(t *testing.T) {
	seed := func() (*attnFixture, *attnCountingConcernRepo) {
		f := newAttnFixture()
		for i := 0; i < attentionRunScanLimit+5; i++ {
			runID, stageID := attnID(4000+3*i), attnID(4001+3*i)
			at := attnT0.Add(time.Duration(i) * time.Minute)
			f.runs.seed(runID, "acme/app", run.StateRunning, at, "")
			f.runs.addStage(runID, stageID, run.StageTypeImplement, run.StageStateRunning, at)
			f.concerns.rows = append(f.concerns.rows, attnServerCheckRow(attnID(4002+3*i), runID, stageID, at))
		}
		return f, &attnCountingConcernRepo{fakeConcernRepo: f.concerns}
	}

	f, counting := seed()
	cfg := f.config()
	cfg.ConcernRepo = counting
	got := decodeAttention(t, getAttention(t, New(cfg), attnReader(""), "?limit=500"))
	if got.ScannedRuns != attentionRunScanLimit || !got.Truncated {
		t.Fatalf("scanned_runs = %d truncated = %v, want %d + truncated", got.ScannedRuns, got.Truncated, attentionRunScanLimit)
	}
	if len(got.Items) != attentionRunScanLimit {
		t.Errorf("items = %d, want %d (one server-check concern per scanned run)", len(got.Items), attentionRunScanLimit)
	}
	if counting.calls != attentionRunScanLimit {
		t.Errorf("ListOpenByRun calls = %d, want %d (one per scanned run, none past the scan cap)", counting.calls, attentionRunScanLimit)
	}

	f, counting = seed()
	cfg = f.config()
	cfg.ConcernRepo = counting
	got = decodeAttention(t, getAttention(t, New(cfg), attnReader(""), "?limit=10"))
	if len(got.Items) != 10 || !got.Truncated {
		t.Errorf("items = %d truncated = %v, want 10 + truncated (?limit cuts the list)", len(got.Items), got.Truncated)
	}
}

// TestAttention_ServerCheckCarriesWaiveProposal pins criterion
// attention-item-carries-waive-proposal (ADR-143 A1, #5229): a server-check
// paged_concern carries context.waive_proposal on BOTH server-check paths,
// the parked-gate gate view and the direct listing, and a reviewer-raised row
// and a stale proposal carry none.
func TestAttention_ServerCheckCarriesWaiveProposal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stageState run.StageState
	}{
		{"parked_gate_view", run.StageStateAwaitingApproval},
		{"direct", run.StageStateRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttnFixture()
			runID, stageID := attnID(1), attnID(2)
			f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
			f.runs.addStage(runID, stageID, run.StageTypeImplement, tc.stageState, attnT0)
			f.concerns.rows = append(f.concerns.rows,
				attnServerCheckRow(attnID(3), runID, stageID, attnT0.Add(time.Minute)),
				attnServerCheckRow(attnID(5), runID, stageID, attnT0.Add(time.Minute)),
				attnReviewerRow(attnID(4), runID, stageID, attnT0.Add(2*time.Minute)),
			)
			f.audit.seeded = append(f.audit.seeded,
				attnHeadEntry(runID, 10, "pull_request_opened", attnProposalHead, attnT0),
				attnWaiveProposalEntry(runID, stageID, attnID(3), 11, attnProposalHead, attnT0.Add(3*time.Minute)),
				// A proposal at an older head is stale: pending nowhere.
				attnWaiveProposalEntry(runID, stageID, attnID(5), 12, "0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0", attnT0.Add(3*time.Minute)),
			)
			got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
			sc := attnFindConcernItem(got.Items, attnID(3))
			if sc == nil || sc.Context.WaiveProposal == nil {
				t.Fatalf("server-check item = %+v, want context.waive_proposal", sc)
			}
			p := sc.Context.WaiveProposal
			if p.ProposalSequence != 11 || p.HeadSHA != attnProposalHead || p.ProposedBy != "operator-agent/1.0" ||
				p.ProposedByActorKind != string(audit.ActorAgent) || !p.AgentAuthored || p.Reason == "" || p.Evidence == "" ||
				p.CheckKey == "" || p.ConcernID != attnID(3).String() {
				t.Errorf("waive_proposal = %+v, want sequence 11 at the recorded head, agent-authored", p)
			}
			if stale := attnFindConcernItem(got.Items, attnID(5)); stale == nil || stale.Context.WaiveProposal != nil {
				t.Errorf("stale-proposal item = %+v, want listed with no waive_proposal", stale)
			}
			if rv := attnFindConcernItem(got.Items, attnID(4)); rv != nil && rv.Context.WaiveProposal != nil {
				t.Errorf("reviewer item carries a waive_proposal: %+v", rv)
			}
			if len(got.Degraded) != 0 {
				t.Errorf("degraded = %+v, want none", got.Degraded)
			}
		})
	}
}

// TestAttention_ServerCheckCarriesWaiveProposal_ReadFailureOmits: when the
// proposal fold's read fails, the item is still listed but carries NO
// waive_proposal, so the panel never offers Confirm on an unreadable state.
// On the gate-view path the failure also surfaces as a
// gate_view_history_incomplete degraded entry naming waive_proposals.
func TestAttention_ServerCheckCarriesWaiveProposal_ReadFailureOmits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stageState run.StageState
		wantGap    bool
	}{
		{"parked_gate_view", run.StageStateAwaitingApproval, true},
		{"direct", run.StageStateRunning, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttnFixture()
			runID, stageID := attnID(1), attnID(2)
			f.runs.seed(runID, "acme/app", run.StateRunning, attnT0, "")
			f.runs.addStage(runID, stageID, run.StageTypeImplement, tc.stageState, attnT0)
			f.concerns.rows = append(f.concerns.rows, attnServerCheckRow(attnID(3), runID, stageID, attnT0.Add(time.Minute)))
			f.audit.seeded = append(f.audit.seeded,
				attnHeadEntry(runID, 10, "pull_request_opened", attnProposalHead, attnT0),
				attnWaiveProposalEntry(runID, stageID, attnID(3), 11, attnProposalHead, attnT0.Add(3*time.Minute)),
			)
			f.audit.listByCategoryErrCategory = CategoryConcernWaiveProposed
			got := decodeAttention(t, getAttention(t, New(f.config()), attnReader(""), ""))
			sc := attnFindConcernItem(got.Items, attnID(3))
			if sc == nil {
				t.Fatalf("server-check item not listed on a proposal read failure: %v", attnKinds(got.Items))
			}
			if sc.Context.WaiveProposal != nil {
				t.Errorf("waive_proposal = %+v, want omitted on a read failure", sc.Context.WaiveProposal)
			}
			gap := false
			for _, d := range got.Degraded {
				if d.Reason == attentionDegradedGateViewHistoryIncomplete && strings.Contains(d.Detail, gateViewGapWaiveProposals) {
					gap = true
				}
			}
			if gap != tc.wantGap {
				t.Errorf("waive_proposals degraded entry = %v, want %v (degraded %+v)", gap, tc.wantGap, got.Degraded)
			}
		})
	}
}
