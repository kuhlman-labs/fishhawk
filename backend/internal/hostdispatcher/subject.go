// Package hostdispatcher defines the ADR-104 host-dispatcher identity (E88.2 /
// #4526): the host-dispatcher/<host> token subject grammar, the ratified scope
// profile (ADR-104 Q4), the clamp the bearer middleware applies to every
// dispatcher-subject token and the route allowlist it enforces. It is a leaf
// package (standard library only) so the server,
// `fishhawkd token issue`, `fishhawkd token migrate` and the dispatcher itself
// share ONE definition of what a host dispatcher is. Long-form contract:
// README.md.
package hostdispatcher

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// SubjectPrefix is the token-subject prefix every host dispatcher carries.
const SubjectPrefix = "host-dispatcher/"

// SourceTag is the record-act `source` a host-dispatcher subject must send on
// POST /v0/runs/{id}/auto-drive/acts, so its run_auto_driven rows are
// distinguishable from a drive_run or campaign-driver row.
const SourceTag = "host_dispatcher"

// MaxLabelLen bounds the <host> label.
const MaxLabelLen = 64

// ErrInvalidSubject is returned for a subject that carries the dispatcher
// prefix but not a valid host-dispatcher/<host> label.
var ErrInvalidSubject = errors.New("hostdispatcher: invalid host-dispatcher subject")

// The ratified ADR-104 Q4 profile: read runs and their audit chain, record the
// dispatch act (write:runs), post the host-dispatch marker (write:stages).
// Nothing that approves, merges, files fix-ups, retries, deploys or starts a
// campaign.
const (
	ScopeReadRuns    = "read:runs"
	ScopeReadAudit   = "read:audit"
	ScopeWriteRuns   = "write:runs"
	ScopeWriteStages = "write:stages"
)

// Scopes returns the fixed dispatcher scope profile. A fresh slice per call,
// so no caller can widen the profile for every other caller by appending.
func Scopes() []string {
	return []string{ScopeReadRuns, ScopeReadAudit, ScopeWriteRuns, ScopeWriteStages}
}

// IsSubject reports whether subject is a host-dispatcher subject. It is a
// PREFIX match on purpose: a malformed host-dispatcher/ subject is still a
// dispatcher, so it is classified as an agent and refused, never treated as a
// human.
func IsSubject(subject string) bool {
	return strings.HasPrefix(subject, SubjectPrefix)
}

// ParseSubject returns the <host> label of a valid host-dispatcher/<host>
// subject. The label is non-empty, at most MaxLabelLen characters, and drawn
// from [A-Za-z0-9._-] (no slash, so one host has exactly one subject).
func ParseSubject(subject string) (string, error) {
	label, ok := strings.CutPrefix(subject, SubjectPrefix)
	if !ok {
		return "", fmt.Errorf("%w: %q lacks the %q prefix", ErrInvalidSubject, subject, SubjectPrefix)
	}
	if err := validateLabel(label); err != nil {
		return "", fmt.Errorf("%w: %q %v", ErrInvalidSubject, subject, err)
	}
	return label, nil
}

// ValidateSubject reports whether subject is a valid host-dispatcher/<host>
// subject.
func ValidateSubject(subject string) error {
	_, err := ParseSubject(subject)
	return err
}

// SubjectFor renders the subject for label, validating it.
func SubjectFor(label string) (string, error) {
	if err := validateLabel(label); err != nil {
		return "", fmt.Errorf("%w: label %q %v", ErrInvalidSubject, label, err)
	}
	return SubjectPrefix + label, nil
}

func validateLabel(label string) error {
	if label == "" {
		return errors.New("has an empty host label")
	}
	if len(label) > MaxLabelLen {
		return fmt.Errorf("host label exceeds %d characters", MaxLabelLen)
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("host label contains %q (allowed: A-Z a-z 0-9 . _ -)", r)
		}
	}
	return nil
}

// ClampScopes returns in ∩ Scopes(), in in's order with duplicates dropped.
// The middleware applies it to every dispatcher-subject token, so a
// hand-minted dispatcher token carrying write:approvals still cannot approve.
func ClampScopes(in []string) []string {
	allowed := map[string]bool{}
	for _, sc := range Scopes() {
		allowed[sc] = true
	}
	out := make([]string, 0, len(allowed))
	for _, sc := range in {
		if allowed[sc] {
			out = append(out, sc)
			allowed[sc] = false
		}
	}
	return out
}

// OutsideProfile returns the members of requested that are not in Scopes(),
// in requested's order. `fishhawkd token issue` refuses a dispatcher mint that
// names any of them.
func OutsideProfile(requested []string) []string {
	allowed := map[string]bool{}
	for _, sc := range Scopes() {
		allowed[sc] = true
	}
	var out []string
	for _, sc := range requested {
		if !allowed[sc] {
			out = append(out, sc)
		}
	}
	return out
}

// routes is the closed set of REST routes a host-dispatcher subject may call:
// exactly the routes `fishhawk-mcp host-dispatch` uses
// (backend/internal/mcpserver/host_dispatcher*.go and the shared helpers it
// calls). The bearer middleware refuses every other route 403
// host_dispatcher_route_forbidden before the handler and before any
// repository-configured check, so no write:runs / write:stages route outside
// this list (recover, scope-amendment and crew-escalation decisions, cancel,
// concurrency promote, ...) is reachable with a dispatcher token. Widening it
// is a contract change: update the README, the server's route-reach pin and
// the dispatcher's request pin together.
var routes = []string{
	"GET /v0/runs",
	"GET /v0/runs/{run_id}",
	"GET /v0/runs/{run_id}/stages",
	"POST /v0/runs/{run_id}/auto-drive/acts",
	"POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch",
	"POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure",
	"POST /v0/stages/{stage_id}/acceptance-admission",
}

// Routes returns the allowlist as "METHOD /pattern" strings (a fresh slice per
// call).
func Routes() []string {
	return append([]string(nil), routes...)
}

// RouteAllowed reports whether method and escapedPath (the request's
// URL.EscapedPath(), no query) match an allowlisted route. Segments split on
// "/" as http.ServeMux splits them: a {name} segment matches one non-empty
// segment, a literal segment must equal the unescaped segment. A trailing
// slash, an empty or dot segment, an escaped slash or an unparseable escape
// never matches, and
// the method must match exactly (HEAD is not GET): it fails closed.
func RouteAllowed(method, escapedPath string) bool {
	segs, ok := splitPath(escapedPath)
	if !ok {
		return false
	}
	for _, rt := range routes {
		m, pattern, _ := strings.Cut(rt, " ")
		if m != method {
			continue
		}
		want, _ := splitPath(pattern)
		if len(want) != len(segs) {
			continue
		}
		match := true
		for i, w := range want {
			if strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}") {
				continue
			}
			if w != segs[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func splitPath(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") {
		return nil, false
	}
	raw := strings.Split(p[1:], "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		u, err := url.PathUnescape(seg)
		if err != nil || u == "" || u == "." || u == ".." || strings.Contains(u, "/") {
			return nil, false
		}
		out = append(out, u)
	}
	return out, true
}
