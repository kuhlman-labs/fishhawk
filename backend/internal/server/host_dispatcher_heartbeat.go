package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

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
// upserted per (account, host). Since ADR-094 Amendment A5 (#5614) the
// snapshot may also DECLARE the host's admission limits (host_capacity,
// host_implement_limit) for that same (account, host). The route is not
// run-scoped, so the run account sweep does not apply: the host label comes
// from the token's host-dispatcher/<host> subject and the account from the
// token row, never from the body. The ladder, in order:
//
//  1. no heartbeat repository wired: 503 not_configured;
//  2. requireWriteScope(write:runs): anonymous 401 authentication_required, a
//     token without write:runs 403 insufficient_scope;
//  3. a subject that is not a valid host-dispatcher/<host>: 403
//     host_dispatcher_subject_required;
//  4. a body over 256 KiB, malformed JSON or an unknown key (a `host` or
//     `account_id` key included): 400 validation_failed;
//  5. a host_capacity or host_implement_limit that is not an integer literal
//     (a string, a fraction or exponent, a boolean, an object or an array):
//     400 validation_failed naming that field (ADR-104 A2 item 5: a type error
//     is structural). JSON null reads absent. An integer literal that
//     overflows int is NOT a type error: it is out of range (condition C3);
//  6. a structural Validate failure: 400 validation_failed naming the field;
//  7. otherwise drop off-grammar run_refusals entries, bound each detail, fold
//     an off-grammar tick_failure_code to hostdispatcher.TickFailureCodeInvalid,
//     drop the whole declaration when either value is outside 1..64
//     (DropInvalidDeclaration, A5 item 1), and upsert under the server clock:
//     200 hostdispatcher.HeartbeatResponse {dropped_refusals,
//     dropped_declaration (omitted when none), host_label (the subject's)}.
//     The upsert replaces the whole payload, so a heartbeat that omits a
//     declaration field removes that field's earlier declaration, and an
//     out-of-range heartbeat carries no earlier declaration forward. A store
//     failure answers 503 when the database is unavailable, else 500.
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
	var in heartbeatIngestBody
	if err := dec.Decode(&in); err != nil {
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
	if field, got := in.nonIntegerDeclaration(); field != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("hostdispatcher: heartbeat %s must be an integer from %d to %d; got %s",
				field, hostdispatcher.MinDeclaredLimit, hostdispatcher.MaxDeclaredLimit, got),
			map[string]any{"field": field})
		return
	}
	hb := in.heartbeat()
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
	droppedDeclaration := hb.DropInvalidDeclaration()
	rec := dispatcherheartbeat.Record{
		AccountID:          id.AccountID,
		HostLabel:          label,
		TokenID:            id.TokenID,
		Heartbeat:          hb,
		DroppedDeclaration: droppedDeclaration,
		ReceivedAt:         s.nowFunc().UTC(),
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
	s.writeJSON(w, r, http.StatusOK, hostdispatcher.HeartbeatResponse{
		DroppedRefusals:    dropped,
		DroppedDeclaration: droppedDeclaration,
		HostLabel:          label,
	})
}

// heartbeatIngestBody is the strict ingest decode target: the wire heartbeat
// with its two declaration fields shadowed (a depth-0 field beats the embedded
// one for the same JSON key, and DisallowUnknownFields still knows every key)
// by declaredLimitLiteral, which tells an overflowing integer literal (out of
// range, condition C3) apart from a non-integer (structural). Decoding into
// *int would report both as the same *json.UnmarshalTypeError.
type heartbeatIngestBody struct {
	hostdispatcher.Heartbeat
	HostCapacity       *declaredLimitLiteral `json:"host_capacity"`
	HostImplementLimit *declaredLimitLiteral `json:"host_implement_limit"`
}

// nonIntegerDeclaration returns the first declaration field (host_capacity
// first) whose value is not an integer literal, and what it was.
func (b heartbeatIngestBody) nonIntegerDeclaration() (field, got string) {
	if b.HostCapacity != nil && b.HostCapacity.notInteger != "" {
		return hostdispatcher.DeclarationFieldHostCapacity, b.HostCapacity.notInteger
	}
	if b.HostImplementLimit != nil && b.HostImplementLimit.notInteger != "" {
		return hostdispatcher.DeclarationFieldHostImplementLimit, b.HostImplementLimit.notInteger
	}
	return "", ""
}

// heartbeat returns the wire heartbeat carrying the decoded declaration.
func (b heartbeatIngestBody) heartbeat() hostdispatcher.Heartbeat {
	hb := b.Heartbeat
	hb.HostCapacity = b.HostCapacity.intPtr()
	hb.HostImplementLimit = b.HostImplementLimit.intPtr()
	return hb
}

// declaredLimitLiteral is one decoded declaration value. JSON null never
// reaches UnmarshalJSON (encoding/json sets the pointer field nil: absent).
type declaredLimitLiteral struct {
	// value is the integer; an integer literal that overflows int saturates
	// to math.MaxInt or math.MinInt, so DropInvalidDeclaration drops it.
	value int
	// notInteger describes a non-integer value ("a string", ...); empty for
	// an integer literal.
	notInteger string
}

// UnmarshalJSON records the value and never fails, so decoding continues and
// an unknown key elsewhere in the body still answers its own 400.
func (d *declaredLimitLiteral) UnmarshalJSON(b []byte) error {
	d.value, d.notInteger = parseDeclaredLimitLiteral(b)
	return nil
}

func (d *declaredLimitLiteral) intPtr() *int {
	if d == nil {
		return nil
	}
	v := d.value
	return &v
}

// parseDeclaredLimitLiteral reads one JSON value the decoder has already
// syntax-checked. Only -?[0-9]+ is an integer literal: 4.0 and 4e0 are not.
func parseDeclaredLimitLiteral(b []byte) (int, string) {
	if len(b) == 0 {
		return 0, "an empty value"
	}
	digits := b
	if digits[0] == '-' {
		digits = digits[1:]
	}
	integer := len(digits) > 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			integer = false
			break
		}
	}
	if integer {
		n, err := strconv.ParseInt(string(b), 10, strconv.IntSize)
		var ne *strconv.NumError
		if errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
			if b[0] == '-' {
				return math.MinInt, ""
			}
			return math.MaxInt, ""
		}
		if err == nil {
			return int(n), ""
		}
	}
	switch b[0] {
	case '"':
		return 0, "a string"
	case 't', 'f':
		return 0, "a boolean"
	case '{':
		return 0, "an object"
	case '[':
		return 0, "an array"
	}
	return 0, "a non-integer number"
}
