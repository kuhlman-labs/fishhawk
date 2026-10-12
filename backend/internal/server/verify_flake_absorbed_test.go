package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// flakeAbsorbedEvidence returns a gate_evidence payload carrying a passed
// verify run plus the flake_absorbed member of the SHARED cross-module golden
// testdata/wire/verify_flake_absorbed_evidence.json — the exact bytes the
// runner marshals (#4229).
func flakeAbsorbedEvidence(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "verify_flake_absorbed_evidence.json"))
	if err != nil {
		t.Fatalf("read shared wire fixture: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode shared wire fixture: %v", err)
	}
	member, ok := m["flake_absorbed"]
	if !ok {
		t.Fatal("shared wire fixture has no flake_absorbed member")
	}
	return `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}],"flake_absorbed":` + string(member) + `}`
}

// verifyFlakeAbsorbedRows returns every appended verify_flake_absorbed row.
func verifyFlakeAbsorbedRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range au.appended {
		if p.Category == CategoryVerifyFlakeAbsorbed {
			out = append(out, p)
		}
	}
	return out
}

// TestTraceUpload_RecordsVerifyFlakeAbsorbed drives the REAL trace upload
// handler with the shared golden: one stage-scoped, system-actor row whose
// payload carries the bundle coordinates and every evidence field as top-level
// wire keys with the runner's exact values.
func TestTraceUpload_RecordsVerifyFlakeAbsorbed(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t))

	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	rows := verifyFlakeAbsorbedRows(au)
	if len(rows) != 1 {
		t.Fatalf("verify_flake_absorbed rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.StageID == nil || *row.StageID != stageID {
		t.Errorf("row stage_id = %v, want stage-scoped %s", row.StageID, stageID)
	}
	if row.ActorKind == nil || *row.ActorKind != audit.ActorKind("system") {
		t.Errorf("row actor = %v, want system", row.ActorKind)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &keys); err != nil {
		t.Fatalf("decode payload keys: %v", err)
	}
	for _, k := range []string{"stage_id", "content_hash", "iteration", "tree_sha", "packages", "package_dirs", "tests", "failed_modules", "patch_coverage"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("payload has no top-level %q key: %s", k, row.Payload)
		}
	}
	var p verifyFlakeAbsorbedPayload
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.StageID != stageID.String() || p.ContentHash != sha256Hex(body) {
		t.Errorf("coordinates = (%q, %q), want (%s, %s)", p.StageID, p.ContentHash, stageID, sha256Hex(body))
	}
	if p.TreeSHA != "c1e8a4f7b2d9e6a3c0f5b8d1e4a7c2f9b6d3e0a5" || p.PatchCoverage != "pass" || p.Iteration != 2 {
		t.Errorf("evidence identity = %+v", p.FlakeAbsorbedEvidence)
	}
	if !slices.Equal(p.Packages, []string{"github.com/kuhlman-labs/fishhawk/backend/internal/run", "github.com/kuhlman-labs/fishhawk/runner/internal/upload"}) ||
		!slices.Equal(p.PackageDirs, []string{"backend/internal/run", "runner/internal/upload"}) ||
		!slices.Equal(p.Tests, []string{"TestDispatchLiveness_HeartbeatWindow", "TestShipTrace_TerminalEgressBudget"}) ||
		!slices.Equal(p.FailedModules, []string{"backend", "runner"}) {
		t.Errorf("evidence lists = %+v", p.FlakeAbsorbedEvidence)
	}
}

// TestTraceUpload_VerifyFlakeAbsorbedRawVariantOnly pins the raw-variant guard:
// a REDACTED upload of an evidence-bearing bundle records nothing. The redacted
// POST is the only upload, so the dedup cannot mask a hoisted call.
func TestTraceUpload_VerifyFlakeAbsorbedRawVariantOnly(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t))

	if w := shipRequest(t, s, runID, stageID, "redacted", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rows := verifyFlakeAbsorbedRows(au); len(rows) != 0 {
		t.Fatalf("redacted upload recorded %d verify_flake_absorbed rows, want 0", len(rows))
	}
}

func TestTraceUpload_NoVerifyFlakeAbsorbedWithoutEvidence(t *testing.T) {
	cases := map[string]string{
		"no gate_evidence event":           "",
		"gate_evidence without the member": `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}]}`,
		"unparsable gate_evidence":         `{"flake_absorbed":"not-an-object"}`,
	}
	for name, ge := range cases {
		t.Run(name, func(t *testing.T) {
			s, sf, _, au := newTraceServer(t)
			runID, stageID := uuid.New(), uuid.New()
			priv, _ := sf.issue(t, runID)
			body := gateIsolationBundle(t, nil, ge)
			if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}
			if rows := verifyFlakeAbsorbedRows(au); len(rows) != 0 {
				t.Fatalf("recorded %d verify_flake_absorbed rows, want 0", len(rows))
			}
		})
	}
}

// TestTraceUpload_VerifyFlakeAbsorbedRePostDeduplicated: a re-POST of the SAME
// raw bundle appends no second row — read back from the audit repo after both
// calls (committed state) — while a different stage's upload is its own record.
func TestTraceUpload_VerifyFlakeAbsorbedRePostDeduplicated(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t))
	for i := 0; i < 2; i++ {
		if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
			t.Fatalf("POST %d status = %d:\n%s", i, w.Code, w.Body.String())
		}
	}
	if rows := verifyFlakeAbsorbedRows(au); len(rows) != 1 {
		t.Fatalf("verify_flake_absorbed rows after a re-POST = %d, want 1 (deduplicated on stage_id + content_hash)", len(rows))
	}
	if w := shipRequest(t, s, runID, uuid.New(), "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("other-stage status = %d", w.Code)
	}
	if rows := verifyFlakeAbsorbedRows(au); len(rows) != 2 {
		t.Fatalf("rows after another stage's upload = %d, want 2", len(rows))
	}
}

// TestTraceUpload_VerifyFlakeAbsorbedDedupReadErrorStillRecords pins the
// fail-open dedup: a failed read appends anyway.
func TestTraceUpload_VerifyFlakeAbsorbedDedupReadErrorStillRecords(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	au.listByCategoryErrCategory = CategoryVerifyFlakeAbsorbed
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t))
	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rows := verifyFlakeAbsorbedRows(au); len(rows) != 1 {
		t.Fatalf("rows with a failed dedup read = %d, want 1", len(rows))
	}
}

// TestTraceUpload_VerifyFlakeAbsorbedAppendFailureDoesNotUnwind pins
// best-effort: a failed append never fails the upload.
func TestTraceUpload_VerifyFlakeAbsorbedAppendFailureDoesNotUnwind(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	au.appendErrCategory = CategoryVerifyFlakeAbsorbed
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t))
	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 despite the failed append:\n%s", w.Code, w.Body.String())
	}
	if rows := verifyFlakeAbsorbedRows(au); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 (append failed)", len(rows))
	}
}

func TestRecordVerifyFlakeAbsorbed_NilAuditRepoIsNoop(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	s.cfg.AuditRepo = nil
	// Must not panic.
	s.recordVerifyFlakeAbsorbed(context.Background(), uuid.New(), uuid.New(), "h",
		gateIsolationBundle(t, nil, flakeAbsorbedEvidence(t)))
}

// TestCategoryVerifyFlakeAbsorbed_Registered: the emitted category is in the
// audit registry (the completeness sweep is the repo-wide guard; this names it).
func TestCategoryVerifyFlakeAbsorbed_Registered(t *testing.T) {
	if CategoryVerifyFlakeAbsorbed != "verify_flake_absorbed" || !audit.IsKnownCategory(CategoryVerifyFlakeAbsorbed) {
		t.Fatalf("%q is not a registered audit category", CategoryVerifyFlakeAbsorbed)
	}
}
