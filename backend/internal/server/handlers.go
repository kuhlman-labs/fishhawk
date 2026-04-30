package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// registerRoutes wires every endpoint onto mux. Method-aware patterns
// require Go 1.22+ ServeMux. Add new routes here as the API surface
// grows under E3.6 (#46); for v0 the surface is just liveness.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealth)
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
