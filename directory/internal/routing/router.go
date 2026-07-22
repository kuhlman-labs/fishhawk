package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

// Routed paths. Every one is GET-only by construction: the directory
// answers with a 302 and never reads a request body, so the classic
// "302 rewrites POST to GET" hazard cannot arise.
const (
	PathOnboardingStart = "/v0/onboarding/start"
	PathInstallCallback = "/v0/install/callback"
	PathLogin           = "/v0/login"
	PathHealthz         = "/healthz"
)

// Assignment is a recorded (provider, account_key) → home_region row.
// There is deliberately NO cell_base_url column: region → cell resolves
// exclusively from Config.
type Assignment struct {
	Provider   string
	AccountKey string
	HomeRegion string
}

// InstallState is a single-use nonce minted when onboarding starts and
// consumed when the forge's App-install callback comes back, binding the
// callback to the account the directory already assigned.
type InstallState struct {
	Nonce      string
	Provider   string
	AccountKey string
	HomeRegion string
	ExpiresAt  time.Time
}

// Store is the persistence the router needs. store.Store implements it.
type Store interface {
	// AssignRegion records provider/accountKey → region first-write-wins
	// and returns the EFFECTIVE region: an existing row is never moved.
	AssignRegion(ctx context.Context, provider, accountKey, region string) (Assignment, error)
	// LookupRegion returns the recorded assignment, or an error wrapping
	// ErrNotFound when the account has never been assigned.
	LookupRegion(ctx context.Context, provider, accountKey string) (Assignment, error)
	// PutInstallState records a freshly minted single-use nonce.
	PutInstallState(ctx context.Context, st InstallState) error
	// ConsumeInstallState atomically deletes and returns a nonce,
	// erroring with ErrNotFound (unknown or already consumed) or
	// ErrExpired.
	ConsumeInstallState(ctx context.Context, nonce string) (InstallState, error)
}

// ErrNotFound and ErrExpired are the store-facing sentinels the router
// maps onto fail-closed HTTP responses. The store package returns errors
// that wrap these.
var (
	// ErrNotFound means no such row (unassigned account, or an unknown /
	// already-consumed nonce).
	ErrNotFound = errors.New("routing: not found")
	// ErrExpired means the install-state nonce is past its lifetime.
	ErrExpired = errors.New("routing: expired")
)

// Router serves the directory's routing surface.
type Router struct {
	store Store
	cfg   Config
	log   *slog.Logger

	// now and newNonce are injectable so tests get deterministic pins.
	now      func() time.Time
	newNonce func() (string, error)
}

// Option customizes a Router.
type Option func(*Router)

// WithClock overrides the wall clock (tests).
func WithClock(now func() time.Time) Option {
	return func(r *Router) { r.now = now }
}

// WithNonceSource overrides nonce generation (tests).
func WithNonceSource(gen func() (string, error)) Option {
	return func(r *Router) { r.newNonce = gen }
}

// WithLogger overrides the logger.
func WithLogger(l *slog.Logger) Option {
	return func(r *Router) { r.log = l }
}

// New builds a Router over the given store and configuration.
func New(store Store, cfg Config, opts ...Option) *Router {
	r := &Router{
		store:    store,
		cfg:      cfg,
		log:      slog.Default(),
		now:      time.Now,
		newNonce: randomNonce,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Handler mounts the routing surface. All routed paths are GET-only.
func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealthz, r.handleHealthz)
	mux.HandleFunc("GET "+PathOnboardingStart, r.handleOnboardingStart)
	mux.HandleFunc("GET "+PathInstallCallback, r.handleInstallCallback)
	mux.HandleFunc("GET "+PathLogin, r.handleLogin)
	return mux
}

func (r *Router) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	regions := strings.Join(r.cfg.SupportedRegions, ",")
	_, _ = fmt.Fprintf(w, `{"status":"ok","supported_regions":%q}`+"\n", regions)
}

// handleOnboardingStart assigns a region from EXPLICIT input validated
// against the supported-region list, records the (provider, account_key)
// → home_region row, mints a single-use install-state nonce, and only
// then redirects into the resolved cell.
//
// Region DISCOVERY (e.g. reading an enterprise's GHEC data-residency
// region) is deliberately out of scope: the region arrives as explicit
// input here.
func (r *Router) handleOnboardingStart(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	provider, accountKey, ok := r.requireAccount(w, q)
	if !ok {
		return
	}
	region := normalizeRegion(q.Get("region"))
	if region == "" {
		r.fail(w, http.StatusBadRequest, "region is required; supported: "+strings.Join(r.cfg.SupportedRegions, ","))
		return
	}
	if !r.cfg.Supports(region) {
		// Fail closed BEFORE any write: an unsupported region must never
		// be recorded.
		r.fail(w, http.StatusBadRequest, fmt.Sprintf("region %q is not supported; supported: %s", region, strings.Join(r.cfg.SupportedRegions, ",")))
		return
	}

	assigned, err := r.store.AssignRegion(req.Context(), provider, accountKey, region)
	if err != nil {
		r.serverError(w, "record region assignment", err)
		return
	}

	nonce, err := r.newNonce()
	if err != nil {
		r.serverError(w, "mint install state", err)
		return
	}
	st := InstallState{
		Nonce:      nonce,
		Provider:   assigned.Provider,
		AccountKey: assigned.AccountKey,
		HomeRegion: assigned.HomeRegion,
		ExpiresAt:  r.now().Add(r.cfg.HandoffTTL),
	}
	if err := r.store.PutInstallState(req.Context(), st); err != nil {
		r.serverError(w, "record install state", err)
		return
	}

	r.redirectToCell(w, req, assigned)
}

// handleInstallCallback consumes the single-use install-state nonce the
// forge hands back as `state` and routes to the account it was minted
// for. An absent, unknown, already-consumed, or expired nonce fails
// closed — the directory never guesses a cell.
func (r *Router) handleInstallCallback(w http.ResponseWriter, req *http.Request) {
	nonce := strings.TrimSpace(req.URL.Query().Get("state"))
	if nonce == "" {
		r.fail(w, http.StatusBadRequest, "state is required")
		return
	}
	st, err := r.store.ConsumeInstallState(req.Context(), nonce)
	switch {
	case errors.Is(err, ErrNotFound):
		r.fail(w, http.StatusForbidden, "unknown or already-consumed install state")
		return
	case errors.Is(err, ErrExpired):
		r.fail(w, http.StatusForbidden, "install state expired")
		return
	case err != nil:
		r.serverError(w, "consume install state", err)
		return
	}
	r.redirectToCell(w, req, Assignment{
		Provider:   st.Provider,
		AccountKey: st.AccountKey,
		HomeRegion: st.HomeRegion,
	})
}

// handleLogin routes an already-onboarded account to its home cell. An
// account with no recorded region fails closed with 404 — the directory
// does not assign a region on the login path.
func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	provider, accountKey, ok := r.requireAccount(w, req.URL.Query())
	if !ok {
		return
	}
	assigned, err := r.store.LookupRegion(req.Context(), provider, accountKey)
	switch {
	case errors.Is(err, ErrNotFound):
		r.fail(w, http.StatusNotFound, fmt.Sprintf("no home region recorded for %s/%s; onboard first", provider, accountKey))
		return
	case err != nil:
		r.serverError(w, "look up home region", err)
		return
	}
	r.redirectToCell(w, req, assigned)
}

// redirectToCell emits the 302 into the resolved cell.
//
// The Location PRESERVES the request: the cell base URL is joined with
// the ORIGINAL request path and the full original query string (code,
// state, installation_id and friends all survive), with the signed
// handoff parameters appended. Nothing is dropped and nothing is proxied.
func (r *Router) redirectToCell(w http.ResponseWriter, req *http.Request, a Assignment) {
	base, err := r.cfg.Resolve(a.HomeRegion)
	if err != nil {
		// Fail closed: an account whose recorded region has no configured
		// cell gets an explicit error, NEVER a fall-through to a
		// different region's cell.
		r.log.Error("directory: unroutable home region",
			"provider", a.Provider, "account_key", a.AccountKey, "home_region", a.HomeRegion)
		r.fail(w, http.StatusServiceUnavailable,
			fmt.Sprintf("no cell configured for home region %q", a.HomeRegion))
		return
	}

	nonce, err := r.newNonce()
	if err != nil {
		r.serverError(w, "mint handoff nonce", err)
		return
	}
	pin, err := handoff.Sign(handoff.Params{
		Provider:   a.Provider,
		AccountKey: a.AccountKey,
		HomeRegion: a.HomeRegion,
		ExpiresAt:  r.now().Add(r.cfg.HandoffTTL),
		Nonce:      nonce,
	}, r.cfg.HandoffSecret)
	if err != nil {
		r.serverError(w, "sign region handoff", err)
		return
	}

	location, err := buildLocation(base, req.URL, pin)
	if err != nil {
		r.serverError(w, "build redirect location", err)
		return
	}
	w.Header().Set("Location", location)
	// 302 Found (RFC 9110 §15.4.3): the browser re-issues the request at
	// the cell, so no request body ever transits the global plane.
	w.WriteHeader(http.StatusFound)
}

// buildLocation joins the cell base URL with the original request path
// and query, then appends the signed handoff parameters.
func buildLocation(base string, orig *url.URL, pin url.Values) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse cell base url %q: %w", base, err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + orig.EscapedPath()

	q := orig.Query()
	for k, vals := range pin {
		// Overwrite rather than append: a caller-supplied fh_* parameter
		// must never survive alongside the directory's own.
		q.Del(k)
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// requireAccount pulls and validates the account identity parameters.
func (r *Router) requireAccount(w http.ResponseWriter, q url.Values) (provider, accountKey string, ok bool) {
	provider = strings.ToLower(strings.TrimSpace(q.Get("provider")))
	accountKey = strings.TrimSpace(q.Get("account_key"))
	if provider == "" {
		r.fail(w, http.StatusBadRequest, "provider is required")
		return "", "", false
	}
	if accountKey == "" {
		r.fail(w, http.StatusBadRequest, "account_key is required")
		return "", "", false
	}
	return provider, accountKey, true
}

func (*Router) fail(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

func (r *Router) serverError(w http.ResponseWriter, what string, err error) {
	r.log.Error("directory: "+what+" failed", "error", err)
	http.Error(w, what+" failed", http.StatusInternalServerError)
}

// randomNonce returns a 128-bit hex nonce.
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
