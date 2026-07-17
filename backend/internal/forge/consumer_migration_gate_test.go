package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the contract gate for the ADR-058 / E45.4 Forge-interface
// extraction. The refactor moved a block of vocabulary (RepoRef,
// PullRequest, MergeMethod, the CreateCheckRun*/CheckRun* enums,
// TreeEntry, GitCommit, Repository, BranchProtection, RulesetRequiredCheck,
// ComparePatch*, and the sentinel errors) out of githubclient into forge,
// then re-declared each moved name in githubclient as an
// identity-preserving alias (type RepoRef = forge.RepoRef; var ErrNotFound
// = forge.ErrNotFound). Consumers were migrated to spell the moved names
// forge.* instead of githubclient.*.
//
// The alias is exactly what makes this migration invisible to every other
// check: a file that still says githubclient.RepoRef compiles, type-checks,
// and passes its own tests unchanged, because the two spellings are the
// SAME type and the SAME error value. A no-op touch of a migrated file
// would therefore satisfy scope presence without doing the migration.
// This gate is the only thing that distinguishes a real migration from a
// no-op: it walks the committed backend source and fails, naming
// file:line, when a githubclient.<movedName> reference appears outside the
// unmigrated[] allowlist below.
//
// Detection is AST-based, not textual, and mirrors the #1855
// credential-scope gate (credential_scope_gate_test.go) it sits beside —
// it reuses that file's walkGoSources/repoRoot helpers. A line regex over
// "githubclient." would miss an aliased import (import gh "…/githubclient";
// gh.RepoRef) and would false-match the identifier inside a string or
// comment. Parsing sees the selector the compiler sees: a SelectorExpr
// whose base identifier resolves to the githubclient import and whose
// selector is a moved name.

// githubclientPath is the import path whose local name binds the
// githubclient package selector this gate detects.
const githubclientPath = "github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"

// movedNames is the set of identifiers relocated from githubclient to
// forge by ADR-058 / E45.4. A githubclient.<name> selector for any of
// these is an unmigrated reference. The set is the union of the moved DTO
// types, the MergeMethod/CheckRun* enum types and their consts, and the
// moved sentinel error vars — it must stay in lockstep with the alias
// block in githubclient/client.go (the two are the same contract seen
// from the two packages).
var movedNames = map[string]bool{
	// DTO types.
	"RepoRef":              true,
	"Repository":           true,
	"GitCommit":            true,
	"TreeEntry":            true,
	"PullRequest":          true,
	"PullRequestRef":       true,
	"MergeMethod":          true,
	"BranchProtection":     true,
	"RulesetRequiredCheck": true,
	"ComparePatchFile":     true,
	"ComparePatchResult":   true,
	"CheckRunStatus":       true,
	"CheckRunConclusion":   true,
	"CreateCheckRunParams": true,
	"CreateCheckRunResult": true,
	// Enum consts.
	"MergeMethodSquash":                true,
	"MergeMethodMerge":                 true,
	"MergeMethodRebase":                true,
	"CheckRunStatusQueued":             true,
	"CheckRunStatusInProgress":         true,
	"CheckRunStatusCompleted":          true,
	"CheckRunConclusionSuccess":        true,
	"CheckRunConclusionFailure":        true,
	"CheckRunConclusionNeutral":        true,
	"CheckRunConclusionCancelled":      true,
	"CheckRunConclusionTimedOut":       true,
	"CheckRunConclusionActionRequired": true,
	"CheckRunConclusionSkipped":        true,
	// Sentinel errors.
	"ErrNotFound":                true,
	"ErrForbidden":               true,
	"ErrValidation":              true,
	"ErrNotInstalled":            true,
	"ErrPullRequestExists":       true,
	"ErrMergeConflict":           true,
	"ErrPullRequestCleanStatus":  true,
	"ErrPullRequestNotMergeable": true,
}

// unmigrated maps a repo-relative path (a package-dir prefix or an exact
// file) to WHY it legitimately still spells a moved name githubclient.*.
// These are the surfaces OUTSIDE E45.4's covered Forge interface: the
// issue-comment and reaction-poll notifiers, the non-forge server read/
// write handlers, and the cmd glue adapter that implements the feedback
// API. Keep the reasons — they are the contract, and a new entry should
// be hard to add without stating one. Everything NOT matched here must
// speak forge.* for the moved names or fail this gate.
var unmigrated = map[string]string{
	"backend/internal/issuecomment/":   "issue-comment notifier — outside the E45.4 covered forge surface",
	"backend/internal/reactionpoller/": "reaction poller — outside the E45.4 covered forge surface",

	"backend/internal/server/boardsync.go":       "project-board sync — non-forge server surface",
	"backend/internal/server/codescanning.go":    "code-scanning ingest — non-forge server surface",
	"backend/internal/server/onboarding.go":      "installations onboarding — non-forge server surface",
	"backend/internal/server/prompt.go":          "prompt assembly — non-forge server surface",
	"backend/internal/server/release_notes.go":   "release-notes read — non-forge server surface",
	"backend/internal/server/release_publish.go": "release publish — non-forge server surface",
	"backend/internal/server/trace.go":           "trace compare-patch read — non-forge server surface",
	"backend/internal/server/workitems.go":       "work-items read — non-forge server surface",

	// cmd glue: feedbackAPIAdapter's RepoRef is alias-compatible with the
	// migrated forge.RepoRef interface it satisfies, so it compiles and
	// registers unchanged. It is not part of the covered consumer surface
	// this pass migrated; a trivial spelling swap is a follow-up.
	"backend/cmd/fishhawkd/workmgmt_wiring.go": "cmd feedback-API adapter — alias-compatible glue outside the covered consumer surface",
}

// movedRef is one githubclient.<movedName> reference, located for
// reporting.
type movedRef struct {
	rel  string
	line int
	name string
	text string
}

func (r movedRef) String() string {
	return r.rel + ":" + strconv.Itoa(r.line) + ": githubclient." + r.name + "  (" + r.text + ")"
}

// githubclientLocalName returns the local name the githubclient package is
// imported under in file — respecting an import alias — or "githubclient"
// when the import is absent, so a bare detection-forms snippet with no
// import block still resolves to the canonical spelling.
func githubclientLocalName(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != githubclientPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "githubclient"
	}
	return "githubclient"
}

// movedNameRefsIn parses src and returns every selector reference to a
// moved name through the githubclient import's local name — a
// SelectorExpr whose base is the package identifier and whose selector is
// in movedNames. A selector on any other base (forge.RepoRef, a struct
// field access x.RepoRef, another package's RepoRef) is not a githubclient
// reference and is not reported.
func movedNameRefsIn(rel, src string) ([]movedRef, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	local := githubclientLocalName(file)
	lines := strings.Split(src, "\n")

	var refs []movedRef
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || base.Name != local || !movedNames[sel.Sel.Name] {
			return true
		}
		p := fset.Position(sel.Pos())
		text := ""
		if p.Line-1 >= 0 && p.Line-1 < len(lines) {
			text = strings.TrimSpace(lines[p.Line-1])
		}
		refs = append(refs, movedRef{rel: rel, line: p.Line, name: sel.Sel.Name, text: text})
		return true
	})
	return refs, nil
}

// isUnmigrated reports whether rel is covered by an unmigrated[] entry.
func isUnmigrated(rel string) bool {
	for prefix := range unmigrated {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// collectMovedNameRefs parses every non-test backend .go file and returns
// all githubclient.<movedName> references, allowlisted or not. A file that
// does not parse fails the gate rather than being skipped: an unparseable
// file is an unscanned file, and an unscanned file is a hole. The walk is
// scoped to backend/ because githubclient is a backend package — runner
// and cli are separate modules that cannot import it, so a same-named
// selector there would be unrelated.
func collectMovedNameRefs(t *testing.T) []movedRef {
	t.Helper()
	var all []movedRef
	walkGoSources(t, func(rel, content string) {
		if !strings.HasPrefix(rel, "backend/") {
			return
		}
		refs, err := movedNameRefsIn(rel, content)
		if err != nil {
			t.Fatalf("parse %s: %v (the gate cannot scan what it cannot parse)", rel, err)
		}
		all = append(all, refs...)
	})
	return all
}

// TestNoGithubclientMovedNameOutsideAllowlist is the contract: after
// ADR-058 / E45.4 every consumer of the moved vocabulary spells it forge.*.
// A githubclient.<movedName> reference outside unmigrated[] is an
// unmigrated (or reintroduced) seam and fails here with its file:line —
// the check the alias makes invisible everywhere else.
func TestNoGithubclientMovedNameOutsideAllowlist(t *testing.T) {
	all := collectMovedNameRefs(t)
	// The unmigrated allowlist is non-empty on the committed tree, so a
	// zero-reference scan means the walk found nothing — a path/prefix bug
	// that would let this assertion pass vacuously.
	if len(all) == 0 {
		t.Fatal("scanned no githubclient moved-name references at all; the walk found nothing," +
			" so this assertion would pass vacuously (repoRoot/prefix bug?)")
	}

	var offenders []string
	for _, r := range all {
		if isUnmigrated(r.rel) {
			continue
		}
		offenders = append(offenders, r.String())
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("%d githubclient-spelled reference(s) to a moved forge vocabulary name outside the"+
			" unmigrated[] allowlist — swap to forge.* (the name is an identity-preserving alias, so this"+
			" still compiles; that is exactly why this gate exists):\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}
}

// TestUnmigratedAllowlistStillReferenced keeps the allowlist honest: an
// entry whose file no longer references a moved name is dead weight that
// would silently re-sanction the path if a githubclient reference moved
// back into it.
func TestUnmigratedAllowlistStillReferenced(t *testing.T) {
	matched := map[string]bool{}
	for _, r := range collectMovedNameRefs(t) {
		for prefix := range unmigrated {
			if strings.HasPrefix(r.rel, prefix) {
				matched[prefix] = true
			}
		}
	}
	for prefix, why := range unmigrated {
		if !matched[prefix] {
			t.Errorf("unmigrated allowlist entry %q (%s) no longer references a githubclient-spelled moved"+
				" name; drop the entry so the allowlist cannot silently re-sanction a path", prefix, why)
		}
	}
}

// TestMovedNameRefDetectionForms pins the detector against the reference
// spellings a textual gate misses or mishandles, and — the load-bearing
// half — proves the detector actually FIRES on a reintroduced
// githubclient.<movedName> reference. Without this, a gate that silently
// detected nothing would pass every scan vacuously.
func TestMovedNameRefDetectionForms(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		detects bool
	}{
		{
			name:    "composite literal of a moved type",
			src:     `var _ = githubclient.RepoRef{Owner: "o", Name: "n"}`,
			detects: true,
		},
		{
			name:    "var of a moved type",
			src:     `var x githubclient.PullRequest`,
			detects: true,
		},
		{
			name:    "pointer to a moved type",
			src:     `var x *githubclient.BranchProtection`,
			detects: true,
		},
		{
			name:    "slice element of a moved type",
			src:     `var s []githubclient.RulesetRequiredCheck`,
			detects: true,
		},
		{
			name:    "moved sentinel error in an errors.Is call",
			src:     `var _ = errors.Is(err, githubclient.ErrNotFound)`,
			detects: true,
		},
		{
			name:    "moved merge-method const",
			src:     `var m = githubclient.MergeMethodSquash`,
			detects: true,
		},
		{
			name:    "moved check-run conclusion const",
			src:     `var c = githubclient.CheckRunConclusionSuccess`,
			detects: true,
		},
		{
			name: "aliased githubclient import is still detected",
			src: `import gh "github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"` + "\n" +
				`var _ = gh.RepoRef{}`,
			detects: true,
		},
		{
			name:    "migrated forge spelling is not detected",
			src:     `var _ = forge.RepoRef{}`,
			detects: false,
		},
		{
			name:    "a non-moved githubclient name is not detected",
			src:     `var x githubclient.DispatchInputs`,
			detects: false,
		},
		{
			name:    "githubclient.Client is not a moved name",
			src:     `var c *githubclient.Client`,
			detects: false,
		},
		{
			name:    "the same selector on another package is not detected",
			src:     `var _ = other.RepoRef{}`,
			detects: false,
		},
		{
			name:    "a bare field selector is not a package reference",
			src:     `var _ = x.RepoRef`,
			detects: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := movedNameRefsIn("x.go", "package p\n"+tt.src+"\n")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := len(refs) > 0; got != tt.detects {
				t.Errorf("movedNameRefsIn(%q) detects = %v, want %v", tt.src, got, tt.detects)
			}
		})
	}
}

// TestGithubclientLocalName pins the import-name resolution the aliased-
// import detection depends on: the default name, an explicit alias, and
// the absent-import fallback used by the bare detection-forms snippets.
func TestGithubclientLocalName(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "default import name",
			src:  `import "github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"`,
			want: "githubclient",
		},
		{
			name: "explicit import alias",
			src:  `import gh "github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"`,
			want: "gh",
		},
		{
			name: "absent import falls back to the canonical name",
			src:  `import "context"`,
			want: "githubclient",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package p\n"+tt.src+"\n", parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := githubclientLocalName(file); got != tt.want {
				t.Errorf("githubclientLocalName() = %q, want %q", got, tt.want)
			}
		})
	}
}
