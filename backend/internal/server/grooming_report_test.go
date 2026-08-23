package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- E54.3 / #2235: grooming_report ingest -----------------------------------

// newGroomingServer wires the plan-upload repos with a stage whose Type is
// `plan` — the PROPOSE stage a grooming report may legitimately ship from.
// Callers needing the negative case pass a different stage type.
func newGroomingServer(t *testing.T, runID, stageID uuid.UUID, stageType run.StageType) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	s, sf, ar, au, rr := newPlanServer(t, runID, stageID)
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: stageType}
	return s, sf, ar, au, rr
}

// validGroomingReportBytes returns the canonical published example — the same
// document docs/spec/examples/grooming-report-v1-example.json ships, so the
// ingest test and the schema reference cannot drift apart.
func validGroomingReportBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/grooming-report-v1-example.json")
	if err != nil {
		t.Fatalf("read canonical grooming-report example: %v", err)
	}
	return b
}

// groomingAuditEntries returns every grooming_report_recorded entry the audit
// fake captured.
func groomingAuditEntries(au *auditFake) []audit.ChainAppendParams {
	var out []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == CategoryGroomingReportRecorded {
			out = append(out, e)
		}
	}
	return out
}

// TestHandleShipPlan_GroomingReport_PersistsAndAudits is the CROSS-BOUNDARY
// end-to-end test: a SIGNED POST /v0/runs/{run_id}/plan carrying a valid
// grooming_report drives the real router and, in one pass, must produce a 201,
// an artifact row (kind grooming_report, schema_version grooming_report_v1, the
// expected sha256 content hash), and a grooming_report_recorded audit entry
// carrying that same hash plus the per-class entry counts.
func TestHandleShipPlan_GroomingReport_PersistsAndAudits(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)
	body := validGroomingReportBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var resp planResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantHash := sha256Hex(body)
	if resp.SchemaVersion != groomingReportSchemaVersion {
		t.Errorf("schema_version = %q, want %q", resp.SchemaVersion, groomingReportSchemaVersion)
	}
	if resp.ContentHash != wantHash {
		t.Errorf("content_hash = %q, want %q", resp.ContentHash, wantHash)
	}
	if resp.Idempotent {
		t.Error("first upload should not be marked idempotent")
	}

	// Artifact row.
	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}
	got := ar.all[0]
	if got.Kind != artifact.KindGroomingReport {
		t.Errorf("artifact kind = %q, want %q", got.Kind, artifact.KindGroomingReport)
	}
	if got.SchemaVersion == nil || *got.SchemaVersion != groomingReportSchemaVersion {
		t.Errorf("artifact schema_version = %v, want %q", got.SchemaVersion, groomingReportSchemaVersion)
	}
	if got.ContentHash != wantHash {
		t.Errorf("artifact content_hash = %q, want %q", got.ContentHash, wantHash)
	}

	// Audit entry.
	entries := groomingAuditEntries(au)
	if len(entries) != 1 {
		t.Fatalf("grooming_report_recorded entries = %d, want 1 (all=%d)", len(entries), len(au.appended))
	}
	var payload struct {
		RunID         string         `json:"run_id"`
		StageID       string         `json:"stage_id"`
		ArtifactID    string         `json:"artifact_id"`
		ContentHash   string         `json:"content_hash"`
		SchemaVersion string         `json:"schema_version"`
		SizeBytes     int            `json:"size_bytes"`
		EntryCounts   map[string]int `json:"entry_counts"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v (%s)", err, entries[0].Payload)
	}
	if payload.ContentHash != wantHash {
		t.Errorf("audit content_hash = %q, want %q", payload.ContentHash, wantHash)
	}
	if payload.ArtifactID != got.ID.String() {
		t.Errorf("audit artifact_id = %q, want %q", payload.ArtifactID, got.ID)
	}
	if payload.SchemaVersion != groomingReportSchemaVersion {
		t.Errorf("audit schema_version = %q, want %q", payload.SchemaVersion, groomingReportSchemaVersion)
	}
	if payload.SizeBytes != len(body) {
		t.Errorf("audit size_bytes = %d, want %d", payload.SizeBytes, len(body))
	}
	want := map[string]int{
		"ordering": 4, "duplicates": 1, "hygiene_defects": 1,
		"dependency_edges": 1, "vision_drift": 1, "decomposition_suggestions": 1,
	}
	for k, v := range want {
		if payload.EntryCounts[k] != v {
			t.Errorf("audit entry_counts[%s] = %d, want %d", k, payload.EntryCounts[k], v)
		}
	}
	// The audit payload carries the HASH, not the body (#2240 reads by hash).
	if bytes.Contains(entries[0].Payload, []byte("proposed_children")) {
		t.Error("audit payload must not inline the report body; the content hash is the durable pointer")
	}
}

// validMilestoneReportBytes returns the shipped milestone example, so the ingest
// test and the schema reference cannot drift apart.
func validMilestoneReportBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/grooming-report-v1-milestone-example.json")
	if err != nil {
		t.Fatalf("read canonical milestone example: %v", err)
	}
	return b
}

// TestHandleGroomingReport_MilestoneScope_RecordsExtendedCensus is the
// HTTP-boundary half of the cross-layer test (E54.9 / #2309): a signed POST of a
// milestone report from a plan stage persists the artifact and records the three
// EXTENDED census counts alongside the six existing ones.
//
// It ALSO carries approval condition C5's observable proof — a milestone report
// proposes no mutation. The apply layer's landed-mutation seam emits
// grooming_mutation_applied audit rows; this ingest reaches NO such seam, so
// that category must be absent after the POST (a never-dialed-seam assertion in
// the spirit of the ones used elsewhere in this epic). The milestone_scope
// carries no tracker write, and the inherited gating is stated in the PR notes.
func TestHandleGroomingReport_MilestoneScope_RecordsExtendedCensus(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)
	body := validMilestoneReportBytes(t)

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(ar.all))
	}

	entries := groomingAuditEntries(au)
	if len(entries) != 1 {
		t.Fatalf("grooming_report_recorded entries = %d, want 1", len(entries))
	}
	var payload struct {
		EntryCounts map[string]int `json:"entry_counts"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	want := map[string]int{
		"ordering": 1, "duplicates": 0, "hygiene_defects": 1,
		"dependency_edges": 0, "vision_drift": 0, "decomposition_suggestions": 0,
		"milestone_inclusions": 4, "milestone_exclusions": 2, "milestone_declined_calls": 1,
	}
	for k, v := range want {
		if payload.EntryCounts[k] != v {
			t.Errorf("entry_counts[%s] = %d, want %d", k, payload.EntryCounts[k], v)
		}
	}

	// C5: no mutation seam was dialed. The apply layer emits
	// grooming_mutation_applied rows; a milestone report proposes no mutation, so
	// none must exist after ingest.
	if n := countAuditCategory(au, workmgmt.GroomingMutationAppliedCategory); n != 0 {
		t.Errorf("grooming_mutation_applied entries = %d, want 0 — a milestone report reaches no mutation seam", n)
	}
}

// TestHandleGroomingReport_OrdinaryReport_CensusOmitsMilestoneKeys is the
// additive-optional proof at the ingest boundary: an ordinary grooming report's
// audit census carries NONE of the three milestone keys, so the existing audit
// payload contract is byte-identical to before this slice.
func TestHandleGroomingReport_OrdinaryReport_CensusOmitsMilestoneKeys(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	s, sf, _, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequest(t, s, runID, stageID, priv, validGroomingReportBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	entries := groomingAuditEntries(au)
	if len(entries) != 1 {
		t.Fatalf("grooming_report_recorded entries = %d, want 1", len(entries))
	}
	for _, k := range []string{"milestone_inclusions", "milestone_exclusions", "milestone_declined_calls"} {
		if bytes.Contains(entries[0].Payload, []byte(k)) {
			t.Errorf("ordinary report audit payload carries %q; the census must omit milestone keys when no milestone_scope is present", k)
		}
	}
}

// TestHandleShipPlan_GroomingReport_Idempotent: a runner retry re-POSTs the same
// bytes and gets 200 idempotent with exactly one artifact row and one audit
// entry.
func TestHandleShipPlan_GroomingReport_Idempotent(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)
	body := validGroomingReportBytes(t)

	if w := shipPlanRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d:\n%s", w.Code, w.Body.String())
	}
	w2 := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp planResponse
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	if !resp.Idempotent {
		t.Error("second upload should be marked idempotent=true")
	}
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1 (no duplicate row)", len(ar.all))
	}
	if n := len(groomingAuditEntries(au)); n != 1 {
		t.Errorf("grooming_report_recorded entries = %d, want 1 (no second append)", n)
	}
}

// TestHandleShipPlan_GroomingReport_HealsMissingAuditEntry is binding condition
// F2's proof. The artifact row is seeded BY CONSTRUCTION with NO audit entry —
// the state a create-then-append leaves behind when the append failed — and the
// identical retry must END with the entry present. Asserting only the response
// status would pass against the broken design, because the GetByHash
// short-circuit returns success either way; the assertion is therefore on the
// COMMITTED AUDIT STATE after the call returns.
func TestHandleShipPlan_GroomingReport_HealsMissingAuditEntry(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)
	body := validGroomingReportBytes(t)

	// Seed the gapped state directly: artifact persisted, chain silent.
	schemaVersion := groomingReportSchemaVersion
	seeded, err := ar.Create(t.Context(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindGroomingReport,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   sha256Hex(body),
	})
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if n := len(groomingAuditEntries(au)); n != 0 {
		t.Fatalf("fixture must start with ZERO audit entries, got %d", n)
	}

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	entries := groomingAuditEntries(au)
	if len(entries) != 1 {
		t.Fatalf("grooming_report_recorded entries after heal = %d, want 1 — the artifact is durable and the chain must record it", len(entries))
	}
	var payload struct {
		ArtifactID  string `json:"artifact_id"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("decode healed payload: %v", err)
	}
	if payload.ArtifactID != seeded.ID.String() {
		t.Errorf("healed entry artifact_id = %q, want the seeded artifact %q", payload.ArtifactID, seeded.ID)
	}
	if payload.ContentHash != sha256Hex(body) {
		t.Errorf("healed entry content_hash = %q, want %q", payload.ContentHash, sha256Hex(body))
	}
	// No duplicate artifact row was inserted by the heal path.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	// A SECOND identical retry must be a no-op: the entry now exists.
	if w := shipPlanRequest(t, s, runID, stageID, priv, body, ""); w.Code != http.StatusOK {
		t.Fatalf("third upload status = %d, want 200", w.Code)
	}
	if n := len(groomingAuditEntries(au)); n != 1 {
		t.Errorf("grooming_report_recorded entries after a clean retry = %d, want 1 (the heal must be idempotent)", n)
	}
}

// TestHandleShipPlan_GroomingReport_HealReadError_500 pins the fail-closed leg
// of the heal: an audit READ failure must surface 500 (so a further retry can
// re-heal), never a possibly-gapped 200.
func TestHandleShipPlan_GroomingReport_HealReadError_500(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
	priv, _ := sf.issue(t, runID)
	body := validGroomingReportBytes(t)

	schemaVersion := groomingReportSchemaVersion
	if _, err := ar.Create(t.Context(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindGroomingReport,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   sha256Hex(body),
	}); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	au.listByCategoryErr = errors.New("boom")

	w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("heal grooming report audit entry failed")) {
		t.Errorf("body should name the heal failure: %s", w.Body.String())
	}
}

// TestHandleShipPlan_GroomingReportFromNonPlanStage_FailsCategoryB is the
// ingest-guard counterfactual. It asserts BOTH the 400 error code AND — because
// this control's effect is COMMITTED STATE that a byte-identical error would not
// distinguish — that after the call returns NO artifact row was written, NO
// audit entry was appended, and the stage transitioned to failed with
// failure_category B.
//
// The table enumerates EVERY non-plan stage type, `deploy` included. The guard
// is a single `!= StageTypePlan` comparison today, so any one row would exercise
// it — but a rewrite into an enumerated denylist that forgot a type would leave
// a short table green, and `deploy` is exactly the type such a denylist is most
// likely to miss (it is the one stage type with no agent executor).
func TestHandleShipPlan_GroomingReportFromNonPlanStage_FailsCategoryB(t *testing.T) {
	for _, stageType := range []run.StageType{
		run.StageTypeImplement, run.StageTypeReview, run.StageTypeAcceptance, run.StageTypeDeploy,
	} {
		t.Run(string(stageType), func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, au, rr := newGroomingServer(t, runID, stageID, stageType)
			priv, _ := sf.issue(t, runID)
			body := validGroomingReportBytes(t)

			w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("grooming_report_stage_invalid")) {
				t.Errorf("error code missing grooming_report_stage_invalid: %s", w.Body.String())
			}
			// COMMITTED STATE, not just the error identity.
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0 — a report from a non-plan stage must not persist", len(ar.all))
			}
			if n := len(groomingAuditEntries(au)); n != 0 {
				t.Errorf("grooming_report_recorded entries = %d, want 0", n)
			}
			if !sawFailureCategoryB(rr) {
				t.Errorf("stage did not transition to failed with category B; transitions=%+v", rr.transitionStageCalls)
			}
		})
	}
}

// sawFailureCategoryB reports whether any recorded transition failed the stage
// with category B.
func sawFailureCategoryB(rr *promptRunRepo) bool {
	for _, c := range rr.transitionStageCalls {
		if c.Completion != nil && c.Completion.FailureCategory != nil && *c.Completion.FailureCategory == run.FailureB {
			return true
		}
	}
	return false
}

// TestHandleShipPlan_GroomingReport_Invalid_FailsCategoryB drives the two
// validation rejection classes through the REAL ingest path — a schema violation
// (an ordering entry with no rubric citation, the load-bearing control) and a
// semantic violation (a per-run-minted id that cannot recompose) — asserting
// each yields 400 grooming_report_invalid, no artifact row, no audit entry, and
// a category-B stage failure. Both fixtures are hand-written literals.
func TestHandleShipPlan_GroomingReport_Invalid_FailsCategoryB(t *testing.T) {
	const envelope = `{
  "kind": "grooming_report",
  "report_version": "grooming_report_v1",
  "ticket_reference": {"type":"github_issue","url":"https://github.com/kuhlman-labs/fishhawk/issues/2235","id":"kuhlman-labs/fishhawk#2235"},
  "generated_by": {"agent":"claude-code","model":"claude-opus-5","timestamp":"2026-08-21T14:02:11Z"},
  "summary": "Groomed one item.",
  "ordering": [%s],
  "duplicates": [], "hygiene_defects": [], "dependency_edges": [],
  "vision_drift": [], "decomposition_suggestions": []
}`
	const itemRef = `{"type":"github_issue","id":"kuhlman-labs/fishhawk#2235","url":"https://github.com/kuhlman-labs/fishhawk/issues/2235"}`

	cases := []struct {
		name  string
		entry string
	}{
		{
			name:  "ordering entry with no rubric citation",
			entry: `{"id":"ordering:github/kuhlman-labs/fishhawk#2235","item_ref":` + itemRef + `,"rank":1,"score":9.5}`,
		},
		{
			name:  "per-run minted entry id",
			entry: `{"id":"ordering:8f14e45fceea167a5a36dedd4bea2543","item_ref":` + itemRef + `,"rank":1,"score":9.5,"rubric_citations":[{"rubric_id":"V1"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, au, rr := newGroomingServer(t, runID, stageID, run.StageTypePlan)
			priv, _ := sf.issue(t, runID)
			body := []byte(replaceFirst(envelope, "%s", tc.entry))

			w := shipPlanRequest(t, s, runID, stageID, priv, body, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("grooming_report_invalid")) {
				t.Errorf("error code missing grooming_report_invalid: %s", w.Body.String())
			}
			if len(ar.all) != 0 {
				t.Errorf("artifacts = %d, want 0 — an invalid report must not persist", len(ar.all))
			}
			if n := len(groomingAuditEntries(au)); n != 0 {
				t.Errorf("grooming_report_recorded entries = %d, want 0", n)
			}
			if !sawFailureCategoryB(rr) {
				t.Errorf("stage did not fail category-B; transitions=%+v", rr.transitionStageCalls)
			}
		})
	}
}

// replaceFirst substitutes the first occurrence of old with new.
func replaceFirst(s, old, new string) string {
	i := bytes.Index([]byte(s), []byte(old))
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

// TestHandleShipPlan_GroomingReport_ConcurrentIdenticalPosts is the atomicity
// proof for the ingest critical section (groomingIngestMu). The runner retries a
// shipped report, so identical POSTs genuinely overlap; the check-then-act
// windows the handler holds (GetByHash → Create → append, and the heal's
// list → append) must not interleave.
//
// Both subtests assert COMMITTED STATE — exactly one artifact row and exactly one
// grooming_report_recorded entry — rather than the response status, because every
// racing request returns a success status whether or not the writes were
// duplicated.
//
// Counterfactual vehicle: deleting the groomingIngestMu Lock/Unlock pair makes
// "first POST, concurrent" write several artifacts and several audit entries.
func TestHandleShipPlan_GroomingReport_ConcurrentIdenticalPosts(t *testing.T) {
	const n = 8

	// fire runs n identical signed POSTs concurrently, released from one
	// barrier so they overlap inside the handler, and returns their statuses.
	fire := func(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte) []int {
		t.Helper()
		codes := make([]int, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i] = shipPlanRequest(t, s, runID, stageID, priv, body, "").Code
			}(i)
		}
		close(start)
		wg.Wait()
		return codes
	}

	t.Run("first POST, concurrent", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
		priv, _ := sf.issue(t, runID)
		body := validGroomingReportBytes(t)

		codes := fire(t, s, runID, stageID, priv, body)
		created := 0
		for i, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusOK:
			default:
				t.Fatalf("request %d status = %d, want 201 or 200", i, c)
			}
		}
		if created != 1 {
			t.Errorf("201 responses = %d, want exactly 1 (the rest must dedup to 200)", created)
		}
		if len(ar.all) != 1 {
			t.Errorf("artifacts = %d, want 1 — concurrent identical POSTs must not fork the artifact row", len(ar.all))
		}
		if got := len(groomingAuditEntries(au)); got != 1 {
			t.Errorf("grooming_report_recorded entries = %d, want 1 — the existence check and the append must be atomic", got)
		}
	})

	// The gapped state is seeded BY CONSTRUCTION (artifact durable, chain
	// silent), so every racing request takes the heal branch and the RED lands
	// on the entry count rather than on fixture setup.
	t.Run("concurrent retries heal a gapped chain exactly once", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
		priv, _ := sf.issue(t, runID)
		body := validGroomingReportBytes(t)

		schemaVersion := groomingReportSchemaVersion
		if _, err := ar.Create(t.Context(), artifact.CreateParams{
			StageID:       stageID,
			Kind:          artifact.KindGroomingReport,
			SchemaVersion: &schemaVersion,
			Content:       json.RawMessage(body),
			ContentHash:   sha256Hex(body),
		}); err != nil {
			t.Fatalf("seed artifact: %v", err)
		}
		if got := len(groomingAuditEntries(au)); got != 0 {
			t.Fatalf("fixture must start with ZERO audit entries, got %d", got)
		}

		for i, c := range fire(t, s, runID, stageID, priv, body) {
			if c != http.StatusOK {
				t.Fatalf("retry %d status = %d, want 200", i, c)
			}
		}
		if len(ar.all) != 1 {
			t.Errorf("artifacts = %d, want 1", len(ar.all))
		}
		if got := len(groomingAuditEntries(au)); got != 1 {
			t.Errorf("grooming_report_recorded entries = %d, want 1 — concurrent heals must not double-append", got)
		}
	})
}

// TestHandleShipPlan_GroomingReport_StorageFailures_500 pins the three
// storage-error branches, each of which must surface 500 so the runner retries
// (the retry is idempotent via GetByHash, and a gapped chain self-heals).
func TestHandleShipPlan_GroomingReport_StorageFailures_500(t *testing.T) {
	cases := []struct {
		name    string
		inject  func(*fakeArtifactRepo, *auditFake)
		wantMsg string
	}{
		{
			name:    "artifact create fails",
			inject:  func(ar *fakeArtifactRepo, _ *auditFake) { ar.createErr = errors.New("boom") },
			wantMsg: "create grooming report artifact failed",
		},
		{
			name:    "audit append fails",
			inject:  func(_ *fakeArtifactRepo, au *auditFake) { au.appendErrCategory = CategoryGroomingReportRecorded },
			wantMsg: "append grooming report audit entry failed",
		},
		{
			name:    "dedup lookup fails",
			inject:  func(ar *fakeArtifactRepo, _ *auditFake) { ar.getByHashErr = errors.New("boom") },
			wantMsg: "check existing grooming report failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, stageID := uuid.New(), uuid.New()
			s, sf, ar, au, _ := newGroomingServer(t, runID, stageID, run.StageTypePlan)
			priv, _ := sf.issue(t, runID)
			tc.inject(ar, au)

			w := shipPlanRequest(t, s, runID, stageID, priv, validGroomingReportBytes(t), "")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(tc.wantMsg)) {
				t.Errorf("body should name %q: %s", tc.wantMsg, w.Body.String())
			}
		})
	}
}

// --- E54.8 / #2240: the CHURN GUARD, verified at the ingest boundary --------
//
// #2240 approval condition J1: acceptance criteria 1, 4 and 6 are not claims
// about a function's return value — they are claims about a RUN over an
// unchanged backlog producing an empty proposal set and a visible "no changes
// proposed" outcome, diffed against the last approved and applied state. Every
// test below therefore drives the REAL router through a signed POST
// /v0/runs/{run_id}/plan and reads the verdict off the audit chain, not off
// workmgmt.FilterGroomingChurn's signature.

// groomingRunRepo adds the repo-scoped ListRuns the baseline search needs. The
// shared promptRunRepo's ListRuns only serves the decomposition filter, and it
// is out of this change's scope, so the capability is composed by embedding
// rather than by editing the shared fake.
type groomingRunRepo struct {
	*promptRunRepo
	byRepo      map[string][]*run.Run
	listRunsErr error
	// lastFilter records what the baseline search actually asked for, so the
	// QUERY-level tenant scoping is assertable and not merely assumed from the
	// handler-side re-check.
	lastFilter run.ListRunsFilter
}

// ListRuns models the production query's REPO, WORKFLOW and LIMIT predicates,
// and deliberately not the account one. The real ListRuns keeps rows whose
// account_id IS NULL regardless of the AccountID filter (the untenanted-list
// contract, repository.go), so a fake that filtered on account would hide
// exactly the leak the handler-side tenant re-check exists to close.
//
// WorkflowID and Limit ARE modelled, because #2240's scan-window concern is
// about them: the query's workflow_id equality (empty = no constraint)
// followed by LIMIT, so a repo
// whose history is dominated by non-grooming runs pushes the prior grooming
// run out of an unscoped window.
//
// Truncation is applied in INSERTION order rather than after a newest-first
// sort, even though production sorts before it. A fake that sorted would hand
// groomingChurnBaseline an already-correct order and make that function's OWN
// explicit newest-first sort untestable — the very control
// TestGroomingIngest_MostRecentPriorRunWins exists to pin. Tests that care
// about the window therefore choose insertion order explicitly.
func (r *groomingRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.lastFilter = f
	if r.listRunsErr != nil {
		return nil, r.listRunsErr
	}
	out := make([]*run.Run, 0, len(r.byRepo[f.Repo]))
	for _, rn := range r.byRepo[f.Repo] {
		if f.WorkflowID != "" && rn != nil && rn.WorkflowID != f.WorkflowID {
			continue
		}
		out = append(out, rn)
		if f.Limit > 0 && len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

// churnFixture wires a server whose run belongs to a repo, with the plumbing
// the baseline search walks: ListRuns by repo -> ListStagesForRun -> artifact
// ListForStage -> the prior run's audit chain.
type churnFixture struct {
	s    *Server
	sf   *signingFake
	ar   *fakeArtifactRepo
	au   *auditFake
	rr   *groomingRunRepo
	repo string
}

const churnRepo = "kuhlman-labs/example"

// churnWorkflow is the grooming workflow every fixture run declares. The
// baseline query is workflow-scoped (#2240 fix-up), so a candidate that does
// not declare it is not in the scan window at all.
const churnWorkflow = "backlog_grooming"

// churnFixedTime keeps the fixture's generated_by.timestamp constant so two
// marshals of the same report produce identical bytes — AC6's byte-identity is
// asserted over the audit payload, which must not move because a clock did.
var churnFixedTime = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

// churnCharterHash is a fixed, schema-shaped (64 hex chars) charter content
// hash: the fixtures pin one charter revision so charter drift is not an
// uncontrolled variable in these assertions.
const churnCharterHash = "3726dca74f7729cbe084869f755e3ffff92812011e61fe13084faf603fd13561"

// churnCurrentRunTime is the CURRENT run's created_at. It is fixed and late
// enough that every prior run these fixtures seed — including the ones that
// set an explicit created_at — precedes it, because the baseline now admits
// only STRICT predecessors in (created_at, id) order rather than merely
// "not this run". A fixture whose current run carried the zero time would
// admit no candidate at all and pass every suppression assertion vacuously.
var churnCurrentRunTime = time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

func newChurnFixture(t *testing.T, runID, stageID uuid.UUID) *churnFixture {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	base := newPromptRunRepo()
	base.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypePlan}
	base.getRuns[runID] = &run.Run{ID: runID, Repo: churnRepo, WorkflowID: churnWorkflow, CreatedAt: churnCurrentRunTime}
	base.stagesByRunID = map[uuid.UUID][]*run.Stage{}
	rr := &groomingRunRepo{promptRunRepo: base, byRepo: map[string][]*run.Run{}}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
	})
	return &churnFixture{s: s, sf: sf, ar: ar, au: au, rr: rr, repo: churnRepo}
}

// seedPriorGroomingRun installs a completed prior run on the same repo that
// shipped `report`, with one grooming_mutation_applied audit row per entry id
// in `applied` (outcome applied) and per id in `rejected` (skip reason
// not_approved) — the two halves the apply layer (#2237) records and
// groomingChurnBaseline projects back into dispositions.
func (f *churnFixture) seedPriorGroomingRun(t *testing.T, report *plan.GroomingReport, applied, rejected []string) uuid.UUID {
	t.Helper()
	return f.seedPriorGroomingRunAs(t, nil, report, applied, rejected)
}

// seedPriorGroomingRunAs is seedPriorGroomingRun with the prior run row handed
// to `tweak` before it is registered, so a test can vary the two properties a
// repo string does not carry — the tenant (account / installation) and
// created_at.
func (f *churnFixture) seedPriorGroomingRunAs(t *testing.T, tweak func(*run.Run), report *plan.GroomingReport, applied, rejected []string) uuid.UUID {
	t.Helper()
	priorRun, priorStage := uuid.New(), uuid.New()
	pr := &run.Run{ID: priorRun, Repo: f.repo, WorkflowID: churnWorkflow}
	if tweak != nil {
		tweak(pr)
	}
	f.rr.getRuns[priorRun] = pr
	f.rr.stagesByRunID[priorRun] = []*run.Stage{{ID: priorStage, RunID: priorRun, Type: run.StageTypePlan}}
	f.rr.byRepo[f.repo] = append(f.rr.byRepo[f.repo], f.rr.getRuns[priorRun])

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal prior report: %v", err)
	}
	if _, perr := plan.ParseGroomingReport(body); perr != nil {
		t.Fatalf("prior report fixture does not validate: %v", perr)
	}
	sv := plan.GroomingReportVersion
	// An UNPARSEABLE grooming_report artifact sits AHEAD of the real one on the
	// same stage, so the baseline search's parse guard is discriminated: without
	// it the search returns nil on the first row and the real baseline is lost.
	junk := []byte(`{"kind":"grooming_report","report_version":"not_a_version"}`)
	if _, cerr := f.ar.Create(context.Background(), artifact.CreateParams{
		StageID: priorStage, Kind: artifact.KindGroomingReport, SchemaVersion: &sv,
		Content: junk, ContentHash: sha256Hex(junk),
	}); cerr != nil {
		t.Fatalf("seed unparseable prior artifact: %v", cerr)
	}
	if _, cerr := f.ar.Create(context.Background(), artifact.CreateParams{
		StageID: priorStage, Kind: artifact.KindGroomingReport, SchemaVersion: &sv,
		Content: body, ContentHash: sha256Hex(body),
	}); cerr != nil {
		t.Fatalf("seed prior artifact: %v", cerr)
	}

	record := func(entryID, outcome, skip string) {
		p, _ := json.Marshal(map[string]any{"entry_id": entryID, "outcome": outcome, "skip_reason": skip})
		if _, aerr := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: priorRun, Category: workmgmt.GroomingMutationAppliedCategory, Payload: p,
		}); aerr != nil {
			t.Fatalf("seed apply audit: %v", aerr)
		}
	}
	for _, id := range applied {
		record(id, string(workmgmt.GroomingOutcomeApplied), "")
	}
	for _, id := range rejected {
		record(id, string(workmgmt.GroomingOutcomeSkipped), workmgmt.GroomingSkipNotApproved)
	}
	return priorRun
}

// churnVerdict returns the decoded payload of the single
// grooming_churn_filtered audit entry, failing when there is not exactly one.
type churnPayload struct {
	ArtifactID string                        `json:"artifact_id"`
	Summary    workmgmt.GroomingChurnSummary `json:"summary"`
	Suppressed []workmgmt.GroomingSuppression
	Resurfaced []workmgmt.GroomingResurface
	Thresholds workmgmt.GroomingThresholds `json:"thresholds"`
	Degraded   []string                    `json:"degraded"`
}

func churnVerdict(t *testing.T, au *auditFake) churnPayload {
	t.Helper()
	var found []audit.ChainAppendParams
	for _, e := range au.appended {
		if e.Category == workmgmt.GroomingChurnFilteredCategory {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("grooming_churn_filtered entries = %d, want exactly 1", len(found))
	}
	var p churnPayload
	if err := json.Unmarshal(found[0].Payload, &p); err != nil {
		t.Fatalf("decode churn payload: %v", err)
	}
	return p
}

func countChurnEntries(au *auditFake) int {
	n := 0
	for _, e := range au.appended {
		if e.Category == workmgmt.GroomingChurnFilteredCategory {
			n++
		}
	}
	return n
}

func churnItem(n string) plan.ItemRef {
	return plan.ItemRef{Type: "github_issue", ID: "kuhlman-labs/example#" + n,
		URL: "https://github.com/kuhlman-labs/example/issues/" + n}
}

// churnReport builds a schema-valid grooming report with three ordering
// entries at the given ranks/scores plus one hygiene defect, so a single
// fixture exercises the ordering thresholds and the significance bar.
func churnReport(ranks []int, scores []float64, hygieneDefect string) *plan.GroomingReport {
	gr := &plan.GroomingReport{
		Kind:                     plan.KindGroomingReport,
		ReportVersion:            plan.GroomingReportVersion,
		TicketReference:          plan.TicketReference{Type: "github_issue", ID: "kuhlman-labs/example#1", URL: "https://github.com/kuhlman-labs/example/issues/1"},
		GeneratedBy:              plan.GeneratedBy{Agent: "claude-code", Model: "claude-opus-5", Timestamp: churnFixedTime},
		Summary:                  "churn-guard boundary fixture",
		CharterRef:               &plan.GroomingCharterRef{Path: ".fishhawk/charter.md", ContentHash: churnCharterHash, Ref: "main"},
		Ordering:                 []plan.OrderingEntry{},
		Duplicates:               []plan.DuplicateCandidate{},
		HygieneDefects:           []plan.HygieneDefect{},
		DependencyEdges:          []plan.DependencyEdge{},
		VisionDrift:              []plan.VisionDriftFlag{},
		DecompositionSuggestions: []plan.DecompositionSuggestion{},
	}
	for i, rank := range ranks {
		ref := churnItem(strconv.Itoa(i + 1))
		gr.Ordering = append(gr.Ordering, plan.OrderingEntry{
			ID:      plan.GroomingEntryID(plan.GroomingClassOrdering, "", ref),
			ItemRef: ref, Rank: rank, Score: scores[i],
			RubricCitations: []plan.RubricCitation{{RubricID: "U1", Note: "n"}},
			Rationale:       "regenerated prose",
		})
	}
	if hygieneDefect != "" {
		ref := churnItem("9")
		gr.HygieneDefects = append(gr.HygieneDefects, plan.HygieneDefect{
			ID:      plan.GroomingEntryID(plan.GroomingClassHygiene, hygieneDefect, ref),
			ItemRef: ref, Defect: hygieneDefect, Detail: "detail", SuggestedFix: "3",
		})
	}
	return gr
}

func shipGrooming(t *testing.T, f *churnFixture, runID, stageID uuid.UUID, report *plan.GroomingReport) ([]byte, int) {
	t.Helper()
	priv, _ := f.sf.issue(t, runID)
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	w := shipPlanRequest(t, f.s, runID, stageID, priv, body, "")
	return body, w.Code
}

// withConventions swaps the process-wide conventions loader for one test.
func withConventions(t *testing.T, conv workmgmt.Conventions, err error) {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, err }
	t.Cleanup(func() { conventionsLoader = prev })
}

// TestGroomingIngest_UnchangedBacklogProposesNothing is the J1 proof for AC1,
// AC4 and AC6 AT THE BOUNDARY: a real signed ingest of a report identical to
// the prior run's, whose entries all APPLIED, must emit a
// grooming_churn_filtered verdict carrying no_changes_proposed with zero
// proposals. Nothing here calls FilterGroomingChurn directly.
func TestGroomingIngest_UnchangedBacklogProposesNothing(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1, 2, 3}, []float64{9.5, 8.0, 7.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)
	// The CURRENT run is listed first, with its own plan stage enumerable, so
	// the baseline search's self-exclusion is discriminated: without it the
	// report just persisted under this very stage shadows the real prior run
	// and the baseline collapses to nothing.
	f.rr.byRepo[churnRepo] = []*run.Run{f.rr.getRuns[runID]}
	f.rr.stagesByRunID[runID] = []*run.Stage{{ID: stageID, RunID: runID, Type: run.StageTypePlan}}
	f.seedPriorGroomingRun(t, report, ids, nil)

	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if !v.Summary.NoChangesProposed {
		t.Errorf("no_changes_proposed = false over an unchanged, fully-applied backlog; proposed=%v", v.Summary.ProposedIDs)
	}
	if v.Summary.Proposed != 0 {
		t.Errorf("proposed = %d (%v), want 0", v.Summary.Proposed, v.Summary.ProposedIDs)
	}
	if v.Summary.Suppressed != 4 {
		t.Errorf("suppressed = %d, want 4 — every difference COMPUTED and reported", v.Summary.Suppressed)
	}
	if v.Summary.BaselineEntries != 4 {
		t.Errorf("baseline_entries = %d, want 4; the baseline was not assembled from the prior applied run", v.Summary.BaselineEntries)
	}
	if len(v.Degraded) != 0 {
		t.Errorf("degraded = %v, want none on the fully-wired path", v.Degraded)
	}

	// RETRY DEDUPLICATION — and nothing more. A second ingest of the SAME bytes
	// takes the idempotent path and must leave exactly ONE verdict.
	//
	// This is deliberately NOT the AC6 assertion, and the earlier version of
	// this block that claimed it was, was vacuous: the retry heals rather than
	// appends, so `before` and the re-read payload were the same audit record
	// and the comparison held even if two distinct runs produced different
	// verdicts. Run-over-run byte identity is proved by
	// TestGroomingIngest_DistinctRunsProduceIdenticalVerdict, which executes the
	// guard in two separate runs and compares their serialized verdicts.
	before := churnPayloadBytes(t, f.au)
	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if n := countChurnEntries(f.au); n != 1 {
		t.Errorf("churn entries after a retry = %d, want 1 (heal, never duplicate)", n)
	}
	if string(before) != string(churnPayloadBytes(t, f.au)) {
		t.Error("the retry REWROTE the recorded verdict; the healed entry must be the original one")
	}
}

// TestGroomingIngest_DistinctRunsProduceIdenticalVerdict is AC6's real proof:
// two SEPARATE runs — separate run ids, stage ids, artifact rows and audit
// chains — over the same backlog against the same applied baseline must
// serialize the same verdict. The second run additionally ships the report's
// arrays in a DIFFERENT order, which is the jitter AC6 exists to absorb: an
// agent re-emitting the same findings in a different sequence must not change
// the bytes.
//
// The identity fields (run_id, stage_id, artifact_id) necessarily differ and
// are stripped before the comparison — and asserted to differ, so the test
// cannot silently degenerate into comparing one record with itself, which is
// exactly how the retry-path assertion above went vacuous.
func TestGroomingIngest_DistinctRunsProduceIdenticalVerdict(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)

	// One run of the guard, end to end, in its own fixture. reverse ships the
	// same findings with the arrays walked backwards.
	oneRun := func(t *testing.T, reverse bool) (core []byte, identity map[string]string) {
		t.Helper()
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)

		scores := []float64{9.50, 8.00, 7.00, 6.00, 5.00}
		prior := churnReport([]int{1, 2, 3, 4, 5}, scores, "")
		var ids []string
		for _, e := range prior.Ordering {
			ids = append(ids, e.ID)
		}
		f.seedPriorGroomingRun(t, prior, ids, nil)

		// Entries 1 and 3 swap across two positions (both PROPOSED); entries 2,
		// 4 and 5 hold (all SUPPRESSED); the hygiene defect is new (PROPOSED).
		// MULTIPLE entries on each side is what makes the reversal below
		// discriminating: with one proposal per list, array order could not
		// change the serialized bytes whatever the guard did.
		next := churnReport([]int{3, 2, 1, 4, 5}, scores, "missing_estimate")
		if reverse {
			for i, j := 0, len(next.Ordering)-1; i < j; i, j = i+1, j-1 {
				next.Ordering[i], next.Ordering[j] = next.Ordering[j], next.Ordering[i]
			}
		}
		if _, code := shipGrooming(t, f, runID, stageID, next); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if v.Summary.Proposed < 2 || v.Summary.Suppressed < 2 {
			t.Fatalf("fixture: proposed=%d suppressed=%d — the comparand must carry at least two of each for array order to matter",
				v.Summary.Proposed, v.Summary.Suppressed)
		}
		return churnVerdictCore(t, f.au)
	}

	firstCore, firstID := oneRun(t, false)
	secondCore, secondID := oneRun(t, true)

	for _, k := range []string{"run_id", "stage_id", "artifact_id"} {
		if firstID[k] == "" || firstID[k] == secondID[k] {
			t.Fatalf("%s = %q / %q — the two verdicts are not from distinct runs, so the comparison below proves nothing",
				k, firstID[k], secondID[k])
		}
	}
	if string(firstCore) != string(secondCore) {
		t.Errorf("two distinct runs over the same backlog serialized different verdicts:\n%s\n%s", firstCore, secondCore)
	}
}

// churnVerdictCore splits the single recorded churn verdict into the
// run-invariant part (everything the guard decided) and the per-run identity
// fields, so run-over-run byte identity is assertable over the former.
func churnVerdictCore(t *testing.T, au *auditFake) ([]byte, map[string]string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(churnPayloadBytes(t, au), &fields); err != nil {
		t.Fatalf("decode churn payload: %v", err)
	}
	identity := map[string]string{}
	for _, k := range []string{"run_id", "stage_id", "artifact_id"} {
		var v string
		if raw, ok := fields[k]; ok {
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("decode %s: %v", k, err)
			}
		}
		identity[k] = v
		delete(fields, k)
	}
	// encoding/json sorts map keys, so the re-marshal is itself deterministic;
	// every retained value is the ORIGINAL raw JSON, not a re-encoding.
	core, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("re-marshal verdict core: %v", err)
	}
	return core, identity
}

func churnPayloadBytes(t *testing.T, au *auditFake) []byte {
	t.Helper()
	for _, e := range au.appended {
		if e.Category == workmgmt.GroomingChurnFilteredCategory {
			return e.Payload
		}
	}
	t.Fatal("no grooming_churn_filtered entry")
	return nil
}

// TestGroomingIngest_FirstEverRunProposesEverything is the control: with no
// prior run on the repo the baseline is empty, so the guard proposes
// everything and says so. Without it, a guard that suppressed unconditionally
// would pass the test above.
func TestGroomingIngest_FirstEverRunProposesEverything(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)
	f.rr.byRepo[churnRepo] = []*run.Run{f.rr.getRuns[runID]} // only the current run

	report := churnReport([]int{1, 2, 3}, []float64{9.5, 8.0, 7.0}, "missing_estimate")
	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.NoChangesProposed {
		t.Error("no_changes_proposed = true on a FIRST-ever grooming run")
	}
	if v.Summary.Proposed != 4 {
		t.Errorf("proposed = %d (%v), want all 4", v.Summary.Proposed, v.Summary.ProposedIDs)
	}
	if v.Summary.BaselineEntries != 0 {
		t.Errorf("baseline_entries = %d, want 0", v.Summary.BaselineEntries)
	}
}

// TestGroomingIngest_SubThresholdMovementSuppressedAtTheBoundary is AC2 at the
// ingest boundary: after a fully-applied prior run, a one-position swap with a
// 0.02 score move is suppressed while a three-position move is proposed — the
// shipped conservative defaults, resolved from real conventions, deciding on
// the real path.
func TestGroomingIngest_SubThresholdMovementSuppressedAtTheBoundary(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	prior := churnReport([]int{1, 2, 3}, []float64{9.50, 8.00, 7.00}, "")
	var ids []string
	for _, e := range prior.Ordering {
		ids = append(ids, e.ID)
	}
	f.seedPriorGroomingRun(t, prior, ids, nil)

	// Ranks must stay a permutation of 1..N (grooming-report-v1's semantic
	// rule), so the movements are expressed as a rotation:
	//   entry 1: rank 1->2, score 9.50->9.52  — one position, 0.02: UNDER both bars
	//   entry 2: rank 2->3, score unchanged     — one position: UNDER the bar
	//   entry 3: rank 3->1                      — two positions: AT the bar, proposed
	next := churnReport([]int{2, 3, 1}, []float64{9.52, 8.00, 7.00}, "")
	if _, code := shipGrooming(t, f, runID, stageID, next); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.Proposed != 1 || v.Summary.ProposedIDs[0] != next.Ordering[2].ID {
		t.Errorf("proposed = %v, want only the 3-position move %q", v.Summary.ProposedIDs, next.Ordering[2].ID)
	}
	for _, s := range v.Suppressed {
		if s.Reason != workmgmt.GroomingSuppressBelowOrderingThreshold {
			t.Errorf("suppression %+v: reason = %q, want %q", s, s.Reason, workmgmt.GroomingSuppressBelowOrderingThreshold)
		}
	}
	if v.Summary.Suppressed != 2 {
		t.Errorf("suppressed = %d, want 2", v.Summary.Suppressed)
	}
}

// TestGroomingIngest_ThresholdsComeFromRepoConventions is AC3 at the boundary:
// the significance bar the guard applies is THIS repo's declared one, read
// through the same conventions seam every other per-repo policy uses.
func TestGroomingIngest_ThresholdsComeFromRepoConventions(t *testing.T) {
	five := 5
	conv := workmgmt.Default()
	conv.Grooming = &workmgmt.Grooming{Thresholds: &workmgmt.GroomingThresholdSpec{
		MinRankMovement:           &five,
		SignificantHygieneDefects: []string{"absent_done_means"},
	}}
	withConventions(t, conv, nil)

	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)
	scores := []float64{9.5, 8.0, 7.0, 6.0}
	prior := churnReport([]int{1, 2, 3, 4}, scores, "")
	var ids []string
	for _, e := range prior.Ordering {
		ids = append(ids, e.ID)
	}
	f.seedPriorGroomingRun(t, prior, ids, nil)

	// Entries 1 and 4 swap ends: a THREE-position move, which the shipped
	// default of 2 would propose and the repo's declared 5 must not. Plus a NEW
	// hygiene defect outside the declared significance set (J2: suppressed on
	// its FIRST appearance, not proposed once and only then suppressible).
	next := churnReport([]int{4, 2, 3, 1}, scores, "missing_estimate")
	if _, code := shipGrooming(t, f, runID, stageID, next); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if v.Thresholds.MinRankMovement != 5 {
		t.Errorf("recorded min_rank_movement = %d, want the repo's declared 5", v.Thresholds.MinRankMovement)
	}
	if v.Summary.Proposed != 0 {
		t.Errorf("proposed = %v, want none: the 3-position move is under a declared bar of 5 and the hygiene defect is outside the significance set",
			v.Summary.ProposedIDs)
	}
	var sawHygieneBar bool
	for _, s := range v.Suppressed {
		if s.Reason == workmgmt.GroomingSuppressBelowHygieneSignificance {
			sawHygieneBar = true
		}
	}
	if !sawHygieneBar {
		t.Errorf("suppressed = %+v, want a below_hygiene_significance entry for the NEW sub-significance defect (J2)", v.Suppressed)
	}
}

// TestGroomingIngest_RejectedProposalDoesNotReappear is AC5 at the boundary,
// driven off a REJECTED disposition recorded in the prior run's audit chain.
func TestGroomingIngest_RejectedProposalDoesNotReappear(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	prior := churnReport([]int{1}, []float64{9.5}, "missing_estimate")
	f.seedPriorGroomingRun(t, prior, nil, []string{prior.HygieneDefects[0].ID})

	same := churnReport([]int{1}, []float64{9.5}, "missing_estimate")
	same.HygieneDefects[0].Detail = "an entirely re-worded justification"
	if _, code := shipGrooming(t, f, runID, stageID, same); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	for _, id := range v.Summary.ProposedIDs {
		if id == prior.HygieneDefects[0].ID {
			t.Fatal("a REJECTED proposal reappeared on the next run over an unchanged basis")
		}
	}
	var reason string
	for _, s := range v.Suppressed {
		if s.EntryID == prior.HygieneDefects[0].ID {
			reason = s.Reason
		}
	}
	if reason != workmgmt.GroomingSuppressPreviouslyRejected {
		t.Errorf("reason = %q, want %q", reason, workmgmt.GroomingSuppressPreviouslyRejected)
	}
}

// TestGroomingIngest_BaselineIsTenantScoped pins the tenant discriminator. A
// repo STRING is not an identity: ADR-057's nullable account_id and
// per-installation forge routing (GHES + github.com) both permit two different
// tenants to own runs whose Repo is the same "owner/name". Without the
// discriminator, ANOTHER tenant's dispositions decide this run's suppressions
// and that tenant's entry ids land in this run's audit payload.
//
// The fake's ListRuns models the production query's repo predicate only —
// which is honest, because the real query's account predicate deliberately
// keeps account_id IS NULL rows visible — so the foreign runs below reach the
// walk exactly as they would in production.
func TestGroomingIngest_BaselineIsTenantScoped(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	const tenantA, tenantB = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	installA, installB := int64(1001), int64(2002)

	// The BOUNDARY case, driven through the real signed ingest. The current run
	// is UNTENANTED (account_id NULL), which is the sharpest version of the
	// leak: ListRuns treats an empty AccountID as "no constraint", so the query
	// returns every account's runs on this repo string and the handler-side
	// discriminator is the ONLY thing standing between them and this run's
	// suppressions. (A tenanted current run cannot be driven from here — the
	// ownership middleware 403s a runner-signed POST whose identity carries no
	// account — so the query-level half is asserted below.)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)

	// Two foreign prior runs on the SAME repo string, each having applied every
	// entry: adopting either would suppress this run's whole report and write
	// the other tenant's entry ids into this run's audit payload.
	newest := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	f.seedPriorGroomingRunAs(t, func(r *run.Run) {
		r.AccountID = tenantB // another tenant, same repo name
		r.CreatedAt = newest
	}, report, ids, nil)
	f.seedPriorGroomingRunAs(t, func(r *run.Run) {
		r.InstallationID = &installB // another forge installation (GHES vs github.com)
		r.CreatedAt = newest.Add(-time.Hour)
	}, report, ids, nil)

	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.BaselineEntries != 0 {
		t.Errorf("baseline_entries = %d, want 0 — a run outside this tenant/installation became the baseline", v.Summary.BaselineEntries)
	}
	if v.Summary.Proposed != 3 || v.Summary.Suppressed != 0 {
		t.Errorf("proposed=%v suppressed=%d; another tenant's dispositions suppressed this run's report",
			v.Summary.ProposedIDs, v.Summary.Suppressed)
	}

	// The QUERY-level half, one call below the HTTP boundary because the
	// middleware bars a tenanted runner-signed POST. Same fixture shape: a
	// tenanted current run must (a) scope the query to its own account and (b)
	// still ADOPT a prior run in that same account and installation — the
	// positive control, without which "reject everything" would pass.
	t.Run("query scoping and the same-tenant accept path", func(t *testing.T) {
		curID, curStage := uuid.New(), uuid.New()
		g := newChurnFixture(t, curID, curStage)
		cur := g.rr.getRuns[curID]
		cur.AccountID = tenantA
		cur.InstallationID = &installA

		// The foreign run is NEWEST and carries the OPPOSITE dispositions
		// (rejected, not applied), so adopting it is visible in the assertion
		// below rather than masked by an identical baseline.
		g.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.AccountID = tenantB
			r.InstallationID = &installA
			r.CreatedAt = newest.Add(time.Hour)
		}, report, nil, ids)
		g.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.AccountID = tenantA
			r.InstallationID = &installA
			r.CreatedAt = newest
		}, report, ids, nil)

		base, degraded := g.s.groomingChurnBaseline(context.Background(), cur)
		if len(degraded) != 0 {
			t.Fatalf("degraded = %v, want none", degraded)
		}
		if g.rr.lastFilter.AccountID != tenantA || g.rr.lastFilter.Repo != churnRepo {
			t.Errorf("ListRuns filter = {Repo:%q AccountID:%q}, want {%q, %q} — the query itself must be tenant-scoped, not only the walk",
				g.rr.lastFilter.Repo, g.rr.lastFilter.AccountID, churnRepo, tenantA)
		}
		if len(base.Entries) != len(ids) {
			t.Fatalf("baseline entries = %d, want %d from the SAME-tenant prior run", len(base.Entries), len(ids))
		}
		for _, id := range ids {
			if base.Entries[id].Disposition != workmgmt.GroomingDispositionApplied {
				t.Errorf("entry %s disposition = %q, want applied", id, base.Entries[id].Disposition)
			}
		}
	})
}

// TestGroomingIngest_MostRecentPriorRunWins pins the RECENCY property the
// baseline's contract claims. The two prior runs carry DIVERGENT dispositions
// for the same entry — the older rejected it, the newer applied it — so the
// suppression REASON identifies which baseline was adopted. Diffing against the
// older one would suppress against stale dispositions, the unsafe direction for
// a suppression control.
//
// The fake returns runs in insertion order (oldest first), so this discriminates
// the explicit created_at sort rather than the query's ORDER BY convention.
func TestGroomingIngest_MostRecentPriorRunWins(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1}, []float64{9.5}, "missing_estimate")
	entry := report.HygieneDefects[0].ID
	older := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	f.seedPriorGroomingRunAs(t, func(r *run.Run) { r.CreatedAt = older },
		report, nil, []string{entry}) // rejected, seeded FIRST
	f.seedPriorGroomingRunAs(t, func(r *run.Run) { r.CreatedAt = older.Add(48 * time.Hour) },
		report, []string{entry}, nil) // applied, seeded SECOND

	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	var reason string
	for _, s := range v.Suppressed {
		if s.EntryID == entry {
			reason = s.Reason
		}
	}
	if reason != workmgmt.GroomingSuppressAlreadyApplied {
		t.Errorf("suppression reason = %q, want %q — the baseline came from the OLDER prior run, whose dispositions are stale",
			reason, workmgmt.GroomingSuppressAlreadyApplied)
	}
}

// TestGroomingIngest_ANewerRunIsNotABaseline pins the PREDECESSOR boundary
// (#2240 fix-up). Excluding the current run's own id is not the same rule as
// "a PRIOR run": ListRuns returns every same-tenant run on the repo whenever
// it was created, so a grooming run that started LATER and settled FIRST — the
// concurrency case — would sort newest-first and let FUTURE dispositions
// suppress findings in this earlier run.
func TestGroomingIngest_ANewerRunIsNotABaseline(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)

	t.Run("a run created after this one cannot suppress anything", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		// A concurrently-started grooming run that COMPLETED FIRST, having
		// applied every entry. Adopting it would suppress this whole report
		// against dispositions that did not exist when this run began.
		f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime.Add(time.Minute)
		}, report, ids, nil)

		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if v.Summary.BaselineEntries != 0 {
			t.Errorf("baseline_entries = %d, want 0 — a run created AFTER this one became the baseline", v.Summary.BaselineEntries)
		}
		if v.Summary.Proposed != 3 || v.Summary.NoChangesProposed {
			t.Errorf("proposed=%v no_changes=%v; a FUTURE run's dispositions suppressed this run's findings",
				v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
		}
	})

	t.Run("the newest PRECEDING run wins over a newer concurrent one", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		entry := report.HygieneDefects[0].ID
		// Divergent dispositions for the SAME entry, so the suppression reason
		// identifies which run was adopted rather than two identical baselines
		// masking the difference. The later run is seeded FIRST, so insertion
		// order cannot be what produces the right answer.
		f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime.Add(time.Minute)
		}, report, nil, ids) // AFTER this run: rejected
		f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime.Add(-time.Hour)
		}, report, ids, nil) // BEFORE this run: applied

		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		var reason string
		for _, sp := range v.Suppressed {
			if sp.EntryID == entry {
				reason = sp.Reason
			}
		}
		if reason != workmgmt.GroomingSuppressAlreadyApplied {
			t.Errorf("suppression reason = %q, want %q — the baseline came from a run created AFTER this one",
				reason, workmgmt.GroomingSuppressAlreadyApplied)
		}
	})

	// The ID half of the total order, one call below the HTTP boundary because
	// the fixture cannot choose the seeded run's uuid: at an IDENTICAL
	// created_at the declared order is id DESC, so a candidate whose id sorts
	// ABOVE the current run's is "newer" and must be refused too. Without the
	// tiebreak the predicate would be a bare created_at compare and a
	// same-instant peer would be adopted non-deterministically.
	t.Run("an equal created_at is broken by id, not adopted", func(t *testing.T) {
		curStage := uuid.New()
		f := newChurnFixture(t, uuid.New(), curStage)
		priorID := f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime
		}, report, ids, nil)

		below := &run.Run{ID: uuidJustBelow(t, priorID), Repo: churnRepo, CreatedAt: churnCurrentRunTime}
		base, degraded := f.s.groomingChurnBaseline(context.Background(), below)
		if len(degraded) != 0 {
			t.Fatalf("degraded = %v, want none", degraded)
		}
		if len(base.Entries) != 0 {
			t.Errorf("baseline entries = %d, want 0 — a same-instant run with a HIGHER id sorts newer and is not a predecessor", len(base.Entries))
		}
		// The positive control: the same fixture with a current id ABOVE the
		// candidate's DOES adopt it, so the assertion above is the tiebreak and
		// not a fixture that can never produce a baseline.
		above := &run.Run{ID: uuidJustAbove(t, priorID), Repo: churnRepo, CreatedAt: churnCurrentRunTime}
		base, degraded = f.s.groomingChurnBaseline(context.Background(), above)
		if len(degraded) != 0 {
			t.Fatalf("degraded = %v, want none", degraded)
		}
		if len(base.Entries) != len(ids) {
			t.Errorf("baseline entries = %d, want %d — a same-instant run with a LOWER id IS a predecessor", len(base.Entries), len(ids))
		}
	})
}

// uuidJustBelow / uuidJustAbove return the adjacent uuid in the big-endian
// byte order uuid.String() renders positionally, so numeric adjacency IS string
// adjacency. Deterministic, unlike generating uuids until one happens to sort
// the right way.
func uuidJustBelow(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	out := id
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] > 0 {
			out[i]--
			return out
		}
		out[i] = 0xff
	}
	t.Fatalf("no uuid sorts below %s", id)
	return uuid.Nil
}

func uuidJustAbove(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	out := id
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xff {
			out[i]++
			return out
		}
		out[i] = 0
	}
	t.Fatalf("no uuid sorts above %s", id)
	return uuid.Nil
}

// TestGroomingIngest_AnUnsettledRunDoesNotShadowASettledOne pins the second
// half of the "prior run" boundary (#2240 fix-up): a grooming run whose report
// was never decided — or whose applies all failed — records NO disposition, so
// its baseline is entry-less. Adopting it because it is merely the most recent
// would re-propose the entire unchanged backlog after every undecided
// intermediate run, which is exactly the convergence break AC1/AC4 exist to
// prevent. The last SETTLED baseline must survive it.
func TestGroomingIngest_AnUnsettledRunDoesNotShadowASettledOne(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)

	// Older: decided AND applied — the real baseline.
	f.seedPriorGroomingRunAs(t, func(r *run.Run) {
		r.CreatedAt = churnCurrentRunTime.Add(-48 * time.Hour)
	}, report, ids, nil)
	// Newer: shipped the same report and settled NOTHING (undecided, or every
	// apply failed). It is the most recent prior run by created_at.
	f.seedPriorGroomingRunAs(t, func(r *run.Run) {
		r.CreatedAt = churnCurrentRunTime.Add(-time.Hour)
	}, report, nil, nil)

	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.BaselineEntries != len(ids) {
		t.Errorf("baseline_entries = %d, want %d — the dispositionless newer run shadowed the settled older one",
			v.Summary.BaselineEntries, len(ids))
	}
	if !v.Summary.NoChangesProposed || v.Summary.Proposed != 0 {
		t.Errorf("proposed=%v no_changes=%v; an unchanged backlog after an UNDECIDED intermediate run must still converge to nothing proposed",
			v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
	}
	// The hygiene entry is the one whose reason IDENTIFIES the disposition: an
	// unchanged ordering entry is suppressed at the ordering threshold without
	// ever reading its disposition, so only this one distinguishes "adopted the
	// settled baseline" from "adopted a baseline at all".
	hygiene := report.HygieneDefects[0].ID
	var reason string
	for _, sp := range v.Suppressed {
		if sp.EntryID == hygiene {
			reason = sp.Reason
		}
	}
	if reason != workmgmt.GroomingSuppressAlreadyApplied {
		t.Errorf("entry %s suppression reason = %q, want %q from the last SETTLED baseline", hygiene, reason, workmgmt.GroomingSuppressAlreadyApplied)
	}
}

// TestGroomingIngest_ChurnDegradeModes covers each defensive branch in the
// wiring, asserting the NAMED degrade reason AND that the guard fell toward
// PROPOSING — a suppression control that cannot read its inputs must never
// suppress.
func TestGroomingIngest_ChurnDegradeModes(t *testing.T) {
	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "")

	t.Run("conventions unreadable resolves the shipped defaults", func(t *testing.T) {
		withConventions(t, workmgmt.Conventions{}, errors.New("conventions boom"))
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: an unreadable config must not fail the ingest", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "conventions_unreadable") {
			t.Errorf("degraded = %v, want conventions_unreadable named", v.Degraded)
		}
		if v.Thresholds.MinRankMovement != workmgmt.DefaultGroomingMinRankMovement {
			t.Errorf("min_rank_movement = %d, want the shipped default", v.Thresholds.MinRankMovement)
		}
	})

	t.Run("prior-run lookup failure proposes everything", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		f.rr.listRunsErr = errors.New("list runs boom")
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "baseline_list_runs_failed") {
			t.Errorf("degraded = %v, want baseline_list_runs_failed named", v.Degraded)
		}
		if v.Summary.Proposed != 2 || v.Summary.NoChangesProposed {
			t.Errorf("proposed = %v no_changes=%v; an unreadable baseline must fall toward PROPOSING",
				v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
		}
	})

	t.Run("unknown run resolves no repo", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		delete(f.rr.getRuns, runID)
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "run_lookup_failed") || !churnDegradedHas(v.Degraded, "baseline_repo_unknown") {
			t.Errorf("degraded = %v, want run_lookup_failed and baseline_repo_unknown", v.Degraded)
		}
		if v.Summary.Proposed != 2 {
			t.Errorf("proposed = %v, want everything proposed", v.Summary.ProposedIDs)
		}
	})

	t.Run("an unparseable prior report is not a baseline", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		priorRun, priorStage := uuid.New(), uuid.New()
		f.rr.getRuns[priorRun] = &run.Run{ID: priorRun, Repo: churnRepo, WorkflowID: churnWorkflow}
		f.rr.stagesByRunID[priorRun] = []*run.Stage{{ID: priorStage, RunID: priorRun, Type: run.StageTypePlan}}
		f.rr.byRepo[churnRepo] = []*run.Run{f.rr.getRuns[priorRun]}
		sv := plan.GroomingReportVersion
		if _, err := f.ar.Create(context.Background(), artifact.CreateParams{
			StageID: priorStage, Kind: artifact.KindGroomingReport, SchemaVersion: &sv,
			Content: []byte(`{"kind":"grooming_report","report_version":"nope"}`), ContentHash: "deadbeef",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if v.Summary.BaselineEntries != 0 || v.Summary.Proposed != 2 {
			t.Errorf("baseline_entries=%d proposed=%v; an unreadable prior report must not become a suppressing baseline",
				v.Summary.BaselineEntries, v.Summary.ProposedIDs)
		}
		// And it is NAMED: an unreadable input is a degrade, not the silent
		// "no prior run" answer it is otherwise indistinguishable from.
		if !churnDegradedHas(v.Degraded, "baseline_prior_report_unreadable") {
			t.Errorf("degraded = %v, want baseline_prior_report_unreadable named", v.Degraded)
		}
	})

	t.Run("an undecodable apply record contributes no disposition", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		priorRun := f.seedPriorGroomingRun(t, report, nil, nil)
		// entry_id and outcome decode cleanly; skip_reason is the wrong TYPE, so
		// encoding/json returns an UnmarshalTypeError with the earlier fields
		// already populated. Without the error check the half-decoded record
		// would register an APPLIED disposition and suppress a real proposal.
		bad := []byte(`{"entry_id":"` + report.Ordering[0].ID + `","outcome":"applied","skip_reason":42}`)
		if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: priorRun, Category: workmgmt.GroomingMutationAppliedCategory, Payload: bad,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if v.Summary.Proposed != 2 {
			t.Errorf("proposed = %v, want everything: an undecodable apply row records no disposition", v.Summary.ProposedIDs)
		}
	})
}

// TestGroomingIngest_ChurnAuditFailureIs500 pins the fail-closed half: a guard
// whose verdict silently vanished is indistinguishable from a guard that never
// ran, so an audit-write failure surfaces 500 for the runner to retry.
func TestGroomingIngest_ChurnAuditFailureIs500(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)
	f.s.cfg.AuditRepo = &churnAuditFailFake{auditFake: f.au}

	report := churnReport([]int{1}, []float64{9.5}, "")
	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the churn verdict cannot be recorded", code)
	}
}

// churnAuditFailFake lets the report_recorded append through and fails only the
// churn verdict, so the 500 above is attributable to the guard's own write.
type churnAuditFailFake struct {
	*auditFake
}

func (f *churnAuditFailFake) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == workmgmt.GroomingChurnFilteredCategory {
		return nil, errors.New("churn audit boom")
	}
	return f.auditFake.AppendChained(ctx, p)
}

// churnDegradedHas reports whether a named degrade reason is present.
func churnDegradedHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestGroomingIngest_RetryHealsAGappedChurnVerdict pins the retry-path heal
// specifically. Create-then-audit is two non-atomic steps: a first POST whose
// artifact Create succeeded and whose churn append failed leaves the report
// durable and the guard's verdict absent — and a naive retry would take the
// GetByHash short-circuit and report success having never recorded it.
//
// The gapped state is seeded BY CONSTRUCTION (the artifact row is created
// directly on the repo, with no audit entries) rather than by driving a failing
// first POST, so the RED lands on the heal assertion and not on fixture setup.
func TestGroomingIngest_RetryHealsAGappedChurnVerdict(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "")
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sv := plan.GroomingReportVersion
	if _, cerr := f.ar.Create(context.Background(), artifact.CreateParams{
		StageID: stageID, Kind: artifact.KindGroomingReport, SchemaVersion: &sv,
		Content: body, ContentHash: sha256Hex(body),
	}); cerr != nil {
		t.Fatalf("seed gapped artifact: %v", cerr)
	}
	if n := countChurnEntries(f.au); n != 0 {
		t.Fatalf("fixture: churn entries = %d, want 0 (the gapped state)", n)
	}

	priv, _ := f.sf.issue(t, runID)
	w := shipPlanRequest(t, f.s, runID, stageID, priv, body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the idempotent retry path)", w.Code)
	}
	if n := countChurnEntries(f.au); n != 1 {
		t.Fatalf("churn entries after the healing retry = %d, want 1; a retry that reports success with the verdict still absent is the gap this heal closes", n)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.Proposed != 2 {
		t.Errorf("healed verdict proposed = %v, want the real guard output, not a placeholder", v.Summary.ProposedIDs)
	}

	// The heal's OWN failure branch, which the success path above cannot reach:
	// the retry finds the report durable and the verdict absent, and the write
	// that would close the gap fails. Reporting 200 there would be the same
	// silent gap in a different disguise, so it must 500 for a further retry to
	// re-heal — the same fail-closed rule the first-write path obeys.
	t.Run("a failing heal write surfaces 500", func(t *testing.T) {
		gapRun, gapStage := uuid.New(), uuid.New()
		g := newChurnFixture(t, gapRun, gapStage)
		if _, cerr := g.ar.Create(context.Background(), artifact.CreateParams{
			StageID: gapStage, Kind: artifact.KindGroomingReport, SchemaVersion: &sv,
			Content: body, ContentHash: sha256Hex(body),
		}); cerr != nil {
			t.Fatalf("seed gapped artifact: %v", cerr)
		}
		if n := countChurnEntries(g.au); n != 0 {
			t.Fatalf("fixture: churn entries = %d, want 0 (the gapped state)", n)
		}
		g.s.cfg.AuditRepo = &churnAuditFailFake{auditFake: g.au}

		gpriv, _ := g.sf.issue(t, gapRun)
		gw := shipPlanRequest(t, g.s, gapRun, gapStage, gpriv, body, "")
		if gw.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 when the retry cannot heal the missing churn verdict", gw.Code)
		}
		// Attributable to the HEAL branch, not to the first-write one: both
		// return 500, and only the message distinguishes which ran.
		if !strings.Contains(gw.Body.String(), "heal grooming churn audit entry failed") {
			t.Errorf("body = %s, want the heal-path churn failure", gw.Body.String())
		}
		if n := countChurnEntries(g.au); n != 0 {
			t.Errorf("churn entries = %d, want 0 — the 500 must reflect a verdict that was NOT recorded", n)
		}
	})
}

// churnArtifactListFailFake fails ListForStage for ONE stage and delegates
// everything else, so a single prior candidate becomes unreadable while the
// current run's own ingest (GetByHash / Create) and every older candidate keep
// working.
type churnArtifactListFailFake struct {
	*fakeArtifactRepo
	failStage uuid.UUID
}

func (f *churnArtifactListFailFake) ListForStage(ctx context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	if stageID == f.failStage {
		return nil, errors.New("artifact list boom")
	}
	return f.fakeArtifactRepo.ListForStage(ctx, stageID)
}

// churnAuditListFailFake fails the disposition query for ONE prior run and
// delegates everything else — including AppendChained, so the verdict this
// test reads still lands in the shared auditFake.
type churnAuditListFailFake struct {
	*auditFake
	failRun uuid.UUID
}

func (f *churnAuditListFailFake) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if runID == f.failRun && category == workmgmt.GroomingMutationAppliedCategory {
		return nil, errors.New("audit list boom")
	}
	return f.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestGroomingIngest_AnUnreadableBaselineInputDoesNotFallBackToAnOlderRun is
// the multi-candidate error path (#2240 fix-up). Every other degrade test has
// exactly ONE candidate, where "failed to read" and "no baseline" are
// indistinguishable in effect — both propose. With a SETTLED OLDER candidate
// behind the failing one they diverge sharply: if a retrieval failure resolves
// to "this run shipped nothing", the scan walks past it and adopts the older
// run's dispositions, and the operator's since-superseded decisions suppress
// the current report. That is a suppression decided by stale state, the one
// direction this guard must never fail in.
//
// Both baseline inputs get the same treatment: the report retrieval and the
// disposition query.
func TestGroomingIngest_AnUnreadableBaselineInputDoesNotFallBackToAnOlderRun(t *testing.T) {
	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)

	// seedTwo installs the OLDER settled candidate (which would suppress the
	// entire report if adopted) and a NEWER one, returning the newer run's id
	// and its plan stage so the caller can break exactly one of its inputs.
	seedTwo := func(t *testing.T, f *churnFixture) (uuid.UUID, uuid.UUID) {
		t.Helper()
		f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime.Add(-48 * time.Hour)
		}, report, ids, nil)
		newer := f.seedPriorGroomingRunAs(t, func(r *run.Run) {
			r.CreatedAt = churnCurrentRunTime.Add(-time.Hour)
		}, report, ids, nil)
		return newer, f.rr.stagesByRunID[newer][0].ID
	}

	// The positive control, asserted rather than assumed: with BOTH candidates
	// readable this fixture suppresses everything. So the two subtests below
	// cannot pass by proposing for some unrelated reason.
	t.Run("control: both candidates readable suppresses everything", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		seedTwo(t, f)
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if !v.Summary.NoChangesProposed || v.Summary.Proposed != 0 {
			t.Fatalf("proposed=%v no_changes=%v; the control must suppress, or the failure subtests prove nothing",
				v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
		}
	})

	t.Run("an unreadable prior report stops the scan", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		_, newerStage := seedTwo(t, f)
		f.s.cfg.ArtifactRepo = &churnArtifactListFailFake{fakeArtifactRepo: f.ar, failStage: newerStage}

		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201: an unreadable baseline input must not fail the ingest", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "baseline_prior_report_unreadable") {
			t.Errorf("degraded = %v, want baseline_prior_report_unreadable named", v.Degraded)
		}
		if v.Summary.BaselineEntries != 0 || v.Summary.Proposed != len(ids) || v.Summary.NoChangesProposed {
			t.Errorf("baseline_entries=%d proposed=%v no_changes=%v; an unreadable NEWER candidate must not resolve to the OLDER run's dispositions",
				v.Summary.BaselineEntries, v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
		}
	})

	t.Run("an unreadable disposition query stops the scan", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		newerRun, _ := seedTwo(t, f)
		f.s.cfg.AuditRepo = &churnAuditListFailFake{auditFake: f.au, failRun: newerRun}

		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "baseline_dispositions_unreadable") {
			t.Errorf("degraded = %v, want baseline_dispositions_unreadable named", v.Degraded)
		}
		if v.Summary.BaselineEntries != 0 || v.Summary.Proposed != len(ids) || v.Summary.NoChangesProposed {
			t.Errorf("baseline_entries=%d proposed=%v no_changes=%v; a failed disposition query must not read as an unsettled run and fall back to an older baseline",
				v.Summary.BaselineEntries, v.Summary.ProposedIDs, v.Summary.NoChangesProposed)
		}
	})
}

// seedNonGroomingRuns fills the scan window with runs of ANOTHER workflow on
// the SAME repo — the implement/review/deploy traffic a real repo is dominated
// by. They are appended BEFORE the grooming candidate, so an unscoped window
// truncates at the limit and never reaches it.
func (f *churnFixture) seedNonGroomingRuns(n int) {
	for i := 0; i < n; i++ {
		id := uuid.New()
		f.rr.byRepo[f.repo] = append(f.rr.byRepo[f.repo], &run.Run{
			ID: id, Repo: f.repo, WorkflowID: "feature_change",
			CreatedAt: churnCurrentRunTime.Add(-time.Duration(i+1) * time.Minute),
		})
	}
}

// TestGroomingIngest_BusyRepoDoesNotPushTheBaselineOutOfTheWindow pins the
// scan-window scoping (#2240 fix-up). The window is capped as a runaway guard,
// and a cap is only safe if what it bounds is dense in grooming runs — which
// repo+account is NOT, because implement, review and deploy runs share the
// repo string. Without the workflow predicate the prior grooming run falls off
// the end of a busy repo's window and the whole unchanged backlog is
// re-proposed: a silent loss of AC1/AC4 convergence.
func TestGroomingIngest_BusyRepoDoesNotPushTheBaselineOutOfTheWindow(t *testing.T) {
	withConventions(t, workmgmt.Default(), nil)
	runID, stageID := uuid.New(), uuid.New()
	f := newChurnFixture(t, runID, stageID)

	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "missing_estimate")
	var ids []string
	for _, e := range report.Ordering {
		ids = append(ids, e.ID)
	}
	ids = append(ids, report.HygieneDefects[0].ID)

	// A full window's worth of non-grooming traffic, then the settled prior
	// grooming run behind all of it.
	f.seedNonGroomingRuns(groomingBaselineRunScanLimit)
	f.seedPriorGroomingRunAs(t, func(r *run.Run) {
		r.CreatedAt = churnCurrentRunTime.Add(-time.Hour)
	}, report, ids, nil)

	if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if got := f.rr.lastFilter.WorkflowID; got != churnWorkflow {
		t.Errorf("ListRuns filter workflow_id = %q, want %q — the scan window must be workflow-scoped, not repo-only", got, churnWorkflow)
	}
	v := churnVerdict(t, f.au)
	if v.Summary.BaselineEntries != len(ids) || !v.Summary.NoChangesProposed || v.Summary.Proposed != 0 {
		t.Errorf("baseline_entries=%d proposed=%v no_changes=%v; %d intervening non-grooming runs pushed the prior grooming run out of the scan window",
			v.Summary.BaselineEntries, v.Summary.ProposedIDs, v.Summary.NoChangesProposed, groomingBaselineRunScanLimit)
	}
}

// TestGroomingIngest_AFullScanWindowIsNamed pins the residual the workflow
// scoping narrows but cannot remove: a repo with more grooming runs than the
// cap can still hide its baseline behind the window. Resolving empty there is
// the safe direction (everything proposed), but resolving empty SILENTLY is
// not — it reads identically to "first-ever grooming run", and every other
// input-resolution degrade in this function is named.
func TestGroomingIngest_AFullScanWindowIsNamed(t *testing.T) {
	report := churnReport([]int{1, 2}, []float64{9.5, 8.0}, "")

	t.Run("a full window without a baseline is named", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		// Grooming-workflow runs that shipped no report: in the window, past
		// the workflow predicate, and contributing no baseline — so the scan
		// reaches the cap having resolved nothing.
		for i := 0; i < groomingBaselineRunScanLimit; i++ {
			f.rr.byRepo[churnRepo] = append(f.rr.byRepo[churnRepo], &run.Run{
				ID: uuid.New(), Repo: churnRepo, WorkflowID: churnWorkflow,
				CreatedAt: churnCurrentRunTime.Add(-time.Duration(i+1) * time.Minute),
			})
		}
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if !churnDegradedHas(v.Degraded, "baseline_scan_capped") {
			t.Errorf("degraded = %v, want baseline_scan_capped named", v.Degraded)
		}
		if v.Summary.Proposed != 2 {
			t.Errorf("proposed = %v, want everything: a capped scan resolves an empty baseline", v.Summary.ProposedIDs)
		}
	})

	t.Run("a first-ever run is not called capped", func(t *testing.T) {
		withConventions(t, workmgmt.Default(), nil)
		runID, stageID := uuid.New(), uuid.New()
		f := newChurnFixture(t, runID, stageID)
		if _, code := shipGrooming(t, f, runID, stageID, report); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		v := churnVerdict(t, f.au)
		if churnDegradedHas(v.Degraded, "baseline_scan_capped") {
			t.Errorf("degraded = %v; an empty scan is the CORRECT first-run answer, not a capped window", v.Degraded)
		}
	})
}
