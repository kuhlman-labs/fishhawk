package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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
func TestHandleShipPlan_GroomingReportFromNonPlanStage_FailsCategoryB(t *testing.T) {
	for _, stageType := range []run.StageType{run.StageTypeImplement, run.StageTypeReview, run.StageTypeAcceptance} {
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
