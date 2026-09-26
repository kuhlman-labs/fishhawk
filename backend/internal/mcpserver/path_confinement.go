package mcpserver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// pathConfinementSchemaNote is the one shared sentence appended to every
// path-taking input's jsonschema description, so a refused agent learns the
// rule from the tool surface instead of only from the refusal (E66.63 /
// #3589). One const, referenced from every description, keeps the eight verbs'
// wording from drifting apart.
const pathConfinementSchemaNote = " Over the HTTP MCP transport the path must resolve inside an operator-configured allowed checkout root (fishhawkd --mcp-allowed-roots / FISHHAWKD_MCP_ALLOWED_ROOTS, fishhawk-mcp --allowed-roots / FISHHAWK_MCP_ALLOWED_ROOTS); a path outside every root — or any path at all when no root is configured — is refused path_outside_allowed_roots."

// pathOutsideAllowedRootsCode is the stable, named error token every
// confinement refusal carries. Callers (and tests) match on this rather than
// on the surrounding prose.
const pathOutsideAllowedRootsCode = "path_outside_allowed_roots"

// allowedRootsRemedy names the operator-side knobs that configure the
// allow-list. Shared by every refusal so an agent is told where the roots come
// from without each call site restating it.
const allowedRootsRemedy = "configure the allow-list with fishhawkd --mcp-allowed-roots / FISHHAWKD_MCP_ALLOWED_ROOTS, or fishhawk-mcp --allowed-roots / FISHHAWK_MCP_ALLOWED_ROOTS"

// confinePath is the ONE shared path-confinement resolver every path-taking
// MCP verb calls before it reads, dials or spawns anything (E66.63 / #3589,
// implementing the operator decision recorded on the issue: option (c) stdio
// unchanged + option (a) HTTP confinement, fail-closed).
//
// The contract, in evaluation order:
//
//   - STDIO transport: returns nil unconditionally. The stdio process is
//     spawned BY the caller and runs as them in their own checkout, so the
//     caller already owns every path this process can reach; confining it would
//     buy nothing and would break the local loop. This is the deliberate
//     residual of operator option (c).
//   - EMPTY input: returns nil. Presence/absence is the #2479/#2482 ladder's
//     decision, not this resolver's — an omitted working_dir is refused (or
//     inherited) THERE, and the inherited value comes back through here.
//   - NON-ABSOLUTE input: refused. A relative path resolves against the
//     daemon's own cwd, so it can never be shown to be inside a root.
//   - NO usable root configured: refused. This is the FAIL-CLOSED posture the
//     operator chose: over HTTP, an unconfigured deployment accepts no path at
//     all rather than accepting every path.
//   - A configured root that is NOT absolute: refused, naming the bad root. A
//     relative root is unresolvable operator misconfiguration, and silently
//     dropping it would narrow the allow-list without telling anyone.
//   - Otherwise: the candidate and every root are normalized (filepath.Clean,
//     then symlink evaluation) and the candidate must lie inside one of them.
//
// Containment is decided by SEPARATOR-ANCHORED prefix on the cleaned,
// symlink-resolved paths — compared BYTE-WISE — so `/roots/repo-evil` is not
// inside `/roots/repo`. Because the comparison is byte-wise, a
// case-insensitive filesystem (Windows, and macOS by default) is NOT
// case-folded here: an operator must supply roots in the same canonical case
// the filesystem reports.
//
// The refusal message names the field, the caller's OWN path and the operator
// knobs — and NOTHING about the filesystem. It is byte-identical for an
// existing and a non-existing outside-root path, so the refusal cannot be used
// as an existence oracle (the #3589 information-leak half).
func (r *runResolver) confinePath(field, in string) error {
	if !r.httpTransport {
		return nil
	}
	if in == "" {
		return nil
	}
	if !filepath.IsAbs(in) {
		return fmt.Errorf(
			"%s: %s %q must be an absolute path over the HTTP MCP transport before it can be checked against the allowed checkout roots; %s",
			pathOutsideAllowedRootsCode, field, in, allowedRootsRemedy)
	}
	if len(r.allowedRoots) == 0 {
		return fmt.Errorf(
			"%s: no allowed checkout root is configured, so %s is refused over the HTTP MCP transport (fail closed); %s",
			pathOutsideAllowedRootsCode, field, allowedRootsRemedy)
	}

	candidate, err := resolveForConfinement(in)
	if err != nil {
		// A resolution failure that is NOT "does not exist" (a permission
		// error, a symlink loop) leaves containment UNDECIDABLE, so refuse.
		// The message deliberately omits the underlying error, which could
		// itself leak filesystem shape.
		return fmt.Errorf(
			"%s: %s %q could not be resolved for containment checking, so it is refused over the HTTP MCP transport (fail closed); %s",
			pathOutsideAllowedRootsCode, field, in, allowedRootsRemedy)
	}

	for _, root := range r.allowedRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf(
				"%s: the configured allowed checkout root %q is not an absolute path, so %s cannot be checked against it and is refused (fail closed); %s",
				pathOutsideAllowedRootsCode, root, field, allowedRootsRemedy)
		}
		resolvedRoot, rerr := resolveForConfinement(root)
		if rerr != nil {
			// An unresolvable root is skipped, not fatal: one bad entry must
			// not disable the others. When NO root resolves, the loop falls
			// through to the outside-root refusal below, which is still
			// fail-closed.
			continue
		}
		if pathWithinRoot(candidate, resolvedRoot) {
			return nil
		}
	}
	return fmt.Errorf(
		"%s: %s %q is not inside any allowed checkout root over the HTTP MCP transport; %s",
		pathOutsideAllowedRootsCode, field, in, allowedRootsRemedy)
}

// resolveForConfinement returns the cleaned, symlink-resolved form of p for
// byte-wise containment comparison.
//
// filepath.Clean alone is NOT sufficient: it removes `..` elements LEXICALLY
// without consulting the filesystem, so a cleaned path can still traverse out
// of a root through a symlink. filepath.EvalSymlinks alone is not sufficient
// either: it errors on a path that does not exist, and a not-yet-created
// spec_file must still be confinable. So this walks up to the longest EXISTING
// ancestor, resolves THAT, and rejoins the remaining cleaned segments — the
// resolved prefix is authoritative and the non-existent tail cannot contain a
// symlink (it contains nothing).
//
// A non-not-exist error from EvalSymlinks (permission denied, ELOOP) is
// returned to the caller, which refuses.
func resolveForConfinement(p string) (string, error) {
	cleaned := filepath.Clean(p)
	cur := cleaned
	var remaining []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if len(remaining) == 0 {
				return resolved, nil
			}
			return filepath.Join(append([]string{resolved}, remaining...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Walked to the filesystem root without finding an existing
			// ancestor; the lexically cleaned path is the best available
			// answer, and containment is still decided against it.
			return cleaned, nil
		}
		remaining = append([]string{filepath.Base(cur)}, remaining...)
		cur = parent
	}
}

// pathWithinRoot reports whether candidate is root itself or lies beneath it,
// comparing the two already-cleaned, already-symlink-resolved paths BYTE-WISE
// with a SEPARATOR anchor. The anchor is what stops `/roots/repo-evil` from
// reading as inside `/roots/repo`: a bare strings.HasPrefix would accept it.
// TrimSuffix keeps a root of "/" working (Clean("/") is "/", so the naive
// root+separator would be "//").
func pathWithinRoot(candidate, root string) bool {
	if candidate == root {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(candidate, strings.TrimSuffix(root, sep)+sep)
}

// specCandidateVerdict is confineSpecCandidate's three-way answer for ONE
// candidate path the `.fishhawk/workflows.yaml` walk is about to read.
type specCandidateVerdict int

const (
	// specCandidateAllow: read it.
	specCandidateAllow specCandidateVerdict = iota
	// specCandidateStopWalk: the candidate's own LEXICAL location has left the
	// allowed roots, so the walk has reached the confinement boundary. Treated
	// exactly like the existing `.git`-dir boundary — stop and report "no
	// spec" — rather than as an escape attempt, because walking UP out of a
	// root is the ordinary end of the search, not a poisoned input.
	specCandidateStopWalk
	// specCandidateRefuse: the candidate is lexically INSIDE a root but
	// resolves OUTSIDE one (a symlink escape), or containment is otherwise
	// undecidable. This is an escape attempt: refuse, reading nothing.
	specCandidateRefuse
)

// confineSpecCandidate is the POST-DISCOVERY half of confinement (binding
// approval condition 1 on E66.63 / #3589): confining the SUPPLIED working_dir
// is not enough, because discoverSpec walks up from it and selects a file that
// may itself be a symlink pointing outside every root. This is consulted for
// each candidate BEFORE os.ReadFile, so a discovered escape is refused having
// read ZERO bytes.
//
// It is inert on stdio and when no candidate check applies, exactly like
// confinePath, and it distinguishes the two ways a candidate can be outside a
// root:
//
//   - LEXICALLY outside (the walk climbed above every root): stop the walk.
//   - Lexically inside but RESOLVING outside (a symlink escape): refuse.
func (r *runResolver) confineSpecCandidate(candidate string) specCandidateVerdict {
	if !r.httpTransport {
		return specCandidateAllow
	}
	if len(r.allowedRoots) == 0 {
		// Fail closed. The verb's own confinePath on the supplied working_dir
		// already refused before the walk started, so this is defence in
		// depth for any future caller that reaches the walk directly.
		return specCandidateRefuse
	}
	if !filepath.IsAbs(candidate) {
		return specCandidateRefuse
	}

	resolvedRoots := make([]string, 0, len(r.allowedRoots))
	for _, root := range r.allowedRoots {
		if !filepath.IsAbs(root) {
			return specCandidateRefuse
		}
		resolved, err := resolveForConfinement(root)
		if err != nil {
			continue
		}
		resolvedRoots = append(resolvedRoots, resolved)
	}

	// LOCATION test first: is the candidate's containing DIRECTORY inside a
	// root? The directory is resolved the same way the roots are (so a root
	// reached through a symlink — macOS's /var -> /private/var — still matches),
	// but the LEAF is deliberately not followed here: following it is what the
	// escape test below does. A candidate whose own directory is outside every
	// root means the walk has climbed out of the confinement region, which is a
	// search boundary, not an escape attempt.
	dirInside := false
	if resolvedDir, derr := resolveForConfinement(filepath.Dir(candidate)); derr == nil {
		for _, root := range resolvedRoots {
			if pathWithinRoot(resolvedDir, root) {
				dirInside = true
				break
			}
		}
	}

	resolved, err := resolveForConfinement(candidate)
	if err != nil {
		return specCandidateRefuse
	}
	for _, root := range resolvedRoots {
		if pathWithinRoot(resolved, root) {
			return specCandidateAllow
		}
	}
	if dirInside {
		// The containing directory is inside a root but the candidate resolves
		// outside one: the LEAF is a symlink escape. Refuse — the condition-1
		// case.
		return specCandidateRefuse
	}
	return specCandidateStopWalk
}

// discoverSpecConfined is the confinement-aware spec resolver every path-taking
// verb calls in place of the bare discoverSpec (binding approval condition 1 on
// E66.63 / #3589). It exists because confining the SUPPLIED working_dir is not
// enough: the file that is actually READ is one the DISCOVERY WALK selects, and
// that file can be a symlink pointing outside every allowed root.
//
// It re-implements discoverSpec's walk rather than threading a hook into it,
// deliberately: spec_discover.go is outside this change's declared scope and
// both mid-stage scope amendments were already spent on export_surface_test.go
// and runbook.md, so the confined walk lives here beside the control it
// enforces. The duplication is bounded to the boundary logic — the READ and the
// blob-SHA computation still go through the SAME package-level helpers
// (os.ReadFile + gitBlobSHA) discoverSpec uses, so there is one definition of
// what a discoveredSpec contains. Keep the two boundary conditions (the `.git`
// dir and the filesystem root) in step with discoverSpec if either changes;
// TestDiscoverSpecConfined_MatchesUnconfinedWalkOnStdio pins that equivalence.
//
// Delegation: the EXPLICIT arm and the whole stdio posture hand straight back to
// discoverSpec. An explicit path came from the caller, so the verb's own
// confinePath already resolved and confined that exact file; stdio is
// deliberately unconfined.
func (r *runResolver) discoverSpecConfined(startDir, explicit string) (*discoveredSpec, error) {
	if explicit != "" || !r.httpTransport {
		return discoverSpec(startDir, explicit)
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return nil, fmt.Errorf("resolve working dir: %w", err)
	}
	for {
		candidate := filepath.Join(dir, specFileName)

		// Confinement BEFORE the open, so a refused candidate is never read.
		switch r.confineSpecCandidate(candidate) {
		case specCandidateRefuse:
			return nil, confinedSpecDiscoveryError(candidate)
		case specCandidateStopWalk:
			// The walk has climbed out of the allowed roots. Treated exactly
			// like the `.git` boundary below: no spec, not an error.
			return nil, nil
		case specCandidateAllow:
			// fall through to the read.
		}

		data, rerr := os.ReadFile(candidate) //nolint:gosec // confined above; the path is inside an allowed root.
		switch {
		case rerr == nil:
			return &discoveredSpec{
				Path:     candidate,
				Contents: data,
				BlobSHA:  gitBlobSHA(data),
			}, nil
		case errors.Is(rerr, fs.ErrNotExist):
			// fall through and check the .git boundary / walk up.
		default:
			return nil, fmt.Errorf("read %s: %w", candidate, rerr)
		}

		// Stop after checking the dir that contains .git (repo root), mirroring
		// discoverSpec: a spec at the root was picked up above, so reaching
		// here means the repo does not configure Fishhawk locally.
		if _, serr := os.Stat(filepath.Join(dir, ".git")); serr == nil {
			return nil, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil // filesystem root; nothing more to walk.
		}
		dir = parent
	}
}

// confinedSpecDiscoveryError is the refusal confineSpecCandidate's
// specCandidateRefuse verdict becomes. It names the DISCOVERED path (which the
// caller did not supply and therefore may not know) and the same operator
// knobs, and says nothing about whether the escape target exists.
func confinedSpecDiscoveryError(candidate string) error {
	return fmt.Errorf(
		"%s: the workflow spec discovered at %q does not resolve inside any allowed checkout root over the HTTP MCP transport, so it was NOT read; %s",
		pathOutsideAllowedRootsCode, candidate, allowedRootsRemedy)
}
