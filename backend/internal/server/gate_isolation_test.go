package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gateIsolationGolden reads one class member ("fallback" | "refused" |
// "container") of the SHARED cross-module golden
// testdata/wire/gate_isolation_evidence.json — the exact bytes the runner
// marshals into gate_evidence.gate_isolation (#2135).
func gateIsolationGolden(t *testing.T, class string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "gate_isolation_evidence.json"))
	if err != nil {
		t.Fatalf("read shared wire fixture: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode shared wire fixture: %v", err)
	}
	member, ok := m[class]
	if !ok {
		t.Fatalf("shared wire fixture has no %q member", class)
	}
	return member
}

// gateIsolationBundle packs a manifest (extra fields merged in) plus, when
// gateEvidence is non-empty, ONE gate_evidence event carrying it verbatim.
func gateIsolationBundle(t *testing.T, manifestExtra map[string]any, gateEvidence string) []byte {
	t.Helper()
	manifest := map[string]any{"bundle_schema": "v1"}
	for k, v := range manifestExtra {
		manifest[k] = v
	}
	mp, _ := json.Marshal(manifest)
	var raw bytes.Buffer
	ml, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(mp)})
	raw.Write(ml)
	raw.WriteByte('\n')
	if gateEvidence != "" {
		gl, err := json.Marshal(map[string]any{"seq": 2, "kind": "gate_evidence", "data": json.RawMessage(gateEvidence)})
		if err != nil {
			t.Fatalf("marshal gate_evidence line: %v", err)
		}
		raw.Write(gl)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return gz.Bytes()
}

// isolationOnlyEvidence is a gate_evidence payload carrying ONLY the
// isolation member — the refusal shape, where no gate ran.
func isolationOnlyEvidence(t *testing.T, class string) string {
	return `{"gate_isolation":` + string(gateIsolationGolden(t, class)) + `}`
}

// gateIsolationRows returns every appended gate_isolation_recorded row.
func gateIsolationRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range au.appended {
		if p.Category == CategoryGateIsolationRecorded {
			out = append(out, p)
		}
	}
	return out
}

func TestTraceUpload_RecordsGateIsolation(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback"))

	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	rows := gateIsolationRows(au)
	if len(rows) != 1 {
		t.Fatalf("gate_isolation_recorded rows = %d, want 1", len(rows))
	}
	if rows[0].StageID == nil || *rows[0].StageID != stageID {
		t.Errorf("row stage_id = %v, want stage-scoped %s", rows[0].StageID, stageID)
	}
	var p gateIsolationRecordedPayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.StageID != stageID.String() || p.ContentHash != sha256Hex(body) {
		t.Errorf("coordinates = (%q, %q), want (%s, %s)", p.StageID, p.ContentHash, stageID, sha256Hex(body))
	}
	if p.Class != "fallback" || p.Path != "clone" || !strings.Contains(p.ContainerUnavailable, "FISHHAWK_GATE_IMAGE is empty") {
		t.Errorf("evidence = %+v, want class fallback / path clone / container_unavailable naming the empty image", p.GateIsolationEvidence)
	}
}

// TestTraceUpload_GateIsolationDeclaredPayload (E51.3 / #2136, approval
// condition 5) pins the bundle-to-audit serialization boundary for the
// gate_container members of the shared golden: the gate_isolation_recorded
// payload carries image_digest (container_declared), build_context_digest
// (container_build) and declared_unhonored (fallback_declared_unhonored) as
// top-level wire keys with the runner's exact values. It reads the RAW payload
// keys, not the decoded struct, so a field dropped by the embed or renamed on
// the wire fails here. The E51.26 / #4046 credential posture rides the same
// boundary: every container member's payload carries credentials, and a
// non-container member's payload carries NO credentials key (absent).
func TestTraceUpload_GateIsolationDeclaredPayload(t *testing.T) {
	cases := []struct {
		member string
		keys   []string
		absent []string
	}{
		{"container", []string{"credentials"}, nil},
		{"container_declared", []string{"image_source", "image_digest", "image_id", "policy_warning", "credentials"}, nil},
		{"container_build", []string{"image_source", "image_id", "build_dockerfile", "build_context", "build_context_digest", "distinct_images_count", "credentials"}, nil},
		{"fallback_declared_unhonored", []string{"image_source", "declared_unhonored"}, []string{"credentials"}},
		{"refused", nil, []string{"credentials"}},
	}
	for _, c := range cases {
		t.Run(c.member, func(t *testing.T) {
			s, sf, _, au := newTraceServer(t)
			runID, stageID := uuid.New(), uuid.New()
			priv, _ := sf.issue(t, runID)
			body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, c.member))
			if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}
			rows := gateIsolationRows(au)
			if len(rows) != 1 {
				t.Fatalf("gate_isolation_recorded rows = %d, want 1", len(rows))
			}
			var got, want map[string]json.RawMessage
			if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if err := json.Unmarshal(gateIsolationGolden(t, c.member), &want); err != nil {
				t.Fatalf("decode golden: %v", err)
			}
			for _, k := range c.keys {
				if len(want[k]) == 0 {
					t.Fatalf("golden member %s lacks %q — the fixture no longer exercises it", c.member, k)
				}
				if !bytes.Equal(got[k], want[k]) {
					t.Errorf("payload %s = %s, want %s", k, got[k], want[k])
				}
			}
			for k, v := range want {
				if !bytes.Equal(got[k], v) {
					t.Errorf("payload %s = %s, want the golden's %s", k, got[k], v)
				}
			}
			for _, k := range c.absent {
				if v, ok := got[k]; ok {
					t.Errorf("payload carries %s = %s on a non-container member; want the key absent", k, v)
				}
			}
		})
	}
}

// TestTraceUpload_GateIsolationRawVariantOnly pins the raw-variant guard: a
// REDACTED upload of an evidence-bearing bundle records nothing. The redacted
// POST is the only upload, so the dedup cannot mask a hoisted call.
func TestTraceUpload_GateIsolationRawVariantOnly(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "refused"))

	if w := shipRequest(t, s, runID, stageID, "redacted", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rows := gateIsolationRows(au); len(rows) != 0 {
		t.Fatalf("redacted upload recorded %d gate_isolation_recorded rows, want 0", len(rows))
	}
}

func TestTraceUpload_NoGateIsolationWithoutEvidence(t *testing.T) {
	cases := map[string]string{
		"no gate_evidence event":           "",
		"gate_evidence without the member": `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}]}`,
		"unparsable gate_evidence":         `{"verify_runs":"not-a-list"}`,
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
			if rows := gateIsolationRows(au); len(rows) != 0 {
				t.Fatalf("recorded %d gate_isolation_recorded rows, want 0", len(rows))
			}
		})
	}
}

// TestTraceUpload_GateIsolationRePostDeduplicated pins condition 7's choice:
// a re-POST of the SAME raw bundle does not append a second row.
func TestTraceUpload_GateIsolationRePostDeduplicated(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback"))
	for i := 0; i < 2; i++ {
		if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
			t.Fatalf("POST %d status = %d:\n%s", i, w.Code, w.Body.String())
		}
	}
	if rows := gateIsolationRows(au); len(rows) != 1 {
		t.Fatalf("gate_isolation_recorded rows after a re-POST = %d, want 1 (deduplicated on stage_id + content_hash)", len(rows))
	}
	// A DIFFERENT stage uploading the same bytes is a distinct record.
	if w := shipRequest(t, s, runID, uuid.New(), "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("other-stage status = %d", w.Code)
	}
	if rows := gateIsolationRows(au); len(rows) != 2 {
		t.Fatalf("rows after another stage's upload = %d, want 2", len(rows))
	}
}

// TestTraceUpload_GateIsolationDedupReadErrorStillRecords pins the fail-open
// dedup: a failed read appends anyway (a lost record is worse than a duplicate).
func TestTraceUpload_GateIsolationDedupReadErrorStillRecords(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	au.listByCategoryErrCategory = CategoryGateIsolationRecorded
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback"))
	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rows := gateIsolationRows(au); len(rows) != 1 {
		t.Fatalf("rows with a failed dedup read = %d, want 1", len(rows))
	}
}

// TestTraceUpload_GateIsolationAppendFailureDoesNotUnwind pins best-effort: a
// failed append never fails the upload.
func TestTraceUpload_GateIsolationAppendFailureDoesNotUnwind(t *testing.T) {
	s, sf, _, au := newTraceServer(t)
	au.appendErrCategory = CategoryGateIsolationRecorded
	runID, stageID := uuid.New(), uuid.New()
	priv, _ := sf.issue(t, runID)
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback"))
	if w := shipRequest(t, s, runID, stageID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 despite the failed append:\n%s", w.Code, w.Body.String())
	}
	if rows := gateIsolationRows(au); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 (append failed)", len(rows))
	}
}

func TestRecordGateIsolation_NilAuditRepoIsNoop(t *testing.T) {
	s, _, _, _ := newTraceServer(t)
	s.cfg.AuditRepo = nil
	// Must not panic.
	s.recordGateIsolation(context.Background(), uuid.New(), uuid.New(), "h",
		gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "fallback")))
}

// failSnoopRepo snapshots the audit chain at the instant the stage is
// transitioned to failed, so a test can prove what was on the chain BEFORE the
// stage failed.
type failSnoopRepo struct {
	*policyRunRepo
	au           *auditFake
	atFailed     []string
	failedAtSeqN int
}

func (r *failSnoopRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if to == run.StageStateFailed && r.atFailed == nil {
		r.au.mu.Lock()
		for _, p := range r.au.appended {
			r.atFailed = append(r.atFailed, p.Category)
		}
		r.failedAtSeqN = len(r.au.appended)
		r.au.mu.Unlock()
	}
	return r.policyRunRepo.TransitionStage(ctx, id, to, c)
}

// TestTraceUpload_GateIsolationRecordedOnAgentFailedBundle pins condition 2's
// backend half: a refused stage's agent-failed bundle still records its
// isolation path, and the row is on the chain BEFORE the stage fails. The fake
// chain is append-ordered (AppendChained assigns sequence in append order under
// the run row lock), so a row's chain sequence is its 1-based append index.
func TestTraceUpload_GateIsolationRecordedOnAgentFailedBundle(t *testing.T) {
	s, sf, repo, au := newPolicyTraceServer(t, nil)
	snoop := &failSnoopRepo{policyRunRepo: repo, au: au}
	s.cfg.RunRepo = snoop
	priv, _ := sf.issue(t, repo.runRow.ID)
	body := gateIsolationBundle(t, map[string]any{
		"agent_failed":         true,
		"agent_failure_reason": "gate refused: profile=hosted and the container path is unavailable",
	}, isolationOnlyEvidence(t, "refused"))

	if w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, body, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if repo.stage.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (the agent-failed branch ran)", repo.stage.State)
	}
	if !slices.Contains(snoop.atFailed, CategoryGateIsolationRecorded) {
		t.Fatalf("chain at the failed transition = %v; gate_isolation_recorded must already be on it", snoop.atFailed)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	isoSeq := 0
	for i, p := range au.appended {
		if p.Category == CategoryGateIsolationRecorded {
			isoSeq = i + 1
		}
	}
	if isoSeq == 0 || isoSeq > snoop.failedAtSeqN {
		t.Fatalf("gate_isolation_recorded sequence = %d, want <= %d (every row from the failure onward sits above it)", isoSeq, snoop.failedAtSeqN)
	}
	for i := snoop.failedAtSeqN; i < len(au.appended); i++ {
		if i+1 <= isoSeq {
			t.Errorf("stage-failure row %q at sequence %d is not above gate_isolation_recorded (%d)", au.appended[i].Category, i+1, isoSeq)
		}
	}
}

// TestIsolationOnlyEvidence_IsNotAGateVerdict pins condition 5's backend half:
// a gate_evidence carrying ONLY the isolation member (a refusal where no gate
// ran) yields no policy verification signal and renders no verify outcome in
// the implement-review prompt's gate-evidence section.
func TestIsolationOnlyEvidence_IsNotAGateVerdict(t *testing.T) {
	body := gateIsolationBundle(t, nil, isolationOnlyEvidence(t, "refused"))

	if sig := verificationSignalFromBundle(body); sig != nil {
		t.Errorf("verificationSignalFromBundle = %+v, want nil (no gate ran, so no verification evidence)", sig)
	}
	ev, err := bundle.ExtractGateEvidence(body)
	if err != nil {
		t.Fatalf("ExtractGateEvidence: %v", err)
	}
	if ev.GateIsolation == nil {
		t.Fatal("fixture lost its isolation member; the test would be vacuous")
	}
	pe := gateEvidenceForReview(ev, nil)
	if len(pe.VerifyRuns) != 0 || pe.VerifySummary != nil {
		t.Fatalf("review gate evidence = %+v, want no verify runs and no summary", pe)
	}
	got, err := prompt.Build("implement_review", prompt.Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: &plan.Plan{PlanVersion: "standard_v1", Summary: "isolation-only evidence"},
		Diff:         "- M pkg/bar/bar.go\n",
		GateEvidence: pe,
	})
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	for _, verdict := range []string{"Verify summary: outcome=", "outcome: passed", "Verify runs (committed-tree gate):\n"} {
		if strings.Contains(got, verdict) {
			t.Errorf("isolation-only evidence rendered a gate verdict %q in the review prompt", verdict)
		}
	}
}

// --- gate view -------------------------------------------------------------

// seedGateIsolationRow seeds one gate_isolation_recorded chain entry.
func seedGateIsolationRow(t *testing.T, au *auditFake, runID, stageID uuid.UUID, seq int64, payload json.RawMessage) {
	t.Helper()
	rid, sid := runID, stageID
	au.seeded = append(au.seeded, &audit.Entry{
		Sequence: seq, RunID: &rid, StageID: &sid,
		Category: CategoryGateIsolationRecorded, Payload: payload,
	})
}

// gateIsolationRowPayload composes a recorded-row payload around a golden
// member, exactly as recordGateIsolation does.
func gateIsolationRowPayload(t *testing.T, stageID uuid.UUID, class string) json.RawMessage {
	t.Helper()
	var ev bundle.GateIsolationEvidence
	if err := json.Unmarshal(gateIsolationGolden(t, class), &ev); err != nil {
		t.Fatalf("decode golden %s: %v", class, err)
	}
	b, err := json.Marshal(gateIsolationRecordedPayload{StageID: stageID.String(), ContentHash: "h-" + class, GateIsolationEvidence: ev})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func TestGateIsolationForRun_NilAuditRepoNoGap(t *testing.T) {
	s, _, _, _ := gateViewServer(t)
	s.cfg.AuditRepo = nil
	var resp gateViewResponse
	if got := s.gateIsolationForRun(context.Background(), uuid.New(), &resp); got != nil {
		t.Errorf("block = %+v, want nil", got)
	}
	if resp.HistoryIncomplete || len(resp.HistoryGaps) != 0 {
		t.Errorf("unconfigured audit repo recorded a gap: %v", resp.HistoryGaps)
	}
}
