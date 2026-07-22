package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

// RoutedOnboardingPath is the ONE cell surface the directory routes today
// (ADR-062, E44.7 / #1831). It carries explicit account identity in its query,
// which is the only thing the directory can resolve a region from.
//
// The OAuth login/callback pair is deliberately NOT routed: a callback arrives
// from the forge with code+state and no account parameter, and the cell mints
// that state only AFTER the redirect, so the directory cannot pre-register a
// correlation either. Routing those surfaces needs a correlation design that
// does not exist yet; guessing an account there would route a caller's traffic
// on a coin flip. Deferred — see docs/deploy/regional-cells.md.
const RoutedOnboardingPath = "/v0/onboarding/start"

// handoffCtxKey carries the verified handoff into the wrapped handler. It is
// its own unexported type so no other package (and no other key) can collide.
type handoffCtxKey struct{}

// handoffFrom returns the handoff verified by withRegionPin for this request,
// and whether one was present at all.
func handoffFrom(ctx context.Context) (handoff.Params, bool) {
	p, ok := ctx.Value(handoffCtxKey{}).(handoff.Params)
	return p, ok
}

// withRegionPin verifies a directory handoff and stamps the account's home
// region before delegating to next.
//
// It is mounted on the routed surface ITSELF — the exact path the directory
// redirects to — rather than on a bespoke endpoint, so the redirect target and
// the verifier can never drift apart (ADR-062 A2.1).
//
// Three postures, and the middleware is mounted in all of them:
//
//   - NO fh_* parameters: pass through untouched. A single-cell deployment
//     never sees a handoff and must behave exactly as it did before.
//   - fh_sig present, pin surface DISABLED (no cell region or no shared
//     secret): REFUSE with 503. This is the fail-closed posture — a cell that
//     cannot verify a residency claim must not serve the routed surface as
//     though the claim were absent. Not mounting the middleware at all would
//     silently bypass instead.
//   - fh_sig present and the surface enabled: verify, pin, then delegate. A
//     handoff that fails any check is refused, never passed through unpinned.
func (s *Server) withRegionPin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !handoff.Has(q) {
			next(w, r)
			return
		}

		if s.cfg.HandoffSecret == "" || !s.cfg.RegionPinner.Enabled() {
			s.writeError(w, r, http.StatusServiceUnavailable, "region_pin_disabled",
				"this cell is not configured for regional handoffs (FISHHAWKD_HOME_REGION and FISHHAWKD_HANDOFF_SECRET must both be set)",
				nil)
			return
		}

		params, err := handoff.Verify(s.cfg.HandoffSecret, q, s.handoffNow())
		if err != nil {
			status, code := handoffErrorStatus(err)
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "region handoff rejected",
				slog.String("path", r.URL.Path),
				slog.String("code", code),
				slog.String("error", err.Error()),
			)
			s.writeError(w, r, status, code, "the region handoff could not be verified", nil)
			return
		}

		if err := s.cfg.RegionPinner.Pin(r.Context(), params.Provider, params.AccountKey, params.HomeRegion); err != nil {
			status, code := pinErrorStatus(err)
			// The typed refusals (409/404/400) describe the CALLER's request
			// and are safe to echo. An unclassified fault is a server-side
			// error whose text can carry driver, query or host detail, and
			// this surface answers before any auth decision — the handoff is
			// itself the credential — so it is genericized, matching the
			// directory router's own posture (TestAssignStoreFailureIsInternalError).
			message := err.Error()
			if status == http.StatusInternalServerError {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError, "region pin failed",
					slog.String("provider", params.Provider),
					slog.String("account_key", params.AccountKey),
					slog.String("error", err.Error()),
				)
				message = "the account's home region could not be recorded"
			}
			s.writeError(w, r, status, code, message, map[string]any{
				"provider":    params.Provider,
				"account_key": params.AccountKey,
				"region":      params.HomeRegion,
			})
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), handoffCtxKey{}, params)))
	}
}

// handoffNow is the clock handoff expiry is judged against. It reuses the
// server's injectable nowFunc so a test can drive the expired branch without
// sleeping, and tolerates a hand-built Server (nowFunc unset) rather than
// panicking on a nil call.
func (s *Server) handoffNow() time.Time {
	if s.nowFunc == nil {
		return time.Now()
	}
	return s.nowFunc()
}

// handoffErrorStatus maps a codec error onto a status. A missing or malformed
// parameter is the caller's request being wrong (400); a bad signature or an
// expired handoff is a credential being refused (403).
func handoffErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, handoff.ErrBadSignature):
		return http.StatusForbidden, "handoff_signature_invalid"
	case errors.Is(err, handoff.ErrExpired):
		return http.StatusForbidden, "handoff_expired"
	case errors.Is(err, handoff.ErrMissingParam), errors.Is(err, handoff.ErrMalformed):
		return http.StatusBadRequest, "handoff_malformed"
	case errors.Is(err, handoff.ErrNoSecret):
		// Unreachable via the guard above; mapped anyway so a future caller
		// cannot turn a configuration fault into a 500.
		return http.StatusServiceUnavailable, "region_pin_disabled"
	default:
		return http.StatusForbidden, "handoff_invalid"
	}
}

// pinErrorStatus maps a pin refusal onto a status. The two residency
// refusals — a handoff for another region, and an account already homed
// elsewhere — are both 409: the request is well-formed and authentic, it just
// conflicts with an assignment that already stands.
func pinErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, account.ErrRegionDisabled):
		return http.StatusServiceUnavailable, "region_pin_disabled"
	case errors.Is(err, account.ErrRegionMismatch):
		return http.StatusConflict, "region_mismatch"
	case errors.Is(err, account.ErrAlreadyPinned):
		return http.StatusConflict, "region_conflict"
	case errors.Is(err, account.ErrUnknownAccount):
		return http.StatusNotFound, "account_not_found"
	case errors.Is(err, account.ErrInvalidPin):
		return http.StatusBadRequest, "validation_failed"
	default:
		return http.StatusInternalServerError, "region_pin_failed"
	}
}

// onboardingStartResponse is the body of GET /v0/onboarding/start.
type onboardingStartResponse struct {
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	// CellRegion is the region THIS cell serves, or "" on a single-cell
	// deployment with no configured region.
	CellRegion string `json:"cell_region"`
	// HomeRegion is the account's recorded home region. Equal to CellRegion
	// once a handoff has been honored.
	HomeRegion string `json:"home_region"`
	// Pinned reports whether this request carried a verified handoff that
	// stamped the account. False on a pass-through (single-cell) request.
	Pinned bool `json:"pinned"`
	// ReadinessPath is where the caller continues the onboarding walk.
	ReadinessPath string `json:"readiness_path"`
}

// handleOnboardingStart implements GET /v0/onboarding/start — the cell entry
// point the directory redirects a caller to.
//
// Authorization mirrors the two ways a caller can legitimately arrive:
// carrying a handoff the middleware already verified (an HMAC over the
// account identity is itself the credential), or as an authenticated operator
// on a single-cell deployment. An anonymous caller with no handoff is refused
// 401 — the response names an account's region, which is not public.
func (s *Server) handleOnboardingStart(w http.ResponseWriter, r *http.Request) {
	params, viaHandoff := handoffFrom(r.Context())

	provider, accountKey := params.Provider, params.AccountKey
	if !viaHandoff {
		if IdentityFrom(r.Context()).IsAnonymous() {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"an authenticated token or session is required without a verified region handoff", nil)
			return
		}
		provider = r.URL.Query().Get("provider")
		accountKey = r.URL.Query().Get("account_key")
		if provider == "" || accountKey == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"provider and account_key query parameters are required", nil)
			return
		}
	}

	resp := onboardingStartResponse{
		Provider:      provider,
		AccountKey:    accountKey,
		CellRegion:    s.cfg.RegionPinner.CellRegion(),
		Pinned:        viaHandoff,
		ReadinessPath: "/v0/onboarding/readiness",
	}
	if viaHandoff {
		resp.HomeRegion = params.HomeRegion
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}
