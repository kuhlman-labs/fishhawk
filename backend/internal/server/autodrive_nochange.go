package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// fixupNoChangeRepeatError is autoFixup's refusal when the delegated
// route_fixup arm would re-route, unchanged, a concern set whose last fix-up
// pass produced no commit (#5390). The may_route_fixup arm maps it to
// decision_required fixup_no_changes_unaddressed carrying ConcernIDs. It is
// returned BEFORE the budget arithmetic and fixupStageAs, so nothing is
// spent, appended or transitioned.
type fixupNoChangeRepeatError struct {
	StageID uuid.UUID
	// ConcernIDs are the routable open concern ids, sorted, which are
	// exactly the ids the no-change pass routed (less any server-check id).
	ConcernIDs []string
}

func (e *fixupNoChangeRepeatError) Error() string {
	return fmt.Sprintf("the last fix-up pass on implement stage %s routed exactly the %d open concern(s) [%s] and produced no commit; no fix-up was auto-routed",
		e.StageID, len(e.ConcernIDs), strings.Join(e.ConcernIDs, ", "))
}

// noChangeRepeatOfOpenSet reports whether the delegated route_fixup arm is
// about to repeat a no-change fix-up pass unchanged (#5390). routable is the
// id list autoFixup would route after its #5477 server-check filter;
// serverCheck holds the stage's open server-check concern ids.
//
// The guard reads POSITIVE evidence only and returns a refusal when all of
// these hold:
//
//  1. The NEWEST stage_fixup_triggered entry for this stage (by Sequence,
//     StageID double-checked) routed a non-empty concern_ids set S. A
//     missing, empty or malformed concern_ids is no evidence (legacy and
//     positional routes never trip the guard); a malformed payload WARNs.
//  2. The EARLIEST fixup_pushed / fixup_no_changes entry for this stage
//     sequenced after that trigger is a fixup_no_changes. No outcome yet, or a
//     fixup_pushed, is no evidence. This is the same reading as trace.go's
//     noChangePass veto (buildResolutionVetoContext).
//  3. S minus serverCheck equals routable (string form, order-insensitive,
//     duplicates collapsed). Removing serverCheck from S composes with #5477:
//     a human's non-delegated route may have included a server-check row,
//     which the delegated arm never routes.
//
// Every audit read error is returned wrapped (FAIL-CLOSED), like
// countFixupPasses and fixupRefundedPasses: the arm must not route on
// unknown evidence.
//
// Limitation: succeedFixupNoChangesStage dedupes on ANY prior
// fixup_no_changes row for the stage, so only the FIRST no-change window per
// stage writes an outcome row; a later no-change window reads as "no outcome
// yet" and does not trip the guard (follow-up #5496).
func (s *Server) noChangeRepeatOfOpenSet(ctx context.Context, runID, stageID uuid.UUID, routable, serverCheck []uuid.UUID) (*fixupNoChangeRepeatError, error) {
	triggers, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return nil, fmt.Errorf("list %s audit entries: %w", CategoryStageFixupTriggered, err)
	}
	var (
		newestSeq     int64
		newestPayload []byte
		found         bool
	)
	for _, e := range triggers {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if !found || e.Sequence > newestSeq {
			newestSeq, newestPayload, found = e.Sequence, e.Payload, true
		}
	}
	if !found {
		return nil, nil
	}
	var p struct {
		ConcernIDs []string `json:"concern_ids"`
	}
	if uerr := json.Unmarshal(newestPayload, &p); uerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "auto-drive: no-change guard could not decode the newest fix-up trigger; no evidence, routing",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", uerr.Error()))
		return nil, nil
	}
	if len(p.ConcernIDs) == 0 {
		return nil, nil
	}

	var (
		outcomeSeq   int64
		outcomeCat   string
		outcomeFound bool
	)
	for _, cat := range []string{CategoryFixupPushed, CategoryFixupNoChanges} {
		entries, oerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if oerr != nil {
			return nil, fmt.Errorf("list %s audit entries: %w", cat, oerr)
		}
		for _, e := range entries {
			if e.StageID == nil || *e.StageID != stageID || e.Sequence <= newestSeq {
				continue
			}
			if !outcomeFound || e.Sequence < outcomeSeq {
				outcomeSeq, outcomeCat, outcomeFound = e.Sequence, cat, true
			}
		}
	}
	if !outcomeFound || outcomeCat != CategoryFixupNoChanges {
		return nil, nil
	}

	drop := make(map[string]bool, len(serverCheck))
	for _, id := range serverCheck {
		drop[id.String()] = true
	}
	routed := map[string]bool{}
	for _, id := range p.ConcernIDs {
		if !drop[id] {
			routed[id] = true
		}
	}
	want := make(map[string]bool, len(routable))
	for _, id := range routable {
		want[id.String()] = true
	}
	if !sameStringSet(routed, want) {
		return nil, nil
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &fixupNoChangeRepeatError{StageID: stageID, ConcernIDs: ids}, nil
}

// sameStringSet reports whether a and b hold the same keys.
func sameStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
