package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dberr"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
)

// hostDispatcherHeartbeatMaxBodyBytes caps the heartbeat body. A full
// heartbeat (hostdispatcher.MaxRunRefusals entries with bounded details) fits
// well under it.
const hostDispatcherHeartbeatMaxBodyBytes = 256 << 10

// handleHostDispatcherHeartbeat serves POST /v0/host-dispatchers/heartbeat
// (#5489, ADR-104 Amendment A2): a host dispatcher's per-tick state snapshot,
// upserted per (account, host). The route is not run-scoped, so the run
// account sweep does not apply: the host label comes from the token's
// host-dispatcher/<host> subject and the account from the token row, never
// from the body. The ladder, in order:
//
//  1. no heartbeat repository wired: 503 not_configured;
//  2. requireWriteScope(write:runs): anonymous 401 authentication_required, a
//     token without write:runs 403 insufficient_scope;
//  3. a subject that is not a valid host-dispatcher/<host>: 403
//     host_dispatcher_subject_required;
//  4. a body over 256 KiB, malformed JSON or an unknown key (a `host` or
//     `account_id` key included): 400 validation_failed;
//  5. a structural Validate failure: 400 validation_failed naming the field;
//  6. otherwise drop off-grammar run_refusals entries, bound each detail, fold
//     an off-grammar tick_failure_code to hostdispatcher.TickFailureCodeInvalid
//     and upsert under the server clock: 200 hostdispatcher.HeartbeatResponse
//     {dropped_refusals}. A store failure
//     answers 503 when the database is unavailable, else 500.
//
// A heartbeat writes no audit row: it is operational state, not an event, and
// a write every tick would flood the chain.
func (s *Server) handleHostDispatcherHeartbeat(w http.ResponseWriter, r *http.Request) {
	repo := s.cfg.HostDispatcherHeartbeatRepo
	if repo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "not_configured",
			"host-dispatcher heartbeat store is not configured", nil)
		return
	}
	if !s.requireWriteScope(w, r, hostdispatcher.ScopeWriteRuns) {
		return
	}
	id := IdentityFrom(r.Context())
	label, err := hostdispatcher.ParseSubject(id.Subject)
	if err != nil {
		s.writeError(w, r, http.StatusForbidden, "host_dispatcher_subject_required",
			"only a host-dispatcher/<host> token may write a heartbeat", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, hostDispatcherHeartbeatMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var hb hostdispatcher.Heartbeat
	if err := dec.Decode(&hb); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"heartbeat body exceeds 256 KiB", map[string]any{"field": "body"})
			return
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"heartbeat body is not valid JSON or contains unknown fields",
			map[string]any{"field": "body", "error": err.Error()})
		return
	}
	if err := hb.Validate(); err != nil {
		field := "body"
		var ve *hostdispatcher.ValidationError
		if errors.As(err, &ve) {
			field = ve.Field
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"field": field})
		return
	}
	dropped := hb.DropInvalidRefusals()
	hb.BoundFields()
	rec := dispatcherheartbeat.Record{
		AccountID:  id.AccountID,
		HostLabel:  label,
		TokenID:    id.TokenID,
		Heartbeat:  hb,
		ReceivedAt: s.nowFunc().UTC(),
	}
	if err := repo.Upsert(r.Context(), rec); err != nil {
		if dberr.IsUnavailable(err) {
			s.writeDBUnavailable(w, r)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"failed to store the heartbeat", map[string]any{internalCauseKey: err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, hostdispatcher.HeartbeatResponse{DroppedRefusals: dropped})
}
