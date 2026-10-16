// Package scopeamendment persists the mid-stage operator-gated scope
// amendment requests an implement agent files while its stage is
// running (E22.X / #961). The agent POSTs the paths it needs folded
// into the effective scope.files plus a reason; the operator approves
// or denies in-loop; both the request and the decision land as audit
// entries (scope_amendment_requested / scope_amendment_decided) on the
// owning stage. Approved rows are folded into the prompt-fetch scope
// AND the runner's pre-commit scope refresh, so the #960 verified-tree
// gates verify the same folded tree that is pushed.
//
// A request still pending when the run can no longer use a decision LAPSES
// (E83.64 / #4127): its stage reached succeeded, cancelled or superseded, or
// its run succeeded or was cancelled. fishhawkd's run-repository terminal
// observers (InstallLapseHooks) and a one-shot startup backfill
// (BackfillStranded) write it, each lapse with one chained
// scope_amendment_lapsed audit entry. A FAILED stage or a FAILED run keeps
// its requests pending (fishhawk_retry_stage reopens a failed run), so a late
// decision plus fishhawk_retry_stage still folds.
//
// Mirrors mcptoken's layout: domain types + Repository here,
// queries.sql + sqlc-generated ./db, postgres.go implementing the
// Repository against pgx.
package scopeamendment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Status is the lifecycle of one amendment request. The agent's poll
// loop terminates when status leaves pending.
type Status string

// Statuses. Pending is the creation state; Decide moves a row to
// approved or denied exactly once; a row still pending when its stage
// reaches succeeded/cancelled/superseded or its run reaches
// succeeded/cancelled moves to lapsed (#4127). All three are terminal.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
	StatusLapsed   Status = "lapsed"
)

// CategoryLapsed is the audit category of the one chained entry written per
// lapsed row (#4127). Internal: not an issue-comment activity line.
const CategoryLapsed = "scope_amendment_lapsed"

// Lapse sources, recorded in the scope_amendment_lapsed payload.
const (
	// LapseSourceStageTerminal: the stage-terminal observer.
	LapseSourceStageTerminal = "stage_terminal"
	// LapseSourceRunTerminal: the run-terminal observer.
	LapseSourceRunTerminal = "run_terminal"
	// LapseSourceStartupBackfill: BackfillStranded at fishhawkd start.
	LapseSourceStartupBackfill = "startup_backfill"
)

// Operation is the per-path operation an amendment declares. Mirrors
// the standard_v1 scope.files operation vocabulary, restricted to the
// two an amendment can widen: modify (an existing file the plan didn't
// declare) and create (a net-new file the #818/#825 gates would
// otherwise fail loud on).
const (
	OperationModify = "modify"
	OperationCreate = "create"
)

// Errors callers switch on. Mirror mcptoken's shape so handlers map
// them to HTTP statuses uniformly.
var (
	// ErrNotFound means no amendment row matches the lookup.
	ErrNotFound = errors.New("scopeamendment: not found")

	// ErrAlreadyDecided means the amendment has already been
	// approved or denied; the decision endpoint maps it to 409.
	ErrAlreadyDecided = errors.New("scopeamendment: already decided")

	// ErrLapsed means the amendment lapsed undecided (#4127) and can
	// never be approved or denied; the decision endpoint maps it to 409
	// amendment_lapsed. Distinct from ErrAlreadyDecided and never wraps it.
	ErrLapsed = errors.New("scopeamendment: lapsed undecided")
)

// PathEntry is one requested path + its operation.
type PathEntry struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// Amendment is the public-facing record of one request.
type Amendment struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	StageID        uuid.UUID
	Paths          []PathEntry
	Reason         string
	Status         Status
	DecisionReason *string
	DecidedBy      *string
	RequestedAt    time.Time
	DecidedAt      *time.Time
}

// CreateParams bundles the inputs to Create.
type CreateParams struct {
	RunID   uuid.UUID
	StageID uuid.UUID
	Paths   []PathEntry
	Reason  string
}

// DecideParams bundles the inputs to Decide.
type DecideParams struct {
	ID        uuid.UUID
	Status    Status // StatusApproved or StatusDenied
	Reason    string
	DecidedBy string
}

// Repository persists amendments.
type Repository interface {
	// Create persists a fresh pending amendment.
	Create(ctx context.Context, p CreateParams) (*Amendment, error)

	// GetByID returns the amendment regardless of status.
	GetByID(ctx context.Context, id uuid.UUID) (*Amendment, error)

	// ListByRun returns every amendment for the run, oldest first.
	ListByRun(ctx context.Context, runID uuid.UUID) ([]*Amendment, error)

	// CountByStage returns the number of amendment rows for the
	// stage, regardless of status — a denied request still consumes
	// per-stage budget (the cap bounds operator interruptions, not
	// approvals).
	CountByStage(ctx context.Context, stageID uuid.UUID) (int, error)

	// Decide transitions a pending amendment to approved/denied.
	// ErrLapsed when the row lapsed undecided (#4127);
	// ErrAlreadyDecided when it was already approved/denied;
	// ErrNotFound when no row matches.
	Decide(ctx context.Context, p DecideParams) (*Amendment, error)
}

// LapseParams bundles the inputs to Lapser.LapseForStage / LapseForRun.
// StageID selects the stage for LapseForStage, RunID the run for
// LapseForRun. Trigger names the transition that lapsed the rows (e.g.
// "stage succeeded", "run cancelled"); it is folded into decision_reason.
type LapseParams struct {
	StageID uuid.UUID
	RunID   uuid.UUID
	Trigger string
	Source  string
}

// Lapser is the lapse capability of the postgres repository (#4127). It is
// deliberately NOT on Repository, so the handler test fakes need no stub.
// Each call flips the selected pending rows to lapsed AND appends one chained
// scope_amendment_lapsed entry per row in ONE transaction: an error rolls
// both back. Pending-only, so a repeat call lapses nothing (idempotent).
type Lapser interface {
	LapseForStage(ctx context.Context, p LapseParams) ([]*Amendment, error)
	LapseForRun(ctx context.Context, p LapseParams) ([]*Amendment, error)
	// LapseStranded lapses every pending row whose stage state or run
	// state already lapses it (StageStateLapses / RunStateLapses), one
	// transaction per stage, and returns the number of rows lapsed.
	LapseStranded(ctx context.Context) (int, error)
}

// PendingWithStage is one pending amendment joined to its stage's state, as
// the ops monitor's amendment scan reads it (E85.2 / #4146).
type PendingWithStage struct {
	ID          uuid.UUID
	RunID       uuid.UUID
	StageID     uuid.UUID
	Status      Status
	RequestedAt time.Time
	// StageState is nil when the stage is not visible to this session
	// (stages is row-level secured through its run, migration 0057).
	StageState *string
	// ObservedAt is the database's now() at read time, so
	// ObservedAt.Sub(RequestedAt) is an age measured in ONE clock domain
	// (#3048).
	ObservedAt time.Time
}

// PendingLister is the read-only pending-amendment scan of the postgres
// repository (E85.2 / #4146). It is deliberately NOT on Repository, so the
// handler test fakes need no stub. ListPendingWithStage returns the pending
// rows requested within lookback, newest first, at most limit of them; a
// non-positive lookback or limit is an error.
type PendingLister interface {
	ListPendingWithStage(ctx context.Context, lookback time.Duration, limit int) ([]PendingWithStage, error)
}

// StageStateLapses reports whether a stage reaching state lapses its pending
// amendments: succeeded, cancelled and superseded do. failed deliberately
// does NOT: a failed stage can be re-run with fishhawk_retry_stage (which
// reopens a failed run), and a late approval folds on that retry.
func StageStateLapses(state string) bool {
	switch run.StageState(state) {
	case run.StageStateSucceeded, run.StageStateCancelled, run.StageStateSuperseded:
		return true
	default:
		return false
	}
}

// RunStateLapses reports whether a run reaching state lapses every pending
// amendment on every one of its stages: succeeded and cancelled do. failed
// deliberately does NOT: fishhawk_retry_stage reopens a failed run
// (server/retry.go, failed → running), so a late approval still folds on that
// retry.
func RunStateLapses(state string) bool {
	switch run.State(state) {
	case run.StateSucceeded, run.StateCancelled:
		return true
	default:
		return false
	}
}

// lapseTimeout bounds one observer-driven lapse transaction.
const lapseTimeout = 10 * time.Second

// LapseOnStageTerminal is the stage-terminal observer body: when stageState
// lapses (StageStateLapses) it lapses the stage's pending rows. It detaches
// from the caller's cancellation (the request that drove the transition may
// end right after the commit), never propagates an error (WARN, the startup
// backfill repairs it), and logs INFO when it lapsed rows.
func LapseOnStageTerminal(ctx context.Context, l Lapser, logger *slog.Logger, stageID uuid.UUID, stageState string) {
	if !StageStateLapses(stageState) {
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lapseTimeout)
	defer cancel()
	lapsed, err := l.LapseForStage(lctx, LapseParams{StageID: stageID, Trigger: "stage " + stageState, Source: LapseSourceStageTerminal})
	logLapse(ctx, logger, "stage", stageID, stageState, len(lapsed), err)
}

// LapseOnRunTerminal is the run-terminal observer body: when runState lapses
// (RunStateLapses) it lapses every pending row on every stage of the run.
// Same detach / WARN / INFO contract as LapseOnStageTerminal.
func LapseOnRunTerminal(ctx context.Context, l Lapser, logger *slog.Logger, runID uuid.UUID, runState string) {
	if !RunStateLapses(runState) {
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lapseTimeout)
	defer cancel()
	lapsed, err := l.LapseForRun(lctx, LapseParams{RunID: runID, Trigger: "run " + runState, Source: LapseSourceRunTerminal})
	logLapse(ctx, logger, "run", runID, runState, len(lapsed), err)
}

func logLapse(ctx context.Context, logger *slog.Logger, kind string, id uuid.UUID, state string, n int, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"scope-amendment lapse failed; the startup backfill will repair it",
			slog.String(kind+"_id", id.String()),
			slog.String(kind+"_state", state),
			slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		logger.LogAttrs(ctx, slog.LevelInfo, "scope amendments lapsed undecided",
			slog.String(kind+"_id", id.String()),
			slog.String(kind+"_state", state),
			slog.Int("lapsed", n))
	}
}

// InstallLapseHooks wires the lapse observers onto the run repository: the
// stage-terminal observer runs LapseOnStageTerminal and the run-terminal
// observer runs LapseOnRunTerminal. fishhawkd calls it once at startup, and
// the cross-boundary test calls the same function. A missing capability is
// logged at WARN (a silently broken wiring would mean no amendment ever
// lapses until the next restart's backfill). Returns the Lapser, or nil when
// amendRepo lacks it.
func InstallLapseHooks(runRepo, amendRepo any, logger *slog.Logger) Lapser {
	if logger == nil {
		logger = slog.Default()
	}
	lapser, ok := amendRepo.(Lapser)
	if !ok {
		logger.Warn("scope-amendment lapse hooks NOT wired: amendment repository lacks the Lapser capability",
			slog.String("type", fmt.Sprintf("%T", amendRepo)))
		return nil
	}
	if obs, ok := runRepo.(run.StageTerminalObservable); ok {
		obs.SetStageTerminalObserver(func(ctx context.Context, s *run.Stage) {
			LapseOnStageTerminal(ctx, lapser, logger, s.ID, string(s.State))
		})
	} else {
		logger.Warn("scope-amendment stage-terminal lapse hook NOT wired: run repository lacks StageTerminalObservable",
			slog.String("type", fmt.Sprintf("%T", runRepo)))
	}
	if obs, ok := runRepo.(run.TerminalRunObservable); ok {
		obs.SetRunTerminalObserver(func(ctx context.Context, r *run.Run) {
			LapseOnRunTerminal(ctx, lapser, logger, r.ID, string(r.State))
		})
	} else {
		logger.Warn("scope-amendment run-terminal lapse hook NOT wired: run repository lacks TerminalRunObservable",
			slog.String("type", fmt.Sprintf("%T", runRepo)))
	}
	return lapser
}

// BackfillStranded is the one-shot startup pass (#4127): it lapses every
// pending row whose stage or run already lapses it — the pre-#4127 rows and
// any a crash between a terminal commit and its observer left behind. It
// never blocks start: an error is a WARN. A nil lapser is a no-op.
func BackfillStranded(ctx context.Context, l Lapser, logger *slog.Logger) {
	if l == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	n, err := l.LapseStranded(ctx)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "startup stranded scope-amendment lapse failed",
			slog.Int("lapsed", n), slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		logger.LogAttrs(ctx, slog.LevelInfo, "startup stranded scope-amendment lapse completed", slog.Int("lapsed", n))
	}
}

// ValidatePaths normalizes and validates requested path entries. Same
// containment contract as the fix-up allow_create validation
// (server/fixup.go validateAllowCreate, #823): each path is trimmed;
// empty/whitespace-only, absolute, and ".."-containing entries are
// rejected so the amendment stays repo-relative and cannot widen scope
// outside the tree. Operation must be modify or create. Returns the
// normalized entries, or an error describing the first bad entry.
func ValidatePaths(paths []PathEntry) ([]PathEntry, error) {
	if len(paths) == 0 {
		return nil, errors.New("paths must name at least one file")
	}
	out := make([]PathEntry, 0, len(paths))
	for _, e := range paths {
		trimmed := strings.TrimSpace(e.Path)
		if trimmed == "" {
			return nil, errors.New("paths entries must be non-empty repo-relative paths")
		}
		if strings.HasPrefix(trimmed, "/") {
			return nil, fmt.Errorf("path %q must be repo-relative, not absolute", trimmed)
		}
		if strings.Contains(trimmed, "..") {
			return nil, fmt.Errorf("path %q must not contain '..'", trimmed)
		}
		if e.Operation != OperationModify && e.Operation != OperationCreate {
			return nil, fmt.Errorf("path %q operation must be %q or %q, got %q",
				trimmed, OperationModify, OperationCreate, e.Operation)
		}
		out = append(out, PathEntry{Path: trimmed, Operation: e.Operation})
	}
	return out, nil
}
