// Package childcancel cancels the non-terminal decomposition children of a
// cancelled (or otherwise terminal) parent run (#4186).
//
// Before it, cancelling a decomposed parent transitioned only the parent, so
// every minted child that had not yet settled stayed pending/running forever:
// nothing could dispatch it (its parent's fan-out was gone), and it surfaced as
// an undispatched_child restart blocker. This package is the ONE core both
// consumers share: the server's run-cancel sinks (CascadeFromParent, wired
// after every post-admission cancel transition) and the one-shot operator
// backfill `fishhawkd reconcile-orphan-children` (FindOrphans + Reconcile).
//
// Every cancel goes through the run state machine (TransitionRun) and appends
// ONE system-actor decomposition_child_cancelled row on the CHILD's chain,
// deduplicated per (child chain, parent_run_id). Long-form contract:
// README.md next to this file.
package childcancel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Category is the audit category of the one row appended per cancelled child.
// Registered in backend/internal/audit/categories.go. INTERNAL: it is not an
// issue-comment activity surface.
const Category = "decomposition_child_cancelled"

// The reason vocabulary of a decomposition_child_cancelled row.
const (
	// ReasonParentCancelled: a run-cancel sink cancelled the parent and the
	// cascade cancelled this child in the same call.
	ReasonParentCancelled = "parent_cancelled"
	// ReasonParentTerminal: the orphan backfill found this child non-terminal
	// under an already cancelled or succeeded parent.
	ReasonParentTerminal = "parent_terminal"
)

// SourceBackfill is the cancel_source the orphan backfill stamps. The server
// sinks pass their own cancel_source vocabulary (operator_cancel,
// run_budget_exceeded, stage_budget_exceeded, stage_cancelled).
const SourceBackfill = "orphan_backfill"

// pageSize is the ListRuns page the child and orphan scans walk by Offset
// until a short page — the same shape as server.listAllDecomposedChildren.
const pageSize = 100

// Outcome is what CancelChild did to one child.
type Outcome string

// The outcome vocabulary.
const (
	// OutcomeCancelled: the child's run transitioned to cancelled (or a
	// same-state re-apply succeeded). Err may still carry an append error:
	// the state change landed, the audit row did not.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeSkippedTerminal: the child was already terminal (from the
	// snapshot, or a race the state machine refused). No transition, no row.
	OutcomeSkippedTerminal Outcome = "skipped_terminal"
	// OutcomeFailed: TransitionRun failed with a non-transition error. No row.
	OutcomeFailed Outcome = "failed"
)

// RunStore is the narrow run surface the package reads and writes. Satisfied
// by run.Repository.
type RunStore interface {
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
	ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error)
	ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error)
	TransitionRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error)
}

// AuditStore is the narrow audit surface. Satisfied by audit.Repository. When
// the concrete store also implements audit.DedupedChainAppender (the Postgres
// repository), the append is atomic under the run-row lock.
type AuditStore interface {
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// ChildResult is the per-child record CancelChild returns.
type ChildResult struct {
	ChildRunID  uuid.UUID
	ParentRunID uuid.UUID
	// FromState is the child's state in the snapshot the caller passed.
	FromState run.State
	// ImplementStageID / ImplementStageState name the child's implement stage
	// (nil / "" when it has none or the stage read failed).
	ImplementStageID    *uuid.UUID
	ImplementStageState run.StageState
	// LiveStage reports the implement stage was dispatched or running: a spawn
	// attempt exists, so a runner may still be finishing it. The run is
	// cancelled anyway (cancel, not refuse); the stage row is untouched.
	LiveStage bool
	Outcome   Outcome
	// Appended is true when a decomposition_child_cancelled row landed, false
	// when none was attempted or the dedup found an existing row.
	Appended bool
	// Err is the TransitionRun error for OutcomeFailed, or the append error
	// for OutcomeCancelled.
	Err error
}

// Orphan is one non-terminal decomposition child FindOrphans found under a
// cancelled or succeeded parent.
type Orphan struct {
	Child       *run.Run
	ParentState run.State
}

// CancelChild cancels one decomposition child of parentID and records why.
//
// (a) A child whose snapshot State is terminal is skipped with no transition
// and no row: postgresRepo.TransitionRun treats a same-state call as a
// successful no-op, so without this skip an already-cancelled child would
// "transition" and get a spurious row. (b) The child's stages are read
// best-effort to name the implement stage and derive LiveStage; a read error
// records an empty stage state rather than aborting the cancel. (c)
// TransitionRun(child, cancelled): an InvalidTransitionError (the child raced
// to succeeded/failed) is OutcomeSkippedTerminal with no row; any other error
// is OutcomeFailed with no row. (d) ONE system-actor row on the child's chain,
// deduplicated per parent_run_id; an append error is returned in Err with the
// outcome still OutcomeCancelled, because the state change landed.
func CancelChild(ctx context.Context, runs RunStore, au AuditStore, child *run.Run,
	parentID uuid.UUID, parentState run.State, reason, source string) ChildResult {
	res := ChildResult{ChildRunID: child.ID, ParentRunID: parentID, FromState: child.State}
	if child.State.IsTerminal() {
		res.Outcome = OutcomeSkippedTerminal
		return res
	}

	if stages, err := runs.ListStagesForRun(ctx, child.ID); err == nil {
		for _, st := range stages {
			if st.Type != run.StageTypeImplement {
				continue
			}
			id := st.ID
			res.ImplementStageID = &id
			res.ImplementStageState = st.State
			res.LiveStage = st.State == run.StageStateDispatched || st.State == run.StageStateRunning
			break
		}
	}

	if _, err := runs.TransitionRun(ctx, child.ID, run.StateCancelled); err != nil {
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			res.Outcome = OutcomeSkippedTerminal
			return res
		}
		res.Outcome = OutcomeFailed
		res.Err = err
		return res
	}
	res.Outcome = OutcomeCancelled

	var stageID any
	if res.ImplementStageID != nil {
		stageID = res.ImplementStageID.String()
	}
	payload, _ := json.Marshal(map[string]any{
		"parent_run_id":         parentID.String(),
		"parent_state":          string(parentState),
		"reason":                reason,
		"cancel_source":         source,
		"from_state":            string(res.FromState),
		"implement_stage_id":    stageID,
		"implement_stage_state": string(res.ImplementStageState),
		"live_stage":            res.LiveStage,
	})
	actor := audit.ActorSystem
	appended, err := appendOnce(ctx, au, child.ID, parentID, audit.ChainAppendParams{
		RunID:     child.ID,
		Timestamp: time.Now().UTC(),
		Category:  Category,
		ActorKind: &actor,
		Payload:   payload,
	})
	res.Appended = appended
	if err != nil {
		res.Err = fmt.Errorf("append %s: %w", Category, err)
	}
	return res
}

// appendOnce appends p at most once per (child chain, parent_run_id). On a
// store implementing audit.DedupedChainAppender the scan runs inside the
// append transaction under the run-row lock; a *DedupedDuplicateError is the
// benign already-recorded branch. Otherwise it falls back to the non-atomic
// list-then-append (mirroring server.appendRetirementDropOnce): a list error
// proceeds without the guard, a match appends nothing.
func appendOnce(ctx context.Context, au AuditStore, childID, parentID uuid.UUID, p audit.ChainAppendParams) (bool, error) {
	if d, ok := au.(audit.DedupedChainAppender); ok {
		_, err := d.AppendChainedDeduped(ctx, p, audit.DedupeSpec{
			PayloadKey: "parent_run_id", PayloadValue: parentID.String(),
		})
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if entries, err := au.ListForRunByCategory(ctx, childID, Category); err == nil && alreadyRecorded(entries, parentID) {
		return false, nil
	}
	if _, err := au.AppendChained(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// alreadyRecorded reports whether entries already carry a row for parentID —
// the fallback leg's scan of the dedup key.
func alreadyRecorded(entries []*audit.Entry, parentID uuid.UUID) bool {
	for _, e := range entries {
		var payload struct {
			ParentRunID string `json:"parent_run_id"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil {
			continue
		}
		if payload.ParentRunID == parentID.String() {
			return true
		}
	}
	return false
}

// CascadeFromParent cancels every non-terminal decomposition child of parent
// with ReasonParentCancelled and the caller's cancel_source. It pages ListRuns
// by DecomposedFrom to exhaustion BEFORE touching any child, so a list error
// returns with no child transitioned.
func CascadeFromParent(ctx context.Context, runs RunStore, au AuditStore, parent *run.Run, source string) ([]ChildResult, error) {
	var children []*run.Run
	for offset := 0; ; offset += pageSize {
		page, err := runs.ListRuns(ctx, run.ListRunsFilter{
			DecomposedFrom: &parent.ID,
			Limit:          pageSize,
			Offset:         offset,
		})
		if err != nil {
			return nil, fmt.Errorf("list decomposition children of %s: %w", parent.ID, err)
		}
		children = append(children, page...)
		if len(page) < pageSize {
			break
		}
	}
	results := make([]ChildResult, 0, len(children))
	for _, child := range children {
		results = append(results, CancelChild(ctx, runs, au, child, parent.ID, parent.State, ReasonParentCancelled, source))
	}
	return results, nil
}

// orphanParentState reports whether a child of a parent in state s is an
// orphan the backfill cancels. Only cancelled and succeeded parents qualify:
// a FAILED parent is deliberately excluded because ReviveRun can re-admit it,
// and its children are then wanted again; a non-terminal parent still owns
// its fan-out.
func orphanParentState(s run.State) bool {
	return s == run.StateCancelled || s == run.StateSucceeded
}

// FindOrphans returns every non-terminal (pending or running) decomposition
// child whose parent is cancelled or succeeded, across every tenant (the
// filter's AccountID is left empty, as host-admin subcommands do). It COLLECTS
// the full candidate set before resolving parents, so a later Reconcile that
// shrinks the running set cannot shift an offset page mid-scan. Each parent is
// read once; a parent GetRun error fails the whole scan closed.
func FindOrphans(ctx context.Context, runs RunStore) ([]Orphan, error) {
	var candidates []*run.Run
	for _, st := range []run.State{run.StatePending, run.StateRunning} {
		for offset := 0; ; offset += pageSize {
			page, err := runs.ListRuns(ctx, run.ListRunsFilter{State: string(st), Limit: pageSize, Offset: offset})
			if err != nil {
				return nil, fmt.Errorf("list %s runs: %w", st, err)
			}
			candidates = append(candidates, page...)
			if len(page) < pageSize {
				break
			}
		}
	}
	parents := map[uuid.UUID]run.State{}
	var out []Orphan
	for _, c := range candidates {
		if c.DecomposedFrom == nil {
			continue
		}
		pid := *c.DecomposedFrom
		ps, ok := parents[pid]
		if !ok {
			p, err := runs.GetRun(ctx, pid)
			if err != nil {
				return nil, fmt.Errorf("read parent %s of child %s: %w", pid, c.ID, err)
			}
			ps = p.State
			parents[pid] = ps
		}
		if !orphanParentState(ps) {
			continue
		}
		out = append(out, Orphan{Child: c, ParentState: ps})
	}
	return out, nil
}

// Reconcile cancels each orphan with ReasonParentTerminal / SourceBackfill.
func Reconcile(ctx context.Context, runs RunStore, au AuditStore, orphans []Orphan) []ChildResult {
	results := make([]ChildResult, 0, len(orphans))
	for _, o := range orphans {
		results = append(results, CancelChild(ctx, runs, au, o.Child, *o.Child.DecomposedFrom, o.ParentState, ReasonParentTerminal, SourceBackfill))
	}
	return results
}
