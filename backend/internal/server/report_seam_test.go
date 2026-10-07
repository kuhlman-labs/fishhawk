package server

// Tests for the shared proposal-report seam (report_seam.go, #4012). The
// upkeep wrappers' behavior is pinned by the UNCHANGED upkeep_* suites; these
// tests pin the seam's own parameterization (kind, category, family) and the
// controls a later role (comms) will rely on. Fakes are composed from the
// existing ones (groomingApplyAuditFake, groomingApplyApprovalRepo,
// ukApplyArtifactRepo, upkeepBindingFixture, udGitHubInstallServer) without
// modifying them. Every COUNTERFACTUAL below names the fixture state that makes
// the deletion observable; each was run RED and restored.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func newSeamAudit() *groomingApplyAuditFake {
	return &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake()}
}

// seamAppend appends one row of category with payload onto runID's chain and
// returns its sequence (groomingApplyAuditFake numbers rows 1-based).
func seamAppend(t *testing.T, au *groomingApplyAuditFake, runID uuid.UUID, category string, payload any) int64 {
	t.Helper()
	var body []byte
	switch p := payload.(type) {
	case []byte:
		body = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body = b
	}
	e, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: body,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e.Sequence
}

// seamCount counts the rows of category appended for runID.
func seamCount(au *groomingApplyAuditFake, runID uuid.UUID, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, p := range au.appended {
		if p.RunID == runID && p.Category == category {
			n++
		}
	}
	return n
}

func seamTotal(au *groomingApplyAuditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	return len(au.appended)
}

// seamFamilyAppender adds audit.FamilyWindowAppender to the fallback fake,
// recording the family each close was driven with and answering a fixed
// consumed set. It does NOT implement audit.UpkeepWindowAppender.
type seamFamilyAppender struct {
	*groomingApplyAuditFake
	mu         sync.Mutex
	families   []string
	categories []string
	consumed   []*audit.Entry
}

func (a *seamFamilyAppender) AppendChainedFamilyDispositionBatch(context.Context, string, string, []audit.ChainAppendParams) ([]*audit.Entry, error) {
	return nil, errors.New("seamFamilyAppender: batch not used by the settlement")
}

func (a *seamFamilyAppender) AppendChainedFamilyWindowClose(_ context.Context, family string, p audit.ChainAppendParams, _ string) (*audit.Entry, []*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.families = append(a.families, family)
	a.categories = append(a.categories, p.Category)
	return &audit.Entry{Sequence: 99, Category: p.Category}, a.consumed, nil
}

var _ audit.FamilyWindowAppender = (*seamFamilyAppender)(nil)

func seamStage() *run.Stage {
	return &run.Stage{ID: uuid.New(), RunID: uuid.New(), Type: run.StageTypePlan}
}

// ---------------------------------------------------------------------------
// Stage binding
// ---------------------------------------------------------------------------

func groomingScanSpec(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/workflow-v2-backlog-grooming.yaml")
	if err != nil {
		t.Fatalf("read backlog-grooming example: %v", err)
	}
	return b
}

// TestResolveStageArtifactBinding_KindParameterized: ONE cached spec whose plan
// stage produces grooming_report answers per the QUERIED kind.
// COUNTERFACTUAL: hard-code spec.ArtifactUpkeepReport in stageProducesKind's
// comparison. The fixture's spec declares only grooming_report, so the
// grooming leg then reads WorkflowDeclares=false → red (observed).
func TestResolveStageArtifactBinding_KindParameterized(t *testing.T) {
	ctx := context.Background()

	t.Run("queried kind is declared", func(t *testing.T) {
		s, rr, runRow, planStage, implStage, _ := upkeepBindingFixture(t, groomingScanSpec(t), "backlog_grooming", true)
		b, err := s.resolveStageArtifactBinding(ctx, runRow.ID, planStage, spec.ArtifactGroomingReport)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if want := (stageArtifactBinding{Resolved: true, WorkflowDeclares: true, StageDeclares: true}); b != want {
			t.Errorf("plan-stage binding = %+v, want %+v", b, want)
		}
		b, err = s.resolveStageArtifactBinding(ctx, runRow.ID, implStage, spec.ArtifactGroomingReport)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if want := (stageArtifactBinding{Resolved: true, WorkflowDeclares: true}); b != want {
			t.Errorf("implement-stage binding = %+v, want %+v", b, want)
		}
		if rr.listCalls() != 2 {
			t.Errorf("ListStagesForRun calls = %d, want 2", rr.listCalls())
		}
	})

	t.Run("other kind is not declared and lists no stages", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, groomingScanSpec(t), "backlog_grooming", false)
		b, err := s.resolveStageArtifactBinding(ctx, runRow.ID, planStage, spec.ArtifactUpkeepReport)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if want := (stageArtifactBinding{Resolved: true}); b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
		if rr.listCalls() != 0 {
			t.Errorf("ListStagesForRun calls = %d, want 0 for a workflow not declaring the kind", rr.listCalls())
		}
	})

	t.Run("unmappable stage", func(t *testing.T) {
		s, _, runRow, _, _, _ := upkeepBindingFixture(t, groomingScanSpec(t), "backlog_grooming", true)
		stray := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 7, Type: run.StageTypePlan}
		b, err := s.resolveStageArtifactBinding(ctx, runRow.ID, stray, spec.ArtifactGroomingReport)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if want := (stageArtifactBinding{Resolved: true, WorkflowDeclares: true, Undecidable: stageBindingStageUnmappable}); b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
	})

	t.Run("unresolved workflow", func(t *testing.T) {
		s, _, runRow, planStage, _, _ := upkeepBindingFixture(t, groomingScanSpec(t), "no_such_workflow", false)
		b, err := s.resolveStageArtifactBinding(ctx, runRow.ID, planStage, spec.ArtifactGroomingReport)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if want := (stageArtifactBinding{Undecidable: stageBindingWorkflowUnresolved}); b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
	})

	t.Run("list stages failure is returned", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, groomingScanSpec(t), "backlog_grooming", true)
		rr.listStagesErr = errors.New("store down")
		_, err := s.resolveStageArtifactBinding(ctx, runRow.ID, planStage, spec.ArtifactGroomingReport)
		if err == nil || !strings.Contains(err.Error(), "list stages for run "+runRow.ID.String()) {
			t.Errorf("err = %v, want the wrapped list-stages failure", err)
		}
	})
}

// TestStageRefusesOtherProposal: fails open only when nothing is resolvable,
// closed inside a declaring workflow on a declaring or unmappable stage.
func TestStageRefusesOtherProposal(t *testing.T) {
	cases := []struct {
		b    stageArtifactBinding
		want bool
	}{
		{stageArtifactBinding{}, false},
		{stageArtifactBinding{Undecidable: stageBindingWorkflowUnresolved}, false},
		{stageArtifactBinding{Resolved: true}, false},
		{stageArtifactBinding{Resolved: true, WorkflowDeclares: true}, false},
		{stageArtifactBinding{Resolved: true, WorkflowDeclares: true, StageDeclares: true}, true},
		{stageArtifactBinding{Resolved: true, WorkflowDeclares: true, Undecidable: stageBindingStageUnmappable}, true},
	}
	for _, c := range cases {
		if got := stageRefusesOtherProposal(c.b); got != c.want {
			t.Errorf("stageRefusesOtherProposal(%+v) = %v, want %v", c.b, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Ingest helpers
// ---------------------------------------------------------------------------

// TestProposalGuardBodyDetail_NamesTheGivenKind: every branch names the
// CALLER's report kind and honors the caller's allowlist.
// COUNTERFACTUAL: hard-code "upkeep_report" for reportKind in the parse-error
// branch. The fixture passes "comms_report", so the want string differs → red
// (observed).
func TestProposalGuardBodyDetail_NamesTheGivenKind(t *testing.T) {
	const kind = "comms_report"
	allowed := map[plan.ArtifactKind]bool{plan.ArtifactKindClarificationRequest: true}

	if got, want := proposalGuardBodyDetail([]byte(`{`), errors.New("boom"), kind, allowed),
		"the body is not a parseable comms_report (boom)"; got != want {
		t.Errorf("parse-error detail = %q, want %q", got, want)
	}
	if got, want := proposalGuardBodyDetail([]byte(`{"summary":"x"}`), nil, kind, allowed),
		`the body carries no top-level "kind", so it was read as a plan`; got != want {
		t.Errorf("kind-less detail = %q, want %q", got, want)
	}
	if got, want := proposalGuardBodyDetail([]byte(`{"kind":"upkeep_report"}`), nil, kind, allowed),
		`its top-level kind "upkeep_report" is a recognized artifact kind but is not allowed on a stage declaring produces: comms_report`; got != want {
		t.Errorf("recognized-but-disallowed detail = %q, want %q", got, want)
	}
	long := strings.Repeat("z", proposalGuardMaxKindBytes+10)
	got := proposalGuardBodyDetail([]byte(`{"kind":"`+long+`"}`), nil, kind, allowed)
	if want := fmt.Sprintf("its top-level kind %q is not a recognized artifact kind", long[:proposalGuardMaxKindBytes]+"...[truncated]"); got != want {
		t.Errorf("unknown-kind detail = %q, want %q", got, want)
	}
}

// TestRecordedReportArtifactAndRow_CategoryParameterized: rows of TWO report
// categories with different artifacts and sequences; each query answers from
// its own category only.
// COUNTERFACTUAL: hard-code audit.UpkeepReportRecordedCategory in
// recordedReportRow's list call. The fixture's newest upkeep row (u2) has the
// highest sequence and differs from the newest comms row (c2), so the comms
// query returns u2 → red (observed).
func TestRecordedReportArtifactAndRow_CategoryParameterized(t *testing.T) {
	ctx := context.Background()
	au := newSeamAudit()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	runID := uuid.New()
	u1, c1, c2, u2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, map[string]string{"artifact_id": u1.String()})
	c1Seq := seamAppend(t, au, runID, audit.CommsReportRecordedCategory, map[string]string{"artifact_id": c1.String()})
	seamAppend(t, au, runID, audit.CommsReportRecordedCategory, map[string]string{"artifact_id": c2.String()})
	seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, map[string]string{"artifact_id": u2.String()})

	for _, tc := range []struct {
		category string
		want     uuid.UUID
	}{
		{audit.CommsReportRecordedCategory, c2},
		{audit.UpkeepReportRecordedCategory, u2},
	} {
		id, found, err := s.recordedReportArtifact(ctx, runID, tc.category)
		if err != nil || !found || id != tc.want {
			t.Errorf("recordedReportArtifact(%s) = (%s, %v, %v), want (%s, true, nil)", tc.category, id, found, err, tc.want)
		}
	}

	row, err := s.recordedReportRow(ctx, runID, audit.CommsReportRecordedCategory, c1.String())
	if err != nil || row == nil || row.Sequence != c1Seq {
		t.Errorf("recordedReportRow(comms, c1) = (%+v, %v), want the row at sequence %d", row, err, c1Seq)
	}
	if _, err := s.recordedReportRow(ctx, runID, audit.CommsReportRecordedCategory, u1.String()); err == nil ||
		err.Error() != "no comms_report_recorded row names artifact "+u1.String() {
		t.Errorf("recordedReportRow(comms, upkeep artifact) err = %v, want the category-named no-row error", err)
	}

	t.Run("absent", func(t *testing.T) {
		id, found, err := s.recordedReportArtifact(ctx, uuid.New(), audit.CommsReportRecordedCategory)
		if err != nil || found || id != uuid.Nil {
			t.Errorf("absent = (%s, %v, %v), want (nil, false, nil)", id, found, err)
		}
	})
	t.Run("undecodable newest row", func(t *testing.T) {
		other := uuid.New()
		seq := seamAppend(t, au, other, audit.CommsReportRecordedCategory, []byte(`not json`))
		_, found, err := s.recordedReportArtifact(ctx, other, audit.CommsReportRecordedCategory)
		if found || err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("decode comms_report_recorded row %d: ", seq)) {
			t.Errorf("undecodable = (%v, %v), want a decode error naming row %d", found, err, seq)
		}
	})
	t.Run("unparseable artifact id", func(t *testing.T) {
		other := uuid.New()
		seq := seamAppend(t, au, other, audit.CommsReportRecordedCategory, map[string]string{"artifact_id": "nope"})
		_, found, err := s.recordedReportArtifact(ctx, other, audit.CommsReportRecordedCategory)
		if found || err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf(`comms_report_recorded row %d names artifact "nope": `, seq)) {
			t.Errorf("unparseable = (%v, %v), want the names-artifact error for row %d", found, err, seq)
		}
	})
	t.Run("list failure", func(t *testing.T) {
		boom := errors.New("boom")
		bad := New(Config{Addr: "127.0.0.1:0", AuditRepo: &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake(), listErr: boom}})
		_, _, err := bad.recordedReportArtifact(ctx, runID, audit.CommsReportRecordedCategory)
		if err == nil || err.Error() != "list comms_report_recorded rows: boom" || !errors.Is(err, boom) {
			t.Errorf("list failure = %v, want %q wrapping the store error", err, "list comms_report_recorded rows: boom")
		}
	})
}

// TestUpkeepRecordedReadsPinErrorText (approval condition 2): the migrated
// latestUpkeepReport, upkeepRecordedArtifact and upkeepRecordedRow keep their
// pre-seam error strings byte-for-byte.
// COUNTERFACTUAL: in latestUpkeepReport wrap the seam's error directly
// (fmt.Errorf("list upkeep_report_recorded rows for run %s: %w", runID, err))
// instead of le.Err. The fixture's list failure then reads
// "…for run X: list upkeep_report_recorded rows: boom", a double wrap → red
// (observed).
func TestUpkeepRecordedReadsPinErrorText(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	t.Run("list failure text", func(t *testing.T) {
		boom := errors.New("boom")
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake(), listErr: boom}})
		_, err := s.latestUpkeepReport(ctx, runID)
		if want := fmt.Sprintf("list upkeep_report_recorded rows for run %s: boom", runID); err == nil || err.Error() != want || !errors.Is(err, boom) {
			t.Errorf("latestUpkeepReport list failure = %v, want %q wrapping the store error", err, want)
		}
		_, _, err = s.upkeepRecordedArtifact(ctx, runID)
		if want := "list upkeep_report_recorded rows: boom"; err == nil || err.Error() != want || !errors.Is(err, boom) {
			t.Errorf("upkeepRecordedArtifact list failure = %v, want %q", err, want)
		}
		_, err = s.upkeepRecordedRow(ctx, runID, uuid.NewString())
		if want := "list upkeep_report_recorded rows: boom"; err == nil || err.Error() != want || !errors.Is(err, boom) {
			t.Errorf("upkeepRecordedRow list failure = %v, want %q", err, want)
		}
	})

	t.Run("wrong kind names the selected row's sequence", func(t *testing.T) {
		au := newSeamAudit()
		arts := &ukApplyArtifactRepo{byID: map[uuid.UUID]*artifact.Artifact{}, byStage: map[uuid.UUID][]*artifact.Artifact{}}
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ArtifactRepo: arts})
		older := &artifact.Artifact{ID: uuid.New(), StageID: uuid.New(), Kind: artifact.KindUpkeepReport}
		wrong := &artifact.Artifact{ID: uuid.New(), StageID: uuid.New(), Kind: artifact.KindGroomingReport}
		arts.add(older)
		arts.add(wrong)
		seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, map[string]string{"artifact_id": older.ID.String()})
		seq := seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, map[string]string{"artifact_id": wrong.ID.String()})
		_, err := s.latestUpkeepReport(ctx, runID)
		want := fmt.Sprintf("artifact %s named by upkeep_report_recorded row %d has kind %q, want %q",
			wrong.ID, seq, artifact.KindGroomingReport, artifact.KindUpkeepReport)
		if err == nil || err.Error() != want {
			t.Errorf("wrong-kind err = %v, want %q", err, want)
		}
	})

	t.Run("undecodable and unparseable rows", func(t *testing.T) {
		au := newSeamAudit()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
		seq := seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, []byte(`not json`))
		_, err := s.latestUpkeepReport(ctx, runID)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("decode upkeep_report_recorded row %d: ", seq)) {
			t.Errorf("undecodable err = %v", err)
		}
		seq = seamAppend(t, au, runID, audit.UpkeepReportRecordedCategory, map[string]string{"artifact_id": "nope"})
		_, err = s.latestUpkeepReport(ctx, runID)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf(`upkeep_report_recorded row %d names artifact "nope": `, seq)) {
			t.Errorf("unparseable err = %v", err)
		}
	})

	t.Run("absent is the sentinel", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newSeamAudit()})
		if _, err := s.latestUpkeepReport(ctx, uuid.New()); !errors.Is(err, errUpkeepReportAbsent) {
			t.Errorf("absent err = %v, want errUpkeepReportAbsent", err)
		}
	})
}

// TestDecodeStrictSingleJSONBody: unknown keys at every depth and trailing
// content are refused with the CALLER's two messages.
// COUNTERFACTUAL: delete dec.DisallowUnknownFields(). The nested-unknown-key
// fixture ({"a":{"c":1}}) then decodes cleanly → returns true → red (observed).
func TestDecodeStrictSingleJSONBody(t *testing.T) {
	const shapeMsg, trailingMsg = "SHAPE-MESSAGE", "TRAILING-MESSAGE"
	s := New(Config{Addr: "127.0.0.1:0"})
	type inner struct {
		B int `json:"b"`
	}
	type body struct {
		A inner `json:"a"`
	}
	call := func(raw string, nilBody bool) (bool, *httptest.ResponseRecorder, body) {
		var dst body
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(raw))
		if nilBody {
			r.Body = nil
		}
		w := httptest.NewRecorder()
		ok := s.decodeStrictSingleJSONBody(w, r, &dst, shapeMsg, trailingMsg)
		return ok, w, dst
	}

	if ok, w, _ := call(`{"a":{"c":1}}`, false); ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), shapeMsg) {
		t.Errorf("nested unknown key: ok=%v code=%d body=%s, want 400 with the shape message", ok, w.Code, w.Body.String())
	}
	if ok, w, _ := call(`{"a":{"b":1}} {}`, false); ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), trailingMsg) {
		t.Errorf("trailing content: ok=%v code=%d body=%s, want 400 with the trailing message", ok, w.Code, w.Body.String())
	}
	if ok, _, dst := call(``, false); !ok || dst != (body{}) {
		t.Errorf("empty body: ok=%v dst=%+v, want pass with the zero value", ok, dst)
	}
	if ok, _, _ := call(``, true); !ok {
		t.Error("nil body: want pass")
	}
	if ok, _, dst := call(`{"a":{"b":7}}`, false); !ok || dst.A.B != 7 {
		t.Errorf("valid body: ok=%v dst=%+v, want pass with b=7", ok, dst)
	}
}

// ---------------------------------------------------------------------------
// Apply helpers
// ---------------------------------------------------------------------------

// TestReportGateRatified: one assertion per outcome.
// COUNTERFACTUAL: drop `rejections > 0` from the contested check. The
// contested fixture carries a grant AND a rejection, so it then reads
// ratified → red (observed).
func TestReportGateRatified(t *testing.T) {
	ctx := context.Background()
	stageID := uuid.New()

	if out, err := New(Config{Addr: "127.0.0.1:0"}).reportGateRatified(ctx, stageID); out != reportGateNoRepository || err != nil {
		t.Errorf("no repository = (%v, %v), want (no_repository, nil)", out, err)
	}

	listFail := &groomingApplyApprovalRepo{fakeApprovalRepo: newFakeApprovalRepo()}
	boom := errors.New("approvals unreadable")
	listFail.setListErr(boom)
	if out, err := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: listFail}).reportGateRatified(ctx, stageID); out != reportGateUnreadable || !errors.Is(err, boom) {
		t.Errorf("list error = (%v, %v), want (unreadable, the store error)", out, err)
	}

	submit := func(repo *fakeApprovalRepo, subject string, d approval.Decision) {
		if _, err := repo.Submit(ctx, approval.SubmitParams{StageID: stageID, ApproverSubject: subject, Decision: d}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	ungranted := newFakeApprovalRepo()
	if out, err := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ungranted}).reportGateRatified(ctx, stageID); out != reportGateContestedOrUngranted || err != nil {
		t.Errorf("ungranted = (%v, %v), want (contested_or_ungranted, nil)", out, err)
	}

	contested := newFakeApprovalRepo()
	submit(contested, "alice", approval.DecisionApprove)
	submit(contested, "bob", approval.DecisionReject)
	if out, err := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: contested}).reportGateRatified(ctx, stageID); out != reportGateContestedOrUngranted || err != nil {
		t.Errorf("contested = (%v, %v), want (contested_or_ungranted, nil)", out, err)
	}

	ratified := newFakeApprovalRepo()
	submit(ratified, "alice", approval.DecisionApprove)
	if out, err := New(Config{Addr: "127.0.0.1:0", ApprovalRepo: ratified}).reportGateRatified(ctx, stageID); out != reportGateRatifiedOK || err != nil {
		t.Errorf("ratified = (%v, %v), want (ratified, nil)", out, err)
	}

	var zero reportGateOutcome
	if zero == reportGateRatifiedOK {
		t.Error("the zero reportGateOutcome must not be ratified (fail closed)")
	}
}

// commsDisposition is a minimal comms disposition payload for the settlement
// tests (the comms payload shape is phase 5's; the seam only reads sequences).
func commsDisposition(artifactID, entryID string) map[string]string {
	return map[string]string{"artifact_id": artifactID, "entry_id": entryID}
}

// TestSettleReportWindow_CommsFallbackPermanence: without the atomic
// capability, a second settlement reuses the FIRST watermark and never extends
// the bound.
// COUNTERFACTUAL: make the fallback always append (ignore `existing`). The
// fixture appends a late disposition AFTER the first settlement, so the second
// settlement then appends a second watermark above it and consumes it → red
// on both the watermark count and the consumed set (observed).
func TestSettleReportWindow_CommsFallbackPermanence(t *testing.T) {
	ctx := context.Background()
	au := newSeamAudit()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	st := seamStage()
	art := uuid.NewString()

	early := seamAppend(t, au, st.RunID, audit.CommsDispositionRecordedCategory, commsDisposition(art, "e1"))
	first, err := s.settleReportWindow(ctx, commsWindowFamily, st, art, "approved")
	if err != nil {
		t.Fatalf("first settlement: %v", err)
	}
	if len(first) != 1 || first[0].Sequence != early {
		t.Fatalf("first consumed = %+v, want exactly the early disposition (seq %d)", first, early)
	}

	seamAppend(t, au, st.RunID, audit.CommsDispositionRecordedCategory, commsDisposition(art, "late"))
	second, err := s.settleReportWindow(ctx, commsWindowFamily, st, art, "rejected")
	if err != nil {
		t.Fatalf("second settlement: %v", err)
	}
	if n := seamCount(au, st.RunID, audit.CommsApplyWindowClosedCategory); n != 1 {
		t.Errorf("comms_apply_window_closed rows = %d, want exactly 1 (permanence)", n)
	}
	if len(second) != 1 || second[0].Sequence != early {
		t.Errorf("second consumed = %+v, want only the early disposition; the late one is above the first watermark", second)
	}
}

// TestSettleReportWindow_CommsAtomicPath: with audit.FamilyWindowAppender the
// comms family drives the atomic close with family=comms and appends nothing
// through the fallback; the upkeep family deliberately ignores the GENERIC
// capability (it asserts the typed one).
// COUNTERFACTUAL: make genericFamilyCloser return (nil, false). The fixture's
// repo implements FamilyWindowAppender only, so the comms settle then takes
// the fallback: no recorded family and one fallback watermark → red
// (observed).
func TestSettleReportWindow_CommsAtomicPath(t *testing.T) {
	ctx := context.Background()
	consumed := []*audit.Entry{{Sequence: 3, Category: audit.CommsDispositionRecordedCategory}}
	au := &seamFamilyAppender{groomingApplyAuditFake: newSeamAudit(), consumed: consumed}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	st := seamStage()

	got, err := s.settleReportWindow(ctx, commsWindowFamily, st, uuid.NewString(), "approved")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(got) != 1 || got[0] != consumed[0] {
		t.Errorf("consumed = %+v, want the atomic close's set", got)
	}
	au.mu.Lock()
	families, categories := append([]string(nil), au.families...), append([]string(nil), au.categories...)
	au.mu.Unlock()
	if len(families) != 1 || families[0] != audit.WindowFamilyComms || categories[0] != audit.CommsApplyWindowClosedCategory {
		t.Errorf("atomic close calls: families=%v categories=%v, want one comms close with the comms watermark", families, categories)
	}
	if n := seamTotal(au.groomingApplyAuditFake); n != 0 {
		t.Errorf("fallback appended %d rows on the atomic path, want 0", n)
	}

	// The upkeep descriptor asserts audit.UpkeepWindowAppender, which this
	// fake lacks: it takes the fallback and leaves the generic close unused.
	if _, err := s.settleReportWindow(ctx, upkeepWindowFamily, st, uuid.NewString(), "approved"); err != nil {
		t.Fatalf("upkeep settle: %v", err)
	}
	au.mu.Lock()
	calls := len(au.families)
	au.mu.Unlock()
	if calls != 1 || seamCount(au.groomingApplyAuditFake, st.RunID, audit.UpkeepApplyWindowClosedCategory) != 1 {
		t.Errorf("upkeep settle: generic closes=%d, want 1 (unchanged); want one fallback upkeep watermark", calls)
	}
}

// TestSettleReportWindow_UnknownFamilyWritesNothing: an unregistered family is
// refused before any write.
// COUNTERFACTUAL: delete the LookupWindowFamily refusal. The fixture repo has
// no atomic capability, so the fallback then appends a watermark with an
// empty category → the row count goes 0 → 1 → red (observed).
func TestSettleReportWindow_UnknownFamilyWritesNothing(t *testing.T) {
	au := newSeamAudit()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	fam := reportWindowFamily{name: "nope", atomicClose: genericFamilyCloser("nope")}
	_, err := s.settleReportWindow(context.Background(), fam, seamStage(), uuid.NewString(), "approved")
	if !errors.Is(err, audit.ErrUnknownWindowFamily) {
		t.Errorf("err = %v, want ErrUnknownWindowFamily", err)
	}
	if n := seamTotal(au); n != 0 {
		t.Errorf("rows appended = %d, want 0", n)
	}
}

// TestSettleReportWindow_FamilyIsolationInFallback: an upkeep watermark on the
// SAME artifact id does not close the comms window.
// COUNTERFACTUAL: scan audit.UpkeepApplyWindowClosedCategory for the existing
// watermark in the fallback. The fixture's upkeep watermark (seq 1) then reads
// as the comms window: no comms watermark is appended and the comms
// disposition (seq 2) is not consumed → red (observed).
func TestSettleReportWindow_FamilyIsolationInFallback(t *testing.T) {
	ctx := context.Background()
	au := newSeamAudit()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	st := seamStage()
	art := uuid.NewString()
	seamAppend(t, au, st.RunID, audit.UpkeepApplyWindowClosedCategory, groomingWindowPayload{ArtifactID: art, Settlement: "rejected"})
	disp := seamAppend(t, au, st.RunID, audit.CommsDispositionRecordedCategory, commsDisposition(art, "e1"))

	got, err := s.settleReportWindow(ctx, commsWindowFamily, st, art, "approved")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if n := seamCount(au, st.RunID, audit.CommsApplyWindowClosedCategory); n != 1 {
		t.Errorf("comms watermarks = %d, want 1 (the upkeep watermark is another family's)", n)
	}
	if len(got) != 1 || got[0].Sequence != disp {
		t.Errorf("consumed = %+v, want the comms disposition at seq %d", got, disp)
	}
}

// TestCollapseConsumed_LastWinsBySequence: the higher-sequence row wins even
// when listed FIRST; other-artifact, id-less, junk and nil rows are skipped.
// COUNTERFACTUAL: drop the `cur.s > e.Sequence` comparison. The fixture lists
// the seq-5 row before the seq-2 row for the same id, so the later-listed
// seq-2 row then wins → red (observed).
func TestCollapseConsumed_LastWinsBySequence(t *testing.T) {
	const art = "A"
	type v struct{ Verdict string }
	row := func(seq int64, payload string) *audit.Entry {
		return &audit.Entry{Sequence: seq, Payload: []byte(payload)}
	}
	entries := []*audit.Entry{
		row(5, `{"id":"X","artifact_id":"A","verdict":"late"}`),
		row(2, `{"id":"X","artifact_id":"A","verdict":"early"}`),
		row(6, `{"id":"Y","artifact_id":"B","verdict":"other-artifact"}`),
		row(7, `not json`),
		row(8, `{"id":"","artifact_id":"A","verdict":"id-less"}`),
		nil,
	}
	decode := func(payload []byte) (string, string, v, bool) {
		var p struct {
			ID         string `json:"id"`
			ArtifactID string `json:"artifact_id"`
			Verdict    string `json:"verdict"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return "", "", v{}, false
		}
		return p.ID, p.ArtifactID, v{p.Verdict}, true
	}
	got := collapseConsumed(entries, art, decode)
	if len(got) != 1 || got["X"].Verdict != "late" {
		t.Errorf("collapsed = %+v, want only X=late", got)
	}
}

// TestRunScopedWorkTarget: coordinates come from the CALLER's owner/name,
// connections from the conventions, scope from the run's installation, else
// the GitHub lookup.
// COUNTERFACTUAL: drop the `conv.Provider == workmgmtgithub.ProviderName`
// guard. The non-GitHub leg's GitHub client is a counting stub, so the lookup
// then fires (hits 0 → 1) → red (observed).
func TestRunScopedWorkTarget(t *testing.T) {
	ctx := context.Background()
	inst := int64(42)
	project := &workmgmt.Project{}
	jira := &workmgmt.JiraConnection{}
	gitlab := &workmgmt.GitLabConnection{}
	ghConv := workmgmt.Conventions{Provider: workmgmtgithub.ProviderName, Project: project, Jira: jira, GitLab: gitlab}

	t.Run("installation from the run, explicit coordinates", func(t *testing.T) {
		gh, hits := udGitHubInstallServer(t, http.StatusOK, `{"id":77}`)
		s := New(Config{Addr: "127.0.0.1:0", GitHub: gh})
		// The run's repo differs from the passed coordinates: the helper must
		// never re-split runRow.Repo.
		target, err := s.runScopedWorkTarget(ctx, &run.Run{Repo: "elsewhere/other", InstallationID: &inst}, "o", "n", ghConv)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if target.Repo != (workmgmt.Repo{Owner: "o", Name: "n"}) {
			t.Errorf("repo = %+v, want the caller's o/n", target.Repo)
		}
		if target.Project != project || target.Jira != jira || target.GitLab != gitlab {
			t.Errorf("conventions connections not copied: %+v", target)
		}
		if target.Scope != forge.FromGitHubInstallationID(42) || *hits != 0 {
			t.Errorf("scope = %+v hits = %d, want installation 42 and no lookup", target.Scope, *hits)
		}
	})

	t.Run("GitHub provider resolves the installation", func(t *testing.T) {
		gh, hits := udGitHubInstallServer(t, http.StatusOK, `{"id":77}`)
		s := New(Config{Addr: "127.0.0.1:0", GitHub: gh})
		target, err := s.runScopedWorkTarget(ctx, &run.Run{Repo: "o/n"}, "o", "n", ghConv)
		if err != nil || target.Scope != forge.FromGitHubInstallationID(77) || *hits != 1 {
			t.Errorf("target scope = %+v err = %v hits = %d, want installation 77 from one lookup", target.Scope, err, *hits)
		}
	})

	t.Run("lookup error returns a zero scope", func(t *testing.T) {
		gh, _ := udGitHubInstallServer(t, http.StatusInternalServerError, `{"message":"boom"}`)
		s := New(Config{Addr: "127.0.0.1:0", GitHub: gh})
		target, err := s.runScopedWorkTarget(ctx, &run.Run{Repo: "o/n"}, "o", "n", ghConv)
		if err == nil || !target.Scope.IsZero() || target.Repo != (workmgmt.Repo{Owner: "o", Name: "n"}) {
			t.Errorf("target = %+v err = %v, want the error, a zero scope and the coordinates filled", target, err)
		}
	})

	t.Run("non-GitHub provider makes no lookup", func(t *testing.T) {
		gh, hits := udGitHubInstallServer(t, http.StatusOK, `{"id":77}`)
		s := New(Config{Addr: "127.0.0.1:0", GitHub: gh})
		target, err := s.runScopedWorkTarget(ctx, &run.Run{Repo: "o/n"}, "o", "n", workmgmt.Conventions{Provider: "jira"})
		if err != nil || !target.Scope.IsZero() || *hits != 0 {
			t.Errorf("target scope = %+v err = %v hits = %d, want zero scope, no error, no lookup", target.Scope, err, *hits)
		}
	})
}

// TestStartDetachedReportApply_DetachedAndShutdownDrains: fn runs on a context
// detached from an already-cancelled caller, Shutdown blocks while it is
// parked, and waitReportApply returns after release.
// COUNTERFACTUAL (1): drop s.bgReportApply.Wait() from Shutdown. fn is parked
// on the release channel, so Shutdown then returns inside the window → red
// here AND in the unchanged TestApplyApprovedUpkeep_DetachedAndShutdownDrains
// (observed). COUNTERFACTUAL (2): drop context.WithoutCancel. The fixture
// cancels the caller ctx before launch, so fn's ctx is already cancelled → red
// (observed).
func TestStartDetachedReportApply_DetachedAndShutdownDrains(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: timescale.D(10 * time.Second)})
	caller, cancel := context.WithCancel(context.Background())
	cancel()

	started := make(chan error, 1)
	release := make(chan struct{})
	budget := timescale.D(30 * time.Second)
	s.startDetachedReportApply(caller, budget, func(ctx context.Context) {
		dl, ok := ctx.Deadline()
		if !ok || time.Until(dl) > budget {
			started <- fmt.Errorf("fn ctx deadline = %v (set=%v), want one bounded by the budget", dl, ok)
		} else {
			started <- ctx.Err()
		}
		<-release
	})
	select {
	case err := <-started:
		if err != nil {
			close(release)
			t.Fatalf("fn ctx: %v, want a live detached context", err)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		close(release)
		t.Fatal("the detached fn never started")
	}

	shut := make(chan error, 1)
	go func() { shut <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		close(release)
		t.Fatalf("Shutdown returned (%v) while the detached apply was parked; it must drain bgReportApply", err)
	case <-time.After(timescale.D(300 * time.Millisecond)):
	}
	close(release)
	s.waitReportApply()
	select {
	case <-shut:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Shutdown did not return after the detached apply was released")
	}
}
