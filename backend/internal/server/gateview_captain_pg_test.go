package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The gate-view captain block (E76.3 / #3766, ADR-083 rule 4) driven through
// the REAL handler over a pgtest database: Postgres run / concern / audit
// repositories and the production captain.Store, with captain and
// approval_submitted entries seeded BY CONSTRUCTION on the chain, so the
// handler -> currentCaptain -> captain.Derive -> audit_entries path is
// exercised end to end.

const gateCaptainRepo = "kuhlman-labs/gate-captain"

type gateCaptainPG struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	srv   *Server
}

func newGateCaptainPG(t *testing.T) *gateCaptainPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	au := audit.NewPostgresRepository(pool)
	return &gateCaptainPG{pool: pool, audit: au, srv: New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      run.NewPostgresRepository(pool),
		AuditRepo:    au,
		ConcernRepo:  concern.NewPostgresRepository(pool),
		CaptainStore: captain.NewStore(pool),
	})}
}

// seedRun inserts a run for gateCaptainRepo in acct's partition (nil =
// untenanted).
func (f *gateCaptainPG) seedRun(t *testing.T, acct *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, account_id)
		VALUES ($1, $2, 'feature_change', 'sha', 'cli', 'running', 'local', $3)`, id, gateCaptainRepo, acct); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func (f *gateCaptainPG) seedCaptain(t *testing.T, acct *uuid.UUID, category, subject string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"repo": gateCaptainRepo, "subject": subject, "identity_verified": captain.IdentityVerified(subject)})
	kind := audit.ActorUser
	if _, err := f.audit.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, ActorSubject: &subject, Payload: raw, AccountID: acct,
	}); err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
}

func (f *gateCaptainPG) seedApproval(t *testing.T, runID uuid.UUID, decision, approver string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"stage_id": uuid.NewString(), "decision": decision, "surface": "api", "approver": approver})
	kind := actorKindForSubject(approver)
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "approval_submitted",
		ActorKind: &kind, ActorSubject: &approver, Payload: raw,
	}); err != nil {
		t.Fatalf("seed approval_submitted: %v", err)
	}
}

// view reads the gate view with an identity carrying NO account, so the
// captain can only be resolved through the RUN's account.
func (f *gateCaptainPG) view(t *testing.T, runID uuid.UUID) (gateViewResponse, string) {
	t.Helper()
	w := callGateView(f.srv, runID, "", gateViewReadIdentity())
	if w.Code != http.StatusOK {
		t.Fatalf("gate-view = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp gateViewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode gate-view: %v", err)
	}
	return resp, w.Body.String()
}

// TestGateViewCaptain_NonCaptainApproverNoted: a human approver (bob) who is
// not the seated captain (alice) is named, and the note is composed. The
// approval itself is untouched: this is a read.
func TestGateViewCaptain_NonCaptainApproverNoted(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", "github:bob")
	resp, _ := f.view(t, runID)
	c := resp.Captain
	if c == nil {
		t.Fatal("captain block missing")
	}
	if c.Subject != "github:alice" || !c.IdentityVerified || c.Vacant {
		t.Errorf("captain = %+v, want seated verified github:alice", c)
	}
	if !slices.Equal(c.NonCaptainApprovers, []string{"github:bob"}) || c.Note != "approved by github:bob; captain is github:alice" {
		t.Errorf("captain = %+v, want bob named with the note", c)
	}
}

// TestGateViewCaptain_CaptainApproverNoNote (C6): the ONLY approver IS the
// seated captain, by construction. The block is present with an empty list
// and NO note — deleting the inequality guard would compose "approved by
// github:alice; captain is github:alice".
func TestGateViewCaptain_CaptainApproverNoNote(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", "github:alice")
	resp, body := f.view(t, runID)
	if resp.Captain == nil || resp.Captain.Subject != "github:alice" {
		t.Fatalf("captain block = %+v, want present for github:alice", resp.Captain)
	}
	if resp.Captain.Note != "" || len(resp.Captain.NonCaptainApprovers) != 0 {
		t.Errorf("captain = %+v, want no note and no non-captain approvers", resp.Captain)
	}
	if !strings.Contains(body, `"non_captain_approvers":[]`) {
		t.Errorf("non_captain_approvers not rendered as an empty list:\n%s", body)
	}
}

// TestGateViewCaptain_AgentAndRejectExcluded: an agent-kind approver (the
// delegated operator-agent subject) and a human REJECT are neither named; a
// repeated human approve is named once.
func TestGateViewCaptain_AgentAndRejectExcluded(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", operatorrole.DelegatedApprovalActorSubject)
	f.seedApproval(t, runID, "reject", "github:carol")
	f.seedApproval(t, runID, "approve", "github:bob")
	f.seedApproval(t, runID, "approve", "github:bob")
	resp, _ := f.view(t, runID)
	if resp.Captain == nil || !slices.Equal(resp.Captain.NonCaptainApprovers, []string{"github:bob"}) {
		t.Errorf("captain = %+v, want only github:bob named", resp.Captain)
	}
}

// TestGateViewCaptain_VacantSeat: a relinquished seat renders vacant:true
// with no subject and no note, even with a human approver on the run.
func TestGateViewCaptain_VacantSeat(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedCaptain(t, nil, captain.CategoryRelinquished, "github:alice")
	f.seedApproval(t, runID, "approve", "github:bob")
	resp, _ := f.view(t, runID)
	if resp.Captain == nil || !resp.Captain.Vacant || resp.Captain.Subject != "" || resp.Captain.Note != "" {
		t.Errorf("captain = %+v, want vacant with no subject and no note", resp.Captain)
	}
	if resp.HistoryIncomplete {
		t.Errorf("a vacancy recorded history gaps %v", resp.HistoryGaps)
	}
}

// TestGateViewCaptain_ResolvesInRunAccount (approval condition 4): the
// captain is seated ONLY in the run's account partition and the request
// identity carries no account, so only a run-account read finds it.
func TestGateViewCaptain_ResolvesInRunAccount(t *testing.T) {
	f := newGateCaptainPG(t)
	acct := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "acct-"+acct.String()[:8]); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	runID := f.seedRun(t, &acct)
	f.seedCaptain(t, &acct, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", "github:bob")
	resp, _ := f.view(t, runID)
	if resp.Captain == nil || resp.Captain.Vacant || resp.Captain.Subject != "github:alice" {
		t.Errorf("captain = %+v, want github:alice resolved in the run's account", resp.Captain)
	}
}

// TestGateViewCaptain_CaptainReadErrorIsGap: a FAILING captain read omits the
// block and names the captain_record gap — never reported as a vacancy.
func TestGateViewCaptain_CaptainReadErrorIsGap(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	closed, err := pgxpool.New(context.Background(), f.pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	closed.Close()
	f.srv.cfg.CaptainStore = captain.NewStore(closed)
	resp, body := f.view(t, runID)
	if resp.Captain != nil || strings.Contains(body, `"captain"`) {
		t.Errorf("captain block rendered from a failed read:\n%s", body)
	}
	if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, gateViewGapCaptainRecord) {
		t.Errorf("history gaps = %v, want %q", resp.HistoryGaps, gateViewGapCaptainRecord)
	}
}

// gateCaptainFailingApprovals fails ONLY the approval_submitted read.
type gateCaptainFailingApprovals struct {
	audit.Repository
}

func (r gateCaptainFailingApprovals) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == "approval_submitted" {
		return nil, errors.New("injected approval_submitted read failure")
	}
	return r.Repository.ListForRunByCategory(ctx, runID, category)
}

// TestGateViewCaptain_ApprovalReadErrorIsGap: with a captain seated, a failed
// approval read omits the block (never built from a partial read) and names
// the approval_submitted gap.
func TestGateViewCaptain_ApprovalReadErrorIsGap(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", "github:bob")
	f.srv.cfg.AuditRepo = gateCaptainFailingApprovals{f.audit}
	resp, _ := f.view(t, runID)
	if resp.Captain != nil {
		t.Errorf("captain = %+v, want omitted on a failed approval read", resp.Captain)
	}
	if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, gateViewGapApprovalSubmitted) {
		t.Errorf("history gaps = %v, want %q", resp.HistoryGaps, gateViewGapApprovalSubmitted)
	}
}

// TestGateViewCaptain_UndecodableApprovalIsGap: an approval_submitted payload
// whose decision is not a string cannot be classified, so the block is
// omitted with the approval_submitted gap rather than built without it.
func TestGateViewCaptain_UndecodableApprovalIsGap(t *testing.T) {
	f := newGateCaptainPG(t)
	runID := f.seedRun(t, nil)
	f.seedCaptain(t, nil, captain.CategoryAssigned, "github:alice")
	f.seedApproval(t, runID, "approve", "github:bob")
	kind := audit.ActorUser
	subject := "github:carol"
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "approval_submitted",
		ActorKind: &kind, ActorSubject: &subject, Payload: json.RawMessage(`{"decision":5,"approver":"github:carol"}`),
	}); err != nil {
		t.Fatalf("seed malformed approval_submitted: %v", err)
	}
	resp, _ := f.view(t, runID)
	if resp.Captain != nil {
		t.Errorf("captain = %+v, want omitted on an undecodable approval", resp.Captain)
	}
	if !slices.Contains(resp.HistoryGaps, gateViewGapApprovalSubmitted) {
		t.Errorf("history gaps = %v, want %q", resp.HistoryGaps, gateViewGapApprovalSubmitted)
	}
}
