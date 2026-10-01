package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gate_divergence_pg_test.go is the E75.5 / #3733 detection hook's
// cross-boundary test (approval condition 2): a REAL plan reject driven
// through POST /v0/stages/{id}/approvals against the REAL decision index, the
// REAL chained audit repository and the REAL run/approval stores on Postgres.
// It proves (a) the hook fires only AFTER writeApprovalAudit's
// approval_submitted entry is on the chain, (b) the doctrine version the rule
// compares is the run's real workflow_sha — a matched one fires, a mismatched
// one does not — and (c) the shipped default records nothing on the same
// walk. The answer verb's end-to-end walk is a sibling slice's test; this
// file stops at the precedent_divergence entry.
//
// It reuses gate_precedent_pg_test.go's gpPG seed helpers (real Extract +
// Store), whose prior runs carry workflow_sha "sha-gp".

// dvPGServer wires the full approval path on fixture f's pool.
func dvPGServer(f *gpPG, cfg *precedent.DivergenceConfig) *Server {
	return New(Config{Addr: "127.0.0.1:0",
		RunRepo:          run.NewPostgresRepository(f.pool),
		ConcernRepo:      concern.NewPostgresRepository(f.pool),
		ApprovalRepo:     approval.NewPostgresRepository(f.pool),
		AuditRepo:        f.audit,
		PrecedentIndex:   decisionindex.NewStore(f.pool),
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true},
		DivergenceConfig: cfg,
	})
}

// dvPGGateRun parks a fresh untenanted run whose workflow_sha (the doctrine
// version) is sha at its plan gate.
func dvPGGateRun(t *testing.T, f *gpPG, sha string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
	           VALUES ($1, $2, 'feature_change', $3, 'cli', 'running', 'local')`, runID, gpPGRepo, sha)
	planStage := f.seedStage(t, runID, 0, "plan", "awaiting_approval")
	f.seedStage(t, runID, 1, "implement", "pending")
	return runID, planStage
}

func dvPGReject(t *testing.T, s *Server, stageID uuid.UUID) {
	t.Helper()
	w := submitApproval(t, s, stageID, `{"decision":"reject","comment":"wrong fork: the plan edits the generated file"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reject = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

func dvPGChain(t *testing.T, f *gpPG, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	es, err := f.audit.ListForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("list chain: %v", err)
	}
	return es
}

func dvPGSeedApproves(t *testing.T, f *gpPG, n int) map[int64]bool {
	t.Helper()
	seqs := map[int64]bool{}
	for i := 0; i < n; i++ {
		seqs[f.seedDecision(t, nil, "approve", fmt.Sprintf("prior approve %d", i))] = true
	}
	return seqs
}

func dvPGConfig() *precedent.DivergenceConfig {
	return &precedent.DivergenceConfig{Enabled: true, MinDecisions: 3, MinAgreement: 0.8, Window: 30 * 24 * time.Hour}
}

// TestDivergencePG_RejectAgainstApprovePrecedent_MatchedDoctrineFires: four
// prior human approves under sha-gp, a fifth run under sha-gp rejected through
// HTTP. Exactly one precedent_divergence entry lands, on the plan stage, AFTER
// the reject's approval_submitted entry, citing the seeded decisions.
func TestDivergencePG_RejectAgainstApprovePrecedent_MatchedDoctrineFires(t *testing.T) {
	f := newGPPG(t)
	seqs := dvPGSeedApproves(t, f, 4)
	runID, planStage := dvPGGateRun(t, f, "sha-gp")
	dvPGReject(t, dvPGServer(f, dvPGConfig()), planStage)

	var approvalSeq, divergenceSeq int64
	var p precedentDivergencePayload
	n := 0
	for _, e := range dvPGChain(t, f, runID) {
		switch e.Category {
		case "approval_submitted":
			approvalSeq = e.Sequence
		case CategoryPrecedentDivergence:
			n++
			divergenceSeq = e.Sequence
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.StageID == nil || *e.StageID != planStage {
				t.Errorf("entry stage = %v, want the plan stage %s", e.StageID, planStage)
			}
		}
	}
	if n != 1 {
		t.Fatalf("precedent_divergence entries = %d, want 1", n)
	}
	if approvalSeq == 0 || divergenceSeq <= approvalSeq {
		t.Fatalf("approval_submitted seq %d, precedent_divergence seq %d: the hook must run after the reject is recorded",
			approvalSeq, divergenceSeq)
	}
	if p.DecisionClass != "plan_approval" || p.Outcome != "reject" || p.ModalOutcome != "approve" ||
		p.HumanCount != 4 || p.AgreementRatio != 1 || p.Threshold.DoctrineVersion != "sha-gp" ||
		p.DecisionSequence != approvalSeq {
		t.Fatalf("payload = %+v", p)
	}
	for _, c := range p.Cited {
		if !seqs[c.SourceSequence] || c.SourceEntryHash == "" {
			t.Errorf("unexpected citation %d/%q", c.SourceSequence, c.SourceEntryHash)
		}
	}
}

// TestDivergencePG_MismatchedDoctrineDoesNotFire: the identical walk, except
// the rejected run's workflow_sha differs from every prior decision's — so the
// doctrine-version condition is the ONLY unmet one and nothing is recorded.
func TestDivergencePG_MismatchedDoctrineDoesNotFire(t *testing.T) {
	f := newGPPG(t)
	dvPGSeedApproves(t, f, 4)
	runID, planStage := dvPGGateRun(t, f, "sha-other")
	dvPGReject(t, dvPGServer(f, dvPGConfig()), planStage)

	sawApproval := false
	for _, e := range dvPGChain(t, f, runID) {
		if e.Category == "approval_submitted" {
			sawApproval = true
		}
		if e.Category == CategoryPrecedentDivergence {
			t.Fatalf("a precedent_divergence entry landed under a mismatched doctrine version: %s", e.Payload)
		}
	}
	if !sawApproval {
		t.Fatal("the reject was not recorded; the walk did not reach the hook")
	}
}

// TestDivergencePG_ShippedDefaultRecordsNothing: the matched-doctrine walk
// with a nil DivergenceConfig (the shipped default) records the reject and no
// precedent_divergence entry.
func TestDivergencePG_ShippedDefaultRecordsNothing(t *testing.T) {
	f := newGPPG(t)
	dvPGSeedApproves(t, f, 4)
	runID, planStage := dvPGGateRun(t, f, "sha-gp")
	dvPGReject(t, dvPGServer(f, nil), planStage)

	sawApproval := false
	for _, e := range dvPGChain(t, f, runID) {
		if e.Category == "approval_submitted" {
			sawApproval = true
		}
		if e.Category == CategoryPrecedentDivergence {
			t.Fatalf("a precedent_divergence entry landed under the shipped default: %s", e.Payload)
		}
	}
	if !sawApproval {
		t.Fatal("the reject was not recorded")
	}
}
