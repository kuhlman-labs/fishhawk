package server

import (
	"encoding/json"
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
}

// writeError renders an error envelope to w with the given status.
// The logger surfaces 5xx errors at LevelError and 4xx at LevelInfo
// so misuse vs. server faults are easy to filter in production.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := errorEnvelope{Error: errorBody{Code: code, Message: msg, Details: details}}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode error response",
			slog.String("error", err.Error()),
		)
	}

	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	s.cfg.Logger.LogAttrs(r.Context(), level, "http error response",
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("message", msg),
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
	)
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
