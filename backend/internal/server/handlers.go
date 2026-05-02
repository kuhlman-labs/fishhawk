package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// registerRoutes wires every endpoint onto mux. Method-aware patterns
// require Go 1.22+ ServeMux. Add new routes here as handlers land
// per docs/api/v0.openapi.yaml.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v0/runs", s.handleCreateRun)
	mux.HandleFunc("GET /v0/runs/{run_id}", s.handleGetRun)
	mux.HandleFunc("POST /v0/runs/{run_id}/signing-key", s.handleIssueSigningKey)
	mux.HandleFunc("POST /v0/runs/{run_id}/trace", s.handleShipTrace)
	mux.HandleFunc("POST /webhooks/github", s.handleWebhook)
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// handleHealth answers liveness probes with a small JSON payload that
// also exposes the running version. Operators rely on the version
// field to confirm a deploy reached this instance.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:  "ok",
		Version: version.Version,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "encode health response",
			slog.String("error", err.Error()),
		)
	}
}
