package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// upkeep_report stage-binding tests (#3921, slice 1). The fake is a LOCAL
// wrapper over *promptRunRepo (approval condition 4: the shared fake is not
// edited): it serves any number of seeded runs, injects a per-id GetRun error,
// and counts ListStagesForRun calls so the "ordinary workflows never list
// stages" property is asserted rather than assumed.

// upkeepRunRepo is the multi-run wrapper. Runs are seeded into the embedded
// promptRunRepo's getRuns map; stage lists into its stagesByRunID map.
type upkeepRunRepo struct {
	*promptRunRepo

	mu              sync.Mutex
	getRunErrs      map[uuid.UUID]error
	listStagesErr   error
	listStagesCalls int
}

func newUpkeepRunRepo() *upkeepRunRepo {
	return &upkeepRunRepo{promptRunRepo: newPromptRunRepo(), getRunErrs: map[uuid.UUID]error{}}
}

func (r *upkeepRunRepo) seedRun(rn *run.Run) { r.getRuns[rn.ID] = rn }

func (r *upkeepRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.mu.Lock()
	err, injected := r.getRunErrs[id]
	r.mu.Unlock()
	if injected {
		return nil, err
	}
	return r.promptRunRepo.GetRun(ctx, id)
}

func (r *upkeepRunRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	r.mu.Lock()
	r.listStagesCalls++
	err := r.listStagesErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.promptRunRepo.ListStagesForRun(ctx, runID)
}

func (r *upkeepRunRepo) listCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listStagesCalls
}

const upkeepTestRepo = "kuhlman-labs/fishhawk"

// upkeepScanSpec is the SHIPPED upkeep-scan declaration: plan stage `scan`
// declares produces: upkeep_report; implement stage `file` declares nothing.
func upkeepScanSpec(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../docs/spec/examples/workflow-v2-upkeep-scan.yaml")
	if err != nil {
		t.Fatalf("read upkeep-scan example: %v", err)
	}
	return b
}

// upkeepBindingFixture seeds one run carrying the given cached spec + workflow
// id, and its two stage rows: a plan stage (sequence 0) and an implement stage
// (sequence 1). stagesByRunID is left nil unless listStages, so an unexpected
// ListStagesForRun is loud (promptRunRepo errors when unseeded).
func upkeepBindingFixture(t *testing.T, wfSpec []byte, workflowID string, listStages bool) (*Server, *upkeepRunRepo, *run.Run, *run.Stage, *run.Stage, *bytes.Buffer) {
	t.Helper()
	rr := newUpkeepRunRepo()
	runRow := &run.Run{
		ID: uuid.New(), Repo: upkeepTestRepo, AccountID: "acct-1",
		WorkflowID: workflowID, WorkflowSpec: wfSpec, State: run.StateRunning,
	}
	rr.seedRun(runRow)
	planStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 0, Type: run.StageTypePlan}
	implStage := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Sequence: 1, Type: run.StageTypeImplement}
	if listStages {
		rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runRow.ID: {implStage, planStage}}
	}
	var logs bytes.Buffer
	s := New(Config{
		Addr:    "127.0.0.1:0",
		RunRepo: rr,
		Logger:  slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return s, rr, runRow, planStage, implStage, &logs
}

func TestResolveUpkeepStageBinding(t *testing.T) {
	ctx := context.Background()

	t.Run("declared scan stage", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, planStage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true, StageDeclaresUpkeep: true}
		if b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
		if rr.listCalls() != 1 {
			t.Errorf("ListStagesForRun calls = %d, want 1", rr.listCalls())
		}
	})

	t.Run("non-declaring stage of an upkeep workflow", func(t *testing.T) {
		s, _, runRow, _, implStage, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, implStage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true}
		if b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
	})

	t.Run("ordinary workflow never lists stages", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, []byte(appliesToSpec("")), "guarded", false)
		b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, planStage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := upkeepStageBinding{Resolved: true}
		if b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
		if rr.listCalls() != 0 {
			t.Errorf("ListStagesForRun calls = %d, want 0 for a workflow declaring no upkeep_report", rr.listCalls())
		}
	})

	t.Run("stage absent from the listed rows is unmappable", func(t *testing.T) {
		s, _, runRow, _, _, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		stray := &run.Stage{ID: uuid.New(), RunID: runRow.ID, Type: run.StageTypePlan}
		b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, stray)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true, Undecidable: upkeepBindingStageUnmappable}
		if b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
	})

	t.Run("ListStagesForRun error is returned", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		rr.listStagesErr = errors.New("db down")
		if _, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, planStage); err == nil || !strings.Contains(err.Error(), "db down") {
			t.Fatalf("err = %v, want the list error", err)
		}
	})

	t.Run("no run row is unresolved, not an error", func(t *testing.T) {
		s, _, _, planStage, _, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		b, err := s.resolveUpkeepStageBinding(ctx, uuid.New(), planStage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		want := upkeepStageBinding{Undecidable: upkeepBindingWorkflowUnresolved}
		if b != want {
			t.Errorf("binding = %+v, want %+v", b, want)
		}
	})

	t.Run("generic GetRun error is returned", func(t *testing.T) {
		s, rr, runRow, planStage, _, _ := upkeepBindingFixture(t, upkeepScanSpec(t), "upkeep_scan", true)
		rr.getRunErrs[runRow.ID] = errors.New("connection reset")
		if _, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, planStage); err == nil || !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("err = %v, want the GetRun error", err)
		}
	})

	t.Run("no cached spec is unresolved", func(t *testing.T) {
		s, _, runRow, planStage, _, _ := upkeepBindingFixture(t, nil, "upkeep_scan", true)
		b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, planStage)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if b.Resolved || b.Undecidable != upkeepBindingWorkflowUnresolved {
			t.Errorf("binding = %+v, want unresolved", b)
		}
	})

	t.Run("nil RunRepo is unresolved (guard fails open)", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		b, err := s.resolveUpkeepStageBinding(ctx, uuid.New(), &run.Stage{ID: uuid.New(), Type: run.StageTypePlan})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if b.Resolved || b.Undecidable != upkeepBindingWorkflowUnresolved || upkeepStageRefusesOtherProposal(b) {
			t.Errorf("binding = %+v, want unresolved and not refusing", b)
		}
	})
}

func TestUpkeepRefusalDecisions(t *testing.T) {
	cases := []struct {
		name          string
		b             upkeepStageBinding
		ingestRefusal string
		refusesOther  bool
	}{
		{
			name:          "declared stage",
			b:             upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true, StageDeclaresUpkeep: true},
			ingestRefusal: "", refusesOther: true,
		},
		{
			name:          "plan stage of a workflow that never declares upkeep",
			b:             upkeepStageBinding{Resolved: true},
			ingestRefusal: upkeepRefusalStageDoesNotDeclare, refusesOther: false,
		},
		{
			name:          "non-declaring stage of an upkeep workflow",
			b:             upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true},
			ingestRefusal: upkeepRefusalStageDoesNotDeclare, refusesOther: false,
		},
		{
			name:          "unmappable stage of an upkeep workflow",
			b:             upkeepStageBinding{Resolved: true, WorkflowDeclaresUpkeep: true, Undecidable: upkeepBindingStageUnmappable},
			ingestRefusal: upkeepRefusalStageBindingUndecidable, refusesOther: true,
		},
		{
			name:          "workflow unresolved",
			b:             upkeepStageBinding{Undecidable: upkeepBindingWorkflowUnresolved},
			ingestRefusal: upkeepRefusalStageBindingUndecidable, refusesOther: false,
		},
		{
			// Not produced by resolveUpkeepStageBinding (Resolved=false never
			// carries WorkflowDeclaresUpkeep); pins that the guard keys on
			// Resolved rather than inferring it.
			name:          "unresolved binding claiming an upkeep workflow",
			b:             upkeepStageBinding{WorkflowDeclaresUpkeep: true, Undecidable: upkeepBindingWorkflowUnresolved},
			ingestRefusal: upkeepRefusalStageBindingUndecidable, refusesOther: false,
		},
		{
			name:          "zero value fails the ingest closed",
			b:             upkeepStageBinding{},
			ingestRefusal: upkeepRefusalStageDoesNotDeclare, refusesOther: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upkeepIngestRefusal(tc.b); got != tc.ingestRefusal {
				t.Errorf("upkeepIngestRefusal(%+v) = %q, want %q", tc.b, got, tc.ingestRefusal)
			}
			if got := upkeepStageRefusesOtherProposal(tc.b); got != tc.refusesOther {
				t.Errorf("upkeepStageRefusesOtherProposal(%+v) = %v, want %v", tc.b, got, tc.refusesOther)
			}
		})
	}
}

func TestCheckUpkeepRunRefs(t *testing.T) {
	ctx := context.Background()
	// seedRef seeds a cited run with the given repo and account.
	seedRef := func(rr *upkeepRunRepo, repo, account string) uuid.UUID {
		id := uuid.New()
		rr.seedRun(&run.Run{ID: id, Repo: repo, AccountID: account})
		return id
	}

	t.Run("same repo (any case) and same or absent account accepted", func(t *testing.T) {
		s, rr, runRow, _, _, logs := upkeepBindingFixture(t, nil, "", false)
		ids := []uuid.UUID{
			seedRef(rr, upkeepTestRepo, "acct-1"),
			seedRef(rr, strings.ToUpper(upkeepTestRepo), "acct-1"),
			seedRef(rr, upkeepTestRepo, ""), // one AccountID empty: repo-only
			runRow.ID,                       // the reporting run itself
		}
		refused, ok, err := s.checkUpkeepRunRefs(ctx, runRow, ids)
		if err != nil || !ok || refused != uuid.Nil {
			t.Fatalf("= (%v, %v, %v), want (nil, true, nil)", refused, ok, err)
		}
		if strings.Contains(logs.String(), "cited run refused") {
			t.Errorf("accepted refs logged a refusal: %s", logs.String())
		}
	})

	t.Run("reporting run without an account compares repo only", func(t *testing.T) {
		s, rr, runRow, _, _, _ := upkeepBindingFixture(t, nil, "", false)
		runRow.AccountID = ""
		id := seedRef(rr, upkeepTestRepo, "acct-other")
		if _, ok, err := s.checkUpkeepRunRefs(ctx, runRow, []uuid.UUID{id}); err != nil || !ok {
			t.Fatalf("ok=%v err=%v, want accepted", ok, err)
		}
	})

	refusals := []struct {
		name   string
		seed   func(rr *upkeepRunRepo) uuid.UUID
		reason string
	}{
		{name: "unknown run", seed: func(*upkeepRunRepo) uuid.UUID { return uuid.New() }, reason: "unknown_run"},
		// Same account as the reporting run, so ONLY the repo check refuses.
		{name: "another repo", seed: func(rr *upkeepRunRepo) uuid.UUID { return seedRef(rr, "other/repo", "acct-1") }, reason: "foreign_repo"},
		{name: "empty repo", seed: func(rr *upkeepRunRepo) uuid.UUID { return seedRef(rr, "", "acct-1") }, reason: "foreign_repo"},
		// Same repo string, both accounts set and different: ONLY the account
		// check refuses.
		{name: "same repo, another account", seed: func(rr *upkeepRunRepo) uuid.UUID { return seedRef(rr, upkeepTestRepo, "acct-2") }, reason: "foreign_account"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			s, rr, runRow, _, _, logs := upkeepBindingFixture(t, nil, "", false)
			good := seedRef(rr, upkeepTestRepo, "acct-1")
			bad := tc.seed(rr)
			later := uuid.New() // unknown, but never reached: the first refusal wins
			refused, ok, err := s.checkUpkeepRunRefs(ctx, runRow, []uuid.UUID{good, bad, later})
			if err != nil {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if ok || refused != bad {
				t.Fatalf("= (%v, %v), want (%v, false)", refused, ok, bad)
			}
			// Unknown vs foreign reaches ONLY the WARN log.
			if l := logs.String(); !strings.Contains(l, `"reason":"`+tc.reason+`"`) || !strings.Contains(l, bad.String()) {
				t.Errorf("WARN log = %s, want reason %q naming %s", l, tc.reason, bad)
			}
		})
	}

	t.Run("empty repos on both sides never match", func(t *testing.T) {
		s, rr, runRow, _, _, _ := upkeepBindingFixture(t, nil, "", false)
		runRow.Repo = ""
		id := seedRef(rr, "", "acct-1")
		if refused, ok, err := s.checkUpkeepRunRefs(ctx, runRow, []uuid.UUID{id}); err != nil || ok || refused != id {
			t.Fatalf("= (%v, %v, %v), want (%v, false, nil)", refused, ok, err, id)
		}
	})

	t.Run("transport error is returned", func(t *testing.T) {
		s, rr, runRow, _, _, _ := upkeepBindingFixture(t, nil, "", false)
		id := uuid.New()
		rr.getRunErrs[id] = errors.New("connection reset")
		if _, ok, err := s.checkUpkeepRunRefs(ctx, runRow, []uuid.UUID{id}); err == nil || ok || !strings.Contains(err.Error(), "connection reset") {
			t.Fatalf("ok=%v err=%v, want the transport error", ok, err)
		}
	})

	t.Run("missing repo or reporting run is an error", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if _, ok, err := s.checkUpkeepRunRefs(ctx, &run.Run{ID: uuid.New()}, []uuid.UUID{uuid.New()}); err == nil || ok {
			t.Errorf("nil RunRepo: ok=%v err=%v, want error", ok, err)
		}
		s2, _, _, _, _, _ := upkeepBindingFixture(t, nil, "", false)
		if _, ok, err := s2.checkUpkeepRunRefs(ctx, nil, nil); err == nil || ok {
			t.Errorf("nil runRow: ok=%v err=%v, want error", ok, err)
		}
	})
}

// TestUpkeepRunOwnershipRefusal is the extracted predicate's direct table
// (#3922): the SAME rules checkUpkeepRunRefs applies and the flake gather
// relies on to show the agent only runs the ingest would accept.
func TestUpkeepRunOwnershipRefusal(t *testing.T) {
	reporting := &run.Run{Repo: upkeepTestRepo, AccountID: "acct-1"}
	cases := []struct {
		name      string
		ref       *run.Run
		reporting *run.Run
		want      string
	}{
		{"same repo, same account", &run.Run{Repo: upkeepTestRepo, AccountID: "acct-1"}, reporting, ""},
		{"same repo, other case", &run.Run{Repo: strings.ToUpper(upkeepTestRepo), AccountID: "acct-1"}, reporting, ""},
		// Account-less same-repo run: the account is compared only when BOTH
		// rows carry one (#3922 approval condition 8).
		{"account-less same-repo ref", &run.Run{Repo: upkeepTestRepo}, reporting, ""},
		{"account-less reporting run", &run.Run{Repo: upkeepTestRepo, AccountID: "acct-2"}, &run.Run{Repo: upkeepTestRepo}, ""},
		{"other repo", &run.Run{Repo: "other/repo", AccountID: "acct-1"}, reporting, upkeepOwnershipForeignRepo},
		{"empty ref repo", &run.Run{AccountID: "acct-1"}, reporting, upkeepOwnershipForeignRepo},
		{"both repos empty", &run.Run{}, &run.Run{}, upkeepOwnershipForeignRepo},
		{"same repo, other account", &run.Run{Repo: upkeepTestRepo, AccountID: "acct-2"}, reporting, upkeepOwnershipForeignAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upkeepRunOwnershipRefusal(tc.ref, tc.reporting); got != tc.want {
				t.Errorf("upkeepRunOwnershipRefusal(%+v, %+v) = %q, want %q", tc.ref, tc.reporting, got, tc.want)
			}
		})
	}
}
