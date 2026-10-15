package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// First-officer seat identity (ADR-089 D2, E85.6 / #4148). A seat is an API
// token whose subject is first_officer/campaign/<uuid> or
// first_officer/run/<uuid>. bearerAuth hands every authenticated seat-subject
// token to resolveFirstOfficerSeat, which clamps its scopes to the fixed seat
// profile, retires the seat when its anchor is terminal or gone, and stamps the
// anchor's account. Every arm fails CLOSED. Long-form contract:
// backend/internal/firstofficer/README.md.

// Audit categories the seat identity writes (registered in
// backend/internal/audit/categories.go).
const (
	auditFirstOfficerSeatRetired   = "first_officer_seat_retired"
	auditFirstOfficerActionRefused = "first_officer_action_refused"
)

// Retirement reasons recorded on first_officer_seat_retired.
const (
	seatRetiredAnchorTerminal = "anchor_terminal"
	seatRetiredAnchorMissing  = "anchor_missing"
)

// seatAnchorState is what resolveSeatAnchor learned about a live seat's anchor.
type seatAnchorState struct {
	state     string
	terminal  bool
	missing   bool
	accountID string
}

// resolveFirstOfficerSeat is the bearerAuth hook for a seat-subject api token.
// It returns the identity the request proceeds under and true, or writes the
// refusal and returns false. Order matters:
//
//  1. Clamp scopes to firstofficer.ClampScopes — ALWAYS, before anything else
//     can fail, so no later arm can leak a wider identity.
//  2. An unparseable seat subject → 401 first_officer_seat_invalid.
//  3. Unwired token / anchor repos → 503 first_officer_seat_unverifiable.
//  4. An anchor read error → 503 first_officer_seat_unverifiable, nothing
//     revoked (an outage must not retire a seat).
//  5. A terminal or missing anchor → revoke every active token for the subject,
//     append first_officer_seat_retired, 401 first_officer_seat_retired.
//  6. Live anchor → stamp the anchor's account onto the identity (approval
//     condition 1): a seat on a tenanted anchor acts within that account; an
//     untenanted anchor leaves it untenanted.
func (s *Server) resolveFirstOfficerSeat(w http.ResponseWriter, r *http.Request, id Identity) (Identity, bool) {
	id.Scopes = firstofficer.ClampScopes(id.Scopes)

	anchor, err := firstofficer.ParseSubject(id.Subject)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "first_officer_seat_invalid",
			"the token subject carries the first-officer prefix but is not first_officer/campaign/<uuid> or first_officer/run/<uuid>", nil)
		return Identity{}, false
	}
	if s.cfg.APITokenRepo == nil ||
		(anchor.Kind == firstofficer.AnchorRun && s.cfg.RunRepo == nil) ||
		(anchor.Kind == firstofficer.AnchorCampaign && s.cfg.CampaignRepo == nil) {
		s.writeError(w, r, http.StatusServiceUnavailable, "first_officer_seat_unverifiable",
			"the first-officer seat's anchor cannot be verified: a required repository is not configured",
			map[string]any{"reason": "unconfigured"})
		return Identity{}, false
	}

	st, err := s.resolveSeatAnchor(r.Context(), anchor)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "first_officer_seat_unverifiable",
			"the first-officer seat's anchor could not be read; retry shortly",
			map[string]any{"reason": "anchor_unreadable", internalCauseKey: err.Error()})
		return Identity{}, false
	}
	if st.missing || st.terminal {
		reason := seatRetiredAnchorTerminal
		if st.missing {
			reason = seatRetiredAnchorMissing
		}
		s.retireFirstOfficerSeat(r, id, anchor, st, reason)
		s.writeError(w, r, http.StatusUnauthorized, "first_officer_seat_retired",
			"this first-officer seat is retired: its "+string(anchor.Kind)+" is "+reasonPhrase(st),
			map[string]any{"reason": reason, "anchor_kind": string(anchor.Kind),
				"anchor_id": anchor.ID.String(), "anchor_state": st.state})
		return Identity{}, false
	}
	id.AccountID = st.accountID
	return id, true
}

func reasonPhrase(st seatAnchorState) string {
	if st.missing {
		return "gone"
	}
	return "terminal (" + st.state + ")"
}

// resolveSeatAnchor reads the anchor's state and account. A not-found anchor
// is missing (no error); any other read failure is an error.
func (s *Server) resolveSeatAnchor(ctx context.Context, a firstofficer.Anchor) (seatAnchorState, error) {
	switch a.Kind {
	case firstofficer.AnchorRun:
		rn, err := s.cfg.RunRepo.GetRun(ctx, a.ID)
		if errors.Is(err, run.ErrNotFound) {
			return seatAnchorState{missing: true}, nil
		}
		if err != nil {
			return seatAnchorState{}, fmt.Errorf("get run: %w", err)
		}
		return seatAnchorState{state: string(rn.State), terminal: rn.State.IsTerminal(), accountID: rn.AccountID}, nil
	default:
		c, err := s.cfg.CampaignRepo.GetCampaign(ctx, a.ID)
		if errors.Is(err, campaign.ErrNotFound) {
			return seatAnchorState{missing: true}, nil
		}
		if err != nil {
			return seatAnchorState{}, fmt.Errorf("get campaign: %w", err)
		}
		st := seatAnchorState{state: string(c.State), terminal: c.State.IsTerminal()}
		if st.terminal {
			// The retirement row's partition only; a failed read falls back
			// to the token's own account rather than blocking retirement.
			if acct, aerr := s.cfg.CampaignRepo.GetCampaignAccountID(ctx, a.ID); aerr == nil {
				st.accountID = acct
			}
			return st, nil
		}
		acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(ctx, a.ID)
		if err != nil {
			return seatAnchorState{}, fmt.Errorf("get campaign account: %w", err)
		}
		st.accountID = acct
		return st, nil
	}
}

// retireFirstOfficerSeat revokes every active token for the seat subject and
// appends first_officer_seat_retired. Revocation is idempotent; a failed list
// or revoke is logged, and the request is refused regardless, so the token is
// never honored past a terminal anchor. Concurrent first requests may each
// append a row (accepted; documented in the firstofficer README).
func (s *Server) retireFirstOfficerSeat(r *http.Request, id Identity, anchor firstofficer.Anchor, st seatAnchorState, reason string) {
	ctx := r.Context()
	toks, err := s.cfg.APITokenRepo.ListForSubject(ctx, id.Subject)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "first-officer seat retirement: list tokens failed",
			slog.String("seat_subject", id.Subject), slog.String("error", err.Error()))
		return
	}
	revoked := []string{}
	for _, t := range toks {
		if _, rerr := s.cfg.APITokenRepo.Revoke(ctx, t.ID, id.Subject); rerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "first-officer seat retirement: revoke failed",
				slog.String("seat_subject", id.Subject), slog.String("token_id", t.ID.String()),
				slog.String("error", rerr.Error()))
			continue
		}
		revoked = append(revoked, t.ID.String())
	}
	if len(revoked) == 0 || s.cfg.AuditRepo == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"seat_subject":      id.Subject,
		"reason":            reason,
		"anchor":            map[string]any{"kind": string(anchor.Kind), "id": anchor.ID.String()},
		"anchor_state":      st.state,
		"revoked_token_ids": revoked,
	})
	actor := audit.ActorSystem
	now := time.Now().UTC()
	if anchor.Kind == firstofficer.AnchorRun && !st.missing {
		_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: anchor.ID, Timestamp: now, Category: auditFirstOfficerSeatRetired,
			ActorKind: &actor, Payload: payload,
		})
	} else {
		acctRaw := st.accountID
		if st.missing {
			acctRaw = id.AccountID
		}
		_, err = s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
			Timestamp: now, Category: auditFirstOfficerSeatRetired,
			ActorKind: &actor, Payload: payload, AccountID: parseAccountUUID(acctRaw),
		})
	}
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "first-officer seat retirement: audit append failed",
			slog.String("seat_subject", id.Subject), slog.String("error", err.Error()))
	}
}

// parseAccountUUID returns nil for an empty or non-UUID account id.
func parseAccountUUID(raw string) *uuid.UUID {
	if raw == "" {
		return nil
	}
	u, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return &u
}

// firstOfficerSeatOf reports the seat anchor of id. ok=false for a non-seat
// identity AND for a seat subject that does not parse; callers that must
// refuse a malformed seat check firstofficer.IsSeatSubject first.
func firstOfficerSeatOf(id Identity) (firstofficer.Anchor, bool) {
	if !firstofficer.IsSeatSubject(id.Subject) {
		return firstofficer.Anchor{}, false
	}
	a, err := firstofficer.ParseSubject(id.Subject)
	if err != nil {
		return firstofficer.Anchor{}, false
	}
	return a, true
}

// firstOfficerRunInSeat reports whether runID lies inside the seat: a run seat
// matches only its own run; a campaign seat matches any run a campaign item of
// that campaign is linked to. An unwired campaign repo or a read failure is an
// error (callers fail closed).
func (s *Server) firstOfficerRunInSeat(ctx context.Context, a firstofficer.Anchor, runID uuid.UUID) (bool, error) {
	if a.Kind == firstofficer.AnchorRun {
		return a.ID == runID, nil
	}
	if s.cfg.CampaignRepo == nil {
		return false, errors.New("campaign repository not configured")
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("list campaign items for run: %w", err)
	}
	for _, it := range items {
		if it.CampaignID == a.ID {
			return true, nil
		}
	}
	return false, nil
}

// recordFirstOfficerRefusal is writeError's 403 hook (ADR-089 D2): every
// permission refusal served to a seat identity also writes a
// first_officer_action_refused row, so a prohibited call is a permission error
// plus a record, never a silent no-op. Best-effort: an append failure is logged
// and never alters the response. A refusal that bypasses writeError (the /mcp
// JSON-RPC tool-scope precheck) records nothing — the documented residual.
func (s *Server) recordFirstOfficerRefusal(r *http.Request, code string, details map[string]any) {
	id := IdentityFrom(r.Context())
	if !firstofficer.IsSeatSubject(id.Subject) || s.cfg.AuditRepo == nil {
		return
	}
	body := map[string]any{
		"seat_subject": id.Subject,
		"method":       r.Method,
		"path":         r.URL.Path,
		"code":         code,
	}
	if rs, ok := details["required_scope"]; ok {
		body["required_scope"] = rs
	}
	payload, _ := json.Marshal(body)
	actor := audit.ActorAgent
	subject := id.Subject
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(r.Context(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: auditFirstOfficerActionRefused,
		ActorKind: &actor, ActorSubject: &subject, Payload: payload,
		AccountID: identityAccountID(r.Context()),
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "first-officer refusal: audit append failed",
			slog.String("seat_subject", id.Subject), slog.String("code", code), slog.String("error", err.Error()))
	}
}
