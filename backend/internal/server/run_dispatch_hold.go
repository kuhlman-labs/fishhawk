package server

// run_dispatch_hold.go is the operator per-run DISPATCH HOLD (E110.50 /
// #5486): POST /v0/runs/{run_id}/dispatch-hold sets or clears a hold that
// the host-dispatch marker enforces for the run and for every run below it
// in its DecomposedFrom / ParentRunID ancestry.
//
// State: the hold has no column. It is derived from the run's OWN audit
// chain, like the campaign decision marker: the latest run_dispatch_hold_set
// row against the latest run_dispatch_hold_cleared row by Sequence
// (dispatchHoldActive). No migration; a reverted binary leaves the rows as
// inert history.
//
// Placement: guardRunDispatchHold runs in the marker's pre-admission block
// after the state switch (the idempotent dispatched arm is exempt, so an
// in-flight stage is never touched) and BEFORE the fan-in verify hold, the
// superseded-pass settle, the host-memory hold, the campaign dispatch hold
// and the CAS / slot admission, so a held stage commits nothing and takes no
// queue row. The refusal is a plain 409 run_dispatch_held, so an MCP slot
// waiter re-POSTing the marker stops on it. There is no per-dispatch
// override: the audited clear is the operator's explicit override.
//
// The hold reason is operator free text. It is rendered ONLY as a data field
// (details.reason, dispatch_hold.reason); no message or next_step embeds it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The hold's audit pair (registered INTERNAL in audit/categories.go).
const (
	CategoryRunDispatchHoldSet     = "run_dispatch_hold_set"
	CategoryRunDispatchHoldCleared = "run_dispatch_hold_cleared"
)

// Hold verb actions.
const (
	runDispatchHoldActionSet   = "set"
	runDispatchHoldActionClear = "clear"
)

// Hold response codes.
const (
	runDispatchHoldCodeHeld        = "run_dispatch_held"
	runDispatchHoldCodeUnavailable = "dispatch_hold_unavailable"
	runDispatchHoldCodeFailed      = "dispatch_hold_failed"
	runDispatchHoldCodeTerminal    = "run_terminal"
)

// runDispatchHoldPayload is the EFFECTIVE hold of a run: its own, or the
// nearest held ancestor's (inherited true). It is the GET /v0/runs/{id}
// dispatch_hold block, the verb's 200 dispatch_hold and the campaign awaiting
// block's dispatch_hold. The MCP mirror (mcpserver RunDispatchHold) decodes
// it; the json tags MUST byte-match.
type runDispatchHoldPayload struct {
	HeldRunID     string    `json:"held_run_id"`
	Inherited     bool      `json:"inherited"`
	Reason        string    `json:"reason"`
	HeldBy        string    `json:"held_by"`
	HeldAt        time.Time `json:"held_at"`
	AuditSequence int64     `json:"audit_sequence"`
}

// runDispatchHoldAuditPayload is the payload of both audit rows. It carries
// only this run's own action, the operator's reason and the actor subject.
type runDispatchHoldAuditPayload struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	Actor  string `json:"actor"`
}

// runDispatchHoldRequest is the verb's body: exactly {action, reason}.
type runDispatchHoldRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// runDispatchHoldResponse is the verb's 200 body. dispatch_hold is the
// EFFECTIVE hold after the act (own or inherited; omitted when none), so a
// clear on a child that still inherits a parent's hold says so.
// dispatch_hold_unavailable reports that the post-act read failed (the act
// itself is recorded).
type runDispatchHoldResponse struct {
	RunID                   string                  `json:"run_id"`
	Action                  string                  `json:"action"`
	Changed                 bool                    `json:"changed"`
	DispatchHold            *runDispatchHoldPayload `json:"dispatch_hold,omitempty"`
	DispatchHoldUnavailable bool                    `json:"dispatch_hold_unavailable,omitempty"`
	AuditSequence           int64                   `json:"audit_sequence,omitempty"`
}

// dispatchHoldActive reports whether the latest set row (by Sequence) is
// newer than the latest cleared row. set and cleared are the latest rows of
// each category; nil when the category has none.
func dispatchHoldActive(set, cleared *audit.Entry) bool {
	return set != nil && (cleared == nil || set.Sequence > cleared.Sequence)
}

// latestBySequence returns the entry with the highest Sequence (the later one
// on a tie), or nil for an empty list.
func latestBySequence(entries []*audit.Entry) *audit.Entry {
	var latest *audit.Entry
	for _, e := range entries {
		if e == nil {
			continue
		}
		if latest == nil || e.Sequence >= latest.Sequence {
			latest = e
		}
	}
	return latest
}

// ownRunDispatchHold reads runID's OWN hold state from its audit chain: two
// category reads, latest-wins. It returns nil when the run is not itself
// held. A read error is returned.
func (s *Server) ownRunDispatchHold(ctx context.Context, runID uuid.UUID) (*runDispatchHoldPayload, error) {
	sets, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryRunDispatchHoldSet)
	if err != nil {
		return nil, fmt.Errorf("list %s for run %s: %w", CategoryRunDispatchHoldSet, runID, err)
	}
	clears, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryRunDispatchHoldCleared)
	if err != nil {
		return nil, fmt.Errorf("list %s for run %s: %w", CategoryRunDispatchHoldCleared, runID, err)
	}
	set := latestBySequence(sets)
	if !dispatchHoldActive(set, latestBySequence(clears)) {
		return nil, nil
	}
	// A malformed payload still holds (the set row exists); only the
	// rendered reason and actor are lost.
	var p runDispatchHoldAuditPayload
	_ = json.Unmarshal(set.Payload, &p)
	heldBy := p.Actor
	if heldBy == "" && set.ActorSubject != nil {
		heldBy = *set.ActorSubject
	}
	return &runDispatchHoldPayload{
		HeldRunID:     runID.String(),
		Reason:        p.Reason,
		HeldBy:        heldBy,
		HeldAt:        set.Timestamp,
		AuditSequence: set.Sequence,
	}, nil
}

// resolveRunDispatchHold returns runRow's EFFECTIVE hold: its own, else the
// nearest held ancestor's through DecomposedFrom / ParentRunID (runParentID,
// at most maxCampaignAncestorDepth hops, the campaign hold's bound). The
// first held run wins; inherited is true when it is an ancestor.
//
// A missing ancestor (run.ErrNotFound), a cycle or a depth past the bound
// stops the walk and decides on the runs read so far, with a WARN naming the
// run: a hold can only be placed through a readable run, and failing closed
// would wedge a broken lineage with no remedy. Any other read error is
// returned (the marker fails closed on it). With no AuditRepo the resolver is
// inert (nil, nil): no hold can then exist.
func (s *Server) resolveRunDispatchHold(ctx context.Context, runRow *run.Run) (*runDispatchHoldPayload, error) {
	if s.cfg.AuditRepo == nil || runRow == nil {
		return nil, nil
	}
	stop := func(why string, at uuid.UUID) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"dispatch hold: ancestry walk stopped; deciding on the runs read so far, so a hold above this point is not applied",
			slog.String("run_id", runRow.ID.String()),
			slog.String("stopped_at_run_id", at.String()),
			slog.String("reason", why))
	}
	cur := runRow
	seen := map[uuid.UUID]bool{cur.ID: true}
	for hops := 0; ; hops++ {
		hold, err := s.ownRunDispatchHold(ctx, cur.ID)
		if err != nil {
			return nil, err
		}
		if hold != nil {
			hold.Inherited = cur.ID != runRow.ID
			return hold, nil
		}
		parent := runParentID(cur)
		if parent == nil {
			return nil, nil
		}
		if seen[*parent] {
			stop(campaignAncestryCycle, cur.ID)
			return nil, nil
		}
		if hops >= maxCampaignAncestorDepth {
			stop(campaignAncestryDepthExceeded, cur.ID)
			return nil, nil
		}
		if s.cfg.RunRepo == nil {
			return nil, nil
		}
		next, err := s.cfg.RunRepo.GetRun(ctx, *parent)
		if errors.Is(err, run.ErrNotFound) {
			stop(campaignAncestryMissingParent, cur.ID)
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read ancestor run %s: %w", *parent, err)
		}
		seen[next.ID] = true
		cur = next
	}
}

// guardRunDispatchHold is the host-dispatch marker's per-run dispatch hold.
// With an effective hold (own or inherited) it refuses 409
// run_dispatch_held for every stage type; a resolver read error answers 500
// dependency_check_failed (fail closed). nil admits.
func (s *Server) guardRunDispatchHold(ctx context.Context, runRow *run.Run, stage *run.Stage) *campaignSourceRefusal {
	hold, err := s.resolveRunDispatchHold(ctx, runRow)
	if err != nil {
		return &campaignSourceRefusal{
			Status:  http.StatusInternalServerError,
			Code:    "dependency_check_failed",
			Message: "could not read the run's dispatch hold or its ancestry; the stage is not admitted until the read succeeds",
			Details: map[string]any{"run_id": runRow.ID.String(), "stage_id": stage.ID.String(), internalCauseKey: err.Error()},
		}
	}
	if hold == nil {
		return nil
	}
	whose := "this run"
	if hold.Inherited {
		whose = "its ancestor run " + hold.HeldRunID
	}
	return &campaignSourceRefusal{
		Status: http.StatusConflict,
		Code:   runDispatchHoldCodeHeld,
		Message: fmt.Sprintf("an operator dispatch hold on %s parks new host dispatches of this run; in-flight stages continue. Clear it with POST /v0/runs/%s/dispatch-hold {\"action\":\"clear\"} (fishhawk_hold_dispatch action clear); there is no per-dispatch override",
			whose, hold.HeldRunID),
		Details: map[string]any{
			"run_id":         runRow.ID.String(),
			"stage_id":       stage.ID.String(),
			"held_run_id":    hold.HeldRunID,
			"inherited":      hold.Inherited,
			"reason":         hold.Reason,
			"held_by":        hold.HeldBy,
			"held_at":        hold.HeldAt,
			"audit_sequence": hold.AuditSequence,
		},
	}
}

// parseRunDispatchHoldBody decodes exactly {action, reason}: unknown fields
// and trailing content are refused, action defaults to set, and the trimmed
// reason must be 1..promoteReasonMax bytes.
func (s *Server) parseRunDispatchHoldBody(w http.ResponseWriter, r *http.Request) (action, reason string, ok bool) {
	const shape = `request body must be a single JSON object {"action": "set"|"clear", "reason": string}`
	var body runDispatchHoldRequest
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, promoteBodyMax))
		dec.DisallowUnknownFields()
		decErr := dec.Decode(&body)
		switch {
		case decErr != nil && !errors.Is(decErr, io.EOF):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed", shape,
				map[string]any{"field": "body", "error": decErr.Error()})
			return "", "", false
		case decErr == nil:
			var trailing json.RawMessage
			if tErr := dec.Decode(&trailing); !errors.Is(tErr, io.EOF) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					shape+"; trailing content is refused", map[string]any{"field": "body"})
				return "", "", false
			}
		}
	}
	action = body.Action
	if action == "" {
		action = runDispatchHoldActionSet
	}
	if action != runDispatchHoldActionSet && action != runDispatchHoldActionClear {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			`action must be "set" or "clear"`,
			map[string]any{"field": "action", "got": body.Action})
		return "", "", false
	}
	reason = strings.TrimSpace(body.Reason)
	if reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: say why this run's dispatches are held or released",
			map[string]any{"field": "reason"})
		return "", "", false
	}
	if len(reason) > promoteReasonMax {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("reason must be at most %d bytes", promoteReasonMax),
			map[string]any{"field": "reason"})
		return "", "", false
	}
	return action, reason, true
}

// handleRunDispatchHold implements POST /v0/runs/{run_id}/dispatch-hold. The
// rungs, every one decided before any write:
//
//	401 authentication_required   — anonymous.
//	403 run_token_forbidden       — a run-bound agent token, even its own run.
//	403 insufficient_scope        — a token without write:runs.
//	400 validation_failed         — a non-UUID run_id, an unknown action, an
//	                                empty or over-1000-byte trimmed reason, an
//	                                unknown body field or trailing content.
//	503 dispatch_hold_unavailable — no run or audit repository.
//	404 run_not_found             — no such run.
//	409 run_terminal              — action set on a terminal run (clear is
//	                                always allowed).
//	500 dispatch_hold_failed      — a run or own-state read error, or the
//	                                append failed; nothing written.
//
// Then the run's OWN state: a set while own-held or a clear while not
// own-held is a 200 changed:false no-op that appends nothing. Otherwise the
// category row is appended (AppendChained, payload {action, reason, actor})
// and 200 changed:true carries its audit_sequence. A pre-check then append,
// not a guarded append: a concurrent duplicate can add a redundant row, and
// latest-wins keeps the state correct under any interleaving.
//
// Auth: memberWrite via requireRunAccount. A delegated operator-agent token
// is admitted: a hold only stops or resumes host dispatch and grants no gate
// act. Any memberWrite caller can clear a hold (accepted; the row records the
// actor). Host-dispatcher tokens are denied by hostdispatcher.RouteAllowed.
func (s *Server) handleRunDispatchHold(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not set or clear a dispatch hold; holding a run's dispatches is an operator action", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:runs") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:runs",
			map[string]any{"required_scope": "write:runs"})
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	action, reason, ok := s.parseRunDispatchHoldBody(w, r)
	if !ok {
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, runDispatchHoldCodeUnavailable,
			"the dispatch hold verb requires the run and audit repositories", nil)
		return
	}
	ctx := r.Context()
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		s.writeError(w, r, http.StatusNotFound, "run_not_found",
			"no run with that id", map[string]any{"run_id": runID.String()})
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, runDispatchHoldCodeFailed,
			"could not read the run; nothing was recorded",
			map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
		return
	}
	if action == runDispatchHoldActionSet && runRow.State.IsTerminal() {
		s.writeError(w, r, http.StatusConflict, runDispatchHoldCodeTerminal,
			"run is "+string(runRow.State)+"; a terminal run dispatches nothing, so it cannot be held",
			map[string]any{"run_id": runID.String(), "run_state": string(runRow.State)})
		return
	}
	own, err := s.ownRunDispatchHold(ctx, runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, runDispatchHoldCodeFailed,
			"could not read the run's dispatch hold state; nothing was recorded",
			map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
		return
	}
	resp := runDispatchHoldResponse{RunID: runID.String(), Action: action}
	if (action == runDispatchHoldActionSet) != (own != nil) {
		category := CategoryRunDispatchHoldSet
		if action == runDispatchHoldActionClear {
			category = CategoryRunDispatchHoldCleared
		}
		subject := id.Subject
		actorKind := actorKindForSubject(subject)
		raw, err := json.Marshal(runDispatchHoldAuditPayload{Action: action, Reason: reason, Actor: subject})
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, runDispatchHoldCodeFailed,
				"could not encode the dispatch hold record; nothing was recorded",
				map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
			return
		}
		entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        runID,
			Timestamp:    time.Now().UTC(),
			Category:     category,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      raw,
		})
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, runDispatchHoldCodeFailed,
				"could not record the dispatch hold; nothing was changed",
				map[string]any{"run_id": runID.String(), internalCauseKey: err.Error()})
			return
		}
		resp.Changed = true
		if entry != nil {
			resp.AuditSequence = entry.Sequence
		}
	}
	effective, err := s.resolveRunDispatchHold(ctx, runRow)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"dispatch hold: post-act effective hold read failed; reporting dispatch_hold_unavailable",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		resp.DispatchHoldUnavailable = true
	} else {
		resp.DispatchHold = effective
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}
