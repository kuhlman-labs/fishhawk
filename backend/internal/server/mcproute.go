package server

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServerFactory builds a fully-registered MCP tool server for ONE request,
// bound to backendURL and apiToken.
//
// It is an INJECTED seam rather than a direct backend/internal/mcpserver
// import, and the reason is a hard constraint, not a style preference:
// mcpserver's in-package tests drive a real server.New (see
// backend/internal/mcpserver/campaign_test.go), so an import edge from this
// package to mcpserver would close an import cycle in mcpserver's TEST binary
// — `go build` stays green and `go test ./internal/mcpserver/...` fails. The
// arrow is therefore inverted: backend/cmd/fishhawkd, which already imports
// both, wires mcpserver.NewServer in. There is still exactly ONE tool-
// registration path; only the direction of the reference changed.
type MCPServerFactory func(backendURL, apiToken string) *mcp.Server

// mcpRouteMode is the verdict resolveMCPRouteState reaches for the /mcp
// route. It is computed ONCE at New() from the immutable Config and read
// by handleMCP on every request — see resolveMCPRouteState for why the
// verdict is not recomputed per request.
type mcpRouteMode int

const (
	// mcpRouteEnabled serves the MCP surface. The state's selfURL is the
	// loopback base URL the per-request tool client dials back on.
	mcpRouteEnabled mcpRouteMode = iota

	// mcpRouteOff is the operator's explicit opt-out (--mcp-route=off).
	// All three methods answer 404.
	mcpRouteOff

	// mcpRouteMisconfigured means the route CANNOT be served safely: the
	// listen address is unparseable, a hostname would not resolve, or an
	// embedder-supplied MCPSelfURL points off-loopback. 503 with the
	// diagnosis in reason.
	mcpRouteMisconfigured

	// mcpRouteNotLoopback means the listener is reachable off-host, so
	// serving the tool surface behind a bare bearer would expose it
	// beyond the operator's machine (ADR-033). 403.
	mcpRouteNotLoopback
)

// mcpRouteState is the resolved, immutable route verdict.
type mcpRouteState struct {
	mode mcpRouteMode

	// selfURL is the base URL the per-request mcpserver tool client
	// dials. Always built from a RESOLVED loopback IP literal, never a
	// hostname: a hostname stored here would be re-resolved by the HTTP
	// client at call time, so a DNS change after construction would send
	// every caller's raw bearer off-host — defeating the refusal this
	// route enforces. Empty unless mode is mcpRouteEnabled.
	selfURL string

	// listenHost is the host portion of Config.Addr, echoed in the
	// loopback refusal so an operator can see what was rejected.
	listenHost string

	// listenAddr is the host:port Start BINDS when the route is enabled,
	// with the host rewritten to the RESOLVED loopback IP for exactly the
	// reason selfURL is: classifying a hostname at New() while handing the
	// HOSTNAME to net.Listen leaves the bind to re-resolve it, so a DNS
	// record changed between construction and Start would put the whole
	// bare-bearer tool surface on an off-host interface that the route
	// still believes is loopback. Binding the pinned literal makes the
	// classified address and the bound address the same address by
	// construction. Empty unless mode is mcpRouteEnabled — every other
	// verdict leaves Config.Addr untouched, so a deployment that does not
	// serve the route binds exactly what it always did.
	listenAddr string

	// reason is the human-readable diagnosis carried by the 503.
	reason string

	// authenticatedOnly is true when the loopback refusal has been LIFTED
	// because an OAuth AS is enabled (ADR-076 slice 3, #2391). In this posture
	// the listener is intentionally reachable off-host, so handleMCP accepts
	// ONLY an audience-validated OAuth identity (Identity.OAuthAudience != "")
	// — a bare fhk_/fhm_ token is refused, keeping ADR-033's
	// no-bare-bearer-off-host invariant intact. False in every other posture,
	// including a normal loopback-served route.
	authenticatedOnly bool

	// challengeResource is the resource identifier the lift enforces, echoed so
	// verifyMCPListenerLoopback can fail closed if the lift was somehow
	// constructed with no audience to enforce. Populated only when
	// authenticatedOnly is true.
	challengeResource string
}

// normalizeMCPRoute maps Config.MCPRoute onto the on/off decision. The
// zero value normalizes to ON so a Config that never mentions the route
// still serves it; only an explicit "off" disables. Strict validation of
// the operator-supplied string lives in fishhawkd's resolveMCPRouteMode,
// which refuses to start on an unrecognized value — this function is the
// permissive last mile for an embedder-constructed Config.
func normalizeMCPRoute(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "off") {
		return "off"
	}
	return "on"
}

// mcpLoopbackHost decides whether host is a loopback address and, when it
// is, returns the resolved loopback IP to PIN into the self URL.
//
// An EMPTY host is deliberately NOT clamped to 127.0.0.1 and is reported
// non-loopback: net.Listen documents that an empty or unspecified host
// listens on every available unicast address, so fishhawkd has already
// bound all interfaces by the time this runs. That is the opposite of
// backend/cmd/fishhawk-mcp/http_transport.go's validateLoopbackAddr,
// which MAY clamp because it controls its own bind — fishhawkd cannot
// unbind, so treating ":8080" as loopback would serve the tool surface
// off-host behind a bare bearer.
//
// A literal IP is decided by net.IP.IsLoopback and pins itself. A
// hostname is resolved via lookupIP and must be loopback on EVERY
// resolved address; the first address is pinned. A resolution failure is
// reported as an error (503), never silently accepted.
func mcpLoopbackHost(host string, lookupIP func(string) ([]net.IP, error)) (string, bool, error) {
	if host == "" {
		return "", false, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return "", false, nil
		}
		return ip.String(), true, nil
	}
	if lookupIP == nil {
		lookupIP = net.LookupIP
	}
	addrs, err := lookupIP(host)
	if err != nil {
		return "", false, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", false, fmt.Errorf("resolve %q: no addresses", host)
	}
	for _, a := range addrs {
		if !a.IsLoopback() {
			return "", false, nil
		}
	}
	return addrs[0].String(), true, nil
}

// resolveMCPRouteState computes the /mcp route verdict from cfg.
//
// The ladder is ordered so every branch has a reaching input and the
// cheapest, most specific diagnosis wins:
//
//  1. explicit off                      -> mcpRouteOff (404)
//  2. no MCPServerFactory wired         -> mcpRouteMisconfigured (503)
//  3. unparseable Config.Addr           -> mcpRouteMisconfigured (503)
//  4. listener host not loopback        -> mcpRouteNotLoopback (403)
//     (or unresolvable                  -> mcpRouteMisconfigured)
//  5. MCPSelfURL set but not loopback   -> mcpRouteMisconfigured (503)
//  6. otherwise                         -> mcpRouteEnabled
//
// Step 3 deliberately precedes step 4: net.SplitHostPort's failure is a
// distinct, actionable diagnosis, and putting the loopback predicate
// first would leave the malformed-Addr branch unreachable.
//
// This runs ONCE, at New(). The verdict is a pure function of the
// immutable Config, and resolving per request would put a blocking DNS
// lookup on every /mcp call with a resolver outage intermittently
// flipping the route to 403.
//
// as is the ALREADY-RESOLVED OAuth AS verdict (server.go resolves it BEFORE
// the /mcp route). It drives the conditional loopback lift: a non-loopback
// listener is refused UNLESS the AS is enabled, in which case the route serves
// in authenticated-only mode (ADR-076 slice 3, #2391).
func resolveMCPRouteState(cfg Config, as oauthASState, lookupIP func(string) ([]net.IP, error)) mcpRouteState {
	if normalizeMCPRoute(cfg.MCPRoute) == "off" {
		return mcpRouteState{mode: mcpRouteOff}
	}

	// No tool registry, nothing to serve — and this is checked before the
	// address work because an unwired deployment cannot serve the route at
	// any address. Production wires it in serve.go; an embedder that mounts
	// this package without it gets a named diagnosis rather than a nil-
	// dereference or an empty tool list.
	if cfg.MCPServerFactory == nil {
		return mcpRouteState{
			mode:   mcpRouteMisconfigured,
			reason: "the MCP route has no tool-server factory wired (Config.MCPServerFactory is nil), so there is no tool registry to serve",
		}
	}

	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return mcpRouteState{
			mode:   mcpRouteMisconfigured,
			reason: fmt.Sprintf("the MCP route cannot resolve its own base URL: listen address %q is not a valid host:port (%v)", cfg.Addr, err),
		}
	}

	resolved, ok, err := mcpLoopbackHost(host, lookupIP)
	switch {
	case err != nil:
		return mcpRouteState{
			mode:       mcpRouteMisconfigured,
			listenHost: host,
			reason:     fmt.Sprintf("the MCP route cannot classify its listen address: %v", err),
		}
	case !ok:
		// The listener is reachable off-host. Refuse UNLESS an OAuth AS is
		// enabled — then LIFT the refusal and serve in authenticated-only mode,
		// so network exposure and auth tightening land together (ADR-076 slice
		// 3, #2391). Note --oauth-require-loopback drives as.mode to
		// oauthASNotLoopback on a public bind, so setting that flag keeps the
		// /mcp refusal too.
		if as.mode != oauthASEnabled {
			return mcpRouteState{mode: mcpRouteNotLoopback, listenHost: host}
		}
		selfURL, bindAddr, serr := liftedSelfURL(cfg, host, port, lookupIP)
		if serr != nil {
			return mcpRouteState{
				mode:       mcpRouteMisconfigured,
				listenHost: host,
				reason: fmt.Sprintf("the MCP route was lifted off loopback for OAuth but cannot resolve its own loopback dial-back base URL: %v; "+
					"serving would forward every caller's bearer off-host, so the route refuses", serr),
			}
		}
		// bindAddr PINS the listener to the SAME resolved literal the selfURL was
		// derived from whenever the bind names a specific host: net.Listen would
		// otherwise re-resolve a hostname in cfg.Addr at Start, and a DNS answer
		// that changed since New() would put the listener on a different address
		// than the tool client dials — forwarding every caller's raw bearer to a
		// host that is not the local listener (the token-exfiltration divergence
		// #2391 closes). For an empty/unspecified bind bindAddr is empty so
		// listenAddr() falls back to cfg.Addr verbatim: net.Listen binds every
		// interface (the operator's deliberate public bind) and the loopback
		// selfURL is reached over the loopback that all-interfaces bind includes,
		// so there is no divergence to pin shut.
		return mcpRouteState{
			mode:              mcpRouteEnabled,
			selfURL:           selfURL,
			listenAddr:        bindAddr,
			listenHost:        host,
			authenticatedOnly: true,
			challengeResource: as.resource.String(),
		}
	}

	// Derive BOTH the bind address and the self URL from the RESOLVED
	// loopback IP. net.JoinHostPort brackets an IPv6 literal, which naive
	// concatenation does not: "::1" + ":8080" would yield the unparseable
	// "http://::1:8080".
	listenAddr := net.JoinHostPort(resolved, port)
	selfURL := "http://" + listenAddr

	if raw := strings.TrimSpace(cfg.MCPSelfURL); raw != "" {
		pinned, perr := pinLoopbackSelfURL(raw, lookupIP)
		if perr != nil {
			return mcpRouteState{
				mode:       mcpRouteMisconfigured,
				listenHost: host,
				reason: fmt.Sprintf("MCPSelfURL %q is not a usable loopback base URL (%v); serving the MCP route would forward every caller's bearer token off-host, "+
					"so the route refuses rather than proxying credentials to a non-loopback destination", raw, perr),
			}
		}
		selfURL = pinned
	}

	return mcpRouteState{mode: mcpRouteEnabled, selfURL: selfURL, listenHost: host, listenAddr: listenAddr}
}

// liftedSelfURL derives BOTH the loopback dial-back base URL AND the address
// Start must BIND for a route lifted off loopback because an OAuth AS is enabled
// (ADR-076 slice 3, #2391). The bearer the tool client forwards must reach the
// SAME listener fishhawkd bound even though the LISTENER is off-host, so the
// returned bindAddr pins the listener to the resolved literal the selfURL was
// derived from whenever the bind names a specific host — closing the gap where
// two independent DNS resolutions (selfURL at New, net.Listen at Start) diverge
// and carry the caller's bearer somewhere the listener is not:
//
//   - an embedder-supplied MCPSelfURL keeps the existing pinLoopbackSelfURL
//     treatment unchanged (it must still resolve to a loopback address). bindAddr
//     is EMPTY: the self URL is an independent loopback dial-back target, so the
//     listener binds the operator's chosen cfg.Addr verbatim;
//   - an empty or unspecified listen host yields http://127.0.0.1:<port> with an
//     EMPTY bindAddr, because net.Listen on an empty/unspecified host binds every
//     local unicast address INCLUDING loopback, so the loopback dial-back is
//     reached without a physical hop and ADR-033's forwarding invariant holds
//     with no address to pin;
//   - a specific host resolves to a literal (a literal pins itself; a hostname
//     goes through lookupIP and pins the first address, a resolution failure
//     being an error the caller renders as mcpRouteMisconfigured). Here selfURL
//     AND bindAddr are BOTH built from that ONE resolution, so the address the
//     tool client dials is provably the address the listener bound — a NARROWING
//     of ADR-033's stricter always-127.0.0.1 posture (the dial-back addresses
//     that same locally-assigned address, delivered by the host's own stack
//     without a physical hop), documented as such in README.md.
func liftedSelfURL(cfg Config, host, port string, lookupIP func(string) ([]net.IP, error)) (selfURL, bindAddr string, err error) {
	if raw := strings.TrimSpace(cfg.MCPSelfURL); raw != "" {
		pinned, perr := pinLoopbackSelfURL(raw, lookupIP)
		return pinned, "", perr
	}
	if host == "" {
		return "http://127.0.0.1:" + port, "", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return "http://127.0.0.1:" + port, "", nil
		}
		lit := net.JoinHostPort(ip.String(), port)
		return "http://" + lit, lit, nil
	}
	if lookupIP == nil {
		lookupIP = net.LookupIP
	}
	addrs, err := lookupIP(host)
	if err != nil {
		return "", "", fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return "", "", fmt.Errorf("resolve %q: no addresses", host)
	}
	lit := net.JoinHostPort(addrs[0].String(), port)
	return "http://" + lit, lit, nil
}

// verifyMCPListenerLoopback is the bind-time half of the loopback refusal:
// resolveMCPRouteState classifies an address, Start binds one, and this
// asserts they are the same kind of address before a single request is
// served.
//
// state.listenAddr already pins the resolved IP literal, so this should be
// unreachable — which is exactly why it is here. A gap between the
// classified address and the bound one would expose the entire bare-bearer
// tool surface off-host while the route reported itself healthy, so the
// posture is fail-closed: Start refuses to serve at all rather than serving
// REST with an off-host /mcp. bound comes from net.Listener.Addr, which is
// always a resolved literal, so no DNS runs here.
func verifyMCPListenerLoopback(state mcpRouteState, bound net.Addr) error {
	if state.mode != mcpRouteEnabled {
		return nil
	}
	if state.authenticatedOnly {
		// The route was LIFTED off loopback for OAuth, so the listener is
		// intentionally off-host and the loopback assertion does not apply.
		// But a lift with NO audience to enforce would serve the bare-bearer
		// tool surface off-host, so fail closed rather than serving it.
		if state.challengeResource == "" {
			return fmt.Errorf("the MCP route was lifted off loopback for OAuth but carries no resource identifier to enforce; " +
				"refusing to serve the tool surface off-host with no audience to validate (ADR-076)")
		}
		return nil
	}
	if bound == nil {
		return fmt.Errorf("the MCP route is enabled but the listener reported no address; refusing to serve a loopback-only tool surface (ADR-033) on an unverifiable listener")
	}
	host, _, err := net.SplitHostPort(bound.String())
	if err != nil {
		return fmt.Errorf("the MCP route is enabled but the bound listener address %q is not a valid host:port (%v); refusing to serve a loopback-only tool surface (ADR-033) on an unverifiable listener", bound.String(), err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the MCP route was classified loopback at startup but the listener bound %q, which is reachable off-host; "+
			"refusing to serve the bare-bearer MCP tool surface beyond loopback (ADR-033) — set FISHHAWKD_ADDR=127.0.0.1:8080, or --mcp-route=off to disable the route explicitly", bound.String())
	}
	return nil
}

// pinLoopbackSelfURL validates an embedder-supplied self URL and rewrites
// its host to the RESOLVED loopback IP. Rewriting — rather than merely
// validating — closes the resolution-to-use gap: a hostname left in place
// is re-resolved by the HTTP client on every tool call, so a DNS record
// flipped after construction would carry the caller's bearer off-host.
func pinLoopbackSelfURL(raw string, lookupIP func(string) ([]net.IP, error)) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host")
	}
	resolved, ok, err := mcpLoopbackHost(u.Hostname(), lookupIP)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("host %q is not a loopback address", u.Hostname())
	}
	// An https URL naming a HOSTNAME cannot survive the pinning below, and
	// the failure would land at the worst possible moment: construction
	// reports the route healthy, then EVERY tool call dies in the TLS
	// handshake, because the dialled host is now an IP literal while the
	// certificate was issued for the hostname. Refuse it at New(), where
	// the operator sees one diagnosis, instead of at call time where they
	// see a stream of handshake errors. The refusal is narrow on purpose:
	// https with an IP LITERAL rewrites nothing, so a certificate carrying
	// that IP SAN still verifies and is left alone. It is checked AFTER the
	// loopback verdict so a public https host keeps the sharper
	// not-a-loopback-address diagnosis.
	if u.Scheme == "https" && net.ParseIP(u.Hostname()) == nil {
		return "", fmt.Errorf("scheme https with hostname %q cannot be served: the host is pinned to its resolved loopback address %s, "+
			"so TLS would be negotiated against a certificate issued for %q and every tool call would fail in the handshake; "+
			"use http://, or name the loopback IP literal the certificate covers", u.Hostname(), resolved, u.Hostname())
	}
	if p := u.Port(); p != "" {
		u.Host = net.JoinHostPort(resolved, p)
	} else {
		u.Host = resolved
	}
	return u.String(), nil
}

// newMCPHandler builds the streamable-HTTP handler that serves the MCP
// surface, with the tool registry constructed PER REQUEST and bound to
// that request's bearer token.
//
// Stateless is the load-bearing option, and as of go-sdk v1.7.0 it is
// load-bearing by CONSTRUCTION rather than by consequence. A stateless
// server neither reads nor sets Mcp-Session-Id and serves every request
// from a temporary session; there is no sessions map to consult, so the
// factory above runs on EVERY request. The token authorizing a tool call
// is therefore provably the token on the request that triggered it, and
// there is no session registry to leak, GC, or hijack.
//
// (Through v1.6.1 the same guarantee held for a weaker reason — the
// handler's sessions map was simply never WRITTEN on the stateless
// branch, so the lookup was always nil. v1.7.0 removed session handling
// from stateless mode outright, per the sessionless direction of SEP-2567.
// The pre-v1.7.0 behavior is restorable via the MCPGODEBUG parameter
// `allowsessionsinstateless=1`; do NOT set it — it would reintroduce the
// session lookup this route's security argument depends on being absent.)
//
// Stateless is also what makes the route eligible for protocol
// 2026-07-28: the streamable transport accepts that version only when
// Stateless is true, so this route negotiates up while a stateful one
// would negotiate down to 2025-11-25.
//
// The honest cost, spelled out because it is a real narrowing: a
// stateless server holds no session to stream from or tear down, so
// every non-POST method answers the spec-prescribed 405 with
// `Allow: POST` — GET, and since v1.7.0 DELETE too (it was a 204 no-op
// through v1.6.1). In-request notifications (the progress heartbeats
// Fishhawk's long-running tools emit) still reach the client — they ride
// the POST response's own stream, which stateless mode preserves.
func newMCPHandler(state mcpRouteState, newServer MCPServerFactory, logger *slog.Logger) http.Handler {
	factory := func(r *http.Request) *mcp.Server {
		tok, ok := tokenFromHeader(r)
		if !ok {
			// DO NOT "simplify" this into a direct
			// mcpserver.NewServer(...) call. Returning nil makes the
			// SDK answer 400 rather than constructing a tool registry
			// with an empty token. It is unreachable in production
			// because handleMCP's ladder already refuses a request
			// without a bearer identity with 401 — removing the guard
			// would silently make the bearer optional here.
			return nil
		}
		return newServer(state.selfURL, tok)
	}
	return mcp.NewStreamableHTTPHandler(factory, &mcp.StreamableHTTPOptions{
		Stateless: true,
		Logger:    logger,
	})
}

// handleMCP serves POST/GET/DELETE /mcp. It runs the refusal ladder, then
// the bearer-identity gate, then delegates to the streamable handler.
//
// The auth gate requires a BEARER identity (a non-anonymous Identity
// carrying a TokenID), not merely an authenticated one. That is what
// makes the csrfExemptPath entry for /mcp safe: a browser cookie session
// can never reach the tool surface, so exempting the path from the
// double-submit check cannot open a browser-driven CSRF path onto it.
//
// Both fhk_ and fhm_ identities are accepted with no special-casing — an
// MCP tool call is exactly as privileged as the equivalent REST call,
// including its denials, because the tools reach data by dialling
// fishhawkd's own REST API with the caller's token.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch s.mcpRoute.mode {
	case mcpRouteOff:
		s.writeError(w, r, http.StatusNotFound, "route_not_found",
			"the MCP route is disabled on this deployment (--mcp-route=off)", nil)
		return
	case mcpRouteMisconfigured:
		s.writeError(w, r, http.StatusServiceUnavailable, "mcp_route_misconfigured",
			s.mcpRoute.reason, nil)
		return
	case mcpRouteNotLoopback:
		s.writeError(w, r, http.StatusForbidden, "mcp_route_loopback_only",
			"the MCP route is loopback-only (ADR-033) and this daemon listens on "+
				strconv.Quote(s.mcpRoute.listenHost)+", which is reachable off-host; "+
				"set FISHHAWKD_ADDR=127.0.0.1:8080 to enable it, or --mcp-route=off to disable it explicitly",
			map[string]any{"listen_host": s.mcpRoute.listenHost})
		return
	}

	// A missing bearer and an unknown bearer are INDISTINGUISHABLE here:
	// bearerAuth resolves both to the anonymous Identity and falls
	// through, and discriminating them would leak token validity. A
	// cookie-session identity (non-anonymous, but no TokenID) lands on
	// the same envelope.
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() || id.TokenID == "" {
		s.writeMCPAuthChallenge(w, r)
		return
	}

	// Lifted (off-loopback) authenticated-only mode: accept ONLY an
	// audience-validated OAuth identity. A bare fhk_/fhm_ token carries a
	// TokenID (so it passed the gate above) but no OAuthAudience, and must be
	// refused off-host with the SAME 401 challenge — giving it a distinct
	// message would make a VALID bare token distinguishable from an invalid
	// one, the exact token-validity leak the byte-identical challenge above
	// prevents.
	if s.mcpRoute.authenticatedOnly && id.OAuthAudience == "" {
		s.writeMCPAuthChallenge(w, r)
		return
	}

	s.mcpHandler.ServeHTTP(w, r)
}

// writeMCPAuthChallenge emits the /mcp 401 authentication_required envelope with
// the RFC 6750 §3 / RFC 9728 §5.1 Bearer discovery challenge. It is the ONE
// builder for all three refusal paths — the no-Authorization-header request, the
// unknown-bearer request and the off-loopback bare-token request — so all three
// produce a BYTE-IDENTICAL status, header and body. The challenge names the
// derived PRM URL when an AS is enabled and is the realm-only form otherwise
// (oauthChallengeHeader), and prmURL is empty on a disabled AS.
func (s *Server) writeMCPAuthChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", oauthChallengeHeader(s.oauthAS.prmURL))
	s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
		"the MCP route requires a Fishhawk bearer token (Authorization: Bearer fhk_… or fhm_…); "+
			"a browser cookie session is not accepted", nil)
}
