package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// #3092 CROSS-LAYER coverage. The per-package units stop at their own package
// boundary: auditcomplete's tests run over fakes, the publisher's over a fake
// forge, and the checks handler's over an in-memory repo. Nothing in them
// proves that REAL repository rows — a real decomposed child run, a real
// implement stage, real chained trace_uploaded entries — drive the resolution
// through auditcomplete and out of the HTTP response and the published Check
// Run. These tests do, against a real Postgres.

// decompositionFixture is the shared real-Postgres seed. It builds a decomposed
// PARENT whose implement stage is the fan-out stage — terminal, but carrying NO
// trace of its own, which is the ONLY audit requirement deliberately left
// unsatisfied — plus `children` decomposition child runs.
//
// Every OTHER requirement auditcomplete's rules demand is seeded here on
// purpose, so an asserted pass can only come from the child resolution and
// never from an unrelated evidence gap (#3092 condition 2). Rule by rule:
//
//   - Rule 1 (plan artifact): a kind=plan, schema_version=standard_v1 artifact
//     on the parent PLAN stage.
//   - Rule 2 (traces): raw + redacted trace_uploaded entries on the parent PLAN
//     stage. The parent IMPLEMENT stage's pair is the DELIBERATE gap.
//   - Rule 3 (pull_request artifact): a kind=pull_request artifact on the
//     parent IMPLEMENT stage — which doubles as the publisher's head_sha
//     source.
//   - Rule 4 (audit chain): every entry is written through the real
//     AppendChained, so the chain genuinely verifies.
//   - Rule 5 (foreign commit): skipped — Config.GitHub is unset, so
//     auditCompleteDeps wires a nil PRHead.
//   - Rule 6 (review pending): skipped — the run carries no workflow spec, so
//     resolveStageReviewers returns nil (no configured agent reviewer).
//   - Rule 7 (security findings): skipped — no implement_security_findings
//     entry is seeded, which the rule treats as the normal async-CodeQL window.
//
// A future rule that adds a NEW requirement will fail the pass assertion loudly
// here rather than silently reverting this test to a tautology.
type decompositionFixture struct {
	parent      *run.Run
	implStage   *run.Stage
	reviewStage *run.Stage
	childIDs    []uuid.UUID
	childImpl   map[uuid.UUID]*run.Stage
	runs        run.Repository
	arts        artifact.Repository
	audits      audit.Repository
	server      *Server
}

// childFixtureSpec describes one decomposition child: the state its implement
// stage is minted in, and whether it ships its trace bundles.
type childFixtureSpec struct {
	implState run.StageState
	traced    bool
}

func seedDecompositionFixture(t *testing.T, specs ...childFixtureSpec) *decompositionFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	arts := artifact.NewPostgresRepository(pool)
	audits := audit.NewPostgresRepository(pool)

	instID := int64(42)
	parent, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha",
		TriggerSource: run.TriggerCLI, InstallationID: &instID,
	})
	if err != nil {
		t.Fatalf("create parent run: %v", err)
	}

	planStage := seedDecompStageRow(t, runs, parent.ID, 1, run.StageTypePlan, run.StageStateSucceeded)
	implStage := seedDecompStageRow(t, runs, parent.ID, 2, run.StageTypeImplement, run.StageStateSucceeded)
	reviewStage := seedDecompStageRow(t, runs, parent.ID, 3, run.StageTypeReview, run.StageStateAwaitingApproval)

	// Rule 1 evidence.
	seedArtifact(t, arts, planStage.ID, artifact.KindPlan, "standard_v1", json.RawMessage(`{"schema_version":"standard_v1"}`))
	// Rule 3 evidence (and the publisher's head_sha source).
	seedArtifact(t, arts, implStage.ID, artifact.KindPullRequest, "", pullRequestArtifactBody("abc12345"))
	// Rule 2 evidence for the PLAN stage. The IMPLEMENT stage stays bare.
	appendTrace(t, audits, parent.ID, planStage.ID, "raw")
	appendTrace(t, audits, parent.ID, planStage.ID, "redacted")

	f := &decompositionFixture{
		parent: parent, implStage: implStage, reviewStage: reviewStage,
		childImpl: map[uuid.UUID]*run.Stage{},
		runs:      runs, arts: arts, audits: audits,
	}
	for _, spec := range specs {
		parentID := parent.ID
		child, err := runs.CreateRun(ctx, run.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "sha",
			TriggerSource: run.TriggerCLI, InstallationID: &instID,
			ParentRunID: &parentID, DecomposedFrom: &parentID,
		})
		if err != nil {
			t.Fatalf("create child run: %v", err)
		}
		childImpl := seedDecompStageRow(t, runs, child.ID, 1, run.StageTypeImplement, spec.implState)
		if spec.traced {
			appendTrace(t, audits, child.ID, childImpl.ID, "raw")
			appendTrace(t, audits, child.ID, childImpl.ID, "redacted")
		}
		f.childIDs = append(f.childIDs, child.ID)
		f.childImpl[child.ID] = childImpl
	}

	f.server = New(Config{
		Addr: "127.0.0.1:0", RunRepo: runs, ArtifactRepo: arts, AuditRepo: audits,
		StageCheckRepo: newStageCheckRepoFake(),
		ExternalURL:    "https://app.fishhawk.example.com",
	})
	return f
}

// seedDecompStageRow creates a stage and walks it to `state` the way the runner
// would. A stage minted directly in a terminal state is not expressible
// through CreateStage, so the transition ladder is walked explicitly.
func seedDecompStageRow(t *testing.T, runs run.Repository, runID uuid.UUID, seq int, typ run.StageType, state run.StageState) *run.Stage {
	t.Helper()
	ctx := context.Background()
	st, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID: runID, Sequence: seq, Type: typ,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	if state == run.StageStatePending {
		return st
	}
	ladder := map[run.StageState][]run.StageState{
		run.StageStateRunning:          {run.StageStateDispatched, run.StageStateRunning},
		run.StageStateSucceeded:        {run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded},
		run.StageStateCancelled:        {run.StageStateCancelled},
		run.StageStateAwaitingApproval: {run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval},
	}
	steps, ok := ladder[state]
	if !ok {
		t.Fatalf("seedStageRow: no transition ladder for %s", state)
	}
	for _, to := range steps {
		st, err = runs.TransitionStage(ctx, st.ID, to, nil)
		if err != nil {
			t.Fatalf("transition stage to %s: %v", to, err)
		}
	}
	return st
}

func seedArtifact(t *testing.T, arts artifact.Repository, stageID uuid.UUID, kind artifact.Kind, schemaVersion string, content json.RawMessage) {
	t.Helper()
	sum := sha256.Sum256(content)
	p := artifact.CreateParams{
		StageID: stageID, Kind: kind, Content: content, ContentHash: hex.EncodeToString(sum[:]),
	}
	if schemaVersion != "" {
		v := schemaVersion
		p.SchemaVersion = &v
	}
	if _, err := arts.Create(context.Background(), p); err != nil {
		t.Fatalf("create %s artifact: %v", kind, err)
	}
}

func appendTrace(t *testing.T, audits audit.Repository, runID, stageID uuid.UUID, variant string) {
	t.Helper()
	sid := stageID
	payload, err := json.Marshal(map[string]string{"variant": variant})
	if err != nil {
		t.Fatalf("marshal trace payload: %v", err)
	}
	if _, err := audits.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &sid, Timestamp: time.Now().UTC(),
		Category: "trace_uploaded", Payload: payload,
	}); err != nil {
		t.Fatalf("append trace_uploaded (%s): %v", variant, err)
	}
}

// auditCompleteRowFor drives the REAL GET /v0/stages/{id}/checks handler and
// returns the injected fishhawk_audit_complete row.
func (f *decompositionFixture) auditCompleteRowFor(t *testing.T) stageCheckResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v0/stages/%s/checks", f.reviewStage.ID), nil)
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got stageChecksListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range got.Items {
		if it.Name == AuditCompleteCheckName {
			return it
		}
	}
	t.Fatalf("no %s row: %s", AuditCompleteCheckName, w.Body.String())
	return stageCheckResponse{}
}

// TestAuditCompleteDecomposition_EndToEnd is the repository → auditcomplete →
// HTTP-response seam: real run/stage/artifact/audit rows drive the resolution
// out of the checks endpoint.
func TestAuditCompleteDecomposition_EndToEnd(t *testing.T) {
	t.Run("both children traced -> pass with resolved naming both", func(t *testing.T) {
		f := seedDecompositionFixture(t,
			childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
			childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
		)
		row := f.auditCompleteRowFor(t)
		if row.State != string(stagecheck.StatePass) {
			t.Fatalf("state = %s, want pass; missing=%+v", row.State, row.Missing)
		}
		if len(row.Missing) != 0 {
			t.Fatalf("want no missing items; got %+v", row.Missing)
		}
		if len(row.Resolved) != 1 {
			t.Fatalf("want exactly 1 resolution; got %+v", row.Resolved)
		}
		for _, id := range f.childIDs {
			found := false
			for _, got := range row.Resolved[0].ChildRunIDs {
				if got == id.String() {
					found = true
				}
			}
			if !found {
				t.Errorf("resolution omits child run %s; got %v", id, row.Resolved[0].ChildRunIDs)
			}
		}
	})

	t.Run("a child missing its trace -> fail naming that child run and stage", func(t *testing.T) {
		f := seedDecompositionFixture(t,
			childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
			childFixtureSpec{implState: run.StageStateSucceeded, traced: false},
		)
		row := f.auditCompleteRowFor(t)
		if row.State != string(stagecheck.StateFail) {
			t.Fatalf("state = %s, want fail; missing=%+v", row.State, row.Missing)
		}
		if len(row.Resolved) != 0 {
			t.Fatalf("an untraced child must yield no resolution; got %+v", row.Resolved)
		}
		badChild := f.childIDs[1]
		badStage := f.childImpl[badChild]
		named := false
		for _, m := range row.Missing {
			if strings.Contains(m.Detail, badChild.String()[:8]) && strings.Contains(m.Detail, badStage.ID.String()[:8]) {
				named = true
			}
		}
		if !named {
			t.Fatalf("failure must name child run %s AND its implement stage %s; got %+v", badChild, badStage.ID, row.Missing)
		}
	})

	t.Run("a child still running -> pending with children_pending", func(t *testing.T) {
		f := seedDecompositionFixture(t,
			childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
			childFixtureSpec{implState: run.StageStateRunning, traced: false},
		)
		row := f.auditCompleteRowFor(t)
		if row.State != string(stagecheck.StatePending) {
			t.Fatalf("state = %s, want pending; missing=%+v", row.State, row.Missing)
		}
		if len(row.Resolved) != 0 {
			t.Fatalf("an in-flight child must yield no resolution; got %+v", row.Resolved)
		}
		found := false
		for _, m := range row.Missing {
			if m.Kind == "children_pending" {
				found = true
			}
		}
		if !found {
			t.Fatalf("want a children_pending item; got %+v", row.Missing)
		}
	})
}

// TestAuditCompleteResolutionReachesPublishedCheck is the #3092 condition-1
// cross-boundary assertion. The `resolved` value crosses
// auditcomplete → checks.go → auditcheckpublisher → the forge Check Run, and
// the per-layer tests each stop at their own boundary: a checks.go that called
// PublishResult(..., nil) would leave BOTH of them green while the published
// summary silently stopped naming the satisfying child runs. This test installs
// a fake CheckRunCreator, drives the REAL server publish path
// (recomputeAndPublishAuditComplete → publishAuditCheck), and asserts the
// CAPTURED CreateCheckRunParams.OutputSummary contains both child run ids.
func TestAuditCompleteResolutionReachesPublishedCheck(t *testing.T) {
	f := seedDecompositionFixture(t,
		childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
		childFixtureSpec{implState: run.StageStateSucceeded, traced: true},
	)
	gh := newPublisherFakeGitHub()
	f.server.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      gh,
		Runs:        f.runs,
		Artifacts:   f.arts,
		Audit:       f.audits,
		ExternalURL: "https://app.fishhawk.example.com",
	})

	f.server.recomputeAndPublishAuditComplete(context.Background(), f.parent.ID)

	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 published check run; got %d", len(calls))
	}
	summary := calls[0].params.OutputSummary
	for _, id := range f.childIDs {
		if !strings.Contains(summary, id.String()) {
			t.Errorf("published check-run summary must name satisfying child run %s; got:\n%s", id, summary)
		}
	}
}
