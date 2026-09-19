package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Dev mode (E72.13 / #3500).
//
// A fishhawkd with EITHER dev-only surface mounted — Config.DevFixtures
// (fishhawkd --dev-fixtures / FISHHAWKD_DEV_FIXTURES=1, E72.2) or
// Config.DevStubForge (--dev-stub-forge / FISHHAWKD_DEV_STUB_FORGE=1, E72.3) —
// is in DEV MODE. There is deliberately no separate flag or env var: dev mode
// IS the presence of a dev surface, which is exactly what `scripts/dev
// preview` mounts and what a production deployment never sets. #3500 showed
// the acceptance agent host-dispatching stages on the preview daemon, whose
// spawned runners pushed branches and opened pull requests on the REAL forge
// with the operator's credentials. The controls below are identity-
// independent by design: the escape did not depend on which credential the
// agent held, so the refusal must hold for an anonymous caller, an fhm_
// token and a write:runs operator token identically.
//
// Three observable consequences of dev mode, each additive and absent on a
// production daemon so its wire surfaces stay byte-identical:
//
//   - POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch refuses EVERY
//     caller with 403 host_dispatch_refused_dev_mode and appends a
//     host_dispatch_refused audit row (refuseHostDispatchDevMode). Every MCP
//     host-spawn verb calls that marker fail-closed before cmd.Start, so no
//     runner is spawned.
//   - /healthz advertises dev_mode: true (handleHealth).
//   - Prompt responses carry forge_writes: "deny" (forgeWritesPolicy), which
//     the runner's pre-spawn forge-writes gate honours.

// devSurfaceFixtures / devSurfaceStubForge name the mounted dev surfaces in
// the host_dispatch_refused payload and the 403 details.
const (
	devSurfaceFixtures  = "dev_fixtures"
	devSurfaceStubForge = "dev_stub_forge"
)

// CategoryHostDispatchRefused is the audit category of the run-chain row the
// dev-mode host-dispatch refusal appends (E72.13 / #3500). Registered in
// audit.KnownCategories; the literal is kept here at its emit site (the
// acceptance_dispatched convention) so categories_completeness_test.go's AST
// sweep binds it.
const CategoryHostDispatchRefused = "host_dispatch_refused"

// hostDispatchRefusedReasonDevMode is the `reason` the refusal row and the
// 403 details carry.
const hostDispatchRefusedReasonDevMode = "dev_mode"

// forgeWritesDeny is the promptResponse.ForgeWrites value a dev-mode daemon
// stamps: the runner refuses the stage pre-spawn on it.
const forgeWritesDeny = "deny"

// devModeSurfaces returns the mounted dev-only surfaces in fixed order.
// Empty on a production daemon.
func (s *Server) devModeSurfaces() []string {
	var out []string
	if s.cfg.DevFixtures != nil {
		out = append(out, devSurfaceFixtures)
	}
	if s.cfg.DevStubForge != nil {
		out = append(out, devSurfaceStubForge)
	}
	return out
}

// devModeActive reports whether any dev-only surface is mounted.
func (s *Server) devModeActive() bool {
	return len(s.devModeSurfaces()) > 0
}

// forgeWritesPolicy is the promptResponse.ForgeWrites value: "deny" in dev
// mode, "" (omitted on the wire) otherwise.
func (s *Server) forgeWritesPolicy() string {
	if s.devModeActive() {
		return forgeWritesDeny
	}
	return ""
}

// refuseHostDispatchDevMode appends the host_dispatch_refused row on the run
// chain and answers 403 host_dispatch_refused_dev_mode. The row is
// best-effort (nil AuditRepo / append error → WARN, mirroring
// emitHostDispatchAcceptanceAnchor): the refusal never depends on it, so the
// 403 is written on every path. The stage row is never read, so its state is
// untouched.
func (s *Server) refuseHostDispatchDevMode(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID) {
	surfaces := s.devModeSurfaces()
	subject := IdentityFrom(r.Context()).Subject
	if subject == "" {
		subject = "anonymous"
	}
	s.appendHostDispatchRefused(r, runID, stageID, surfaces, subject)
	s.writeError(w, r, http.StatusForbidden, "host_dispatch_refused_dev_mode",
		"this fishhawkd is running in dev mode (a dev-only surface is mounted); a preview/dev-mode daemon never marks a host spawn, for any caller",
		map[string]any{
			"reason":       hostDispatchRefusedReasonDevMode,
			"dev_surfaces": surfaces,
		})
}

func (s *Server) appendHostDispatchRefused(r *http.Request, runID, stageID uuid.UUID, surfaces []string, subject string) {
	ctx := r.Context()
	if s.cfg.AuditRepo == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: AuditRepo not configured; skipping host_dispatch_refused row",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"reason":       hostDispatchRefusedReasonDevMode,
		"dev_surfaces": surfaces,
		"stage_id":     stageID.String(),
		"subject":      subject,
		"source":       hostDispatchAnchorSource,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: marshal host_dispatch_refused payload failed; row not written",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	sid := stageID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  CategoryHostDispatchRefused,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: append host_dispatch_refused row failed; the refusal still stands",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}
