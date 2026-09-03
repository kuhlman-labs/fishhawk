package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// errorEnvelope is the JSON shape every non-2xx response uses, per
// docs/api/v0.openapi.yaml's `Error` schema. The `code` field is
// the stable, machine-readable identifier; clients switch on it.
// `message` is human-readable and may change between versions.
// `details` is structured per-code; the keys ARE part of the
// contract for that specific code.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// ErrorRef is the correlation handle on a 5xx response (E67.15 / #2587):
	// the request id the requestID middleware already mints and echoes as
	// X-Request-ID. The caller pairs it with the operator's server log to get
	// the full cause, which is redacted out of the 5xx body. Empty (omitted)
	// on 4xx and when no requestID middleware ran, so 4xx bytes are unchanged.
	// It is the caller's own X-Request-ID echoed back, NOT a server-authenticated
	// identifier — a client may supply its own X-Request-ID (middleware.go:155).
	// TestWriteError_5xxWithoutRequestIDOmitsErrorRef pins the omitted-when-no-
	// request-id claim (E67.30 / #2636) on the shipped bytes.
	ErrorRef string `json:"error_ref,omitempty"`
}

// internalCauseKey is a details key carrying the full, non-client-facing
// error cause (E67.15 / #2587, CONDITION 1). writeError ALWAYS strips it
// from the response body — regardless of status AND regardless of the 5xx
// allow-list — and ALWAYS folds its value into the ONE "http error response"
// server-side log record, at any status (E67.31 / #2637). Both halves are
// unconditional, so a cause handed through this channel reaches the operator
// and never the caller, whatever the status.
//
// The two statuses differ in WHO can correlate, and only there. At 5xx the
// body also carries error_ref, so the caller holds the same handle as the log
// record and can quote it to the operator. At 4xx the body is byte-identical
// to pre-#2587 — no error_ref member — while the LOG RECORD still carries the
// error_ref attribute (it is in writeError's unconditional attr slice), so the
// operator can correlate by request id but the caller was handed no handle.
// The 4xx channel is operator-only by design; it is never silently dropped.
//
// Because the strip is unconditional it is NOT a new disclosure surface if
// someone forgets the allow-list: TestWriteError_AlwaysStripsInternalCauseKey
// pins the unconditional removal on a 4xx (where the allow-list does not run),
// and TestWriteError_4xxLogsInternalCauseToOperator pins the 4xx fold.
// Call sites that would otherwise pass a raw cause as the message argument
// (a syntax the redactor cannot see) instead pass a static literal message
// and hand the cause through this channel.
const internalCauseKey = "__cause"

// redactableDetailKeys is the DEFAULT-DENY allow-list applied to a 5xx
// response's details (E67.15 / #2587). writeError drops every 5xx detail
// key that is NOT a member here; on 4xx the map is untouched.
//
// Membership rule (the invariant a new key must satisfy before it is added):
// a key is admitted only when its value is a product-owned enum / identifier
// / boolean / integer, a caller-echoed request field, or a static literal;
// NEVER a value derived from an error, a subprocess, or a third-party API
// response. The `error` key is DELIBERATELY absent — its value is a raw
// cause, which is precisely the information-disclosure surface this gate
// closes; the cause reaches the operator through the log record keyed by
// error_ref, never the caller.
//
// This is a KEY-based control, so it cannot by itself stop a cause smuggled
// under an admitted key (e.g. `reason`); the raw-cause AST guard's
// details-half check (raw_cause_guard_test.go) is the tripwire for that
// within recognized syntaxes, and backend/internal/server/README.md records
// the residual gap outside them. The seed set is the surveyed 5xx detail
// keys (E67.15 re-scan), each of which satisfies the membership rule above.
var redactableDetailKeys = map[string]struct{}{
	"run_id":           {},
	"stage_id":         {},
	"provider":         {},
	"action":           {},
	"item_id":          {},
	"retryable":        {},
	"next_actions":     {},
	"workflow_id":      {},
	"artifact_id":      {},
	"reason":           {},
	"registered":       {},
	"state":            {},
	"repo":             {},
	"stage_type":       {},
	"predicate":        {},
	"ref":              {},
	"got":              {},
	"issue_ref":        {},
	"verdict_sequence": {},
	"installation_id":  {},
	"filed_issue":      {},
	"field":            {},
	"concern_id":       {},
	"epic_number":      {},
	"failed_ordinal":   {},
	"filed":            {},
	"stage":            {},
	// recorded / requested: the durable-row count and batch size a partially
	// applied batch reports (grooming_dispositions.go's mid-batch
	// AppendChained failure, #2843). Product-owned integers the handler
	// computes from the caller's OWN submitted batch — never derived from an
	// error, a subprocess or a third-party response — so they satisfy the
	// membership rule above, on the `failed_ordinal` precedent. Without them
	// the 5xx body carries no count at all and the caller cannot tell how many
	// of its rows survived.
	"recorded":  {},
	"requested": {},
	// resolved / items_total / suggested_grooming_order_limit / budget_seconds:
	// the counts the 504 issue_set_resolution_timeout refusal exists to carry
	// (E54.59 / #3113). A 504 is a 5xx, so WITHOUT these four entries the
	// default-deny redactor above strips exactly the numbers the refusal is
	// made of and the operator is handed an empty body. Each is a product-owned
	// integer the handler computes — three from the resolver's own typed
	// accounting, one from the server's configured budget — never derived from
	// an error, a subprocess or a third-party response, so all four satisfy the
	// membership rule above on the `recorded`/`requested` precedent.
	"resolved":                       {},
	"items_total":                    {},
	"suggested_grooming_order_limit": {},
	"budget_seconds":                 {},
}

// redactErrorDetails returns a NEW map holding only the allow-listed keys of
// details. A nil/empty input (or an input whose every key is dropped) returns
// nil so the errorBody.Details omitempty shape is unchanged. It never mutates
// its argument. This is the client-facing 5xx detail product: built from
// table-owned keys, never from arbitrary input — the redaction-safe-by-
// construction posture backend/internal/diagnostics.ClassifyFailureDetail
// established.
func redactErrorDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		if _, ok := redactableDetailKeys[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitInternalCause separates the non-client-facing internalCauseKey from
// the client details. It returns the cause (rendered as a string, "" when
// absent) and the client details WITHOUT internalCauseKey. When the key is
// absent it returns details unchanged (no allocation), so the common path is
// byte-identical to pre-#2587 behavior. When present it returns a fresh map so
// the caller's map is never mutated.
func splitInternalCause(details map[string]any) (cause string, client map[string]any) {
	if len(details) == 0 {
		return "", details
	}
	raw, ok := details[internalCauseKey]
	if !ok {
		return "", details
	}
	cause = fmt.Sprint(raw)
	client = make(map[string]any, len(details)-1)
	for k, v := range details {
		if k == internalCauseKey {
			continue
		}
		client[k] = v
	}
	if len(client) == 0 {
		client = nil
	}
	return cause, client
}

// writeError renders an error envelope to w with the given status. It is the
// single 5xx information-disclosure chokepoint (E67.15 / #2587): on a 5xx it
// (a) strips every non-allow-listed detail key, (b) sets error_ref to the
// request id so the caller can correlate with the operator log, and (c) emits
// ONE log record joining that ref with the FULL pre-redaction cause — so the
// caller sees a stable diagnostic while the operator keeps everything. The
// non-client-facing internalCauseKey channel is stripped from the body AND
// folded into that log record unconditionally at ANY status (E67.31 / #2637).
// 4xx responses are byte-identical to pre-#2587: no allow-list, no error_ref,
// details verbatim — so a 4xx cause is operator-visible in the log (keyed by
// the error_ref attribute) but the 4xx caller gets no correlation handle. The
// logger surfaces 5xx at LevelError and 4xx at LevelInfo so misuse vs. server
// faults are easy to filter.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]any) {
	ref := RequestIDFrom(r.Context())

	// Unconditionally separate the non-client-facing cause channel from the
	// client details — any status, ignoring the allow-list — so a cause handed
	// via internalCauseKey can never reach the client.
	cause, clientDetails := splitInternalCause(details)

	respDetails := clientDetails
	var bodyRef string
	if status >= 500 {
		// Default-deny: drop every detail key not on the allow-list, and hand
		// the caller a correlation handle in place of the redacted cause.
		respDetails = redactErrorDetails(clientDetails)
		bodyRef = ref
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := errorEnvelope{Error: errorBody{Code: code, Message: msg, Details: respDetails, ErrorRef: bodyRef}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode error response",
			slog.String("error", err.Error()),
		)
	}

	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("message", msg),
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("error_ref", ref),
	}
	if status >= 500 {
		// The caller's 5xx body is redacted, so the operator keeps the FULL
		// pre-redaction details here, joined to the same error_ref in THIS one
		// record — no split across two log lines. `details` stays 5xx-gated
		// while `cause` (below) does not: at 4xx the details already ship
		// verbatim in the body, so re-logging them is pure noise, whereas the
		// cause is stripped from the body at EVERY status and would otherwise
		// reach nobody at all (E67.31 / #2637).
		if len(clientDetails) > 0 {
			attrs = append(attrs, slog.Any("details", clientDetails))
		}
	}
	// Unconditional: an internalCauseKey value is stripped from the body at any
	// status, so the log record is its ONLY destination. The cause != "" guard
	// keeps the common-path record byte-identical — a call with no cause (or an
	// empty one) emits no `cause` attribute at all.
	if cause != "" {
		attrs = append(attrs, slog.String("cause", cause))
	}
	s.cfg.Logger.LogAttrs(r.Context(), level, "http error response", attrs...)
}

// writeJSON renders v as the response body with the given status.
// Centralized so every handler uses the same content-type and
// JSON-encoder configuration.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode response",
			slog.String("error", err.Error()),
		)
	}
}
