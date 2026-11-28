package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Detached, single-flight split-proposal child filing (#3925, E50.25).
//
// The on-approval split filing used to run on the approval REQUEST's context,
// so a client that timed out mid-filing truncated it, and the only re-entry
// was a re-approval the approve CAS no longer admits as a launch. The filing
// now runs DETACHED on context.WithoutCancel(parent) under a phase-count-scaled
// budget, tracked by Server.bgSplitFiling (drained by Shutdown), and every
// launch is gated by a process-local per-run single-flight registry.
//
// A detached filing that aborts (budget expiry, a provider or audit error, a
// process exit) writes NO terminal marker. A missing split_children_filed entry
// is therefore NOT proof that a filing is still making progress: the longest a
// caller has to wait is the filing's budget (splitFilingBudgetFor, reported as
// budget_seconds by the resume verb), and the probe is the resume call itself —
// POST /v0/runs/{run_id}/split-filing/resume answers 409
// split_filing_in_progress while a filing for the run still holds the registry.

// splitHoldKind classifies who holds a run's split-filing registry slot.
type splitHoldKind int

const (
	// splitHoldFirstApproval is a plan-stage approval whose caller OBSERVED the
	// stage before it succeeded. Two first approvals of the SAME stage may
	// co-hold: serializing them is the approve CAS's job (E50.16), and letting
	// them co-hold keeps the CAS's measured 3 → 6 counterfactual observable.
	splitHoldFirstApproval splitHoldKind = iota + 1
	// splitHoldReapproval is an approval whose caller observed the stage
	// already succeeded (a same-state re-approval). It claims exclusively.
	splitHoldReapproval
	// splitHoldResume is the operator resume verb. It claims exclusively.
	splitHoldResume
)

func (k splitHoldKind) String() string {
	switch k {
	case splitHoldFirstApproval:
		return "first_approval"
	case splitHoldReapproval:
		return "reapproval"
	case splitHoldResume:
		return "resume"
	default:
		return "unknown"
	}
}

type splitHolder struct {
	seq     uint64
	kind    splitHoldKind
	stageID uuid.UUID
}

// splitFilingRegistry is the process-local per-run single-flight guard for
// split filing. It is PROCESS-LOCAL: a second replica sees neither another
// replica's claim nor its in-flight filing. Zero value is not ready; build it
// with newSplitFilingRegistry.
type splitFilingRegistry struct {
	mu      sync.Mutex
	cond    *sync.Cond
	seq     uint64
	holders map[uuid.UUID][]splitHolder
}

func newSplitFilingRegistry() *splitFilingRegistry {
	r := &splitFilingRegistry{holders: map[uuid.UUID][]splitHolder{}}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// splitFilingHold is one admitted claim. release is idempotent.
type splitFilingHold struct {
	reg   *splitFilingRegistry
	runID uuid.UUID
	seq   uint64
	once  sync.Once
}

// claim admits a holder when the run has no holder, OR when the claim is a
// first approval and EVERY existing holder is a first approval of the SAME
// stage. Every other combination is refused (nil hold).
func (r *splitFilingRegistry) claim(kind splitHoldKind, runID, stageID uuid.UUID) *splitFilingHold {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing := r.holders[runID]
	for _, h := range existing {
		if kind != splitHoldFirstApproval || h.kind != splitHoldFirstApproval || h.stageID != stageID {
			return nil
		}
	}
	r.seq++
	r.holders[runID] = append(existing, splitHolder{seq: r.seq, kind: kind, stageID: stageID})
	return &splitFilingHold{reg: r, runID: runID, seq: r.seq}
}

// release drops the hold and wakes any waiter. Safe on a nil hold and safe to
// call more than once.
func (h *splitFilingHold) release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		r := h.reg
		r.mu.Lock()
		defer r.mu.Unlock()
		cur := r.holders[h.runID]
		out := cur[:0]
		for _, x := range cur {
			if x.seq != h.seq {
				out = append(out, x)
			}
		}
		if len(out) == 0 {
			delete(r.holders, h.runID)
		} else {
			r.holders[h.runID] = out
		}
		r.cond.Broadcast()
	})
}

// holdersFor reports how many claims the run currently holds (test sync point).
func (r *splitFilingRegistry) holdersFor(runID uuid.UUID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.holders[runID])
}

// waitHoldersAtMost blocks until the run holds at most n claims (test sync
// point; no wall clock).
func (r *splitFilingRegistry) waitHoldersAtMost(runID uuid.UUID, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.holders[runID]) > n {
		r.cond.Wait()
	}
}

// splitFilingBudget is the FLOOR of a detached split filing's budget;
// splitFilingPerChildBudget scales it by phase count. splitFilingPrelaunchBudget
// bounds the launcher's own plan load, which runs before anything is spawned.
// Vars, not consts, so a test can shrink them.
var (
	splitFilingBudget          = 5 * time.Minute
	splitFilingPerChildBudget  = 60 * time.Second
	splitFilingPrelaunchBudget = 30 * time.Second
)

// splitFilingBudgetFor is the PURE budget for filing n phases:
// max(splitFilingBudget, n*splitFilingPerChildBudget).
func splitFilingBudgetFor(n int) time.Duration {
	if scaled := time.Duration(n) * splitFilingPerChildBudget; scaled > splitFilingBudget {
		return scaled
	}
	return splitFilingBudget
}

// splitFilingOutcome is what a launch attempt did.
type splitFilingOutcome int

const (
	// splitFilingLaunched: a detached filer now owns the hold.
	splitFilingLaunched splitFilingOutcome = iota + 1
	// splitFilingNoSplit: the stage's own artifact is absent, undecodable or
	// carries no split_proposal phases; nothing was spawned.
	splitFilingNoSplit
	// splitFilingBusy: the registry refused the claim (nil hold).
	splitFilingBusy
	// splitFilingLoadError: the own-artifact read failed (an IO error, never
	// reported as busy).
	splitFilingLoadError
	// splitFilingNotConfigured: AuditRepo, RunRepo or ArtifactRepo is nil.
	splitFilingNotConfigured
	// splitFilingPlanChanged: the stage's newest plan artifact postdates the
	// transition that succeeded the plan stage (AA1); nothing was spawned.
	splitFilingPlanChanged
)

// loadSplitFilingPhases is the launcher's half that READS: it loads the stage's
// OWN newest plan artifact through loadSplitPlanForStage on a context bounded
// by splitFilingPrelaunchBudget and detached from the caller's cancellation.
// It spawns nothing. On splitFilingLaunched-eligible input it returns the
// pinned source and outcome 0 (proceed); otherwise the outcome says why not.
// The returned source carries the artifact id and times on
// splitFilingPlanChanged so a caller can name them.
func (s *Server) loadSplitFilingPhases(parent context.Context, stage *run.Stage) (splitPlanSource, splitFilingOutcome) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil {
		return splitPlanSource{}, splitFilingNotConfigured
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), splitFilingPrelaunchBudget)
	defer cancel()
	src, status, err := s.loadSplitPlanForStage(ctx, stage)
	if err != nil {
		s.logSplitFilingWarn(parent, stage.RunID, "load approved plan failed", err.Error())
		return src, splitFilingLoadError
	}
	switch status {
	case splitPlanChangedSinceApproval:
		return src, splitFilingPlanChanged
	case splitPlanOK:
	default:
		return src, splitFilingNoSplit
	}
	if src.plan.SplitProposal == nil || len(src.plan.SplitProposal.Phases) == 0 {
		return src, splitFilingNoSplit
	}
	return src, 0
}

// spawnSplitFiling is the launcher's half that WRITES: it takes ownership of
// hold and files src.plan (the PINNED plan the caller loaded, so the artifact a
// caller checked is the artifact filed) on a goroutine tracked by
// bgSplitFiling, under context.WithoutCancel(parent) bounded by
// splitFilingBudgetFor(phases). The goroutine always releases the hold and
// calls Done, including after a panic, which it recovers and logs at ERROR
// (AA4) so a panicking provider can neither wedge the registry nor Shutdown.
func (s *Server) spawnSplitFiling(parent context.Context, stage *run.Stage, hold *splitFilingHold, src splitPlanSource) time.Duration {
	phases := 0
	if src.plan != nil && src.plan.SplitProposal != nil {
		phases = len(src.plan.SplitProposal.Phases)
	}
	budget := splitFilingBudgetFor(phases)
	st := *stage
	s.bgSplitFiling.Add(1)
	s.splitFilingLaunches.Add(1)
	go func() {
		defer s.bgSplitFiling.Done()
		defer hold.release()
		defer func() {
			if rec := recover(); rec != nil {
				s.cfg.Logger.LogAttrs(context.WithoutCancel(parent), slog.LevelError,
					"split filing: detached filer panicked; registry released, no completion marker written",
					slog.String("run_id", st.RunID.String()),
					slog.String("stage_id", st.ID.String()),
					slog.String("panic", fmt.Sprint(rec)),
				)
			}
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget)
		defer cancel()
		s.fileSplitProposalChildrenFrom(ctx, &st, src.plan)
	}()
	return budget
}

// startSplitFilingDetached runs both launcher halves for the approval path. A
// nil hold (the registry refused the claim) is busy and reads nothing. Every
// non-launch outcome releases the hold; on launch the goroutine owns it. A load
// error and a plan changed since the gate closed each log their own WARN here
// (inside loadSplitFilingPhases and below); busy is the caller's to log.
func (s *Server) startSplitFilingDetached(parent context.Context, stage *run.Stage, hold *splitFilingHold) splitFilingOutcome {
	if hold == nil {
		return splitFilingBusy
	}
	src, outcome := s.loadSplitFilingPhases(parent, stage)
	if outcome != 0 {
		hold.release()
		if outcome == splitFilingPlanChanged {
			// AA1 on the approval path: skip the launch and WARN; the approval
			// itself is never failed for it.
			s.logSplitFilingPlanChanged(parent, stage, src)
		}
		return outcome
	}
	s.spawnSplitFiling(parent, stage, hold, src)
	return splitFilingLaunched
}
