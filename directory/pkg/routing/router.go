package routing

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

// Query parameters a routed request must carry to identify its account.
// Both are REQUIRED: the directory resolves a region from account identity
// and nothing else, so a missing one is refused rather than defaulted.
// Guessing a provider would route an account's traffic on a coin flip.
const (
	QueryProvider   = "provider"
	QueryAccountKey = "account_key"
)

// AssignPath is the operator-gated region-assignment endpoint.
const AssignPath = "/v0/directory/assign"

// RegionStore is the persistence the router needs.
//
// It is an interface, not the concrete internal store type, for two
// reasons: the router's own tests need no database, and an out-of-module
// caller (the cell's cross-boundary test) can drive the REAL router without
// importing directory/internal/store, which Go's internal-package rule
// forbids across the module boundary.
type RegionStore interface {
	// AssignRegion assigns region if the account has none and returns the
	// region that actually owns the account — the first assignment ever
	// made, not necessarily the proposal.
	AssignRegion(ctx context.Context, provider, accountKey, region string) (string, error)
	// Lookup returns the owning region, or an error wrapping
	// store.ErrNotFound when the account has no assignment.
	Lookup(ctx context.Context, provider, accountKey string) (string, error)
}

// Router serves the directory's two surfaces. It implements http.Handler.
type Router struct {
	cfg   Config
	store RegionStore
	mux   *http.ServeMux

	// Now and NewNonce are injectable so tests can pin the expiry stamp
	// and the nonce. Both are set to real implementations by New.
	Now      func() time.Time
	NewNonce func() (string, error)
}

// New returns a Router over cfg and s. cfg is validated here too, so an
// invalid Config can never reach a live listener even if the caller built
// it by hand rather than through LoadConfig.
func New(cfg Config, s RegionStore) (*Router, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("%w: region store is nil", ErrInvalidConfig)
	}

	rt := &Router{
		cfg:      cfg,
		store:    s,
		mux:      http.NewServeMux(),
		Now:      time.Now,
		NewNonce: handoff.NewNonce,
	}

	rt.mux.HandleFunc("POST "+AssignPath, rt.handleAssign)
	for _, p := range cfg.RoutedPaths {
		rt.mux.HandleFunc("GET "+p, rt.handleRouted)
	}
	return rt, nil
}

// NewPostgres is New over the directory's own store, built from pool.
//
// It exists so a caller outside this module can stand up the real router
// against a real directory database: internal/store is unimportable across
// the module boundary, but a public constructor that USES it is not.
func NewPostgres(cfg Config, pool *pgxpool.Pool) (*Router, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: database pool is nil", ErrInvalidConfig)
	}
	return New(cfg, store.New(pool))
}

// MigrateSchema applies the directory's embedded migrations to
// databaseURL. Exported alongside NewPostgres for the same reason: an
// out-of-module integration test needs to bootstrap a real directory
// database, and the migration source is module-internal.
func MigrateSchema(databaseURL string) error { return store.MigrateUp(databaseURL) }

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

// assignRequest is the body of POST /v0/directory/assign.
type assignRequest struct {
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	Region     string `json:"region"`
}

// assignResponse reports the region that OWNS the account afterwards.
// Assigned is false when another writer had already claimed the account,
// in which case HomeRegion differs from the proposal — a normal outcome,
// not an error.
type assignResponse struct {
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	HomeRegion string `json:"home_region"`
	Assigned   bool   `json:"assigned"`
}

func (rt *Router) handleAssign(w http.ResponseWriter, r *http.Request) {
	if !rt.authorize(w, r) {
		return
	}

	var req assignRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("body is not a valid assign request: %v", err))
		return
	}
	if req.Provider == "" || req.AccountKey == "" || req.Region == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "provider, account_key and region are all required")
		return
	}
	if _, ok := rt.cfg.CellURL(req.Region); !ok {
		writeError(w, http.StatusBadRequest, "unknown_region",
			fmt.Sprintf("region %q is not configured in %s", req.Region, EnvRegions))
		return
	}

	owner, err := rt.store.AssignRegion(r.Context(), req.Provider, req.AccountKey, req.Region)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "assign_failed", "could not assign a region")
		return
	}

	writeJSON(w, http.StatusOK, assignResponse{
		Provider:   req.Provider,
		AccountKey: req.AccountKey,
		HomeRegion: owner,
		Assigned:   owner == req.Region,
	})
}

// handleRouted answers a routed cell path with a 302 to the cell that owns
// the caller's account, preserving the original path and the caller's whole
// query string and appending the signed handoff.
func (rt *Router) handleRouted(w http.ResponseWriter, r *http.Request) {
	if !rt.authorize(w, r) {
		return
	}

	q := r.URL.Query()
	provider, accountKey := q.Get(QueryProvider), q.Get(QueryAccountKey)
	if provider == "" || accountKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("%s and %s query parameters are required", QueryProvider, QueryAccountKey))
		return
	}

	region, err := rt.store.Lookup(r.Context(), provider, accountKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "account_not_assigned",
				fmt.Sprintf("no region is assigned to %s/%s", provider, accountKey))
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup_failed", "could not resolve the account's region")
		return
	}

	cellURL, ok := rt.cfg.CellURL(region)
	if !ok {
		// The account is pinned to a region this directory has no cell URL
		// for. That is a configuration fault, and the only safe answer is
		// to say so: routing to any other cell would send the account's
		// traffic into a region that does not own it.
		writeError(w, http.StatusInternalServerError, "region_not_configured",
			fmt.Sprintf("account is homed in region %q, which is absent from %s", region, EnvRegions))
		return
	}

	nonce, err := rt.NewNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "nonce_failed", "could not mint a handoff nonce")
		return
	}
	signed, err := handoff.Sign(rt.cfg.HandoffSecret, handoff.Params{
		Provider:   provider,
		AccountKey: accountKey,
		HomeRegion: region,
		ExpiresAt:  rt.Now().Add(rt.cfg.HandoffTTL),
		Nonce:      nonce,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign_failed", "could not sign the handoff")
		return
	}

	target, err := redirectTarget(cellURL, r.URL, signed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "region_not_configured", err.Error())
		return
	}

	w.Header().Set("Location", target)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

// redirectTarget builds `<cell base><original path>?<original query + fh_*>`.
//
// The caller's own parameters — including an OAuth `state`, which is a
// wholly different role from the handoff's replay nonce — survive
// untouched. Any INBOUND fh_* parameter is dropped first: it can only be an
// attempt to smuggle an attacker-chosen handoff through the router, and
// leaving it in place would emit two values for one parameter.
func redirectTarget(cellURL string, orig *url.URL, signed url.Values) (string, error) {
	base, err := url.Parse(cellURL)
	if err != nil {
		return "", fmt.Errorf("cell URL %q is unparsable: %w", cellURL, err)
	}

	q := orig.Query()
	for name := range q {
		if strings.HasPrefix(name, "fh_") {
			q.Del(name)
		}
	}
	for name, values := range signed {
		q[name] = values
	}

	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + orig.Path
	target.RawPath = ""
	target.RawQuery = q.Encode()
	return target.String(), nil
}

// authorize gates BOTH directory surfaces on the operator credential
// (ADR-062 A2.5). An UNSET credential refuses every request with 503: it
// means "this directory is not configured to serve", never "serve openly".
func (rt *Router) authorize(w http.ResponseWriter, r *http.Request) bool {
	if rt.cfg.AdminToken == "" {
		writeError(w, http.StatusServiceUnavailable, "credential_unconfigured",
			fmt.Sprintf("%s is unset; the directory refuses every request until it is configured", EnvAdminToken))
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if subtle.ConstantTimeCompare([]byte(presented), []byte(rt.cfg.AdminToken)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid operator bearer credential is required")
		return false
	}
	return true
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
