package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// Dev-only seeded-fixture surface (E72.2 / #3326, folding in #1874).
//
// Three routes — GET /v0/dev/fixtures, POST /v0/dev/fixtures and
// POST /v0/dev/sign — exist ONLY when Config.DevFixtures is non-nil
// (fishhawkd --dev-fixtures / FISHHAWKD_DEV_FIXTURES=1 with a database).
// A nil applier leaves the surface ABSENT: the mux never learns the
// paths, so every request answers the router's 404 — there is no
// "disabled" 503, because a production deployment must not advertise
// that a fixture-minting route could exist.
//
// Every route is wrapped by devLoopbackOnly: a peer that is not a
// loopback address is refused 403 dev_surface_loopback_only. The
// acceptance sandbox reaches the preview through the runner's egress
// proxy, which binds 127.0.0.1 and dials the allow-listed upstream
// itself, so the preview sees a loopback peer (runner/internal/
// egressproxy). An unparseable RemoteAddr refuses too — fail closed.
//
// CSRF: no csrfExemptPath entry is needed. The csrf middleware passes
// any identity with no SessionID (csrf.go), and these routes are called
// credential-less (no cookie, no bearer) by the acceptance agent, so the
// anonymous identity bypasses the token check. devfixtures_test.go drives
// New(cfg)'s real middleware chain and would 403 if that reading were
// wrong.

// DevFixtureApplier is the slice of *devfixtures.Applier the dev routes
// consume. Kept as an interface so the handler tests can drive every
// error branch with a fake and no database.
type DevFixtureApplier interface {
	// Names returns the catalog's sorted scenario names.
	Names() []string
	// Describe returns the one-line description of name ("" when unknown).
	Describe(name string) string
	// Apply materializes name and returns the fresh ids it minted. An
	// unknown name wraps devfixtures.ErrUnknownScenario.
	Apply(ctx context.Context, name string) (devfixtures.Result, error)
}

// devSignPrivateKeyHeader carries the base64 of the 64-byte Ed25519
// private key POST /v0/runs/{id}/signing-key returned as `private_key`.
// It is read once and never logged: the logging middleware records
// method/path/status only, and the handler below never slogs it.
const devSignPrivateKeyHeader = "X-Fishhawk-Dev-Private-Key"

// devSignMaxBodyBytes bounds the body POST /v0/dev/sign hashes. A raw
// trace bundle is capped at maxTraceBundleBytes (64 MiB) on upload, so
// 32 MiB covers every bundle the acceptance agent could reasonably sign
// while keeping the dev route from being a memory sink.
const devSignMaxBodyBytes = 32 * 1024 * 1024

// devFixturesMaxBodyBytes bounds the body POST /v0/dev/fixtures decodes,
// applied via http.MaxBytesReader BEFORE the decoder reads (the
// stage_progress / refinement pattern). The expected body is a one-field
// object, so 4 KiB is generous; a body past it fails the decode and keeps
// the existing 400 validation_failed shape. Dev-only and loopback-only
// already, so this is defence in depth matching the sibling
// devSignMaxBodyBytes rather than a reachable-defect fix.
const devFixturesMaxBodyBytes = 4 << 10

// devFixturesRequest is the POST /v0/dev/fixtures body.
type devFixturesRequest struct {
	Scenario string `json:"scenario"`
}

// devFixturesListResponse is the GET /v0/dev/fixtures body.
type devFixturesListResponse struct {
	Scenarios []devFixtureScenario `json:"scenarios"`
}

// devFixtureScenario is one catalog entry as GET /v0/dev/fixtures lists it.
type devFixtureScenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// devSignResponse is the POST /v0/dev/sign body.
type devSignResponse struct {
	Signature string `json:"signature"`
}

// devLoopbackOnly refuses any request whose peer is not a loopback
// address. RemoteAddr is host:port as net/http sets it; a value that
// does not split or parse is refused rather than admitted, so a proxy
// or test harness that leaves the field malformed cannot slip past.
func (s *Server) devLoopbackOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			s.writeError(w, r, http.StatusForbidden, "dev_surface_loopback_only",
				"the dev fixtures surface accepts loopback peers only",
				map[string]any{"remote_addr": r.RemoteAddr})
			return
		}
		next(w, r)
	}
}

// handleDevListFixtures renders the catalog in Names() order with each
// scenario's description, so the acceptance agent can discover the
// closed set without a repository read.
func (s *Server) handleDevListFixtures(w http.ResponseWriter, r *http.Request) {
	names := s.cfg.DevFixtures.Names()
	out := devFixturesListResponse{Scenarios: make([]devFixtureScenario, 0, len(names))}
	for _, n := range names {
		out.Scenarios = append(out.Scenarios, devFixtureScenario{
			Name:        n,
			Description: s.cfg.DevFixtures.Describe(n),
		})
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

// handleDevApplyFixture materializes one named scenario and answers 201
// with the fresh ids. Every call mints NEW rows — the acceptance agent
// reads the returned ids rather than assuming stable ones.
func (s *Server) handleDevApplyFixture(w http.ResponseWriter, r *http.Request) {
	var req devFixturesRequest
	r.Body = http.MaxBytesReader(w, r.Body, devFixturesMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	res, err := s.cfg.DevFixtures.Apply(r.Context(), req.Scenario)
	if err != nil {
		if errors.Is(err, devfixtures.ErrUnknownScenario) {
			s.writeError(w, r, http.StatusNotFound, "fixture_scenario_unknown",
				"no fixture scenario with that name",
				map[string]any{"scenario": req.Scenario, "known": s.cfg.DevFixtures.Names()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"apply fixture scenario failed",
			map[string]any{"scenario": req.Scenario, internalCauseKey: err.Error()})
		return
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "dev fixture scenario applied",
		slog.String("scenario", res.Scenario),
		slog.Int("runs", len(res.Runs)))
	s.writeJSON(w, r, http.StatusCreated, res)
}

// handleDevSign signs the raw request body with the Ed25519 private key
// carried in X-Fishhawk-Dev-Private-Key and answers the hex signature
// POST /v0/runs/{id}/trace expects in X-Fishhawk-Signature. It touches
// no repository and never logs the key; it exists because the
// acceptance sandbox cannot be assumed to carry Ed25519 tooling.
func (s *Server) handleDevSign(w http.ResponseWriter, r *http.Request) {
	raw := r.Header.Get(devSignPrivateKeyHeader)
	if raw == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			devSignPrivateKeyHeader+" header is required",
			map[string]any{"field": devSignPrivateKeyHeader, "reason": "missing"})
		return
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			devSignPrivateKeyHeader+" is not valid base64",
			map[string]any{"field": devSignPrivateKeyHeader, "reason": "not_base64"})
		return
	}
	if len(key) != ed25519.PrivateKeySize {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			devSignPrivateKeyHeader+" must decode to a 64-byte Ed25519 private key",
			map[string]any{"field": devSignPrivateKeyHeader, "reason": "wrong_length",
				"got_bytes": len(key), "want_bytes": ed25519.PrivateKeySize})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, devSignMaxBodyBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > devSignMaxBodyBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"body exceeds the dev sign size cap",
			map[string]any{"limit_bytes": devSignMaxBodyBytes})
		return
	}

	sig := signing.Sign(ed25519.PrivateKey(key), signing.ComputeMessage(body))
	s.writeJSON(w, r, http.StatusOK, devSignResponse{Signature: hex.EncodeToString(sig)})
}
