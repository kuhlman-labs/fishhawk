// Package stalesweep reconciles stale non-terminal TOP-LEVEL runs: pending or
// running runs with no activity inside a threshold, which otherwise feed the
// liveness guards, restart blockers and run views forever (#4185).
//
// It is the core of the one-shot operator subcommand `fishhawkd
// sweep-stale-runs`. Find pages every pending and running run, drops
// decomposition children (those belong to `fishhawkd
// reconcile-orphan-children`, #4186), reads each run's stages and audit chain,
// and classifies it (first match wins): fresh, live_runner, stages_settled,
// merged, pr_closed, pr_unobserved, abandoned. Apply transitions the
// candidates whose class calls for it through the run state machine, cancels
// the parked stages of a cancelled run, cascades the cancel to decomposition
// children (childcancel.CascadeFromParent) and appends ONE system-actor
// stale_run_swept row per transitioned run, deduplicated per run. Long-form
// contract: README.md next to this file.
package stalesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcancel"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Category is the audit category of the one row appended per swept run.
// Registered in backend/internal/audit/categories.go. INTERNAL: it is not an
// issue-comment activity surface.
const Category = "stale_run_swept"

// Source is the row's `source` payload value (the dedup key) and the
// cancel_source the child cascade stamps on decomposition_child_cancelled.
const Source = "stale_sweep"

// The PR-evidence audit categories the classifier reads. They are the server's
// own category strings (server.CategoryPRMerged and friends, pinned by
// TestEvidenceCategoriesMatchServer in backend/cmd/fishhawkd); the server
// package is not imported here to keep this package's dependency surface to
// run + audit + childcancel.
const (
	CategoryPRMerged                 = "pr_merged"
	CategoryPostMergeObserved        = "post_merge_observed"
	CategoryMergeObservationRecorded = "merge_observation_recorded"
	CategoryPRClosedWithoutMerge     = "pr_closed_without_merge"
	CategoryPullRequestOpened        = "pull_request_opened"
)

// Class is a candidate's classification.
type Class string

// The class vocabulary, in precedence order.
const (
	// ClassFresh: some activity signal falls inside the threshold. Skipped.
	ClassFresh Class = "fresh"
	// ClassLiveRunner: a host fishhawk-runner process carries this run's id.
	// Skipped.
	ClassLiveRunner Class = "live_runner"
	// ClassStagesSettled: Advance's own walk says the run should already have
	// completed. Reconciled to the completeRun target.
	ClassStagesSettled Class = "stages_settled"
	// ClassMerged: merge evidence is on the chain. Delegated to reconcile-merge.
	ClassMerged Class = "merged"
	// ClassPRClosed: the PR closed without merging. Cancelled.
	ClassPRClosed Class = "pr_closed"
	// ClassPRUnobserved: a PR exists but no merge/close observation does.
	// Skipped unless Options.CancelUnobservedPR.
	ClassPRUnobserved Class = "pr_unobserved"
	// ClassAbandoned: stale with no PR evidence at all. Cancelled.
	ClassAbandoned Class = "abandoned"
)

// Classes is the class vocabulary in precedence order (for stable summaries).
var Classes = []Class{ClassFresh, ClassLiveRunner, ClassStagesSettled, ClassMerged, ClassPRClosed, ClassPRUnobserved, ClassAbandoned}

// Action is what Apply does with a candidate.
type Action string

// The action vocabulary.
const (
	ActionSkip                   Action = "skip"
	ActionReconcileSucceeded     Action = "reconcile_succeeded"
	ActionReconcileFailed        Action = "reconcile_failed"
	ActionReconcileCancelled     Action = "reconcile_cancelled"
	ActionCancel                 Action = "cancel"
	ActionDelegateReconcileMerge Action = "delegate_reconcile_merge"
)

// Transitions reports whether Apply transitions a candidate with action a.
func (a Action) Transitions() bool {
	switch a {
	case ActionReconcileSucceeded, ActionReconcileFailed, ActionReconcileCancelled, ActionCancel:
		return true
	default:
		return false
	}
}

// The reason vocabulary stamped on a stale_run_swept row.
const (
	ReasonStaleSweep           = "stale_sweep"
	ReasonPRClosedWithoutMerge = "pr_closed_without_merge"
	ReasonPRUnobserved         = "stale_sweep_pr_unobserved"
	ReasonStagesSettled        = "stages_settled"
)

// PREvidence summarises the PR evidence on a run, strongest first.
type PREvidence string

// The PR-evidence vocabulary.
const (
	PREvidenceMerged PREvidence = "merged"
	PREvidenceClosed PREvidence = "closed"
	PREvidenceOpened PREvidence = "opened"
	PREvidenceNone   PREvidence = "none"
)

// Outcome is what Apply did to one candidate.
type Outcome string

// The outcome vocabulary.
const (
	// OutcomeTransitioned: the run reached its target. Err may still carry a
	// cascade or append error: the state change landed, something after it
	// did not.
	OutcomeTransitioned Outcome = "transitioned"
	// OutcomeSkippedTerminal: the re-read found the run terminal, or the state
	// machine refused the transition (a race). No row.
	OutcomeSkippedTerminal Outcome = "skipped_terminal"
	// OutcomeSkippedChanged: the run's updated_at moved since the scan, or is
	// now inside the threshold. No transition, no row.
	OutcomeSkippedChanged Outcome = "skipped_changed"
	// OutcomeFailed: the re-read or a transition failed with a non-transition
	// error. Err is set; no row.
	OutcomeFailed Outcome = "failed"
)

// pageSize is the ListRuns page Find walks by Offset until a short page.
const pageSize = 100

// RunStore is the narrow run surface. Satisfied by run.Repository, and a
// superset of childcancel.RunStore so the same value feeds the cascade.
type RunStore interface {
	childcancel.RunStore
	TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, completion *run.StageCompletion) (*run.Stage, error)
}

// AuditStore is the narrow audit surface. Satisfied by audit.Repository, and a
// superset of childcancel.AuditStore. When the concrete store also implements
// audit.DedupedChainAppender (the Postgres repository), the append is atomic
// under the run-row lock.
type AuditStore interface {
	childcancel.AuditStore
	ListForRun(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error)
}

// Options tunes Find and Apply.
type Options struct {
	// Threshold is the inactivity window: a run whose last activity is newer
	// than Now()-Threshold is fresh.
	Threshold time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CancelUnobservedPR makes pr_unobserved runs cancel instead of skip.
	CancelUnobservedPR bool
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Candidate is one classified pending/running top-level run.
type Candidate struct {
	// Run and Stages are the scan snapshot.
	Run    *run.Run
	Stages []*run.Stage
	Class  Class
	Action Action
	// Target is the state Apply drives the run to ("" for a non-transitioning
	// action).
	Target run.State
	// Reason is the stale_run_swept reason ("" for a non-transitioning action).
	Reason       string
	LastActivity time.Time
	PREvidence   PREvidence
	// PullRequestURL is the run's recorded PR URL ("" when none).
	PullRequestURL string
}

// CancelledStage is one stage Apply cancelled on a cancelled run.
type CancelledStage struct {
	StageID   uuid.UUID
	StageType run.StageType
	FromState run.StageState
}

// Result is Apply's per-candidate record.
type Result struct {
	RunID  uuid.UUID
	Class  Class
	Action Action
	// FromState is the run's state at the re-read.
	FromState run.State
	// ToState is the state reached (FromState when nothing transitioned).
	ToState run.State
	// TransitionPath is the sequence of TransitionRun targets that LANDED.
	TransitionPath    []run.State
	Outcome           Outcome
	StagesCancelled   []CancelledStage
	ChildrenCancelled int
	// Appended is true when a stale_run_swept row landed, false when none was
	// attempted or the dedup found an existing row.
	Appended bool
	// Err is the failure for OutcomeFailed, or a cascade/append error for
	// OutcomeTransitioned.
	Err error
}

// Find pages every pending and running run (every tenant: the filter's
// AccountID is left empty, the host-admin convention), COLLECTS the full set
// before reading anything else, drops decomposition children, reads each run's
// stages and audit chain, probes for live runners once, and classifies each
// run. Any run-list, stage or audit read error fails the WHOLE scan closed
// (nil candidates): unreadable evidence must never default a run into a
// cancelling class. A probe failure does NOT fail the scan; it is returned in
// ProbeReport.Err and no run is classified live_runner — the caller must
// refuse to apply on it.
func Find(ctx context.Context, runs RunStore, au AuditStore, probe RunnerProbe, opts Options) ([]Candidate, ProbeReport, error) {
	var collected []*run.Run
	for _, st := range []run.State{run.StatePending, run.StateRunning} {
		for offset := 0; ; offset += pageSize {
			page, err := runs.ListRuns(ctx, run.ListRunsFilter{State: string(st), Limit: pageSize, Offset: offset})
			if err != nil {
				return nil, ProbeReport{}, fmt.Errorf("list %s runs: %w", st, err)
			}
			collected = append(collected, page...)
			if len(page) < pageSize {
				break
			}
		}
	}

	type evidence struct {
		r       *run.Run
		stages  []*run.Stage
		entries []*audit.Entry
	}
	var scanned []evidence
	for _, r := range collected {
		if r.DecomposedFrom != nil {
			continue
		}
		stages, err := runs.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return nil, ProbeReport{}, fmt.Errorf("list stages of run %s: %w", r.ID, err)
		}
		entries, err := au.ListForRun(ctx, r.ID)
		if err != nil {
			return nil, ProbeReport{}, fmt.Errorf("list audit entries of run %s: %w", r.ID, err)
		}
		scanned = append(scanned, evidence{r: r, stages: stages, entries: entries})
	}

	report := probeOnce(ctx, probe)
	now := opts.now()
	out := make([]Candidate, 0, len(scanned))
	for _, e := range scanned {
		out = append(out, classify(e.r, e.stages, e.entries, report.RunIDs, now, opts))
	}
	return out, report, nil
}

// probeOnce runs the probe, folding its error (or an absent probe) into
// ProbeReport.Err with an empty run-id set.
func probeOnce(ctx context.Context, probe RunnerProbe) ProbeReport {
	if probe == nil {
		return ProbeReport{Err: errors.New("no runner probe configured")}
	}
	rep, err := probe.LiveRunners(ctx)
	if err != nil {
		return ProbeReport{Err: err}
	}
	return rep
}

// lastActivity is max(run.UpdatedAt, every stage.UpdatedAt, the newest audit
// entry Timestamp). run.updated_at alone is not trusted: bulk touches stamp
// it on rows nothing has driven for months.
func lastActivity(r *run.Run, stages []*run.Stage, entries []*audit.Entry) time.Time {
	last := r.UpdatedAt
	for _, s := range stages {
		if s.UpdatedAt.After(last) {
			last = s.UpdatedAt
		}
	}
	for _, e := range entries {
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
	}
	return last
}

// settledTarget mirrors orchestrator.Advance's walk and completeRun's target.
// Walking stages in Sequence order, a failed or cancelled stage reached
// before the first PENDING stage completes the run; with no pending stage and
// no non-terminal one either (and at least one stage), every stage is
// terminal and the run completes. The target is failed if any stage failed,
// else cancelled if any was cancelled, else succeeded.
func settledTarget(stages []*run.Stage) (run.State, bool) {
	if len(stages) == 0 {
		return "", false
	}
	ordered := append([]*run.Stage(nil), stages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	settled := false
	allTerminal := true
	for _, s := range ordered {
		if s.State == run.StageStateFailed || s.State == run.StageStateCancelled {
			settled = true
			break
		}
		if s.State == run.StageStatePending {
			allTerminal = false
			break
		}
		if !s.State.IsTerminal() {
			allTerminal = false
		}
	}
	if !settled && !allTerminal {
		return "", false
	}
	target := run.StateSucceeded
	for _, s := range ordered {
		if s.State == run.StageStateFailed {
			return run.StateFailed, true
		}
		if s.State == run.StageStateCancelled {
			target = run.StateCancelled
		}
	}
	return target, true
}

// prEvidence returns the strongest PR evidence on the run: merged, then
// closed, then opened (a recorded PR URL or a pull_request_opened row), then
// none.
func prEvidence(r *run.Run, entries []*audit.Entry) PREvidence {
	var merged, closed, opened bool
	for _, e := range entries {
		switch e.Category {
		case CategoryPRMerged, CategoryPostMergeObserved, CategoryMergeObservationRecorded:
			merged = true
		case CategoryPRClosedWithoutMerge:
			closed = true
		case CategoryPullRequestOpened:
			opened = true
		}
	}
	if r.PullRequestURL != nil && *r.PullRequestURL != "" {
		opened = true
	}
	switch {
	case merged:
		return PREvidenceMerged
	case closed:
		return PREvidenceClosed
	case opened:
		return PREvidenceOpened
	default:
		return PREvidenceNone
	}
}

// classify assigns the first matching class. See the package README for the
// precedence table.
func classify(r *run.Run, stages []*run.Stage, entries []*audit.Entry, live map[uuid.UUID]bool, now time.Time, opts Options) Candidate {
	c := Candidate{
		Run: r, Stages: stages, Action: ActionSkip,
		LastActivity: lastActivity(r, stages, entries),
		PREvidence:   prEvidence(r, entries),
	}
	if r.PullRequestURL != nil {
		c.PullRequestURL = *r.PullRequestURL
	}
	if now.Sub(c.LastActivity) < opts.Threshold {
		c.Class = ClassFresh
		return c
	}
	if live[r.ID] {
		c.Class = ClassLiveRunner
		return c
	}
	// A stages-settled run reconciles to the completeRun target, EXCEPT a
	// succeeded target on a run whose PR closed unmerged or was never
	// observed: stamping it succeeded would claim a change shipped that did
	// not (closed) or may not have (unobserved), so it falls through to the
	// PR-evidence class instead.
	if target, ok := settledTarget(stages); ok &&
		(target != run.StateSucceeded || (c.PREvidence != PREvidenceClosed && c.PREvidence != PREvidenceOpened)) {
		c.Class = ClassStagesSettled
		c.Target = target
		c.Reason = ReasonStagesSettled
		switch target {
		case run.StateFailed:
			c.Action = ActionReconcileFailed
		case run.StateCancelled:
			c.Action = ActionReconcileCancelled
		default:
			c.Action = ActionReconcileSucceeded
		}
		return c
	}
	switch c.PREvidence {
	case PREvidenceMerged:
		c.Class = ClassMerged
		c.Action = ActionDelegateReconcileMerge
	case PREvidenceClosed:
		c.Class = ClassPRClosed
		c.Action = ActionCancel
		c.Target = run.StateCancelled
		c.Reason = ReasonPRClosedWithoutMerge
	case PREvidenceOpened:
		c.Class = ClassPRUnobserved
		if opts.CancelUnobservedPR {
			c.Action = ActionCancel
			c.Target = run.StateCancelled
			c.Reason = ReasonPRUnobserved
		}
	default:
		c.Class = ClassAbandoned
		c.Action = ActionCancel
		c.Target = run.StateCancelled
		c.Reason = ReasonStaleSweep
	}
	return c
}

// transitionPath is the sequence of legal TransitionRun targets from from to
// target: pending→succeeded is not a legal edge, so it walks via running.
func transitionPath(from, target run.State) []run.State {
	if from == run.StatePending && target == run.StateSucceeded {
		return []run.State{run.StateRunning, run.StateSucceeded}
	}
	return []run.State{target}
}

// Apply transitions every candidate whose Action transitions, in order, and
// returns one Result each. Per candidate: (a) re-read the run — terminal →
// skipped_terminal; updated_at moved since the snapshot or inside the
// threshold → skipped_changed; a read error → failed. (b) Walk the legal
// transition path; an InvalidTransitionError → skipped_terminal, any other
// error → failed, both with no row. (c) A cancelled target also CAS-cancels
// every non-terminal stage (pinned to its scanned state; a refused CAS is
// omitted from the record) and cascades the cancel to decomposition children.
// (d) ONE system-actor stale_run_swept row, after every transition, deduped
// per run.
func Apply(ctx context.Context, runs RunStore, au AuditStore, cands []Candidate, opts Options) []Result {
	var out []Result
	for _, c := range cands {
		if !c.Action.Transitions() {
			continue
		}
		out = append(out, applyOne(ctx, runs, au, c, opts))
	}
	return out
}

func applyOne(ctx context.Context, runs RunStore, au AuditStore, c Candidate, opts Options) Result {
	res := Result{RunID: c.Run.ID, Class: c.Class, Action: c.Action, FromState: c.Run.State, ToState: c.Run.State}

	cur, err := runs.GetRun(ctx, c.Run.ID)
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Err = fmt.Errorf("re-read run: %w", err)
		return res
	}
	res.FromState, res.ToState = cur.State, cur.State
	if cur.State.IsTerminal() {
		res.Outcome = OutcomeSkippedTerminal
		return res
	}
	if cur.UpdatedAt.After(c.Run.UpdatedAt) || opts.now().Sub(cur.UpdatedAt) < opts.Threshold {
		res.Outcome = OutcomeSkippedChanged
		return res
	}

	updated := cur
	for _, step := range transitionPath(cur.State, c.Target) {
		next, err := runs.TransitionRun(ctx, c.Run.ID, step)
		if err != nil {
			var inv run.InvalidTransitionError
			if errors.As(err, &inv) {
				res.Outcome = OutcomeSkippedTerminal
				return res
			}
			res.Outcome = OutcomeFailed
			res.Err = fmt.Errorf("transition run to %s: %w", step, err)
			return res
		}
		updated = next
		res.TransitionPath = append(res.TransitionPath, step)
		res.ToState = step
	}
	res.Outcome = OutcomeTransitioned

	var errs []error
	if c.Target == run.StateCancelled {
		for _, st := range c.Stages {
			if st.State.IsTerminal() {
				continue
			}
			if _, err := runs.TransitionStageFrom(ctx, st.ID, st.State, run.StageStateCancelled, nil); err != nil {
				continue
			}
			res.StagesCancelled = append(res.StagesCancelled, CancelledStage{StageID: st.ID, StageType: st.Type, FromState: st.State})
		}
		children, err := childcancel.CascadeFromParent(ctx, runs, au, updated, Source)
		if err != nil {
			errs = append(errs, fmt.Errorf("cascade: %w", err))
		}
		for _, ch := range children {
			if ch.Outcome == childcancel.OutcomeCancelled {
				res.ChildrenCancelled++
			}
			if ch.Err != nil {
				errs = append(errs, fmt.Errorf("cascade child %s: %w", ch.ChildRunID, ch.Err))
			}
		}
	}

	appended, err := appendOnce(ctx, au, c.Run.ID, buildRow(c, res, opts))
	res.Appended = appended
	if err != nil {
		errs = append(errs, fmt.Errorf("append %s: %w", Category, err))
	}
	res.Err = errors.Join(errs...)
	return res
}

// buildRow renders the stale_run_swept row for a transitioned candidate.
func buildRow(c Candidate, res Result, opts Options) audit.ChainAppendParams {
	path := make([]string, 0, len(res.TransitionPath))
	for _, s := range res.TransitionPath {
		path = append(path, string(s))
	}
	stages := make([]map[string]any, 0, len(res.StagesCancelled))
	for _, s := range res.StagesCancelled {
		stages = append(stages, map[string]any{
			"stage_id": s.StageID.String(), "stage_type": string(s.StageType), "from_state": string(s.FromState),
		})
	}
	var prURL any
	if c.PullRequestURL != "" {
		prURL = c.PullRequestURL
	}
	payload, _ := json.Marshal(map[string]any{
		"source":             Source,
		"class":              string(c.Class),
		"reason":             c.Reason,
		"from_state":         string(res.FromState),
		"to_state":           string(res.ToState),
		"transition_path":    path,
		"threshold_days":     int64(opts.Threshold / (24 * time.Hour)),
		"last_activity_at":   c.LastActivity.UTC().Format(time.RFC3339Nano),
		"pr_evidence":        string(c.PREvidence),
		"pull_request_url":   prURL,
		"stages_cancelled":   stages,
		"children_cancelled": res.ChildrenCancelled,
	})
	actor := audit.ActorSystem
	return audit.ChainAppendParams{
		RunID:     c.Run.ID,
		Timestamp: opts.now().UTC(),
		Category:  Category,
		ActorKind: &actor,
		Payload:   payload,
	}
}

// appendOnce appends p at most once per run (dedup key source=stale_sweep). On
// a store implementing audit.DedupedChainAppender the scan runs inside the
// append transaction under the run-row lock; a *DedupedDuplicateError is the
// benign already-recorded branch. Otherwise it falls back to the non-atomic
// list-then-append (mirroring childcancel's appendOnce): a list error proceeds
// without the guard, a match appends nothing.
func appendOnce(ctx context.Context, au AuditStore, runID uuid.UUID, p audit.ChainAppendParams) (bool, error) {
	if d, ok := au.(audit.DedupedChainAppender); ok {
		_, err := d.AppendChainedDeduped(ctx, p, audit.DedupeSpec{PayloadKey: "source", PayloadValue: Source})
		var dup *audit.DedupedDuplicateError
		if errors.As(err, &dup) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if entries, err := au.ListForRunByCategory(ctx, runID, Category); err == nil && alreadyRecorded(entries) {
		return false, nil
	}
	if _, err := au.AppendChained(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// alreadyRecorded reports whether entries already carry a source=stale_sweep
// row — the fallback leg's scan of the dedup key.
func alreadyRecorded(entries []*audit.Entry) bool {
	for _, e := range entries {
		var payload struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil {
			continue
		}
		if payload.Source == Source {
			return true
		}
	}
	return false
}
