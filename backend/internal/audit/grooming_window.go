package audit

// grooming_window.go carries the grooming capture/apply CONCURRENCY PROTOCOL
// (E54.48 / #2991): the audit-layer capability that makes it safe for the
// on-approval apply hook to CONSUME the per-entry dispositions #2843 captures.
//
// THE PROTOCOL, and the three properties it delivers SIMULTANEOUSLY:
//
//   - ONE CAPTURE IS ONE TRANSACTION. AppendChainedGroomingDispositionBatch
//     appends a whole capture batch under the run-row lock after checking for an
//     artifact-bound closing WATERMARK. A watermark cannot land between two rows
//     of one capture, and a mid-batch failure rolls the WHOLE batch back rather
//     than leaving durable partial rows.
//   - THE FIRST WATERMARK IS PERMANENT. AppendChainedGroomingWindowClose scans
//     for an existing artifact-bound watermark and, if one exists, returns it
//     UNCHANGED — a repeated settlement can never extend the consumption bound,
//     so rows outside the first window stay outside it forever.
//   - REJECTION SETTLES AS DECISIVELY AS APPROVAL. The settlement side reads the
//     dispositions and appends the watermark in ONE transaction whatever the
//     settlement string, so the capture/apply TOCTOU is closed on both paths.
//
// Both cores take LockRunForUpdate(RunID) FIRST, then read under the lock, then
// write — all in ONE transaction at the server-default READ COMMITTED isolation
// (TxOptions deliberately unset: a REPEATABLE READ snapshot would predate the
// lock and could observe stale pre-append state, the residual anchored.go
// documents). This is the SAME mechanism AppendChainedTx / AppendChainedAnchoredTx
// / AppendChainedUnderBudgetTx already depend on.
//
// CONDITION 1 (the consumed set is artifact-scoped): the settlement's consumed
// set is always {dispositions recorded against THIS artifact, below THIS
// artifact's watermark}. A run can carry MULTIPLE grooming-report artifacts, so
// a stale disposition captured against a DIFFERENT artifact must not enter the
// consumed set and match an entry id — the same wrong-decision-applied failure
// the whole protocol exists to prevent. Both the first-settlement and the
// permanence paths apply the artifact + below-watermark filter.
//
// THE PROTOCOL IS FAMILY-PARAMETERIZED (#3923). The two Tx cores and the scan
// helpers take a windowFamily — {disposition category, watermark category,
// closed-error constructor, optional binding re-check} — so a second
// disposition family reuses the SAME locking and permanence code instead of a
// copy. Two families exist:
//
//   - grooming (#2991): grooming_disposition_recorded under the
//     grooming_apply_window_closed watermark. Its exported entry points and
//     GroomingWindowClosedError are thin wrappers, byte-identical in behavior
//     and error text to the pre-generalization code.
//   - upkeep (#3923): upkeep_disposition_recorded under the
//     upkeep_apply_window_closed watermark (writer: the #3924 apply). The
//     upkeep batch carries ONE extra in-transaction check the grooming batch
//     does not: the BINDING RE-CHECK. Dispositions bind to the artifact named
//     by the HIGHEST-sequence upkeep_report_recorded row; the server resolves
//     that row before the append, so a report recorded between resolution and
//     append would otherwise land the capture against a superseded report.
//     Under the run-row lock the batch re-reads the newest recorded row and
//     refuses with *UpkeepReportSupersededError (writing NOTHING) when it no
//     longer names the capture's artifact.
//
// Family isolation: every scan filters by the family's OWN categories, so a
// grooming watermark never closes an upkeep window (or vice versa) even when
// the two carry the same artifact_id string.
//
// The capability is kept OFF the Repository interface (the anchored.go /
// RetryBudgetAppender precedent): adding a Repository method would break the ~20
// manually-written full-interface fakes. The server type-asserts it and drives
// the atomic path when present (production postgresRepo), falling back to a
// non-atomic read-then-append for in-memory fakes. The compile-time assertion in
// postgres.go keeps a production repo that silently loses the capability a build
// failure, not a runtime degrade.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditdb "github.com/kuhlman-labs/fishhawk/backend/internal/audit/db"
	rundb "github.com/kuhlman-labs/fishhawk/backend/internal/run/db"
)

// GroomingApplyWindowClosedCategory is the audit category for the WATERMARK the
// capture/apply concurrency protocol appends at settlement (#2991). It closes
// the disposition-capture window for ONE grooming-report artifact: after it
// lands, a capture arriving for that artifact is refused, and the settlement
// consumes exactly the dispositions recorded below it against that artifact. It
// is registered in KnownCategories (categories.go) so an operator can await it
// and GET /v0/runs/{id}/audit?category= can read it.
const GroomingApplyWindowClosedCategory = "grooming_apply_window_closed"

// GroomingDispositionRecordedCategory is the audit category one per-entry
// disposition an operator records against a grooming-report entry lands under
// (#2843). It is DEFINED here — beside the protocol that consumes it — so the
// audit-layer settlement scan and the server capture handler share ONE source
// of truth (server.CategoryGroomingDispositionRecorded aliases it). Registered
// in KnownCategories.
const GroomingDispositionRecordedCategory = "grooming_disposition_recorded"

// UpkeepApplyWindowClosedCategory is the upkeep family's WATERMARK (#3923): the
// #3924 apply appends one per upkeep-report artifact it settles — on approve
// AND reject — through AppendChainedUpkeepWindowClose. After it lands a capture
// for that artifact is refused, and the apply consumes exactly the
// dispositions recorded below it against that artifact. Registered in
// KnownCategories.
const UpkeepApplyWindowClosedCategory = "upkeep_apply_window_closed"

// UpkeepDispositionRecordedCategory is the audit category one captain
// disposition against an upkeep-report finding lands under (#3923; registered
// by #3921). Defined beside the protocol that consumes it, as
// GroomingDispositionRecordedCategory is.
const UpkeepDispositionRecordedCategory = "upkeep_disposition_recorded"

// UpkeepReportRecordedCategory is the row the server appends once per ingested
// upkeep_report artifact (#3921; server.CategoryUpkeepReportRecorded carries the
// same value). The upkeep batch's binding re-check reads it: the
// HIGHEST-sequence row names the report a capture must still be bound to.
const UpkeepReportRecordedCategory = "upkeep_report_recorded"

// GroomingWindowClosedError is returned when a capture arrives for an artifact
// whose window has already been settled — nothing is written. Settlement is the
// settlement string of the closing watermark (approved / rejected), Sequence its
// chain sequence, ClosedAt its timestamp, so the handler's 409 names the facts.
type GroomingWindowClosedError struct {
	ArtifactID string
	Settlement string
	Sequence   int64
	ClosedAt   time.Time
}

func (e *GroomingWindowClosedError) Error() string {
	return fmt.Sprintf("audit: grooming capture window for artifact %s is closed (settlement=%s, watermark sequence %d)",
		e.ArtifactID, e.Settlement, e.Sequence)
}

// GroomingWindowAppender is the OPTIONAL capability the concrete audit
// Repository carries (#2991). It is deliberately kept OFF audit.Repository, the
// AnchoredChainAppender / RetryBudgetAppender precedent.
type GroomingWindowAppender interface {
	// AppendChainedGroomingDispositionBatch appends a whole capture batch under
	// the run-row lock in ONE transaction, after checking for an artifact-bound
	// closing watermark. It returns *GroomingWindowClosedError (writing NOTHING)
	// when the artifact's window is already closed; otherwise it returns the
	// appended entries. artifactID is the closing artifact the capture attaches
	// to; every param must carry the same RunID.
	AppendChainedGroomingDispositionBatch(ctx context.Context, artifactID string, ps []ChainAppendParams) ([]*Entry, error)
	// AppendChainedGroomingWindowClose settles the capture window for artifactID
	// in ONE transaction under the run-row lock, returning the watermark entry
	// and the consumed dispositions ({this artifact, below the watermark}). When
	// a watermark for artifactID already exists it returns that EXISTING entry
	// unchanged (permanence), appending nothing.
	AppendChainedGroomingWindowClose(ctx context.Context, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error)
}

// UpkeepWindowClosedError is returned when an upkeep capture arrives for an
// artifact whose window the #3924 apply has already settled — nothing is
// written. Same fields as GroomingWindowClosedError, so the handler's 409
// names the watermark's facts.
type UpkeepWindowClosedError struct {
	ArtifactID string
	Settlement string
	Sequence   int64
	ClosedAt   time.Time
}

func (e *UpkeepWindowClosedError) Error() string {
	return fmt.Sprintf("audit: upkeep capture window for artifact %s is closed (settlement=%s, watermark sequence %d)",
		e.ArtifactID, e.Settlement, e.Sequence)
}

// UpkeepReportSupersededError is returned by the upkeep batch when, under the
// run-row lock, the HIGHEST-sequence upkeep_report_recorded row no longer
// names ArtifactID: a newer report was recorded after the server resolved the
// capture's binding. Nothing is written. CurrentArtifactID is the artifact the
// newest row names ("" when that row is absent or undecodable — still a
// refusal, the fail-closed direction) and CurrentSequence its chain sequence
// (0 when absent), so the captain can re-capture against the current report.
type UpkeepReportSupersededError struct {
	ArtifactID        string
	CurrentArtifactID string
	CurrentSequence   int64
}

func (e *UpkeepReportSupersededError) Error() string {
	return fmt.Sprintf("audit: upkeep report %s is superseded by %q (upkeep_report_recorded sequence %d); re-capture against the current report",
		e.ArtifactID, e.CurrentArtifactID, e.CurrentSequence)
}

// UpkeepWindowAppender is the upkeep family's OPTIONAL capability (#3923), the
// GroomingWindowAppender shape over the upkeep categories. Kept OFF
// audit.Repository for the same reason; postgresRepo carries it (compile-time
// assertion in postgres.go) and decisionindex.IndexingRepository forwards it.
type UpkeepWindowAppender interface {
	// AppendChainedUpkeepDispositionBatch appends a whole capture batch under
	// the run-row lock in ONE transaction. It writes NOTHING and returns
	// *UpkeepReportSupersededError when the newest upkeep_report_recorded row
	// no longer names artifactID, or *UpkeepWindowClosedError when artifactID's
	// window is already closed.
	AppendChainedUpkeepDispositionBatch(ctx context.Context, artifactID string, ps []ChainAppendParams) ([]*Entry, error)
	// AppendChainedUpkeepWindowClose settles artifactID's upkeep window in ONE
	// transaction under the run-row lock, returning the watermark and the
	// consumed dispositions ({this artifact, below the watermark}). An existing
	// watermark is returned UNCHANGED (permanence), appending nothing.
	AppendChainedUpkeepWindowClose(ctx context.Context, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error)
}

// windowFamily parameterizes the capture/apply protocol over one disposition
// family. name only shapes wrapped error text ("grooming" keeps the
// pre-generalization strings byte-identical).
type windowFamily struct {
	name                string
	dispositionCategory string
	watermarkCategory   string
	// closed builds the family's typed refusal from the permanent watermark.
	closed func(artifactID, settlement string, seq int64, closedAt time.Time) error
	// reportCategory, when non-empty, enables the batch's BINDING RE-CHECK: the
	// highest-sequence row of this category must name the capture's artifact.
	reportCategory string
}

var groomingFamily = windowFamily{
	name:                "grooming",
	dispositionCategory: GroomingDispositionRecordedCategory,
	watermarkCategory:   GroomingApplyWindowClosedCategory,
	closed: func(artifactID, settlement string, seq int64, closedAt time.Time) error {
		return &GroomingWindowClosedError{ArtifactID: artifactID, Settlement: settlement, Sequence: seq, ClosedAt: closedAt}
	},
}

var upkeepFamily = windowFamily{
	name:                "upkeep",
	dispositionCategory: UpkeepDispositionRecordedCategory,
	watermarkCategory:   UpkeepApplyWindowClosedCategory,
	closed: func(artifactID, settlement string, seq int64, closedAt time.Time) error {
		return &UpkeepWindowClosedError{ArtifactID: artifactID, Settlement: settlement, Sequence: seq, ClosedAt: closedAt}
	},
	reportCategory: UpkeepReportRecordedCategory,
}

// AppendChainedGroomingDispositionBatchTx is the transaction-aware core of the
// grooming batch capture: windowDispositionBatchTx over the grooming family.
// Ordering is LOAD-BEARING and mirrors AppendChainedAnchoredTx:
//
//  1. LockRunForUpdate(RunID) FIRST, held for the whole transaction.
//  2. Scan the run's grooming_apply_window_closed entries for one bound to
//     artifactID; if found, return *GroomingWindowClosedError writing NOTHING.
//  3. Delegate EACH param to AppendChainedTx in sequence (its re-entrant
//     LockRunForUpdate inside the same tx is a harmless no-op). A mid-batch
//     failure returns the error, so the caller's BeginFunc rolls the WHOLE batch
//     back — one capture is one transaction.
//
// The caller owns the transaction lifecycle and MUST run it at READ COMMITTED
// (do NOT set TxOptions), for the reason the file header states.
func AppendChainedGroomingDispositionBatchTx(ctx context.Context, tx pgx.Tx, artifactID string, ps []ChainAppendParams) ([]*Entry, error) {
	return windowDispositionBatchTx(ctx, tx, groomingFamily, artifactID, ps)
}

// AppendChainedGroomingWindowCloseTx is the transaction-aware core of the
// grooming settlement: windowCloseTx over the grooming family.
func AppendChainedGroomingWindowCloseTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error) {
	return windowCloseTx(ctx, tx, groomingFamily, p, artifactID)
}

// AppendChainedUpkeepDispositionBatchTx is the transaction-aware core of the
// upkeep batch capture (#3923): the grooming ordering plus the BINDING
// RE-CHECK between the lock and the watermark scan. Same READ COMMITTED rule.
func AppendChainedUpkeepDispositionBatchTx(ctx context.Context, tx pgx.Tx, artifactID string, ps []ChainAppendParams) ([]*Entry, error) {
	return windowDispositionBatchTx(ctx, tx, upkeepFamily, artifactID, ps)
}

// AppendChainedUpkeepWindowCloseTx is the transaction-aware core of the upkeep
// settlement (#3923; caller: the #3924 apply).
func AppendChainedUpkeepWindowCloseTx(ctx context.Context, tx pgx.Tx, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error) {
	return windowCloseTx(ctx, tx, upkeepFamily, p, artifactID)
}

// windowDispositionBatchTx is the family-generic batch core:
//
//  1. LockRunForUpdate(RunID) FIRST, held for the whole transaction.
//  2. When the family has a reportCategory, the BINDING RE-CHECK: the
//     highest-sequence row of that category must name artifactID, else the
//     family's superseded refusal, writing NOTHING. Read UNDER the lock, so a
//     report recorded after the server's resolution is visible here.
//  3. The artifact-bound watermark scan; a hit returns the family's closed
//     error, writing NOTHING.
//  4. Each param through AppendChainedTx; a mid-batch failure rolls the whole
//     batch back via the caller's BeginFunc.
func windowDispositionBatchTx(ctx context.Context, tx pgx.Tx, f windowFamily, artifactID string, ps []ChainAppendParams) ([]*Entry, error) {
	if len(ps) == 0 {
		return nil, nil
	}
	runID := ps[0].RunID

	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, runID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("audit: run %s not found", runID)
		}
		return nil, fmt.Errorf("audit: lock run for %s disposition batch: %w", f.name, err)
	}

	if f.reportCategory != "" {
		if err := checkReportBinding(ctx, tx, f, runID, artifactID); err != nil {
			return nil, err
		}
	}

	if wm, err := lowestWatermarkEntry(ctx, tx, f, runID, artifactID); err != nil {
		return nil, err
	} else if wm != nil {
		return nil, f.closed(artifactID, watermarkSettlement(wm.Payload), wm.Sequence, wm.Timestamp.UTC())
	}

	out := make([]*Entry, 0, len(ps))
	for i := range ps {
		e, err := AppendChainedTx(ctx, tx, ps[i])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// windowCloseTx is the family-generic settlement core:
//
//  1. LockRunForUpdate(p.RunID) FIRST.
//  2. Scan for an EXISTING watermark bound to artifactID. If one exists, return
//     it UNCHANGED alongside the dispositions below it — the PERMANENCE
//     property — appending nothing.
//  3. Otherwise append the watermark via AppendChainedTx and return it with the
//     consumed dispositions ({artifactID, below the new watermark's sequence}).
//     Reading the dispositions and appending the watermark in ONE transaction is
//     what closes the capture/apply TOCTOU.
func windowCloseTx(ctx context.Context, tx pgx.Tx, f windowFamily, p ChainAppendParams, artifactID string) (*Entry, []*Entry, error) {
	rq := rundb.New(tx)
	if _, err := rq.LockRunForUpdate(ctx, p.RunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fmt.Errorf("audit: run %s not found", p.RunID)
		}
		return nil, nil, fmt.Errorf("audit: lock run for %s window close: %w", f.name, err)
	}

	existing, err := lowestWatermarkEntry(ctx, tx, f, p.RunID, artifactID)
	if err != nil {
		return nil, nil, err
	}
	if existing != nil {
		consumed, cerr := consumedDispositions(ctx, tx, f, p.RunID, artifactID, existing.Sequence)
		if cerr != nil {
			return nil, nil, cerr
		}
		return existing, consumed, nil
	}

	watermark, err := AppendChainedTx(ctx, tx, p)
	if err != nil {
		return nil, nil, err
	}
	consumed, err := consumedDispositions(ctx, tx, f, p.RunID, artifactID, watermark.Sequence)
	if err != nil {
		return nil, nil, err
	}
	return watermark, consumed, nil
}

// checkReportBinding is the BINDING RE-CHECK (#3923 approval condition 1): it
// returns *UpkeepReportSupersededError unless the HIGHEST-sequence row of
// f.reportCategory names artifactID. An absent row or an undecodable newest row
// refuses too (CurrentArtifactID ""): the capture's binding cannot be
// confirmed, and an append it cannot confirm is the defect the check exists to
// prevent.
func checkReportBinding(ctx context.Context, tx pgx.Tx, f windowFamily, runID uuid.UUID, artifactID string) error {
	id := runID
	rows, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &id,
		Category: f.reportCategory,
	})
	if err != nil {
		return fmt.Errorf("audit: scan %s report rows: %w", f.name, err)
	}
	var newest *auditdb.AuditEntry
	for i := range rows {
		if newest == nil || rows[i].Sequence > newest.Sequence {
			newest = &rows[i]
		}
	}
	if newest == nil {
		return &UpkeepReportSupersededError{ArtifactID: artifactID}
	}
	current := watermarkArtifactID(newest.Payload)
	if current != artifactID {
		return &UpkeepReportSupersededError{ArtifactID: artifactID, CurrentArtifactID: current, CurrentSequence: newest.Sequence}
	}
	return nil
}

// lowestWatermarkEntry returns the family watermark bound to artifactID with
// the LOWEST sequence (the first one written — the permanent one), or nil.
func lowestWatermarkEntry(ctx context.Context, tx pgx.Tx, f windowFamily, runID uuid.UUID, artifactID string) (*Entry, error) {
	id := runID
	rows, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &id,
		Category: f.watermarkCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: scan %s watermarks: %w", f.name, err)
	}
	var best *Entry
	for i := range rows {
		if watermarkArtifactID(rows[i].Payload) != artifactID {
			continue
		}
		e := rowToEntry(rows[i])
		if best == nil || e.Sequence < best.Sequence {
			best = e
		}
	}
	return best, nil
}

// consumedDispositions lists the run's family disposition entries and returns
// those recorded against artifactID with sequence STRICTLY BELOW belowSeq —
// the artifact-scoped consumed set (condition 1). It returns the raw entries;
// the caller collapses them last-wins per id.
func consumedDispositions(ctx context.Context, tx pgx.Tx, f windowFamily, runID uuid.UUID, artifactID string, belowSeq int64) ([]*Entry, error) {
	id := runID
	rows, err := auditdb.New(tx).ListAuditEntriesByCategory(ctx, auditdb.ListAuditEntriesByCategoryParams{
		RunID:    &id,
		Category: f.dispositionCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: list %s dispositions: %w", f.name, err)
	}
	out := make([]*Entry, 0, len(rows))
	for i := range rows {
		if rows[i].Sequence >= belowSeq {
			continue
		}
		if watermarkArtifactID(rows[i].Payload) != artifactID {
			continue
		}
		out = append(out, rowToEntry(rows[i]))
	}
	return out, nil
}

// watermarkArtifactID decodes the shared "artifact_id" payload field, written
// by the server for the disposition rows, the watermark AND the
// upkeep_report_recorded row. It returns "" for an absent key or malformed
// payload, which never matches a real artifact id — the fail-safe direction.
func watermarkArtifactID(payload []byte) string {
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.ArtifactID
}

// watermarkSettlement decodes the watermark's "settlement" field.
func watermarkSettlement(payload []byte) string {
	var p struct {
		Settlement string `json:"settlement"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Settlement
}
