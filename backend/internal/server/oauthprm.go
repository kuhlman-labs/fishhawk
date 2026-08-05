package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// RFC 9728 Protected Resource Metadata (ADR-076 slice 3, #2391). This file
// carries the PRM document, its two well-known URLs, the ONE bearer challenge
// builder, and the two PRM handlers. The audience-validation half lives in
// middleware.go's bearerAuth; the discovery challenge is emitted from
// mcproute.go's writeMCPAuthChallenge, which reads the prmURL derived here.

// wellKnownPRMPath is the RFC 9728 §3 protected-resource-metadata well-known
// path. RFC 9728 §3.1 constructs a resource's PRM URL by INSERTING this path
// between the resource identifier's origin and its own path.
const wellKnownPRMPath = "/.well-known/oauth-protected-resource"

// oauthPRMetadata is the RFC 9728 §2 protected-resource metadata document. Only
// the fields this deployment can speak to are populated; every value is derived
// from the resolved AS state, never from request input.
type oauthPRMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// protectedResourceMetadataURL derives, per RFC 9728 §3.1, the PRM URL for a
// resource identifier and the route suffix the path-suffixed handler matches on.
//
// RFC 9728 §3.1 INSERTS wellKnownPRMPath between the resource's origin and its
// own path: https://host/mcp -> https://host/.well-known/oauth-protected-resource/mcp.
//
// The two returned values are DELIBERATELY encoded differently, and getting this
// wrong is the CONDITION-1 defect:
//
//   - prmURL (advertised to clients) is built from the resource's EscapedPath, so
//     the URL is valid and a path segment needing encoding stays encoded.
//   - suffix (compared against r.PathValue("resource_path")) is built from the
//     DECODED url.URL.Path, because Go's ServeMux returns PathValue already
//     percent-DECODED. Comparing a decoded PathValue against an EscapedPath-
//     derived suffix can NEVER match for any resource path needing encoding, so
//     the handler would 404 the exact URL its own PRM document advertises. We
//     compare like with like: decoded-to-decoded.
//
// A resource whose scheme cannot host an http(s) PRM document is an error (fail
// closed): resolveOAuthASState carries it as the misconfigured verdict rather
// than advertising an AS that cannot name its own PRM document.
func protectedResourceMetadataURL(resource oauthas.ResourceIdentifier) (prmURL, suffix string, err error) {
	u, err := url.Parse(resource.String())
	if err != nil {
		return "", "", fmt.Errorf("parse resource identifier %q: %w", resource.String(), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("resource identifier scheme %q cannot host an RFC 9728 protected-resource metadata document (need http or https)", u.Scheme)
	}
	origin := u.Scheme + "://" + u.Host
	escPath := strings.TrimSuffix(u.EscapedPath(), "/")
	prmURL = origin + wellKnownPRMPath + escPath
	// Decoded, to match ServeMux's decoded PathValue (see doc above).
	suffix = strings.TrimPrefix(strings.TrimSuffix(u.Path, "/"), "/")
	return prmURL, suffix, nil
}

// oauthChallengeHeader builds the RFC 6750 §3 Bearer challenge carrying the RFC
// 9728 §5.1 resource_metadata discovery pointer.
//
// It returns the realm-only form (correct when no issuer is configured, so
// prmURL is empty) unless a usable prmURL is present. FAIL-CLOSED GUARD: an
// unescapable prmURL — one carrying a double quote, a backslash, or any byte
// < 0x20 or == 0x7f — is OMITTED and the realm-only challenge returned, because
// such a value cannot be safely spliced into a quoted-string header parameter.
func oauthChallengeHeader(prmURL string) string {
	if prmURL == "" || !quotedStringSafe(prmURL) {
		return `Bearer realm="fishhawk"`
	}
	return `Bearer realm="fishhawk", resource_metadata="` + prmURL + `"`
}

// quotedStringSafe reports whether s can be spliced verbatim into an RFC 7230
// quoted-string: no double quote, no backslash, no control byte (< 0x20 or DEL).
func quotedStringSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// protectedResourceMetadataDoc renders the PRM document from the resolved AS
// state. It is called only after oauthASEnabled has confirmed the enabled
// verdict, so issuer and resource are populated.
func (s *Server) protectedResourceMetadataDoc() oauthPRMetadata {
	st := s.oauthAS
	return oauthPRMetadata{
		Resource:               st.resource.String(),
		AuthorizationServers:   []string{st.issuer.String()},
		ScopesSupported:        append([]string(nil), oauthas.SupportedScopes...),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Fishhawk",
	}
}

// handleOAuthPRM answers GET /.well-known/oauth-protected-resource (the bare
// well-known path). It calls oauthASEnabled FIRST so a disabled/misconfigured AS
// answers 503 oauth_as_unconfigured with NO partial document, exactly as the AS
// metadata route does, and sets Cache-Control: no-store.
//
// The bare path serves the identical document whenever the AS is enabled — an
// interoperability convenience for a client that probes the well-known path
// without knowing the resource path. The challenge always names the RFC-correct
// suffixed URL, so a strict client validating `resource` against the request URL
// still lands on the suffixed form.
func (s *Server) handleOAuthPRM(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusOK, s.protectedResourceMetadataDoc())
}

// handleOAuthPRMSuffixed answers GET
// /.well-known/oauth-protected-resource/{resource_path...}. Like the bare route
// it gates on oauthASEnabled first, then compares the decoded PathValue against
// the derived prmSuffix and answers 404 route_not_found on a mismatch — so the
// document is never served under a URL claiming a different resource.
func (s *Server) handleOAuthPRMSuffixed(w http.ResponseWriter, r *http.Request) {
	if !s.oauthASEnabled(w, r) {
		return
	}
	if r.PathValue("resource_path") != s.oauthAS.prmSuffix {
		s.writeError(w, r, http.StatusNotFound, "route_not_found",
			"no protected-resource metadata document is served under that path", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusOK, s.protectedResourceMetadataDoc())
}
