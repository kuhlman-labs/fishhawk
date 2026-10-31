package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
)

// TestApprovalCredential_PostgresCrossBoundary is the cross-layer test of the
// members credential leg (#4633): real api_tokens persistence (a static
// Issue, a device-flow IssueOAuth, and a child minted through POST /v0/tokens)
// → bearerAuth's Identity → the members leg → the approvals table → the audit
// chain. It reads COMMITTED state, not responses alone: with the credential
// guard deleted the static bearer inserts an approvals row and the stage
// advances.
func TestApprovalCredential_PostgresCrossBoundary(t *testing.T) {
	f := newFOPG(t)
	ctx := context.Background()

	runID, stageID := uuid.New(), uuid.New()
	f.exec(`INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, workflow_spec)
		VALUES ($1, 'acme/widgets', 'feature_change', 'sha', 'cli', 'running', 'local', $2)`,
		runID, membersPredicateSpec(1, []string{"github:alice"}))
	f.exec(`INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, 'acceptance', 'human', 'human', 'awaiting_approval')`, stageID, runID)

	static := f.issue("github:alice", "write:approvals", "read:runs")
	issuer, ok := f.tokens.(apitoken.OAuthIssuer)
	if !ok {
		t.Fatal("the postgres api-token repository does not implement apitoken.OAuthIssuer")
	}
	oauthTok, err := issuer.IssueOAuth(ctx, "github:alice", []string{"write:approvals", "read:runs"}, identity.ProviderGitHub)
	if err != nil {
		t.Fatalf("IssueOAuth: %v", err)
	}
	device := oauthTok.PlainText

	// A child token minted by the device-flow bearer through POST /v0/tokens
	// is a STATIC token for the same listed subject.
	cw := f.do(http.MethodPost, "/v0/tokens", device, `{"scopes":["write:approvals"]}`)
	if cw.Code != http.StatusCreated {
		t.Fatalf("POST /v0/tokens = %d, want 201:\n%s", cw.Code, cw.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cw.Body.Bytes(), &created); err != nil || created.Token == "" {
		t.Fatalf("decode child token: %v (%s)", err, cw.Body.String())
	}
	child := created.Token

	approvalsPath := "/v0/stages/" + stageID.String() + "/approvals"
	for name, bearer := range map[string]string{"static": static, "child": child} {
		w := f.do(http.MethodPost, approvalsPath, bearer, `{"decision":"approve"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s bearer: status = %d, want 403:\n%s", name, w.Code, w.Body.String())
		}
		code, details := decodeApprovalError(t, w)
		if code != "approver_predicate_unmet" || details["reason"] != "credential_not_forge_verified" || details["credential_kind"] != identity.CredentialStatic {
			t.Errorf("%s bearer: code=%q details=%v, want approver_predicate_unmet / credential_not_forge_verified / static", name, code, details)
		}
	}

	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE stage_id = $1`, stageID).Scan(&rows); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if rows != 0 {
		t.Fatalf("approvals rows after the static bearers = %d, want 0", rows)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM stages WHERE id = $1`, stageID).Scan(&state); err != nil {
		t.Fatalf("read stage: %v", err)
	}
	if state != "awaiting_approval" {
		t.Fatalf("stage state after the static bearers = %q, want awaiting_approval", state)
	}
	if n := f.countSnapshotRows(runID, "approval_predicate_rejected", identity.CredentialStatic); n != 2 {
		t.Errorf("approval_predicate_rejected rows with members_credential static = %d, want 2", n)
	}

	w := f.do(http.MethodPost, approvalsPath, device, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("device-flow bearer: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE stage_id = $1`, stageID).Scan(&rows); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	if rows != 1 {
		t.Errorf("approvals rows after the device-flow bearer = %d, want 1", rows)
	}
	if n := f.countSnapshotRows(runID, "approval_submitted", identity.CredentialDeviceFlow); n != 1 {
		t.Errorf("approval_submitted rows with members_listed true and members_credential oauth = %d, want 1", n)
	}
}

// countSnapshotRows counts the run's committed audit rows of category whose
// predicate_snapshot records members_listed true and members_credential kind.
func (f *foPG) countSnapshotRows(runID uuid.UUID, category, kind string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries
		  WHERE run_id = $1 AND category = $2
		    AND payload->'predicate_snapshot'->>'members_listed' = 'true'
		    AND payload->'predicate_snapshot'->>'members_credential' = $3`,
		runID, category, kind).Scan(&n); err != nil {
		f.t.Fatalf("count %s rows: %v", category, err)
	}
	return n
}
