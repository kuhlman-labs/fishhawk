package server

// Cross-boundary test for the in-flight advisory pass (E80.6 / #3763), over a
// real pgtest database:
//
//	captain approves the upkeep scan's gate -> applyApprovedUpkeep settles the
//	  window on the real audit repo (UpkeepWindowAppender, consumed in-tx)
//	  -> startUpkeepInflightPass (approved advisory findings only)
//	  -> real run listing {repo, running} -> forge seam compare + head go.mod
//	  -> upkeep.MatchInFlight -> real crewmessage.Mailbox Send (chain entry +
//	     crew_messages projection)
//	  -> E77.7 delivery (resolveDeliverableCrewMessages) + the gate view's
//	     crew block; NO concern opened.
//
// Counterfactuals: (C11) make upkeepInflightSentSummaries return an empty set
// -> the second scan re-sends and A holds two findings; (C12) delete the
// startUpkeepInflightPass call in upkeep_apply.go -> A receives nothing.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

type ifPG struct {
	pool      *pgxpool.Pool
	au        audit.Repository
	arts      artifact.Repository
	approvals *fakeApprovalRepo
	s         *Server
	forges    map[uuid.UUID]*ifForge
}

func newIfPG(t *testing.T) *ifPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &ifPG{
		pool: pool, au: audit.NewPostgresRepository(pool), arts: artifact.NewPostgresRepository(pool),
		approvals: newFakeApprovalRepo(), forges: map[uuid.UUID]*ifForge{},
	}
	f.s = New(Config{
		Addr: "127.0.0.1:0", RunRepo: run.NewPostgresRepository(pool), AuditRepo: f.au, ArtifactRepo: f.arts,
		ApprovalRepo: f.approvals, CrewMailbox: crewmessage.NewMailbox(pool, 0),
		DocumentBaseRef: func(context.Context, forge.RepoRef) (string, error) { return "main", nil },
	})
	prevForge, prevSlot := upkeepInflightForge, upkeepInflightSlot
	upkeepInflightForge = func(_ *Server, rn *run.Run) (patchComparer, forge.FileFetcher, forge.CredentialScope, forge.RepoRef, string) {
		fg, ok := f.forges[rn.ID]
		if !ok {
			return nil, nil, forge.CredentialScope{}, forge.RepoRef{}, "no forge for run"
		}
		return fg, fg, forge.CredentialScope{}, forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, ""
	}
	upkeepInflightSlot = make(chan struct{}, 1)
	t.Cleanup(func() { upkeepInflightForge, upkeepInflightSlot = prevForge, prevSlot })
	return f
}

// seedRun inserts a running run with one stage in stageState.
func (f *ifPG) seedRun(t *testing.T, workflow string, stageType run.StageType, stageState run.StageState) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	id, stageID := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		VALUES ($1, $2, $3, 'sha', 'cli', 'running', 'local')`, id, ifRepo, workflow); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, $3, 'agent', 'claude-code', $4)`, stageID, id, string(stageType), string(stageState)); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	return id, stageID
}

func (f *ifPG) append(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
}

// target seeds a running target run with a recorded head and its forge.
func (f *ifPG) target(t *testing.T, stageState run.StageState, fg *ifForge) uuid.UUID {
	t.Helper()
	id, _ := f.seedRun(t, "feature_change", run.StageTypePlan, stageState)
	f.append(t, id, nil, "pull_request_opened", map[string]any{"head_sha": ifHeadSHA})
	f.forges[id] = fg
	return id
}

// scanAndApprove seeds an upkeep scan carrying the shipped advisory example,
// approves GO-2024-2687 only, approves the gate, and waits for the pass.
func (f *ifPG) scanAndApprove(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	scanID, stageID := f.seedRun(t, "upkeep_scan", run.StageTypePlan, run.StageStateAwaitingApproval)
	body := ukAdvisoryBody(t, nil)
	art, err := f.arts.Create(ctx, artifact.CreateParams{StageID: stageID, Kind: artifact.KindUpkeepReport, Content: body, ContentHash: sha256Hex(body)})
	if err != nil {
		t.Fatalf("create upkeep_report: %v", err)
	}
	f.append(t, scanID, &stageID, CategoryUpkeepReportRecorded, map[string]any{
		"run_id": scanID.String(), "artifact_id": art.ID.String(), "duplicates": []any{},
	})
	for findingID, verdict := range map[string]string{
		ukAdvNet: upkeepVerdictApproved, ukAdvText: "rejected",
	} {
		f.append(t, scanID, &stageID, CategoryUpkeepDispositionRecorded, upkeepDispositionPayload{
			RunID: scanID.String(), StageID: stageID.String(), ArtifactID: art.ID.String(),
			FindingID: findingID, Source: "advisory", Verdict: verdict,
		})
	}
	if _, err := f.approvals.Submit(ctx, approval.SubmitParams{
		StageID: stageID, ApproverSubject: "kuhlman-labs", Decision: approval.DecisionApprove, Surface: "test",
	}); err != nil {
		t.Fatal(err)
	}
	f.s.applyApprovedUpkeep(ctx, &run.Stage{ID: stageID, RunID: scanID, Type: run.StageTypePlan}, approval.DecisionApprove)
	f.s.waitUpkeepApply()
	f.s.waitUpkeepInflight()
	return scanID
}

func (f *ifPG) findings(t *testing.T, runID uuid.UUID) []crewmessage.Message {
	t.Helper()
	rows, err := f.au.ListForRunByCategory(context.Background(), runID, crewmessage.CategorySent)
	if err != nil {
		t.Fatal(err)
	}
	var out []crewmessage.Message
	for _, e := range rows {
		var p struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		m, err := crewmessage.Parse(p.Message)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *m)
	}
	return out
}

func (f *ifPG) passSummaries(t *testing.T, scanID uuid.UUID) []upkeepInflightPassPayload {
	t.Helper()
	rows, err := f.au.ListForRunByCategory(context.Background(), scanID, CategoryUpkeepInflightPassCompleted)
	if err != nil {
		t.Fatal(err)
	}
	var out []upkeepInflightPassPayload
	for _, e := range rows {
		var p upkeepInflightPassPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestUpkeepInflightPG_ApprovedAdvisoryReachesAffectedRunOnce(t *testing.T) {
	f := newIfPG(t)
	ctx := context.Background()

	// A: parked at a plan GATE (stage awaiting_approval, run running) and
	// bumping x/net in backend/go.mod to an affected version below the fix
	// (approval condition 3's arm).
	a := f.target(t, run.StageStateAwaitingApproval, &ifForge{
		patch:   ifGoModPatch(ifGoModPath, "v0.21.0", "v0.22.0"),
		files:   []forge.ComparePatchFile{{Path: ifGoModPath, Status: "modified"}},
		content: map[string]string{ifGoModPath: ifGoModHead},
	})
	// B: frontend-only change.
	b := f.target(t, run.StageStateRunning, &ifForge{
		patch: "diff --git a/frontend/src/app.ts b/frontend/src/app.ts\n@@ -1,1 +1,1 @@\n-a\n+b\n",
		files: []forge.ComparePatchFile{{Path: "frontend/src/app.ts", Status: "modified"}},
	})
	// C: bumps x/net to the FIXED version.
	c := f.target(t, run.StageStateRunning, &ifForge{
		patch:   ifGoModPatch(ifGoModPath, "v0.21.0", "v0.23.0"),
		files:   []forge.ComparePatchFile{{Path: ifGoModPath, Status: "modified"}},
		content: map[string]string{ifGoModPath: strings.Replace(ifGoModHead, "v0.22.0", "v0.23.0", 1)},
	})

	scan1 := f.scanAndApprove(t)

	got := f.findings(t, a)
	if len(got) != 1 {
		t.Fatalf("run A findings = %d, want exactly 1", len(got))
	}
	m := got[0]
	if m.SenderRole != crewmessage.RoleSecurity || m.RecipientRole != crewmessage.RoleReviewer || m.Type != crewmessage.TypeFinding {
		t.Fatalf("A message = %s %s->%s", m.Type, m.SenderRole, m.RecipientRole)
	}
	text := m.Payload.Summary + "\n" + m.Payload.Detail
	for _, want := range []string{"GO-2024-2687", "CVE-2023-45288", "GHSA-4v7x-pqxf-cx7m", "v0.23.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("A message does not name %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"serveH2", "internal/server/serve.go"} {
		if strings.Contains(text, leak) {
			t.Errorf("caller frame %q reached the message", leak)
		}
	}
	if n := len(f.findings(t, b)); n != 0 {
		t.Errorf("run B (frontend only) findings = %d, want 0", n)
	}
	if n := len(f.findings(t, c)); n != 0 {
		t.Errorf("run C (fixed version) findings = %d, want 0", n)
	}
	// The rejected GO-2022-1059 never reaches any run (it cites runner/go.mod,
	// so this is belt-and-braces); only the approved advisory is counted.
	sums := f.passSummaries(t, scan1)
	if len(sums) != 1 || sums[0].AdvisoryFindings != 1 || len(sums[0].Sent) != 1 || sums[0].Sent[0].RunID != a.String() {
		t.Fatalf("scan 1 summary = %+v, want one approved advisory sent to A", sums)
	}

	// E77.7 delivery: A's next review renders it inside the CREW MESSAGE
	// envelope; the gate view lists it; no concern is opened.
	delivery := f.s.resolveDeliverableCrewMessages(ctx, a, uuid.New(), run.StageTypeReview)
	if len(delivery.Messages) != 1 {
		t.Fatalf("review delivery = %d messages, want 1", len(delivery.Messages))
	}
	requireOnlyInsideEnvelope(t, prompt.RenderCrewMessages(delivery.Messages), "GO-2024-2687")
	if gv := f.s.gateViewCrewMessagesFor(ctx, a, &gateViewResponse{}); len(gv) != 1 || gv[0].SenderRole != "security" {
		t.Fatalf("gate view crew block = %+v, want the one security finding", gv)
	}
	open, err := concern.NewPostgresRepository(f.pool).ListOpenByRun(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open concerns on A = %d, want 0: a finding never enters the merge gate", len(open))
	}

	// A SECOND scan with the identical report and diff: the chain dedupe holds.
	scan2 := f.scanAndApprove(t)
	if n := len(f.findings(t, a)); n != 1 {
		t.Fatalf("run A findings after a second scan = %d, want still 1", n)
	}
	sums2 := f.passSummaries(t, scan2)
	if len(sums2) != 1 || len(sums2[0].Sent) != 0 || len(sums2[0].AlreadySent) != 1 || sums2[0].AlreadySent[0].RunID != a.String() {
		t.Fatalf("scan 2 summary = %+v, want A under already_sent", sums2)
	}
}
