package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// OUT-OF-SCOPE PATHS in the verify-fix prompt (#3410).
//
// Run 49f642b8's verify gate failed with `add them to
// backend/internal/audit/categories.go` — a file outside the effective scope —
// and the fix agent, a FRESH invocation that never saw the implement prompt's
// mid-stage scope-amendment recipe, spent its iterations on in-scope
// workarounds. The fix prompt now (1) names every repo-relative path the
// verify output mentions that exists on disk and is NOT in the effective
// scope, with an instruction to file an amendment rather than retry, and (2)
// ALWAYS carries a compact copy of the amendment recipe in the effective-scope
// form, so the recipe reaches the agent even when the detector names nothing.
// Delivery of an approval to the next iteration is the #3434 mid-loop re-fold.

// verifyFixMaxOutOfScopePaths caps the OUT-OF-SCOPE PATHS list so a chatty
// verify output cannot flood the fix prompt.
const verifyFixMaxOutOfScopePaths = 10

// verifyFixOutOfScopeMarker heads the OUT-OF-SCOPE PATHS block. A named
// constant so the e2e prompt assertions and the renderer cannot drift apart.
const verifyFixOutOfScopeMarker = "OUT-OF-SCOPE PATHS named by the verify output (NOT editable in this fix):"

// verifyFixAmendmentRecipeHeader heads the compact amendment recipe.
const verifyFixAmendmentRecipeHeader = "Mid-stage scope amendment (the ONLY way to reach a file outside the effective scope):"

// verifyFixPathTokenRE matches a repo-relative path token: a run of path
// characters containing at least one '/' and ending in a file extension.
// The leading group consumes the character before the token (or the line
// start) and REJECTS a word character, '/' or '.', so `/abs/path.go` and
// `../foo/bar.go` are rejected as WHOLE tokens rather than yielding the
// relative suffix `abs/path.go` / `foo/bar.go` (binding condition 1 on
// #3410). Go regexp has no lookbehind, hence the consuming group.
var verifyFixPathTokenRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_/.])([A-Za-z0-9_][A-Za-z0-9_./-]*/[A-Za-z0-9_.-]+\.[A-Za-z0-9]+)`)

// outOfScopePathsInVerifyOutput is the pure detector: every path token in
// output (verifyFixPathTokenRE, trailing ':' ',' ')' trimmed) that is
// relative, carries no '..' segment, still contains a '/' after path.Clean, is NOT in
// the slash-normalized scope set, and for which exists(rel) is true — deduped,
// sorted, capped at verifyFixMaxOutOfScopePaths. exists is injected (the
// production predicate is fileExistsUnder(repoDir)) so the table test needs
// no filesystem. A module-relative path a tool prints (go test / golangci-lint
// run from inside a module) does not resolve from the repo root and so is NOT
// reported — the intended precision/recall trade; a widened matcher would spam
// every fix prompt.
func outOfScopePathsInVerifyOutput(output string, scope []upload.ScopeFile, exists func(rel string) bool) []string {
	if output == "" || exists == nil {
		return nil
	}
	inScope := make(map[string]bool, len(scope))
	for _, f := range scope {
		if f.Path == "" {
			continue
		}
		// Unconditional backslash normalization (filepath.ToSlash only
		// rewrites the HOST separator), mirroring the backend sweep's
		// toSlashPath so a backslash-form scope entry still matches.
		inScope[path.Clean(strings.ReplaceAll(f.Path, "\\", "/"))] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, m := range verifyFixPathTokenRE.FindAllStringSubmatch(output, -1) {
		tok := strings.TrimRight(m[1], ":,)")
		if tok == "" || path.IsAbs(tok) || hasDotDotSegment(tok) {
			continue
		}
		rel := path.Clean(tok)
		if !strings.Contains(rel, "/") {
			continue
		}
		if inScope[rel] || seen[rel] {
			continue
		}
		if !exists(rel) {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	if len(out) > verifyFixMaxOutOfScopePaths {
		out = out[:verifyFixMaxOutOfScopePaths]
	}
	return out
}

// hasDotDotSegment reports whether any '/'-separated segment of tok is "..",
// checked on the RAW token before path.Clean so `a/../b/c.go` is dropped
// rather than laundered into `b/c.go`.
func hasDotDotSegment(tok string) bool {
	for _, seg := range strings.Split(tok, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// fileExistsUnder returns the production exists predicate for
// outOfScopePathsInVerifyOutput: true only when rel names a REGULAR file
// under repoDir (a directory or a symlink to one is not a path the agent can
// edit as a file).
func fileExistsUnder(repoDir string) func(string) bool {
	return func(rel string) bool {
		info, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(rel)))
		return err == nil && info.Mode().IsRegular()
	}
}

// renderOutOfScopePaths renders the OUT-OF-SCOPE PATHS block: the marker
// heading, one `- <path>` line per path, then the fixed instruction to file an
// amendment rather than retry an in-scope workaround. Empty for no paths.
func renderOutOfScopePaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(verifyFixOutOfScopeMarker + "\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString(`These files exist in the repository but are NOT in your effective scope. If
the failure above can only be fixed by editing one of them — e.g. a registry
the failing test tells you to add an entry to — do NOT retry an in-scope
workaround and do NOT edit it: file a mid-stage scope amendment naming the
path (recipe below) and edit it only after the decision lands as approved.
`)
	return b.String()
}

// renderFixScopeAmendmentRecipe renders the compact mid-stage scope-amendment
// recipe: the same POST / GET ?wait=30 endpoints, bearer env and decision
// branches the implement prompt documents. It is a HAND-MAINTAINED compact copy
// of backend/internal/prompt/prompt.go::writeScopeAmendments — the runner
// module cannot import the backend — so the endpoint path, the `?wait=30`
// poll and the env var names are asserted as literals on both sides' tests to
// bound drift. The 2-per-stage budget is SHARED with any request the implement
// pass already filed.
func renderFixScopeAmendmentRecipe() string {
	return verifyFixAmendmentRecipeHeader + `
1. POST $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments with header
   Authorization: Bearer $FISHHAWK_API_TOKEN and body
   {"paths":[{"path":"dir/file.ext","operation":"modify"|"create"}],"reason":"why each path must change"}.
2. Poll GET $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments?wait=30 (same bearer)
   until your request's status leaves pending (~15 minutes total); keep working
   on in-scope files while you wait.
3. approved → the path is folded into your scope for the NEXT iteration: edit it
   now, and the re-commit includes it. Read decision_reason — an approval reason
   is a binding instruction on the amended paths.
4. denied → read decision_reason and adapt in scope ONLY if the done-means still
   holds; otherwise stop and say so. A green-but-wrong in-scope workaround is
   forbidden.
5. Still pending at the cap is an EXPIRY, NOT a denial: disclose it and either
   adapt in scope (only if the done-means holds) or stop.
At most 2 requests per stage, INCLUDING any the implement pass already filed;
batch every needed path into one request.
`
}
