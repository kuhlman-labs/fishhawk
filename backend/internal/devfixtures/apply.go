package devfixtures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures/catalog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file owns MATERIALIZATION (E72.2 / #3326): Apply turns a validated
// Scenario into committed rows by driving the SAME repositories the
// product writes through — run.Repository for runs and stages (so every
// stage state is reached by a real TransitionStage walk and the
// transition tables, not a raw INSERT, decide what is legal),
// artifact.Repository, approval.Repository and audit.Repository (so the
// per-run hash chain links backdated rows exactly as it links live
// ones). Nothing here bypasses an invariant the product enforces.
//
// The four store interfaces below are deliberately NARROW: each names
// only the method Apply calls, so a fake-driven test can arm every
// error branch without a database (apply_test.go) while the real
// repositories satisfy them unchanged (apply_pg_test.go).

// RunStore is the run/stage slice of run.Repository Apply needs.
type RunStore interface {
	CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error)
	TransitionRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error)
	CreateStage(ctx context.Context, p run.CreateStageParams) (*run.Stage, error)
	TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, completion *run.StageCompletion) (*run.Stage, error)
}

// ArtifactStore is the slice of artifact.Repository Apply needs.
type ArtifactStore interface {
	Create(ctx context.Context, p artifact.CreateParams) (*artifact.Artifact, error)
}

// AuditStore is the slice of audit.Repository Apply needs.
type AuditStore interface {
	AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error)
}

// ApprovalStore is the slice of approval.Repository Apply needs.
type ApprovalStore interface {
	Submit(ctx context.Context, p approval.SubmitParams) (*approval.SubmitResult, error)
}

// Deps carries the stores Apply writes through and the clock it reads.
// Now is the ONE clock every audit row is dated from: a row's Timestamp
// is Now() − age, so a test that injects a pinned clock can assert the
// exact backdate. Nil Now means time.Now.
type Deps struct {
	Runs      RunStore
	Artifacts ArtifactStore
	Audit     AuditStore
	Approvals ApprovalStore
	Now       func() time.Time
}

// validate fails closed on a missing store so a half-wired Deps is
// refused before the first row is written, naming the field.
func (d Deps) validate() error {
	switch {
	case d.Runs == nil:
		return errors.New("devfixtures: Deps.Runs is nil")
	case d.Artifacts == nil:
		return errors.New("devfixtures: Deps.Artifacts is nil")
	case d.Audit == nil:
		return errors.New("devfixtures: Deps.Audit is nil")
	case d.Approvals == nil:
		return errors.New("devfixtures: Deps.Approvals is nil")
	}
	return nil
}

func (d Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// Result maps every scenario handle to the fresh id Apply minted for it.
type Result struct {
	Scenario string               `json:"scenario"`
	Runs     map[string]RunResult `json:"runs"`
}

// RunResult is one run's minted id plus its stages' ids keyed by handle.
type RunResult struct {
	ID     uuid.UUID            `json:"run_id"`
	Stages map[string]uuid.UUID `json:"stages"`
}

// stageWalk is the canonical path Apply walks from the freshly-created
// pending stage to each declared state. Validate has already refused
// every state absent from this table, so the walk cannot dead-end.
var stageWalk = map[run.StageState][]run.StageState{
	run.StageStatePending:          {},
	run.StageStateDispatched:       {run.StageStateDispatched},
	run.StageStateRunning:          {run.StageStateDispatched, run.StageStateRunning},
	run.StageStateSucceeded:        {run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded},
	run.StageStateAwaitingApproval: {run.StageStateDispatched, run.StageStateRunning, run.StageStateAwaitingApproval},
	run.StageStateFailed:           {run.StageStateDispatched, run.StageStateRunning, run.StageStateFailed},
}

// runWalk is the canonical path from the freshly-created pending run to
// each declared run state. pending → cancelled and pending → failed are
// direct edges in run.ValidRunTransition, but a seeded run that ended
// is more useful having RUN first (its stages did), so every terminal
// state is reached through running.
var runWalk = map[run.State][]run.State{
	run.StatePending:   {},
	run.StateRunning:   {run.StateRunning},
	run.StateSucceeded: {run.StateRunning, run.StateSucceeded},
	run.StateFailed:    {run.StateRunning, run.StateFailed},
	run.StateCancelled: {run.StateRunning, run.StateCancelled},
}

// Apply materializes s through deps and returns the minted ids. Rows are
// created in dependency order — runs, then stages in sequence order,
// then artifacts, approvals and audit rows — so every foreign key
// resolves. Every call mints FRESH rows: nothing is looked up or
// deduplicated, which is what lets a preview be re-seeded and an
// acceptance criterion reference "the run this apply returned".
//
// Apply re-runs Validate first, so a hand-built Scenario that skipped
// Load is refused before any write. There is no rollback: a store error
// leaves the rows written so far in place and the returned error names
// the handle that failed, which is the right posture for a dev-only
// fixture surface (the preview database is disposable).
func Apply(ctx context.Context, deps Deps, s *Scenario) (Result, error) {
	if err := deps.validate(); err != nil {
		return Result{}, err
	}
	if s == nil {
		return Result{}, errors.New("devfixtures: nil scenario")
	}
	if err := s.Validate(); err != nil {
		return Result{}, fmt.Errorf("devfixtures: scenario %q: %w", s.Name, err)
	}
	res := Result{Scenario: s.Name, Runs: make(map[string]RunResult, len(s.Runs))}
	stageIDs := make(map[string]uuid.UUID, len(s.Stages))

	for _, r := range s.Runs {
		id, err := applyRun(ctx, deps.Runs, r)
		if err != nil {
			return Result{}, err
		}
		res.Runs[r.Key] = RunResult{ID: id, Stages: map[string]uuid.UUID{}}
	}

	// Sequence order within a run is what a reader of GET
	// /v0/runs/{id}/stages sees; a stable sort keeps declaration order
	// for equal sequences and never reorders across runs' interleaving.
	stages := append([]Stage(nil), s.Stages...)
	sort.SliceStable(stages, func(i, j int) bool { return stages[i].Sequence < stages[j].Sequence })
	for _, st := range stages {
		id, err := applyStage(ctx, deps.Runs, res.Runs[st.Run].ID, st)
		if err != nil {
			return Result{}, err
		}
		stageIDs[st.Key] = id
		res.Runs[st.Run].Stages[st.Key] = id
	}

	for _, a := range s.Artifacts {
		content := []byte(a.Content)
		sum := sha256.Sum256(content)
		var schema *string
		if a.SchemaVersion != "" {
			schema = &a.SchemaVersion
		}
		if _, err := deps.Artifacts.Create(ctx, artifact.CreateParams{
			StageID:       stageIDs[a.Stage],
			Kind:          artifact.Kind(a.Kind),
			SchemaVersion: schema,
			Content:       json.RawMessage(content),
			ContentHash:   hex.EncodeToString(sum[:]),
		}); err != nil {
			return Result{}, fmt.Errorf("devfixtures: artifact %q: %w", a.Key, err)
		}
	}

	for i, ap := range s.Approvals {
		var comment *string
		if ap.Comment != "" {
			comment = &ap.Comment
		}
		if _, err := deps.Approvals.Submit(ctx, approval.SubmitParams{
			StageID:         stageIDs[ap.Stage],
			ApproverSubject: ap.ApproverSubject,
			Decision:        approval.Decision(ap.Decision),
			Comment:         comment,
			Surface:         approval.Surface(ap.Surface),
		}); err != nil {
			return Result{}, fmt.Errorf("devfixtures: approvals[%d] on stage %q: %w", i, ap.Stage, err)
		}
	}

	now := deps.now()
	for i, row := range s.Audit {
		age, _ := row.AgeDuration() // Validate already refused an unparseable age.
		var stageID *uuid.UUID
		if row.Stage != "" {
			id := stageIDs[row.Stage]
			stageID = &id
		}
		kind := audit.ActorKind(row.ActorKind)
		var subject *string
		if row.ActorSubject != "" {
			subject = &row.ActorSubject
		}
		if _, err := deps.Audit.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        res.Runs[row.Run].ID,
			StageID:      stageID,
			Timestamp:    now.Add(-age),
			Category:     row.Category,
			ActorKind:    &kind,
			ActorSubject: subject,
			Payload:      json.RawMessage(row.Payload),
		}); err != nil {
			return Result{}, fmt.Errorf("devfixtures: audit[%d] (%s) on run %q: %w", i, row.Category, row.Run, err)
		}
	}
	return res, nil
}

// applyRun creates one untenanted run pinned to the scenario's inline
// workflow spec and walks it to its declared state.
func applyRun(ctx context.Context, runs RunStore, r Run) (uuid.UUID, error) {
	specBytes := []byte(r.WorkflowSpec)
	specSum := sha256.Sum256(specBytes)
	triggerRef := r.TriggerRef
	created, err := runs.CreateRun(ctx, run.CreateRunParams{
		Repo:          r.Repo,
		WorkflowID:    r.WorkflowID,
		WorkflowSHA:   hex.EncodeToString(specSum[:]),
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &triggerRef,
		WorkflowSpec:  specBytes,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("devfixtures: run %q: create: %w", r.Key, err)
	}
	for _, to := range runWalk[run.State(r.State)] {
		if _, err := runs.TransitionRun(ctx, created.ID, to); err != nil {
			return uuid.Nil, fmt.Errorf("devfixtures: run %q: transition to %s: %w", r.Key, to, err)
		}
	}
	return created.ID, nil
}

// applyStage creates one stage on runID and walks TransitionStage along
// the canonical path to its declared state; a failed stage carries its
// declared category and reason as the StageCompletion.
func applyStage(ctx context.Context, runs RunStore, runID uuid.UUID, st Stage) (uuid.UUID, error) {
	created, err := runs.CreateStage(ctx, run.CreateStageParams{
		RunID:            runID,
		Sequence:         st.Sequence,
		Type:             run.StageType(st.Type),
		ExecutorKind:     run.ExecutorKind(st.ExecutorKind),
		ExecutorRef:      st.ExecutorRef,
		RequiresApproval: st.RequiresApproval,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("devfixtures: stage %q: create: %w", st.Key, err)
	}
	for _, to := range stageWalk[run.StageState(st.State)] {
		var completion *run.StageCompletion
		if to == run.StageStateFailed {
			category := run.FailureCategory(st.FailureCategory)
			reason := st.FailureReason
			if reason == "" {
				reason = "seeded fixture failure"
			}
			completion = &run.StageCompletion{FailureCategory: &category, FailureReason: &reason}
		}
		if _, err := runs.TransitionStage(ctx, created.ID, to, completion); err != nil {
			return uuid.Nil, fmt.Errorf("devfixtures: stage %q: transition to %s: %w", st.Key, to, err)
		}
	}
	return created.ID, nil
}

// Applier binds Deps to the catalog so a caller holding only a scenario
// NAME can list, describe and apply. It is the shape the dev-only
// fixtures route consumes (server.DevFixtureApplier, a sibling slice).
type Applier struct {
	deps Deps
}

// NewApplier returns an Applier over deps.
func NewApplier(deps Deps) *Applier { return &Applier{deps: deps} }

// Names returns the catalog's sorted scenario names.
func (*Applier) Names() []string { return catalog.Names() }

// Describe returns the catalog's one-line description of name ("" when
// unknown).
func (*Applier) Describe(name string) string { return catalog.Description(name) }

// Apply loads name from the catalog and materializes it. An unknown name
// wraps ErrUnknownScenario so the route can answer 404 with the known set.
func (a *Applier) Apply(ctx context.Context, name string) (Result, error) {
	s, err := Load(name)
	if err != nil {
		return Result{}, err
	}
	return Apply(ctx, a.deps, s)
}
