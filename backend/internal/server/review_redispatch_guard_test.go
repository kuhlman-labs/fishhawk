package server

// ADR-091 D6 regression guards (#4176).
//
// The marker that bypasses the #797 same-head dedup (reviewRedispatchCtxKey,
// set only by withReviewRedispatch) and the boot-only reconcile chain that
// reaches it must stay reachable from the boot sweep and nowhere else. The
// older TestReviewRedispatchMarker_SetOnlyByBootRedispatch counts CALLS of
// withReviewRedispatch only, so three bypasses stayed GREEN under it: a raw
// context.WithValue(ctx, reviewRedispatchCtxKey{}, ...), a method value of the
// setter, and a request handler calling a boot-only reconcile entry point.
//
// These guards parse the package's production files with go/ast and pin the
// EXACT multiset of declarations that REFERENCE each D6-sensitive identifier,
// counting every reference (calls, method values, composite literals, var and
// field types, conversions), not only calls. The checkers are pure functions
// over parsed files, so the planted-violation subtests below permanently prove
// that each bypass turns the guard RED. The scan is syntactic (no type
// information): a same-named identifier in another scope would count too,
// which fails toward over-reporting.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// d6Rule pins one identifier. byArg false pins every REFERENCE site
// ("file:decl"); byArg true pins every site with its call's last argument
// ("file:decl(lastArg)"), a non-call reference rendering as "(<non-call>)" so a
// method value can never satisfy an argument-pinned entry.
type d6Rule struct {
	byArg bool
	sites []string
}

// d6Allow IS the ADR-091 D6 / #4176 contract: the exact multiset of sites
// allowed to reference each identifier. A legitimate refactor of the boot chain
// (rename, new wrapper, moved call) turns the guard RED until this table is
// updated in the same change, so the update is reviewable rather than silent.
var d6Allow = map[string]d6Rule{
	// The marker key: only its setter and its reader.
	"reviewRedispatchCtxKey": {sites: []string{
		"review_round_context.go:reviewRedispatchFrom",
		"review_round_context.go:withReviewRedispatch",
	}},
	// The setter: only the boot re-dispatch goroutine body.
	"withReviewRedispatch": {sites: []string{"review_redispatch.go:runOrphanedRoundRedispatch"}},
	// The goroutine body: only the hand-off (its go statement).
	"runOrphanedRoundRedispatch": {sites: []string{"review_redispatch.go:redispatchOrphanedRound"}},
	// The hand-off: only the stage-level body, reached with allowRedispatch.
	"redispatchOrphanedRound": {sites: []string{"review_reconcile.go:reconcileStageOrphanedReviewsMode"}},
	// The boot-mode run reconcile: only the boot sweep entry point.
	"reconcileRunOrphanedReviewsForBoot": {sites: []string{"review_reconcile.go:ReconcileOrphanedReviews"}},
	// The boot-mode pass over a terminal run's discovered rounds (ADR-140 D3,
	// #5047): only the boot sweep entry point.
	"reconcileAdmittedRoundsForBoot": {sites: []string{"review_reconcile.go:ReconcileOrphanedReviews"}},
	// The allowRedispatch *Mode chain: the terminate-only wrappers pass a
	// literal false, only the two boot wrappers pass a literal true, and the
	// chain between them passes the parameter through.
	"reconcileRunOrphanedReviewsMode": {byArg: true, sites: []string{
		"review_reconcile.go:reconcileAdmittedRoundsForBoot(true)",
		"review_reconcile.go:reconcileRunOrphanedReviews(false)",
		"review_reconcile.go:reconcileRunOrphanedReviewsForBoot(true)",
	}},
	"reconcileRunOrphanedReviewsLockedMode": {byArg: true, sites: []string{
		"review_reconcile.go:reconcileRunOrphanedReviewsLocked(false)",
		"review_reconcile.go:reconcileRunOrphanedReviewsMode(allowRedispatch)",
	}},
	"reconcileStageOrphanedReviewsMode": {byArg: true, sites: []string{
		"review_reconcile.go:reconcileRunOrphanedReviewsLockedMode(allowRedispatch)",
		"review_reconcile.go:reconcileStageOrphanedReviews(false)",
	}},
}

const d6RuleText = "ADR-091 D6: the re-dispatch marker and the boot-only reconcile chain are reachable only from the boot sweep; a reviewed refactor of that chain updates d6Allow in review_redispatch_guard_test.go"

// reconcileEntryName is the one exported boot entry point; the module scan
// pins it to a single production caller.
const reconcileEntryName = "ReconcileOrphanedReviews"

var reconcileEntryAllow = []string{"cmd/fishhawkd/serve.go:runServe"}

func (r d6Rule) observe(files map[string]*ast.File, name string) []string {
	if r.byArg {
		return callSites(files, name)
	}
	return refSites(files, name)
}

func d6RuleNames() []string {
	names := make([]string, 0, len(d6Allow))
	for name := range d6Allow {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func d6SortedCopy(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return out
}

func sortedFileNames(files map[string]*ast.File) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseServerProductionFiles parses the package's non-test files. It fails (it
// never returns an empty set) when the directory held nothing or lacks the
// file that declares the marker, so a wrong working directory is RED, not a
// vacuous GREEN.
func parseServerProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no production files parsed: the guard would pass vacuously")
	}
	if _, ok := files["review_round_context.go"]; !ok {
		t.Fatal("review_round_context.go not parsed: the guard would pass vacuously")
	}
	return files
}

// inspectDecl walks d's subtree except the identifier that DECLARES a func or
// type: a declaration is not a reference.
func inspectDecl(d ast.Decl, visit func(ast.Node) bool) {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil {
			ast.Inspect(d.Recv, visit)
		}
		ast.Inspect(d.Type, visit)
		if d.Body != nil {
			ast.Inspect(d.Body, visit)
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				if ts.TypeParams != nil {
					ast.Inspect(ts.TypeParams, visit)
				}
				ast.Inspect(ts.Type, visit)
				continue
			}
			ast.Inspect(spec, visit)
		}
	}
}

// declLabel is the enclosing FuncDecl's name, or "<package>" for a GenDecl.
func declLabel(d ast.Decl) string {
	if fn, ok := d.(*ast.FuncDecl); ok {
		return fn.Name.Name
	}
	return "<package>"
}

// refSites returns one "file:decl" entry per identifier named name that is not
// the declaring identifier of a func or type, sorted so a comparison is
// multiset-exact.
func refSites(files map[string]*ast.File, name string) []string {
	var out []string
	for _, file := range sortedFileNames(files) {
		for _, d := range files[file].Decls {
			site := file + ":" + declLabel(d)
			inspectDecl(d, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == name {
					out = append(out, site)
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// callSites is refSites with the call's last argument pinned: a call whose
// callee is name renders "file:decl(lastArg)", any other reference renders
// "file:decl(<non-call>)".
func callSites(files map[string]*ast.File, name string) []string {
	var out []string
	for _, file := range sortedFileNames(files) {
		for _, d := range files[file].Decls {
			label := file + ":" + declLabel(d)
			callee := map[*ast.Ident]bool{}
			inspectDecl(d, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					var id *ast.Ident
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						id = fun
					case *ast.SelectorExpr:
						id = fun.Sel
					}
					if id != nil && id.Name == name {
						callee[id] = true
						last := ""
						if len(n.Args) > 0 {
							last = types.ExprString(n.Args[len(n.Args)-1])
						}
						out = append(out, label+"("+last+")")
					}
				case *ast.Ident:
					if n.Name == name && !callee[n] {
						out = append(out, label+"(<non-call>)")
					}
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// multisetDiff returns the entries got has beyond want, and the entries want
// has beyond got.
func multisetDiff(got, want []string) (unexpected, missing []string) {
	count := map[string]int{}
	for _, w := range want {
		count[w]++
	}
	for _, g := range got {
		if count[g] > 0 {
			count[g]--
			continue
		}
		unexpected = append(unexpected, g)
	}
	for _, w := range d6SortedCopy(want) {
		if count[w] > 0 {
			count[w]--
			missing = append(missing, w)
		}
	}
	return unexpected, missing
}

// d6Violations diffs every d6Allow identifier's observed sites against the
// table in BOTH directions: an unexpected site is a bypass, and a missing
// allowed site means the table (or the walker) no longer matches the tree.
func d6Violations(files map[string]*ast.File) []string {
	var out []string
	for _, name := range d6RuleNames() {
		rule := d6Allow[name]
		unexpected, missing := multisetDiff(rule.observe(files, name), rule.sites)
		for _, s := range unexpected {
			out = append(out, fmt.Sprintf("%s: unexpected reference at %s; %s", name, s, d6RuleText))
		}
		for _, s := range missing {
			out = append(out, fmt.Sprintf("%s: allow-listed site %s not found (renamed, moved or removed?); %s", name, s, d6RuleText))
		}
	}
	return out
}

// moduleReconcileFiles parses the backend module's non-test files that mention
// the exported boot entry point, keyed by module-relative path. go test runs a
// package's binary in its source directory, so the module root is "../..".
func moduleReconcileFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	root := filepath.Join("..", "..")
	needle := []byte(reconcileEntryName)
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, needle) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		files[rel] = f
		return nil
	})
	if err != nil {
		t.Fatalf("walk backend module: %v", err)
	}
	return files
}

// reconcileEntryViolations pins the exported boot entry point's referrers.
func reconcileEntryViolations(files map[string]*ast.File) []string {
	unexpected, missing := multisetDiff(refSites(files, reconcileEntryName), reconcileEntryAllow)
	var out []string
	for _, s := range unexpected {
		out = append(out, fmt.Sprintf("%s: unexpected reference at %s; only the boot sweep in %v may call it (%s)", reconcileEntryName, s, reconcileEntryAllow, d6RuleText))
	}
	for _, s := range missing {
		out = append(out, fmt.Sprintf("%s: allow-listed site %s not found (renamed, moved or removed?); %s", reconcileEntryName, s, d6RuleText))
	}
	return out
}

// plantFile returns base plus one extra parsed file. The source only needs to
// PARSE: the checkers are syntactic, so it need not type-check.
func plantFile(t *testing.T, base map[string]*ast.File, name, src string) map[string]*ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse planted %s: %v", name, err)
	}
	out := maps.Clone(base)
	out[name] = f
	return out
}

func plantedHandler(body string) string {
	return "package server\n\nfunc (s *Server) handlePlanted(w http.ResponseWriter, r *http.Request) {\n" + body + "\n}\n"
}

// TestReviewRedispatchD6Guard_RealPackage asserts the server package's
// references to every D6-sensitive identifier equal the d6Allow table.
//
// COUNTERFACTUALS: a raw reviewRedispatchCtxKey{} literal, or a call of
// reconcileRunOrphanedReviewsForBoot, planted in a handler turns the first
// check RED while the call-counting TestReviewRedispatchMarker_SetOnlyByBootRedispatch
// stays GREEN; flipping a terminate-only wrapper's literal false to true turns
// the argument-pinned entries RED.
func TestReviewRedispatchD6Guard_RealPackage(t *testing.T) {
	files := parseServerProductionFiles(t)

	// Independent of d6Violations: compare each identifier's observed sites to
	// the table directly, so a broken walker cannot hide behind a stubbed
	// checker (and vice versa).
	for _, name := range d6RuleNames() {
		rule := d6Allow[name]
		got, want := rule.observe(files, name), d6SortedCopy(rule.sites)
		if !slices.Equal(got, want) {
			t.Errorf("%s: observed sites %v, want exactly %v; %s", name, got, want, d6RuleText)
		}
	}

	// Checker liveness: over NO files every allowed site is missing, so a
	// checker that always reports clean (or a walker that finds nothing) fails
	// here instead of passing the real-tree check vacuously.
	empty := strings.Join(d6Violations(map[string]*ast.File{}), "\n")
	for _, name := range d6RuleNames() {
		if !strings.Contains(empty, name+": allow-listed site") {
			t.Errorf("d6Violations over no files does not report %s's allowed sites as missing: the checker is not deciding", name)
		}
	}

	if v := d6Violations(files); len(v) != 0 {
		t.Errorf("ADR-091 D6 guard violations:\n%s", strings.Join(v, "\n"))
	}
}

// TestReconcileOrphanedReviews_CalledOnlyFromServe pins the exported boot entry
// point to exactly one production caller across the backend module. This also
// subsumes the in-package check that no server handler calls it.
func TestReconcileOrphanedReviews_CalledOnlyFromServe(t *testing.T) {
	files := moduleReconcileFiles(t)
	if got := refSites(files, reconcileEntryName); !slices.Equal(got, reconcileEntryAllow) {
		t.Errorf("%s referrers across the backend module = %v, want exactly %v", reconcileEntryName, got, reconcileEntryAllow)
	}
	if v := reconcileEntryViolations(files); len(v) != 0 {
		t.Errorf("boot entry point violations:\n%s", strings.Join(v, "\n"))
	}
}

// TestReviewRedispatchD6Guard_FlagsPlantedViolations is the permanent proof
// that the guard decides: each bypass mode is planted on top of the REAL
// package files, so the planted site is the only difference from the clean
// result.
//
// COUNTERFACTUAL: make d6Violations return nil → every planted subtest is RED.
func TestReviewRedispatchD6Guard_FlagsPlantedViolations(t *testing.T) {
	files := parseServerProductionFiles(t)
	if v := d6Violations(files); len(v) != 0 {
		t.Fatalf("precondition: the real tree must be clean before planting, got:\n%s", strings.Join(v, "\n"))
	}

	const handler = "planted_handler.go:handlePlanted"
	cases := []struct {
		name  string
		ident string
		src   string
		site  string
	}{
		{
			name:  "raw marker key literal",
			ident: "reviewRedispatchCtxKey",
			src:   plantedHandler(`_ = context.WithValue(r.Context(), reviewRedispatchCtxKey{}, reviewRedispatch{Of: 1, Depth: 1})`),
			site:  handler,
		},
		{
			name:  "zero-value marker key var",
			ident: "reviewRedispatchCtxKey",
			src:   plantedHandler("var k reviewRedispatchCtxKey\n_ = context.WithValue(r.Context(), k, nil)"),
			site:  handler,
		},
		{
			name:  "package-level marker key var",
			ident: "reviewRedispatchCtxKey",
			src:   "package server\n\nvar plantedKey = reviewRedispatchCtxKey{}\n",
			site:  "planted_handler.go:<package>",
		},
		{
			name:  "setter method value",
			ident: "withReviewRedispatch",
			src:   plantedHandler("f := withReviewRedispatch\n_ = f"),
			site:  handler,
		},
		{
			name:  "handler calls the boot-mode run reconcile",
			ident: "reconcileRunOrphanedReviewsForBoot",
			src:   plantedHandler(`_, _ = s.reconcileRunOrphanedReviewsForBoot(r.Context(), uuid.Nil)`),
			site:  handler,
		},
		{
			name:  "handler calls the re-dispatch hand-off",
			ident: "redispatchOrphanedRound",
			src:   plantedHandler(`_ = s.redispatchOrphanedRound(r.Context(), uuid.Nil, orphanedReviewStages[0], nil, planreview.ReviewStartedPayload{}, 0)`),
			site:  handler,
		},
		{
			name:  "handler calls a Mode function with true",
			ident: "reconcileRunOrphanedReviewsMode",
			src:   plantedHandler(`_, _ = s.reconcileRunOrphanedReviewsMode(r.Context(), uuid.Nil, nil, true)`),
			site:  "planted_handler.go:handlePlanted(true)",
		},
		{
			name:  "handler calls the admitted-round boot wrapper",
			ident: "reconcileAdmittedRoundsForBoot",
			src:   plantedHandler(`_, _ = s.reconcileAdmittedRoundsForBoot(r.Context(), uuid.Nil, nil)`),
			site:  handler,
		},
		{
			name:  "a non-allowed function calls the Mode chain with true",
			ident: "reconcileRunOrphanedReviewsMode",
			src:   "package server\n\nfunc (s *Server) plantedBootPath(ctx context.Context) {\n\t_, _ = s.reconcileRunOrphanedReviewsMode(ctx, uuid.Nil, nil, true)\n}\n",
			site:  "planted_handler.go:plantedBootPath(true)",
		},
		{
			name:  "handler takes a Mode function as a method value",
			ident: "reconcileRunOrphanedReviewsMode",
			src:   plantedHandler("f := s.reconcileRunOrphanedReviewsMode\n_, _ = f(r.Context(), uuid.Nil, true)"),
			site:  "planted_handler.go:handlePlanted(<non-call>)",
		},
		{
			name:  "handler calls the re-dispatch goroutine body",
			ident: "runOrphanedRoundRedispatch",
			src:   plantedHandler(`s.runOrphanedRoundRedispatch(r.Context(), uuid.Nil, orphanedReviewStages[0], 1, planreview.ReviewStartedPayload{})`),
			site:  handler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := d6Violations(plantFile(t, files, "planted_handler.go", tc.src))
			if len(v) != 1 {
				t.Fatalf("violations = %d, want exactly 1 (the planted site):\n%s", len(v), strings.Join(v, "\n"))
			}
			for _, want := range []string{"unexpected reference", tc.ident, tc.site} {
				if !strings.Contains(v[0], want) {
					t.Errorf("violation %q does not name %q", v[0], want)
				}
			}
		})
	}

	// Flipping a terminate-only wrapper's literal false to true makes the
	// on-demand verb's path re-dispatch. The mutation is applied to the real
	// source; the anti-noop count guarantees it landed. The result is the PAIR:
	// the unexpected (true) site and the now-missing (false) site.
	t.Run("terminate-only wrapper arg flipped to true", func(t *testing.T) {
		const file = "review_reconcile.go"
		const from = "return s.reconcileRunOrphanedReviewsMode(ctx, runID, nil, false)"
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if n := strings.Count(string(src), from); n != 1 {
			t.Fatalf("%q occurs %d times in %s, want exactly 1: the mutation would not land", from, n, file)
		}
		mutated := strings.Replace(string(src), from, "return s.reconcileRunOrphanedReviewsMode(ctx, runID, nil, true)", 1)
		f, err := parser.ParseFile(token.NewFileSet(), file, mutated, 0)
		if err != nil {
			t.Fatalf("parse mutated %s: %v", file, err)
		}
		in := maps.Clone(files)
		in[file] = f

		v := d6Violations(in)
		if len(v) != 2 {
			t.Fatalf("violations = %d, want exactly the pair (unexpected true + missing false):\n%s", len(v), strings.Join(v, "\n"))
		}
		joined := strings.Join(v, "\n")
		for _, want := range []string{
			"unexpected reference at review_reconcile.go:reconcileRunOrphanedReviews(true)",
			"allow-listed site review_reconcile.go:reconcileRunOrphanedReviews(false) not found",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("violations do not name %q:\n%s", want, joined)
			}
		}
	})

	// The module scan: a file elsewhere in the backend module calling the
	// exported boot entry point.
	t.Run("module file calls ReconcileOrphanedReviews", func(t *testing.T) {
		mod := moduleReconcileFiles(t)
		if v := reconcileEntryViolations(mod); len(v) != 0 {
			t.Fatalf("precondition: the real module must be clean before planting, got:\n%s", strings.Join(v, "\n"))
		}
		const site = "internal/mcpserver/planted.go:plantedBootCall"
		src := "package mcpserver\n\nfunc plantedBootCall(srv *server.Server, ctx context.Context) {\n\t_, _ = srv.ReconcileOrphanedReviews(ctx)\n}\n"
		v := reconcileEntryViolations(plantFile(t, mod, "internal/mcpserver/planted.go", src))
		if len(v) != 1 {
			t.Fatalf("violations = %d, want exactly 1 (the planted site):\n%s", len(v), strings.Join(v, "\n"))
		}
		for _, want := range []string{"unexpected reference", site} {
			if !strings.Contains(v[0], want) {
				t.Errorf("violation %q does not name %q", v[0], want)
			}
		}
	})
}

// TestReviewRedispatch_FallbackHoldsStripeLock pins that closeUnstartedRedispatch
// takes the RUN'S stripe lock (reconcileEmitLockFor) before it counts the
// landed terminals and holds it through the synthesis (#4176 item 3). The test
// holds the stripe itself and settles the round (both terminals) inside that
// critical section, simulating a concurrent reconcile pass: with the lock the
// fallback cannot finish until the test releases it, then re-counts landed=2
// and synthesizes nothing.
//
// COUNTERFACTUAL: replace the stripe with a private &sync.Mutex{} (the code
// still compiles and Lock/Unlock stay balanced) → the fallback finishes while
// the stripe is held, having counted landed=0 and synthesized 2, so the round
// ends with 4 terminals → RED. TestReviewRedispatch_FallbackRecountsUnderLock
// cannot see this: it has no concurrency.
func TestReviewRedispatch_FallbackHoldsStripeLock(t *testing.T) {
	f := newRedispatchFixture(t, redispatchFixtureOpts{})
	seq := f.seedEligiblePlanRound(t, advisoryPlanStarted())
	stage := orphanedReviewStages[0]
	runID := f.runRow.ID

	// reconcileEmitMu is a package-global stripe array: a leaked lock would
	// wedge any later test whose run hashes to this stripe, so every exit path
	// releases it.
	lock := reconcileEmitLockFor(runID)
	lock.Lock()
	held := true
	unlock := func() {
		if held {
			held = false
			lock.Unlock()
		}
	}
	t.Cleanup(unlock)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.s.closeUnstartedRedispatch(context.Background(), runID, stage, seq, advisoryPlanStarted(), redispatchSlugRoundNotStarted)
	}()

	select {
	case <-done:
		t.Fatal("closeUnstartedRedispatch completed while the run's stripe lock was held")
	case <-time.After(timescale.D(200 * time.Millisecond)):
	}

	// Settle the round inside the critical section.
	for range 2 {
		f.appendAt(t, f.planStage.ID, afterBoot, stage.failed, planreview.ReviewFailedPayload{
			Reason:    "settled by a concurrent reconcile pass",
			Authority: planreview.AuthorityAdvisory,
		})
	}
	unlock()

	select {
	case <-done:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("closeUnstartedRedispatch did not finish after the stripe lock was released")
	}

	reasons := failedReasonsAfter(t, f.au, runID, stage.failed, seq)
	if len(reasons) != 2 {
		t.Fatalf("%s after the orphaned round = %d (%v), want exactly the 2 the test appended (the fallback must re-count under the lock)", stage.failed, len(reasons), reasons)
	}
	for _, r := range reasons {
		if r == orphanedReviewRedispatchFallbackReason(redispatchSlugRoundNotStarted) {
			t.Errorf("fallback synthesized a terminal for a round already settled under the lock: %q", r)
		}
	}
}

// --- ADR-140 (#5047): the consumer predicate ----------------------------------

// consumerRunRepo faults GetRun per run and counts reads per run, so a row can
// assert which runs a classification read.
type consumerRunRepo struct {
	*orchestratorRepo
	faultMu sync.Mutex
	faults  map[uuid.UUID]error
	reads   map[uuid.UUID]int
}

func newConsumerRunRepo(rr *orchestratorRepo) *consumerRunRepo {
	return &consumerRunRepo{orchestratorRepo: rr, faults: map[uuid.UUID]error{}, reads: map[uuid.UUID]int{}}
}

func (r *consumerRunRepo) fail(id uuid.UUID, err error) {
	r.faultMu.Lock()
	defer r.faultMu.Unlock()
	r.faults[id] = err
}

func (r *consumerRunRepo) readsOf(id uuid.UUID) int {
	r.faultMu.Lock()
	defer r.faultMu.Unlock()
	return r.reads[id]
}

func (r *consumerRunRepo) totalReads() int {
	r.faultMu.Lock()
	defer r.faultMu.Unlock()
	n := 0
	for _, c := range r.reads {
		n += c
	}
	return n
}

func (r *consumerRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.faultMu.Lock()
	r.reads[id]++
	err := r.faults[id]
	r.faultMu.Unlock()
	if err != nil {
		return nil, err
	}
	return r.orchestratorRepo.GetRun(ctx, id)
}

func TestRoundConsumerGoneSlug_Classification(t *testing.T) {
	cases := []struct {
		name            string
		state           run.State
		parent          run.State
		want            string
		wantParentReads int
	}{
		{name: "cancelled_top_level", state: run.StateCancelled, want: redispatchSlugRunTerminal},
		{name: "cancelled_child_of_running_parent", state: run.StateCancelled, parent: run.StateRunning, want: redispatchSlugRunTerminal},
		{name: "succeeded_child_of_succeeded_parent", state: run.StateSucceeded, parent: run.StateSucceeded, want: redispatchSlugRunTerminal, wantParentReads: 1},
		{name: "succeeded_child_of_failed_parent", state: run.StateSucceeded, parent: run.StateFailed, want: redispatchSlugRunTerminal, wantParentReads: 1},
		{name: "succeeded_child_of_cancelled_parent", state: run.StateSucceeded, parent: run.StateCancelled, want: redispatchSlugRunTerminal, wantParentReads: 1},
		{name: "failed_top_level", state: run.StateFailed},
		{name: "failed_child_of_running_parent", state: run.StateFailed, parent: run.StateRunning},
		{name: "failed_child_of_failed_parent", state: run.StateFailed, parent: run.StateFailed},
		{name: "failed_child_of_cancelled_parent", state: run.StateFailed, parent: run.StateCancelled},
		{name: "failed_child_of_succeeded_parent", state: run.StateFailed, parent: run.StateSucceeded},
		{name: "running", state: run.StateRunning},
		{name: "pending", state: run.StatePending},
		{name: "succeeded_top_level", state: run.StateSucceeded},
		{name: "succeeded_child_of_pending_parent", state: run.StateSucceeded, parent: run.StatePending, wantParentReads: 1},
		{name: "succeeded_child_of_running_parent", state: run.StateSucceeded, parent: run.StateRunning, wantParentReads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			repo := newConsumerRunRepo(rr)
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake()})
			ru := rr.seedRun()
			ru.State = tc.state
			parentID := uuid.Nil
			if tc.parent != "" {
				parent := rr.seedRun()
				parent.State = tc.parent
				parentID = parent.ID
				ru.DecomposedFrom = &parent.ID
			}
			got, err := s.roundConsumerGoneSlug(context.Background(), ru.ID)
			if err != nil || got != tc.want {
				t.Fatalf("slug = %q, %v, want %q", got, err, tc.want)
			}
			if n := repo.readsOf(parentID); n != tc.wantParentReads {
				t.Errorf("parent reads = %d, want %d", n, tc.wantParentReads)
			}
		})
	}

	fault := errors.New("injected run read fault")
	unknown := []struct {
		name     string
		arrange  func(rr *orchestratorRepo, repo *consumerRunRepo) uuid.UUID
		wantIs   error
		wantText string
	}{
		{name: "run_missing", arrange: func(*orchestratorRepo, *consumerRunRepo) uuid.UUID { return uuid.New() }, wantIs: run.ErrNotFound},
		{name: "run_read_error", arrange: func(rr *orchestratorRepo, repo *consumerRunRepo) uuid.UUID {
			ru := rr.seedRun()
			repo.fail(ru.ID, fault)
			return ru.ID
		}, wantIs: fault},
		{name: "parent_missing", arrange: func(rr *orchestratorRepo, _ *consumerRunRepo) uuid.UUID {
			ru := rr.seedRun()
			ru.State = run.StateSucceeded
			gone := uuid.New()
			ru.DecomposedFrom = &gone
			return ru.ID
		}, wantIs: run.ErrNotFound},
		{name: "parent_read_error", arrange: func(rr *orchestratorRepo, repo *consumerRunRepo) uuid.UUID {
			ru, parent := rr.seedRun(), rr.seedRun()
			ru.State = run.StateSucceeded
			ru.DecomposedFrom = &parent.ID
			repo.fail(parent.ID, fault)
			return ru.ID
		}, wantIs: fault},
	}
	for _, tc := range unknown {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			repo := newConsumerRunRepo(rr)
			s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: newAuditFake()})
			got, err := s.roundConsumerGoneSlug(context.Background(), tc.arrange(rr, repo))
			if got != "" || !errors.Is(err, tc.wantIs) {
				t.Fatalf("slug = %q, err = %v, want an unknown result wrapping %v", got, err, tc.wantIs)
			}
		})
	}
	t.Run("nil_run_repo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()})
		got, err := s.roundConsumerGoneSlug(context.Background(), uuid.New())
		if err != nil || got != redispatchSlugEligibilityCheckFailed {
			t.Fatalf("slug = %q, %v, want %q", got, err, redispatchSlugEligibilityCheckFailed)
		}
	})
}

// roundRefusalArm wires an otherwise eligible trace-origin implement round so
// exactly one eligibility refusal stands, or none on plain_advisory.
type roundRefusalArm struct {
	name    string
	unwired bool
	mutate  func(*planreview.ReviewStartedPayload)
	after   func(t *testing.T, f *redispatchFixture, seq int64)
	want    string
}

func roundRefusalArms() []roundRefusalArm {
	return []roundRefusalArm{
		{name: "plain_advisory"},
		{name: "gating", want: redispatchSlugGatingAuthority, mutate: func(p *planreview.ReviewStartedPayload) { p.Authority = planreview.AuthorityGating }},
		{name: "reviewer_unwired", want: redispatchSlugReviewerUnwired, unwired: true},
		{name: "depth_cap_reached", want: redispatchSlugDepthCapReached, mutate: func(p *planreview.ReviewStartedPayload) { p.RedispatchDepth = maxReviewRoundRedispatches }},
		{name: "already_redispatched", want: redispatchSlugAlreadyRedispatched, after: func(t *testing.T, f *redispatchFixture, seq int64) {
			f.appendAt(t, f.implStage.ID, terminalRoundAge(time.Hour), categoryReviewRoundRedispatched, reviewRoundRedispatchedPayload{Stage: "implement", OrphanedRoundSequence: seq})
		}},
	}
}

// terminalRoundAge is a round start relative to the wall clock, as the sweep's
// lookback reads it.
func terminalRoundAge(age time.Duration) time.Time { return time.Now().UTC().Add(-age) }

const terminalRoundHead = "terminal-round-head"

// terminalRunArm puts the fixture's run into one terminal shape.
type terminalRunArm struct {
	name    string
	arrange func(f *redispatchFixture)
}

func childOf(f *redispatchFixture, state, parentState run.State) {
	parent := f.rr.seedRun()
	parent.State = parentState
	f.runRow.State = state
	f.runRow.DecomposedFrom = &parent.ID
}

func noConsumerArms() []terminalRunArm {
	return []terminalRunArm{
		{"cancelled", func(f *redispatchFixture) { f.runRow.State = run.StateCancelled }},
		{"succeeded_child_of_terminal_parent", func(f *redispatchFixture) { childOf(f, run.StateSucceeded, run.StateFailed) }},
	}
}

func consumerArms() []terminalRunArm {
	return []terminalRunArm{
		{"failed_top_level", func(f *redispatchFixture) { f.runRow.State = run.StateFailed }},
		{"failed_child_of_terminal_parent", func(f *redispatchFixture) { childOf(f, run.StateFailed, run.StateFailed) }},
		{"succeeded_top_level", func(f *redispatchFixture) { f.runRow.State = run.StateSucceeded }},
		{"succeeded_child_of_live_parent", func(f *redispatchFixture) { childOf(f, run.StateSucceeded, run.StateRunning) }},
	}
}

func newRefusalFixture(t *testing.T, arm roundRefusalArm, opts redispatchFixtureOpts) *redispatchFixture {
	t.Helper()
	opts.reviewerUnwired = arm.unwired
	if opts.traceStore == nil {
		opts.traceStore = newRedispatchTraceStore()
	}
	return newRedispatchFixture(t, opts)
}

// seedRefusalRound seeds the arm's round an hour ago and returns its sequence.
func seedRefusalRound(t *testing.T, f *redispatchFixture, arm roundRefusalArm) int64 {
	t.Helper()
	started := advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead)
	if arm.mutate != nil {
		arm.mutate(&started)
	}
	seq := f.appendAt(t, f.implStage.ID, terminalRoundAge(time.Hour), "implement_review_started", started)
	if arm.after != nil {
		arm.after(t, f, seq)
	}
	return seq
}

func eligibilityOf(t *testing.T, f *redispatchFixture) (bool, string, error) {
	t.Helper()
	stage := orphanedReviewStages[1]
	latest, payload, ok, err := f.s.latestReviewStarted(context.Background(), f.runRow.ID, stage)
	if err != nil || !ok {
		t.Fatalf("setup: latestReviewStarted: ok=%v err=%v", ok, err)
	}
	return f.s.redispatchEligibility(context.Background(), f.runRow.ID, stage, latest, payload)
}

func TestRedispatchEligibility_ConsumerFirst_EveryRefusalCombination(t *testing.T) {
	for _, state := range noConsumerArms() {
		for _, arm := range roundRefusalArms() {
			t.Run(state.name+"/"+arm.name, func(t *testing.T) {
				f := newRefusalFixture(t, arm, redispatchFixtureOpts{})
				seedRefusalRound(t, f, arm)
				state.arrange(f)
				ok, slug, err := eligibilityOf(t, f)
				if ok || slug != redispatchSlugRunTerminal || err != nil {
					t.Fatalf("eligibility = (%v, %q, %v), want run_terminal ahead of every refusal", ok, slug, err)
				}
			})
		}
	}
	for _, state := range consumerArms() {
		for _, arm := range roundRefusalArms() {
			t.Run(state.name+"/"+arm.name, func(t *testing.T) {
				f := newRefusalFixture(t, arm, redispatchFixtureOpts{})
				seedRefusalRound(t, f, arm)
				state.arrange(f)
				ok, slug, err := eligibilityOf(t, f)
				if err != nil || slug != arm.want || ok != (arm.want == "") {
					t.Fatalf("eligibility = (%v, %q, %v), want its own answer %q", ok, slug, err, arm.want)
				}
			})
		}
	}
	t.Run("failed_top_level/failed_implement_stage", func(t *testing.T) {
		f := newRefusalFixture(t, roundRefusalArm{}, redispatchFixtureOpts{implStageState: run.StageStateFailed})
		seedRefusalRound(t, f, roundRefusalArm{})
		f.runRow.State = run.StateFailed
		ok, slug, err := eligibilityOf(t, f)
		if ok || slug != redispatchSlugImplementStageTerminal || err != nil {
			t.Fatalf("eligibility = (%v, %q, %v), want implement_stage_terminal", ok, slug, err)
		}
	})
	t.Run("nil_run_repo/gating", func(t *testing.T) {
		f := newRefusalFixture(t, roundRefusalArm{}, redispatchFixtureOpts{})
		latest := &audit.Entry{Sequence: 1, StageID: &f.implStage.ID}
		f.s.cfg.RunRepo = nil
		ok, slug, err := f.s.redispatchEligibility(context.Background(), f.runRow.ID, orphanedReviewStages[1], latest,
			planreview.ReviewStartedPayload{ConfiguredAgents: 1, Authority: planreview.AuthorityGating})
		if ok || slug != redispatchSlugEligibilityCheckFailed || err != nil {
			t.Fatalf("eligibility = (%v, %q, %v), want eligibility_check_failed ahead of gating_authority", ok, slug, err)
		}
	})
}

// TestRedispatchEligibility_LiveConsumerAndFailedRows pins that a live consumer
// and a failed run read the predicate empty, so the remaining eligibility checks
// decide, and that an unknown run errors instead of reading eligible.
func TestRedispatchEligibility_LiveConsumerAndFailedRows(t *testing.T) {
	arrangements := append([]terminalRunArm{
		{"running", func(f *redispatchFixture) { f.runRow.State = run.StateRunning }},
		{"pending", func(f *redispatchFixture) { f.runRow.State = run.StatePending }},
	}, consumerArms()...)
	for _, state := range arrangements {
		t.Run(state.name, func(t *testing.T) {
			f := newRefusalFixture(t, roundRefusalArm{}, redispatchFixtureOpts{})
			seedRefusalRound(t, f, roundRefusalArm{})
			state.arrange(f)
			ok, slug, err := eligibilityOf(t, f)
			if !ok || slug != "" || err != nil {
				t.Fatalf("eligibility = (%v, %q, %v), want eligible", ok, slug, err)
			}
		})
	}
	t.Run("run_read_error", func(t *testing.T) {
		f := newRefusalFixture(t, roundRefusalArm{}, redispatchFixtureOpts{})
		seedRefusalRound(t, f, roundRefusalArm{})
		repo := newConsumerRunRepo(f.rr)
		fault := errors.New("injected run read fault")
		repo.fail(f.runRow.ID, fault)
		f.s.cfg.RunRepo = repo
		ok, slug, err := eligibilityOf(t, f)
		if ok || slug != "" || !errors.Is(err, fault) {
			t.Fatalf("eligibility = (%v, %q, %v), want the read error", ok, slug, err)
		}
	})
}

// --- ADR-140 (#5047): the redacted bundle loader --------------------------------

// bundleModesStore answers per content hash: a stored body, a body whose read
// fails, or a Get fault. An unknown hash is the store's own definitive miss.
type bundleModesStore struct {
	*redispatchTraceStore
	getFaults  map[string]error
	readFaults map[string]error
}

type failingBundleReader struct{ err error }

func (r failingBundleReader) Read([]byte) (int, error) { return 0, r.err }

func (s *bundleModesStore) Get(ctx context.Context, ref tracestore.BundleRef) (io.ReadCloser, error) {
	if err := s.getFaults[ref.ContentHash]; err != nil {
		return nil, err
	}
	if err := s.readFaults[ref.ContentHash]; err != nil {
		return io.NopCloser(failingBundleReader{err}), nil
	}
	body, err := s.redispatchTraceStore.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", tracestore.ErrNotFound, ref.ContentHash)
	}
	return body, nil
}

func bundleHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestLoadRedactedBundleForHead_Modes(t *testing.T) {
	const head = "bundle-modes-head"
	fault := errors.New("injected store fault")
	good := implementPushGatedBundleWithHeadSHA(t, 2, head)
	other := implementPushGatedBundleWithHeadSHA(t, 1, "another-head")
	corrupt := []byte("these bytes are no bundle")

	type fixture struct {
		f     *redispatchFixture
		store *bundleModesStore
	}
	record := func(t *testing.T, fx fixture, b []byte, stored bool) string {
		t.Helper()
		hash := bundleHash(b)
		if stored {
			fx.store.put(tracestore.VariantRedacted, hash, b)
		}
		fx.f.appendAt(t, fx.f.implStage.ID, terminalRoundAge(time.Hour), "trace_uploaded", map[string]any{"variant": string(tracestore.VariantRedacted), "content_hash": hash})
		return hash
	}
	cases := []struct {
		name    string
		arrange func(t *testing.T, fx fixture)
		wantOK  bool
		wantIs  error
		wantErr bool
	}{
		{name: "usable", wantOK: true, arrange: func(t *testing.T, fx fixture) { record(t, fx, good, true) }},
		{name: "object_missing", arrange: func(t *testing.T, fx fixture) { record(t, fx, good, false) }},
		{name: "wrong_head", arrange: func(t *testing.T, fx fixture) { record(t, fx, other, true) }},
		{name: "corrupt", arrange: func(t *testing.T, fx fixture) { record(t, fx, corrupt, true) }},
		{name: "no_uploaded_row", arrange: func(*testing.T, fixture) {}},
		{name: "get_read_error", wantIs: fault, wantErr: true, arrange: func(t *testing.T, fx fixture) {
			fx.store.getFaults[record(t, fx, good, true)] = fault
		}},
		{name: "body_read_error", wantIs: fault, wantErr: true, arrange: func(t *testing.T, fx fixture) {
			fx.store.readFaults[record(t, fx, good, true)] = fault
		}},
		{name: "trace_uploaded_list_error", wantErr: true, arrange: func(t *testing.T, fx fixture) {
			record(t, fx, good, true)
			fx.f.au.listByCategoryErrCategory = "trace_uploaded"
		}},
		{name: "usable_older_after_newer_read_error", wantOK: true, arrange: func(t *testing.T, fx fixture) {
			record(t, fx, good, true)
			fx.store.getFaults[record(t, fx, other, true)] = fault
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &bundleModesStore{redispatchTraceStore: newRedispatchTraceStore(), getFaults: map[string]error{}, readFaults: map[string]error{}}
			f := newRedispatchFixture(t, redispatchFixtureOpts{traceStore: store})
			tc.arrange(t, fixture{f: f, store: store})
			ctx := context.Background()
			body, ok, err := f.s.loadRedactedBundleForHead(ctx, f.runRow.ID, f.implStage.ID, head)
			if ok != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("load = ok %v, err %v; want ok %v, wantErr %v", ok, err, tc.wantOK, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want %v", err, tc.wantIs)
			}
			if tc.wantOK && !bytes.Equal(body, good) {
				t.Errorf("loaded another bundle's bytes")
			}
			wrapped, wrappedOK := f.s.redactedBundleForHead(ctx, f.runRow.ID, f.implStage.ID, head)
			if wrappedOK != ok || !bytes.Equal(wrapped, body) {
				t.Errorf("the two-value wrapper disagrees: %v versus %v", wrappedOK, ok)
			}
		})
	}
	t.Run("nil_store", func(t *testing.T) {
		f := newRedispatchFixture(t, redispatchFixtureOpts{})
		_, ok, err := f.s.loadRedactedBundleForHead(context.Background(), f.runRow.ID, f.implStage.ID, head)
		if ok || err != nil {
			t.Fatalf("load = ok %v, err %v; want a definitive miss", ok, err)
		}
	})
	t.Run("empty_head_takes_newest", func(t *testing.T) {
		store := &bundleModesStore{redispatchTraceStore: newRedispatchTraceStore(), getFaults: map[string]error{}, readFaults: map[string]error{}}
		f := newRedispatchFixture(t, redispatchFixtureOpts{traceStore: store})
		record(t, fixture{f: f, store: store}, good, true)
		record(t, fixture{f: f, store: store}, other, true)
		body, ok, err := f.s.loadRedactedBundleForHead(context.Background(), f.runRow.ID, f.implStage.ID, "")
		if !ok || err != nil || !bytes.Equal(body, other) {
			t.Fatalf("load = ok %v, err %v, want the newest", ok, err)
		}
	})
}

// --- ADR-140 (#5047): terminal-run discovery ------------------------------------

// faithfulListAllAudit applies what the store's ListAll does and auditFake's does
// not: the category and run filters, the account window (equal or NULL), ts then
// id descending and the limit. Every call's params are recorded.
// TestFaithfulListAll_MatchesStore_PG runs one seed through both, so it cannot
// drift unnoticed.
type faithfulListAllAudit struct {
	*auditFake
	callMu   sync.Mutex
	calls    []audit.ListAllParams
	accounts map[uuid.UUID]*uuid.UUID
}

func newFaithfulListAllAudit(au *auditFake) *faithfulListAllAudit {
	return &faithfulListAllAudit{auditFake: au, accounts: map[uuid.UUID]*uuid.UUID{}}
}

// setRunAccount stamps every appended row of a run with an account; an unset run's
// rows are NULL-account.
func (a *faithfulListAllAudit) setRunAccount(runID uuid.UUID, account *uuid.UUID) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	a.accounts[runID] = account
}

func (a *faithfulListAllAudit) recorded() []audit.ListAllParams {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	return slices.Clone(a.calls)
}

func (a *faithfulListAllAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	a.callMu.Lock()
	a.calls = append(a.calls, p)
	accounts := maps.Clone(a.accounts)
	a.callMu.Unlock()
	if a.listAllErr != nil {
		return nil, a.listAllErr
	}
	if a.listAllErrCategory != "" && p.Category != nil && *p.Category == a.listAllErrCategory {
		return nil, errors.New("faithfulListAllAudit: injected ListAll error for " + a.listAllErrCategory)
	}
	a.mu.Lock()
	rows := make([]*audit.Entry, 0, len(a.seeded)+len(a.appended))
	rows = append(rows, a.seeded...)
	for i := range a.appended {
		ap := a.appended[i]
		rid := ap.RunID
		rows = append(rows, &audit.Entry{
			ID:        uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("appended-%d", i))),
			Sequence:  int64(i + 1),
			RunID:     &rid,
			StageID:   ap.StageID,
			Timestamp: ap.Timestamp,
			Category:  ap.Category,
			Payload:   ap.Payload,
			AccountID: accounts[rid],
		})
	}
	a.mu.Unlock()
	var want *uuid.UUID
	if p.AccountID != "" {
		if id, err := uuid.Parse(p.AccountID); err == nil {
			want = &id
		}
	}
	out := rows[:0:0]
	for _, row := range rows {
		if p.Category != nil && row.Category != *p.Category {
			continue
		}
		if p.RunID != nil && (row.RunID == nil || *row.RunID != *p.RunID) {
			continue
		}
		if want != nil && row.AccountID != nil && *row.AccountID != *want {
			continue
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return bytes.Compare(out[i].ID[:], out[j].ID[:]) > 0
	})
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

// seedPGAccount inserts a real account row and returns its id.
func seedPGAccount(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "acct-"+id.String()); err != nil {
		t.Fatalf("setup: insert account: %v", err)
	}
	return id
}

func TestFaithfulListAll_MatchesStore_PG(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	account := seedPGAccount(t, pool)
	stranger := seedPGAccount(t, pool)

	nullRun, nullStage := seedPGReviewRun(t, runRepo)
	accountRun, accountStage := seedPGReviewRun(t, runRepo)
	strangerRun, strangerStage := seedPGReviewRun(t, runRepo)
	for id, acct := range map[uuid.UUID]uuid.UUID{accountRun: account, strangerRun: stranger} {
		if _, err := pool.Exec(ctx, `UPDATE runs SET account_id = $1 WHERE id = $2`, acct, id); err != nil {
			t.Fatalf("setup: set run account: %v", err)
		}
	}
	tied := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i, row := range []struct {
		runID, stageID uuid.UUID
		age            time.Duration
		category       string
	}{
		{nullRun, nullStage, 3 * time.Hour, "plan_review_started"},
		{accountRun, accountStage, 2 * time.Hour, "plan_review_started"},
		{strangerRun, strangerStage, time.Hour, "plan_review_started"},
		{nullRun, nullStage, 0, "plan_review_started"},
		{accountRun, accountStage, 0, "plan_review_started"},
		{strangerRun, strangerStage, 0, "plan_review_started"},
		{nullRun, nullStage, 90 * time.Minute, "implement_review_started"},
	} {
		appendPGAudit(t, auditRepo, row.runID, &row.stageID, tied.Add(-row.age), row.category, map[string]any{"seed": i})
	}

	everything, err := auditRepo.ListAll(ctx, audit.ListAllParams{})
	if err != nil {
		t.Fatalf("setup: list: %v", err)
	}
	au := newAuditFake()
	au.seeded = everything
	faithful := newFaithfulListAllAudit(au)

	category := "plan_review_started"
	other := "implement_review_started"
	for _, tc := range []struct {
		name string
		p    audit.ListAllParams
		want int
	}{
		{"category", audit.ListAllParams{Category: &category}, 6},
		{"category_limit", audit.ListAllParams{Category: &category, Limit: 4}, 4},
		{"account_window", audit.ListAllParams{Category: &category, AccountID: account.String()}, 4},
		{"account_window_limit", audit.ListAllParams{Category: &category, AccountID: account.String(), Limit: 3}, 3},
		{"other_category", audit.ListAllParams{Category: &other}, 1},
		{"run", audit.ListAllParams{Category: &category, RunID: &accountRun}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			real, err := auditRepo.ListAll(ctx, tc.p)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			got, err := faithful.ListAll(ctx, tc.p)
			if err != nil {
				t.Fatalf("wrapper: %v", err)
			}
			if len(real) != tc.want {
				t.Fatalf("store rows = %d, want %d", len(real), tc.want)
			}
			if !slices.EqualFunc(real, got, func(a, b *audit.Entry) bool { return a.ID == b.ID }) {
				t.Fatalf("wrapper order differs from the store: %v versus %v", entryIDs(got), entryIDs(real))
			}
		})
	}
}

func entryIDs(rows []*audit.Entry) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func constantWindow(window time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return window }
}

func TestDiscoverTerminalRunRounds_RealStoreOrderingAndCap_PG(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, ages ...time.Duration) (*Server, []uuid.UUID) {
		t.Helper()
		s, runRepo, auditRepo := newPGReconcileServer(t, time.Now().Add(time.Hour))
		ids := make([]uuid.UUID, 0, len(ages))
		for _, age := range ages {
			runID, stageID := seedPGReviewRun(t, runRepo)
			appendPGAudit(t, auditRepo, runID, &stageID, terminalRoundAge(age), "plan_review_started", advisoryPlanStarted())
			ids = append(ids, runID)
		}
		return s, ids
	}
	candidateRuns := func(scan terminalRoundScan) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(scan.candidates))
		for _, c := range scan.candidates {
			out = append(out, c.runID)
		}
		return out
	}
	t.Run("cap_reached_inside_bound", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("discovery-page contract must return normally; recovered %v", recovered)
			}
		}()
		s, ids := seed(t, 3*time.Hour, 2*time.Hour, time.Hour)
		scan, err := s.discoverTerminalRunRoundsCapped(ctx, "", 2, constantWindow(terminalRoundDiscoveryBound))
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		// Newest first, and the limit dropped the oldest row, which the store orders last.
		if got, want := candidateRuns(scan), []uuid.UUID{ids[2], ids[1]}; !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want the two newest %v", got, want)
		}
		if !scan.truncated {
			t.Error("truncated = false, want true: the capped page's oldest row is inside the lookback")
		}
	})
	t.Run("cap_reached_past_bound", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("discovery-page contract must return normally; recovered %v", recovered)
			}
		}()
		s, ids := seed(t, 30*time.Hour, 26*time.Hour, time.Hour)
		scan, err := s.discoverTerminalRunRoundsCapped(ctx, "", 2, constantWindow(terminalRoundDiscoveryBound))
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		if got, want := candidateRuns(scan), []uuid.UUID{ids[2]}; !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
		if scan.truncated {
			t.Error("truncated = true, want false: the capped page already reaches past the lookback")
		}
	})
	t.Run("below_cap", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("discovery-page contract must return normally; recovered %v", recovered)
			}
		}()
		s, ids := seed(t, 3*time.Hour, time.Hour)
		scan, err := s.discoverTerminalRunRoundsCapped(ctx, "", 5, constantWindow(terminalRoundDiscoveryBound))
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		if got, want := candidateRuns(scan), []uuid.UUID{ids[1], ids[0]}; !slices.Equal(got, want) {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
		if scan.truncated {
			t.Error("truncated = true below the cap")
		}
	})
}

func TestDiscoverTerminalRunRounds_Identity(t *testing.T) {
	type fx struct {
		f    *redispatchFixture
		repo *consumerRunRepo
	}
	build := func(t *testing.T) fx {
		t.Helper()
		f := newRedispatchFixture(t, redispatchFixtureOpts{})
		repo := newConsumerRunRepo(f.rr)
		f.s.cfg.RunRepo = repo
		f.s.cfg.AuditRepo = newFaithfulListAllAudit(f.au)
		return fx{f, repo}
	}
	scan := func(t *testing.T, x fx, window func(int) time.Duration) terminalRoundScan {
		t.Helper()
		got, err := x.f.s.discoverTerminalRunRounds(context.Background(), "", window)
		if err != nil {
			t.Fatalf("discover: %v", err)
		}
		if n := x.repo.totalReads(); n != 0 {
			t.Errorf("run reads = %d, want 0: discovery reads no run", n)
		}
		return got
	}
	t.Run("newest_row_per_run_stage_wins", func(t *testing.T) {
		x := build(t)
		x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(2*time.Hour), "plan_review_started", advisoryPlanStarted())
		newest := x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
		implement := x.f.appendAt(t, x.f.implStage.ID, terminalRoundAge(time.Hour), "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", "h"))
		got := scan(t, x, constantWindow(terminalRoundDiscoveryBound))
		if len(got.candidates) != 2 {
			t.Fatalf("candidates = %+v, want the two kinds' newest", got.candidates)
		}
		for _, c := range got.candidates {
			want := newest
			wantAgents := 2
			if c.stage.isImplement {
				want, wantAgents = implement, 1
			}
			if c.runID != x.f.runRow.ID || c.sequence != want || c.configured != wantAgents {
				t.Errorf("candidate %+v, want sequence %d with %d agents", c, want, wantAgents)
			}
		}
	})
	t.Run("superseded_row_is_dropped_even_when_only_it_is_in_window", func(t *testing.T) {
		x := build(t)
		// A later-sequenced row wins whatever its timestamp, so an old round cannot
		// hide behind an earlier row's fresher time.
		x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
		x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(30*time.Hour), "plan_review_started", advisoryPlanStarted())
		if got := scan(t, x, constantWindow(terminalRoundDiscoveryBound)); len(got.candidates) != 0 {
			t.Fatalf("candidates = %+v, want none", got.candidates)
		}
	})
	t.Run("undecodable_payload_stays_candidate", func(t *testing.T) {
		x := build(t)
		x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", "this payload is a string")
		var saw []int
		got := scan(t, x, func(configured int) time.Duration {
			saw = append(saw, configured)
			return terminalRoundDiscoveryBound
		})
		if len(got.candidates) != 1 || got.candidates[0].configured != 0 {
			t.Fatalf("candidates = %+v, want the row with configured 0", got.candidates)
		}
		if !slices.Equal(saw, []int{0}) {
			t.Errorf("window saw %v, want configured 0", saw)
		}
	})
	t.Run("nil_run_id_skipped", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("nil-run-id contract must return normally; recovered %v", recovered)
			}
		}()
		x := build(t)
		x.f.au.seeded = append(x.f.au.seeded, &audit.Entry{ID: uuid.New(), Category: "plan_review_started", Timestamp: terminalRoundAge(time.Hour)})
		if got := scan(t, x, constantWindow(terminalRoundDiscoveryBound)); len(got.candidates) != 0 {
			t.Fatalf("candidates = %+v, want none", got.candidates)
		}
	})
	t.Run("expired_row_dropped", func(t *testing.T) {
		x := build(t)
		x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(25*time.Hour), "plan_review_started", advisoryPlanStarted())
		if got := scan(t, x, constantWindow(terminalRoundDiscoveryBound)); len(got.candidates) != 0 {
			t.Fatalf("candidates = %+v, want none", got.candidates)
		}
	})
	t.Run("expired_row_kept_while_a_pending_handoff_names_it", func(t *testing.T) {
		x := build(t)
		seq := x.f.appendAt(t, x.f.planStage.ID, terminalRoundAge(25*time.Hour), "plan_review_started", advisoryPlanStarted())
		key := pendingRedispatchKey{runID: x.f.runRow.ID, stage: "plan", seq: seq}
		markRedispatchPending(key)
		t.Cleanup(func() { clearRedispatchPending(key) })
		if got := scan(t, x, constantWindow(terminalRoundDiscoveryBound)); len(got.candidates) != 1 || got.candidates[0].sequence != seq {
			t.Fatalf("candidates = %+v, want the pending round", got.candidates)
		}
	})
	t.Run("list_error", func(t *testing.T) {
		x := build(t)
		x.f.au.listAllErrCategory = "implement_review_started"
		if _, err := x.f.s.discoverTerminalRunRounds(context.Background(), "", constantWindow(terminalRoundDiscoveryBound)); err == nil {
			t.Fatal("discover swallowed the list error")
		}
	})
}

// --- ADR-140 (#5047): the boot sweep over terminal runs ---------------------------

// newTerminalSweepFixture is a restarted server whose boot marker follows every
// round seeded relative to the wall clock, over the faithful ListAll.
func newTerminalSweepFixture(t *testing.T, opts redispatchFixtureOpts) (*redispatchFixture, *faithfulListAllAudit) {
	t.Helper()
	f := newRedispatchFixture(t, opts)
	faithful := newFaithfulListAllAudit(f.au)
	f.s.cfg.AuditRepo = faithful
	f.s.processStart = time.Now().Add(time.Hour)
	return f, faithful
}

func TestReconcileOrphanedReviews_TerminalRun_NoConsumer_ClosedFailed(t *testing.T) {
	for _, state := range noConsumerArms() {
		for _, arm := range roundRefusalArms() {
			t.Run(state.name+"/"+arm.name, func(t *testing.T) {
				f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), reviewerUnwired: arm.unwired})
				seq := seedRefusalRound(t, f, arm)
				before := len(redispatchedRows(t, f.au, f.runRow.ID))
				state.arrange(f)
				bootSweep(t, f.s)
				want := orphanedReviewNotRedispatchedReason(redispatchSlugRunTerminal)
				if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
					t.Fatalf("reasons = %v, want one %q", got, want)
				}
				if n := len(redispatchedRows(t, f.au, f.runRow.ID)); n != before {
					t.Errorf("%s rows = %d, want %d: a no-consumer round is never re-dispatched", categoryReviewRoundRedispatched, n, before)
				}
			})
		}
	}
}

func TestReconcileOrphanedReviews_TerminalRun_Failed_KeepsExistingEligibility(t *testing.T) {
	t.Run("eligible_redispatched", func(t *testing.T) {
		f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
		f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
		f.seedRedactedBundle(t, implementPushGatedBundleWithHeadSHA(t, 2, terminalRoundHead))
		seq := seedRefusalRound(t, f, roundRefusalArm{})
		f.runRow.State = run.StateFailed
		bootSweep(t, f.s)
		assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[1], seq, 1)
	})
	t.Run("implement_stage_terminal", func(t *testing.T) {
		f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), implStageState: run.StageStateFailed})
		seq := seedRefusalRound(t, f, roundRefusalArm{})
		f.runRow.State = run.StateFailed
		bootSweep(t, f.s)
		want := orphanedReviewNotRedispatchedReason(redispatchSlugImplementStageTerminal)
		if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
			t.Fatalf("reasons = %v, want one %q", got, want)
		}
	})
	for _, arm := range roundRefusalArms()[1:] {
		t.Run(arm.name, func(t *testing.T) {
			f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), reviewerUnwired: arm.unwired})
			seq := seedRefusalRound(t, f, arm)
			f.runRow.State = run.StateFailed
			bootSweep(t, f.s)
			want := orphanedReviewNotRedispatchedReason(arm.want)
			if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
				t.Fatalf("reasons = %v, want one %q", got, want)
			}
		})
	}
}

func TestReconcileOrphanedReviews_TerminalRun_Failed_ReviveStillApplies(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), implStageState: run.StageStateFailed})
	category, reason := run.FailureA, "the implement agent crashed"
	f.implStage.FailureCategory, f.implStage.FailureReason = &category, &reason
	seq := seedRefusalRound(t, f, roundRefusalArm{})
	f.runRow.State = run.StateFailed
	bootSweep(t, f.s)
	want := orphanedReviewNotRedispatchedReason(redispatchSlugImplementStageTerminal)
	if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
		t.Fatalf("reasons = %v, want one %q", got, want)
	}
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.rr, Audit: f.au}
	if w := postRevive(t, f.s, f.runRow.ID, withAuth); w.Code != http.StatusOK {
		t.Fatalf("revive status = %d:\n%s", w.Code, w.Body.String())
	}
	revived, err := f.rr.GetRun(context.Background(), f.runRow.ID)
	if err != nil || revived.State != run.StateRunning {
		t.Fatalf("run after revive = %+v, %v, want running", revived, err)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_ConsumerLive_KeepsExistingClosure(t *testing.T) {
	for _, arm := range roundRefusalArms()[1:] {
		t.Run(arm.name, func(t *testing.T) {
			f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), reviewerUnwired: arm.unwired})
			seq := seedRefusalRound(t, f, arm)
			childOf(f, run.StateSucceeded, run.StateRunning)
			bootSweep(t, f.s)
			want := orphanedReviewNotRedispatchedReason(arm.want)
			if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
				t.Fatalf("reasons = %v, want one %q", got, want)
			}
		})
	}
}

func TestReconcileOrphanedReviews_TerminalRun_Eligible_Redispatched(t *testing.T) {
	for _, state := range consumerArms() {
		t.Run(state.name, func(t *testing.T) {
			f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
			f.seedPlanArtifact(t, validPlanBytes(t), "approved-plan", true)
			f.seedRedactedBundle(t, implementPushGatedBundleWithHeadSHA(t, 2, terminalRoundHead))
			seq := seedRefusalRound(t, f, roundRefusalArm{})
			state.arrange(f)
			bootSweep(t, f.s)
			got := assertRedispatchedRound(t, f.au, f.runRow.ID, orphanedReviewStages[1], seq, 1)
			if got.HeadSHA != terminalRoundHead || got.RoundOrigin != reviewRoundOriginTrace {
				t.Errorf("new round = head %q origin %q", got.HeadSHA, got.RoundOrigin)
			}
		})
	}
}

// TestReconcileOrphanedReviews_TerminalRun_MixedRounds_ExpiredUntouched: a settled
// plan round inside the lookback makes the run a candidate, and the boot pass walks
// every stage of an admitted run. Only the admission keeps the expired implement
// round untouched.
func TestReconcileOrphanedReviews_TerminalRun_MixedRounds_ExpiredUntouched(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	implementSeq := f.appendAt(t, f.implStage.ID, terminalRoundAge(25*time.Hour), "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead))
	planSeq := f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	for range 2 {
		f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_reviewed", map[string]any{})
	}
	f.runRow.State = run.StateCancelled
	bootSweep(t, f.s)
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_failed", implementSeq); n != 0 {
		t.Fatalf("implement_review_failed = %d, want 0: an expired round stays untouched", n)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "plan_review_failed", planSeq); n != 0 {
		t.Errorf("plan_review_failed = %d, want 0 on a settled round", n)
	}
	if n := len(redispatchedRows(t, f.au, f.runRow.ID)); n != 0 {
		t.Errorf("redispatched rows = %d, want 0", n)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_OutsideLookback_Untouched(t *testing.T) {
	f, faithful := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	seq := f.appendAt(t, f.implStage.ID, terminalRoundAge(25*time.Hour), "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead))
	f.runRow.State = run.StateCancelled
	bootSweep(t, f.s)
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); n != 0 {
		t.Fatalf("implement_review_failed = %d, want 0 past the lookback", n)
	}
	calls := faithful.recorded()
	if len(calls) != len(orphanedReviewStages) {
		t.Fatalf("ListAll calls = %d, want one per review kind", len(calls))
	}
	for _, call := range calls {
		if call.Limit != terminalRoundScanLimit || call.AccountID != "" || call.Category == nil {
			t.Errorf("ListAll params = %+v, want limit %d, no account and a category", call, terminalRoundScanLimit)
		}
	}
}

func TestReconcileOrphanedReviews_TerminalRun_CurrentProcessPreserved(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	f.s.processStart = time.Now().Add(-2 * time.Hour)
	seq := seedRefusalRound(t, f, roundRefusalArm{})
	f.runRow.State = run.StateCancelled
	bootSweep(t, f.s)
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); n != 0 {
		t.Fatalf("implement_review_failed = %d, want 0: this process's round is its own", n)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_ListAllErrorAborts(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{reviewerUnwired: true})
	// The live run's orphaned round is the state scan's effect, kept on the abort.
	seq := f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	f.au.listAllErrCategory = "plan_review_started"
	terminated, err := f.s.ReconcileOrphanedReviews(context.Background())
	f.s.waitBackgroundReviews()
	if err == nil || !strings.Contains(err.Error(), "terminal-run rounds") {
		t.Fatalf("err = %v, want the systemic list failure", err)
	}
	if terminated != 1 {
		t.Errorf("terminated = %d, want the state scan's 1", terminated)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "plan_review_failed", seq); n != 2 {
		t.Errorf("plan_review_failed = %d, want the state scan's 2", n)
	}
}

// addTerminalRoundRun seeds a further cancelled run with an in-window round.
func addTerminalRoundRun(t *testing.T, f *redispatchFixture) (*run.Run, *run.Stage) {
	t.Helper()
	ru := f.rr.seedRun()
	ru.State = run.StateCancelled
	stage := f.rr.seedStage(ru.ID, 0, run.StageStateCancelled)
	stage.Type = run.StageTypeImplement
	raw, err := json.Marshal(advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead))
	if err != nil {
		t.Fatalf("setup: marshal: %v", err)
	}
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: ru.ID, StageID: &stage.ID, Timestamp: terminalRoundAge(time.Hour), Category: "implement_review_started", Payload: raw}); err != nil {
		t.Fatalf("setup: append: %v", err)
	}
	return ru, stage
}

func TestReconcileOrphanedReviews_TerminalRun_GetRunErrorIsolated(t *testing.T) {
	var logs syncBuffer
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), logOut: &logs})
	seq := seedRefusalRound(t, f, roundRefusalArm{})
	f.runRow.State = run.StateCancelled
	other, _ := addTerminalRoundRun(t, f)
	repo := newConsumerRunRepo(f.rr)
	fault := errors.New("injected unknown-run fault")
	repo.fail(f.runRow.ID, fault)
	f.s.cfg.RunRepo = repo
	bootSweep(t, f.s)
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); n != 0 {
		t.Fatalf("implement_review_failed = %d, want 0: unknown is never proof", n)
	}
	if got := failedReasonsAfter(t, f.au, other.ID, "implement_review_failed", 0); len(got) != 1 {
		t.Errorf("the other run's closures = %v, want it still swept", got)
	}
	out := logs.String()
	if !strings.Contains(out, f.runRow.ID.String()) || !strings.Contains(out, fault.Error()) {
		t.Errorf("no WARN naming the run and its cause:\n%s", out)
	}
	if !strings.Contains(out, "failed=1") {
		t.Errorf("the unknown run is not counted failed:\n%s", out)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_RunMissingIsolated(t *testing.T) {
	var logs syncBuffer
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore(), logOut: &logs})
	missing := uuid.New()
	stageID := uuid.New()
	raw, err := json.Marshal(advisoryPlanStarted())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: missing, StageID: &stageID, Timestamp: terminalRoundAge(time.Hour), Category: "plan_review_started", Payload: raw}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	other, _ := addTerminalRoundRun(t, f)
	bootSweep(t, f.s)
	if got := failedReasonsAfter(t, f.au, missing, "plan_review_failed", 0); len(got) != 0 {
		t.Fatalf("closures on a missing run = %v, want none", got)
	}
	if got := failedReasonsAfter(t, f.au, other.ID, "implement_review_failed", 0); len(got) != 1 {
		t.Errorf("the other run's closures = %v, want it still swept", got)
	}
	if out := logs.String(); !strings.Contains(out, missing.String()) || !strings.Contains(out, "failed=1") {
		t.Errorf("no WARN naming the missing run:\n%s", out)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_ParentUnreadable_EligibilityCheckFailed(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	seq := seedRefusalRound(t, f, roundRefusalArm{})
	parent := f.rr.seedRun()
	f.runRow.State = run.StateSucceeded
	f.runRow.DecomposedFrom = &parent.ID
	repo := newConsumerRunRepo(f.rr)
	repo.fail(parent.ID, errors.New("injected parent read fault"))
	f.s.cfg.RunRepo = repo
	bootSweep(t, f.s)
	want := orphanedReviewNotRedispatchedReason(redispatchSlugEligibilityCheckFailed)
	if got := failedReasonsAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); !slices.Equal(got, []string{want}) {
		t.Fatalf("reasons = %v, want one %q", got, want)
	}
}

func TestReconcileOrphanedReviews_TerminalRun_Truncated(t *testing.T) {
	const warning = "terminal-run discovery reached its row cap"
	seedRows := func(t *testing.T, f *redispatchFixture, n int) {
		t.Helper()
		for range n {
			f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
		}
	}
	t.Run("cap_reached_warns_naming_the_limit", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("discovery-page contract must return normally; recovered %v", recovered)
			}
		}()
		var logs syncBuffer
		f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{reviewerUnwired: true, logOut: &logs})
		seedRows(t, f, terminalRoundScanLimit)
		f.runRow.State = run.StateCancelled
		bootSweep(t, f.s)
		out := logs.String()
		if !strings.Contains(out, warning) || !strings.Contains(out, fmt.Sprintf("limit=%d", terminalRoundScanLimit)) {
			t.Fatalf("no truncation WARN naming the limit:\n%s", out)
		}
		if n := len(failedReasonsAfter(t, f.au, f.runRow.ID, "plan_review_failed", 0)); n != 2 {
			t.Errorf("plan_review_failed = %d, want the newest round closed", n)
		}
	})
	t.Run("below_cap_does_not_warn", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("discovery-page contract must return normally; recovered %v", recovered)
			}
		}()
		var logs syncBuffer
		f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{reviewerUnwired: true, logOut: &logs})
		seedRows(t, f, terminalRoundScanLimit-1)
		f.runRow.State = run.StateCancelled
		bootSweep(t, f.s)
		if out := logs.String(); strings.Contains(out, warning) {
			t.Fatalf("a truncation WARN below the cap:\n%s", out)
		}
	})
}

func TestReconcileOrphanedReviews_TerminalRun_PendingHandoffNotClosed(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	seq := f.appendAt(t, f.implStage.ID, terminalRoundAge(25*time.Hour), "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead))
	f.runRow.State = run.StateCancelled
	key := pendingRedispatchKey{runID: f.runRow.ID, stage: "implement", seq: seq}
	markRedispatchPending(key)
	t.Cleanup(func() { clearRedispatchPending(key) })
	bootSweep(t, f.s)
	if n := countAfter(t, f.au, f.runRow.ID, "implement_review_failed", seq); n != 0 {
		t.Fatalf("implement_review_failed = %d, want 0 under a pending handoff", n)
	}
}

// TestReconcileOrphanedReviews_TerminalRun_LiveCandidateOwnedByStateScan isolates the
// non-terminal skip: the run was off the state pages, then revived before discovery
// read it, so only that skip keeps the terminal pass off a live run.
func TestReconcileOrphanedReviews_TerminalRun_LiveCandidateOwnedByStateScan(t *testing.T) {
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{reviewerUnwired: true})
	seq := f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	f.runRow.State = run.StateCancelled
	f.s.cfg.RunRepo = revivedAtReadRepo{f.rr}
	terminated, err := f.s.ReconcileOrphanedReviews(context.Background())
	f.s.waitBackgroundReviews()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if terminated != 0 {
		t.Errorf("terminated = %d, want 0", terminated)
	}
	if n := countAfter(t, f.au, f.runRow.ID, "plan_review_failed", seq); n != 0 {
		t.Errorf("plan_review_failed = %d, want 0 on a run the state scan owns", n)
	}
}

// revivedAtReadRepo lists the store's state, while every GetRun reads running.
type revivedAtReadRepo struct{ *orchestratorRepo }

func (r revivedAtReadRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	got, err := r.orchestratorRepo.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	got.State = run.StateRunning
	return got, nil
}

func TestAdmitDiscoveredRounds_Modes(t *testing.T) {
	runID := uuid.New()
	plan, implement := orphanedReviewStages[0], orphanedReviewStages[1]
	admit := admitDiscoveredRounds(runID, map[string]struct{}{"implement": {}})
	recent := &audit.Entry{Sequence: 1, Timestamp: terminalRoundAge(time.Hour)}
	expired := &audit.Entry{Sequence: 2, Timestamp: terminalRoundAge(25 * time.Hour)}
	var none planreview.ReviewStartedPayload
	if admit(plan, recent, none) {
		t.Error("admitted a stage discovery did not name")
	}
	if !admit(implement, recent, none) {
		t.Error("refused a discovered round inside the window")
	}
	if admit(implement, expired, none) {
		t.Error("admitted an expired actual round")
	}
	key := pendingRedispatchKey{runID: runID, stage: "implement", seq: expired.Sequence}
	markRedispatchPending(key)
	t.Cleanup(func() { clearRedispatchPending(key) })
	if !admit(implement, expired, none) {
		t.Error("refused an expired round mid-handoff")
	}
}

func TestReconcileOrphanedReviews_TerminalRun_PerRunAuditErrorIsolated(t *testing.T) {
	var logs syncBuffer
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{logOut: &logs})
	f.appendAt(t, f.planStage.ID, terminalRoundAge(time.Hour), "plan_review_started", advisoryPlanStarted())
	f.runRow.State = run.StateCancelled
	f.au.listByCategoryErrCategory = "plan_review_started"
	if _, err := f.s.ReconcileOrphanedReviews(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	f.s.waitBackgroundReviews()
	if out := logs.String(); !strings.Contains(out, "skipped run on error") || !strings.Contains(out, "failed=1") {
		t.Fatalf("the run's read error is not logged and counted:\n%s", out)
	}
	if n := countAuditCategory(f.au, "plan_review_failed"); n != 0 {
		t.Errorf("plan_review_failed = %d, want 0", n)
	}
}

func TestReconcileSkipNotAdmitted_ReachedOnlyAtBoot(t *testing.T) {
	// The on-demand verb's skip_reason enum never carries the boot-only reason.
	f, _ := newTerminalSweepFixture(t, redispatchFixtureOpts{traceStore: newRedispatchTraceStore()})
	f.appendAt(t, f.implStage.ID, terminalRoundAge(25*time.Hour), "implement_review_started", advisoryImplementStarted(reviewRoundOriginTrace, "", terminalRoundHead))
	out, err := f.s.reconcileRunOrphanedReviews(context.Background(), f.runRow.ID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, st := range out {
		if st.SkipReason == reconcileSkipNotAdmitted {
			t.Errorf("the terminate-only verb reported %q", st.SkipReason)
		}
	}
	if n := countAuditCategory(f.au, "implement_review_failed"); n != 1 {
		t.Errorf("implement_review_failed = %d, want the verb to act on an older round", n)
	}
	stages, err := f.s.reconcileAdmittedRoundsForBoot(context.Background(), f.runRow.ID, func(orphanedReviewStageKind, *audit.Entry, planreview.ReviewStartedPayload) bool { return false })
	if err != nil {
		t.Fatalf("boot pass: %v", err)
	}
	refused := 0
	for _, st := range stages {
		if st.SkipReason == reconcileSkipNotAdmitted {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("not-admitted stages = %d in %+v, want the implement round's one", refused, stages)
	}
}

// TestReviewRedispatch_DaemonRestart_TerminalChildRoundCompletes is the
// cross-boundary restart proof: process A dispatches a real trace-origin round on
// a decomposition child, the child succeeds under a running parent, A's Shutdown
// kills the reviewer with no terminal row, and process B's boot sweep discovers
// the round, which the state scan cannot see, and re-dispatches it. The verdict's
// concern reaches the parent through the late-review import.
func TestReviewRedispatch_DaemonRestart_TerminalChildRoundCompletes(t *testing.T) {
	rr := newOrchestratorRepo()
	art := newFakeArtifactRepo()
	au := newAuditFake()
	au.stampSequence = true
	faithful := newFaithfulListAllAudit(au)
	sf := newSigningFake()
	ts := newRedispatchTraceStore()
	concerns := newFakeConcernRepo()

	parent := rr.seedRun()
	parent.Repo = "kuhlman-labs/example"
	parentImpl := rr.seedStage(parent.ID, 0, run.StageStateAwaitingChildren)
	parentImpl.Type = run.StageTypeImplement

	child := rr.seedRun()
	child.WorkflowID = "feature_change"
	child.WorkflowSpec = specRedispatchAdvisory
	child.Repo = parent.Repo
	child.DecomposedFrom = &parent.ID
	sliceIndex := 0
	child.SliceIndex = &sliceIndex
	planStage := rr.seedStage(child.ID, 0, run.StageStateSucceeded)
	version := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &version, Content: validPlanBytes(t), ContentHash: "approved-plan"}); err != nil {
		t.Fatalf("setup: seed plan: %v", err)
	}
	implStage := rr.seedStage(child.ID, 1, run.StageStateDispatched)
	implStage.Type = run.StageTypeImplement
	implStage.RequiresApproval = true

	blocked := newBlockingReviewer()
	a := New(Config{Addr: "127.0.0.1:0", SigningRepo: sf, TraceStore: ts, AuditRepo: faithful, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{blocked}, ShutdownTimeout: boundedWait})
	t.Cleanup(blocked.unblock)
	priv, _ := sf.issue(t, child.ID)
	const head = "terminal-child-head"
	bundleBytes := implementPushGatedBundleWithHeadSHA(t, 2, head)
	for _, variant := range []string{"raw", "redacted"} {
		if w := shipRequest(t, a, child.ID, implStage.ID, variant, priv, bundleBytes, ""); w.Code != http.StatusAccepted {
			t.Fatalf("setup: server A %s upload = %d:\n%s", variant, w.Code, w.Body.String())
		}
	}
	orphaned := redispatchEntries(t, au, child.ID, "implement_review_started")
	if len(orphaned) != 1 {
		t.Fatalf("setup: started rows = %d, want 1", len(orphaned))
	}
	rr.mu.Lock()
	rr.runs[child.ID].State = run.StateSucceeded
	rr.mu.Unlock()

	// An eligible round with a live consumer is recoverable, so it blocks no reload.
	req := httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil)
	blockers := httptest.NewRecorder()
	a.Handler().ServeHTTP(blockers, req)
	if blockers.Code != http.StatusOK {
		t.Fatalf("restart-blockers = %d:\n%s", blockers.Code, blockers.Body.String())
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(blockers.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode blockers: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("blockers = %s, want none for an eligible round", blockers.Body.String())
	}

	setVar(t, &reviewerShutdownDrain, 50*time.Millisecond)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), boundedWait)
	defer cancel()
	if err := a.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("server A Shutdown: %v", err)
	}
	for _, category := range orphanedReviewStages[1].terminals {
		if n := countAuditCategory(au, category); n != 0 {
			t.Fatalf("%s rows = %d after the kill, want none", category, n)
		}
	}

	completing := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{
		Verdict:  planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{{Severity: planreview.SeverityMedium, Category: "correctness", Note: "a slice round's medium concern"}},
	}, model: "claude-opus-4-8"}
	b := New(Config{Addr: "127.0.0.1:0", TraceStore: ts, AuditRepo: faithful, RunRepo: rr, ArtifactRepo: art, PlanReviewers: singleReviewerSet{completing}, ConcernRepo: concerns, ProcessStart: time.Now().Add(time.Millisecond)})
	bootSweep(t, b)

	assertRedispatchedRound(t, au, child.ID, orphanedReviewStages[1], orphaned[0].Sequence, 1)
	rows, err := au.ListForRunByCategory(context.Background(), parent.ID, CategoryConcernRolledUp)
	if err != nil {
		t.Fatalf("list roll-ups: %v", err)
	}
	if len(rows) != 1 || !strings.Contains(string(rows[0].Payload), `"site":"`+concernRollupSiteChildReview+`"`) {
		t.Fatalf("concern_rolled_up rows = %d, want one at site child_review", len(rows))
	}
	copies, err := concerns.ListByRun(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("list parent concerns: %v", err)
	}
	if len(copies) != 1 || copies[0].Note != "a slice round's medium concern" {
		t.Errorf("parent concerns = %+v, want the imported copy", copies)
	}
}

// --- ADR-140 (#5047): whole-route suites over the pull-request handler ------------

// startRowsFor counts this stage's start rows for one attempt.
func (p *prSeam) startRowsFor(t *testing.T, attempt string) int {
	t.Helper()
	rows, err := p.au.ListForRunByCategory(t.Context(), p.runRow.ID, CategorySupplementalReviewStarted)
	if err != nil {
		t.Fatalf("list start rows: %v", err)
	}
	n := 0
	for _, row := range rows {
		var started struct {
			StageAttempt string `json:"stage_attempt"`
		}
		if err := json.Unmarshal(row.Payload, &started); err != nil {
			t.Fatalf("decode start row: %v", err)
		}
		if started.StageAttempt == attempt {
			n++
		}
	}
	return n
}

// startAllocations counts every atomic start append attempted.
func (p *prSeam) startAllocations() int {
	p.audit.mu.Lock()
	defer p.audit.mu.Unlock()
	return p.audit.dedups
}

// retryAt moves the stage to a new running attempt stamped exactly at.
func (p *prSeam) retryAt(at time.Time) string {
	p.rr.mu.Lock()
	defer p.rr.mu.Unlock()
	st := p.rr.stagesByID[p.stage.ID]
	st.State = run.StageStateRunning
	st.DispatchedAt = &at
	st.FailureCategory, st.FailureReason = nil, nil
	return run.StageAttemptToken(&at)
}

func assertNoLiveHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Header().Get(reviewPassRemainingHeader) != "" || w.Header().Get("Retry-After") != "" {
		t.Errorf("response carries live-pass headers: %v", w.Header())
	}
}

// killOwner posts body with a blocked reviewer, closes the lifetime and asserts the
// owner's 503.
func killOwner(t *testing.T, p *prSeam, body []byte, started <-chan struct{}) {
	t.Helper()
	done := p.postAsync(t.Context(), body)
	waitFor(t, started, "setup: blocked reviewer")
	p.s.reviewers.closeAndCancel()
	assertInterrupted(t, awaitResponse(t, done, "owner"), http.StatusServiceUnavailable)
}

// TestShipPullRequest_LongPass_ReplayContract is the server half of the long-pass
// continuation: replays while the owner lives are told to wait, by headers only a
// live answer carries, and the owner's edge decides once.
func TestShipPullRequest_LongPass_ReplayContract(t *testing.T) {
	liveReplays := func(t *testing.T, p *prSeam, body []byte) {
		t.Helper()
		previous := int64(math.MaxInt64)
		for range 3 {
			w := p.post(t, body)
			assertCode(t, w, http.StatusServiceUnavailable, "review_round_in_flight")
			retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
			if err != nil || retryAfter < 1 {
				t.Errorf("Retry-After = %q, want at least 1", w.Header().Get("Retry-After"))
			}
			remaining, err := strconv.ParseInt(w.Header().Get(reviewPassRemainingHeader), 10, 64)
			if err != nil || remaining < 1 {
				t.Errorf("%s = %q, want at least 1", reviewPassRemainingHeader, w.Header().Get(reviewPassRemainingHeader))
			}
			if remaining > previous {
				t.Errorf("remaining grew from %d to %d", previous, remaining)
			}
			previous = remaining
		}
	}
	t.Run("approve", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep))
		body := p.body(t)
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: blocked reviewer")
		liveReplays(t, p, body)
		close(release)
		if w := awaitResponse(t, done, "owner"); w.Code != http.StatusCreated {
			t.Fatalf("owner = %d:\n%s", w.Code, w.Body.String())
		}
		if w := p.post(t, body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"idempotent":true`) {
			t.Fatalf("replay = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("reject", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, rejectStep))
		body := p.body(t)
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: blocked reviewer")
		liveReplays(t, p, body)
		close(release)
		if w := awaitResponse(t, done, "owner"); w.Code != http.StatusCreated {
			t.Fatalf("owner = %d:\n%s", w.Code, w.Body.String())
		}
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("replay = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedB(t)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("lost_next_process", func(t *testing.T) {
		started := make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, blockStep(started))
		body := p.body(t)
		killOwner(t, p, body, started)
		p.nextProcess(approveStep)
		w := p.post(t, body)
		assertInterrupted(t, w, http.StatusConflict)
		assertNoLiveHeaders(t, w)
		p.assertNothingMoved(t)
	})
	t.Run("closing", func(t *testing.T) {
		started := make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, blockStep(started))
		body := p.body(t)
		killOwner(t, p, body, started)
		w := p.post(t, body)
		assertInterrupted(t, w, http.StatusServiceUnavailable)
		assertNoLiveHeaders(t, w)
		p.assertNothingMoved(t)
	})
	t.Run("lifetime_end", func(t *testing.T) {
		started := make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, blockStep(started))
		shortLifetime(t, p)
		body := p.body(t)
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: blocked reviewer")
		assertInterrupted(t, awaitResponse(t, done, "owner"), http.StatusConflict)
		w := p.post(t, body)
		assertInterrupted(t, w, http.StatusConflict)
		assertNoLiveHeaders(t, w)
		p.assertNothingMoved(t)
	})
}

func TestShipPullRequest_ResumedSupplemental_ReentryOnce_ReplayRefused(t *testing.T) {
	resumed := func(t *testing.T, workflowSpec []byte, steps ...reviewStep) (*prSeam, []byte, string) {
		t.Helper()
		p := newPRSeam(t, workflowSpec, steps...)
		body := p.body(t)
		p.seedArtifact(t, body)
		return p, body, p.retry()
	}
	// The replay's own refusal comes from the recorded start: it never reaches
	// allocation, whose lost-race re-read would answer the same.
	replayRefused := func(t *testing.T, p *prSeam, body []byte, status int, attempt string) {
		t.Helper()
		allocations := p.startAllocations()
		w := p.post(t, body)
		if n := p.startAllocations(); n != allocations {
			t.Errorf("start allocations attempted by the replay = %d, want 0", n-allocations)
		}
		assertInterrupted(t, w, status)
		p.assertNothingMoved(t)
		if n := p.startRowsFor(t, attempt); n != 1 {
			t.Errorf("start rows for the attempt = %d, want 1", n)
		}
	}
	t.Run("next_process_409", func(t *testing.T) {
		started := make(chan struct{})
		p, body, attempt := resumed(t, specImplementGatingReviewers, blockStep(started))
		killOwner(t, p, body, started)
		p.nextProcess(approveStep)
		replayRefused(t, p, body, http.StatusConflict, attempt)
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("replay invoked %d reviewers, want 0", n)
		}
	})
	t.Run("closing_process_503", func(t *testing.T) {
		started := make(chan struct{})
		p, body, attempt := resumed(t, specImplementGatingReviewers, blockStep(started))
		killOwner(t, p, body, started)
		calls := p.rev.calls.Load()
		replayRefused(t, p, body, http.StatusServiceUnavailable, attempt)
		if n := p.rev.calls.Load(); n != calls {
			t.Errorf("replay invoked %d more reviewers", n-calls)
		}
	})
	t.Run("partial_pass_next_process_409", func(t *testing.T) {
		started := make(chan struct{})
		p, body, attempt := resumed(t, specImplementGatingTwo, approveStep, blockStep(started))
		killOwner(t, p, body, started)
		if n := countAuditCategory(p.au, implementReviewedCategory); n != 1 {
			t.Fatalf("setup: verdict rows = %d, want the first reviewer's one", n)
		}
		p.nextProcess(approveStep)
		replayRefused(t, p, body, http.StatusConflict, attempt)
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("replay invoked %d reviewers, want 0", n)
		}
	})
	t.Run("attempt_3_reenters_once", func(t *testing.T) {
		started := make(chan struct{})
		p, body, attempt := resumed(t, specImplementGatingReviewers, blockStep(started))
		killOwner(t, p, body, started)
		p.nextProcess(approveStep)
		assertInterrupted(t, p.post(t, body), http.StatusConflict)
		third := p.retry()
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("attempt 3 = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls on attempt 3 = %d, want the one pass", n)
		}
		if a, b := p.startRowsFor(t, attempt), p.startRowsFor(t, third); a != 1 || b != 1 {
			t.Errorf("start rows = %d and %d, want one per attempt", a, b)
		}
		p.assertAdvancedOnce(t)
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("replay on attempt 3 = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls after the replay = %d, want 1", n)
		}
		p.assertAdvancedOnce(t)
	})
}

func TestShipPullRequest_SupplementalGating_LateEarlierAttemptApproval(t *testing.T) {
	// late plays attempt 1's approval so it lands after attempt 2's DispatchedAt, with
	// a later row timestamp a time-based rule would count.
	late := func(t *testing.T, next reviewStep) (*prSeam, []byte, string) {
		t.Helper()
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep), next)
		body := p.body(t)
		first := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: attempt 1 reviewer")
		dispatched := time.Now().UTC()
		attempt := p.retryAt(dispatched)
		close(release)
		if w := awaitResponse(t, first, "attempt 1"); w.Code != http.StatusCreated {
			t.Fatalf("setup: attempt 1 = %d:\n%s", w.Code, w.Body.String())
		}
		rows, err := p.au.ListForRunByCategory(t.Context(), p.runRow.ID, implementReviewedCategory)
		if err != nil || len(rows) != 1 || !rows[0].Timestamp.After(dispatched) {
			t.Fatalf("setup: the late row must follow attempt 2's dispatch: rows=%d err=%v", len(rows), err)
		}
		if n := p.repo.advances.Load(); n != 0 {
			t.Fatalf("attempt 1's approval advanced attempt 2 (%d edges)", n)
		}
		state, err := p.s.supplementalRoundState(t.Context(), p.runRow.ID, p.stage.ID, prSeamHead, attempt)
		if err != nil || state.currentVerdicts != 0 {
			t.Fatalf("attempt 2's round state = %+v, %v: another attempt's verdict settles nothing", state, err)
		}
		return p, body, attempt
	}
	t.Run("new_reviewer_invoked_stage_held", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p, body, _ := late(t, heldStep(started, release, approveStep))
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "new reviewer")
		if n := p.rev.calls.Load(); n != 2 {
			t.Errorf("reviewer calls = %d, want the new attempt's", n)
		}
		if got := p.state(t); got != run.StageStateRunning {
			t.Errorf("stage = %q, want running while the new reviewer blocks", got)
		}
		close(release)
		awaitResponse(t, done, "attempt 2")
		p.assertAdvancedOnce(t)
	})
	t.Run("new_approve_advances", func(t *testing.T) {
		p, body, attempt := late(t, approveStep)
		verdictsAtEdge := int32(-1)
		p.repo.onTransition = func(ctx context.Context, to run.StageState) error {
			if to == run.StageStateAwaitingApproval {
				state, err := p.s.supplementalRoundState(ctx, p.runRow.ID, p.stage.ID, prSeamHead, attempt)
				if err != nil {
					return err
				}
				verdictsAtEdge = int32(state.currentVerdicts)
			}
			return nil
		}
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("attempt 2 = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t)
		if verdictsAtEdge != 1 {
			t.Errorf("attempt 2's verdicts at the edge = %d, want the new verdict sequenced first", verdictsAtEdge)
		}
	})
	t.Run("new_reject_fails_b", func(t *testing.T) {
		p, body, _ := late(t, rejectStep)
		p.post(t, body)
		p.assertFailedB(t)
	})
	t.Run("replay_before_new_verdict_in_flight", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p, body, _ := late(t, heldStep(started, release, approveStep))
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "new reviewer")
		assertCode(t, p.post(t, body), http.StatusServiceUnavailable, "review_round_in_flight")
		close(release)
		awaitResponse(t, done, "attempt 2")
		p.assertAdvancedOnce(t)
	})
}

func TestShipPullRequest_SupplementalGatingOwnership_Independence(t *testing.T) {
	// live holds A inside its reviewer. The next upload's reviewer is free to finish.
	live := func(t *testing.T) (*prSeam, []byte, <-chan *httptest.ResponseRecorder, func()) {
		t.Helper()
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep), approveStep)
		body := p.body(t)
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: A's reviewer")
		var once sync.Once
		releaseA := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseA)
		return p, body, done, releaseA
	}
	t.Run("other_stage", func(t *testing.T) {
		p, body, done, release := live(t)
		other := p.rr.seedStage(p.runRow.ID, 2, run.StageStateRunning)
		other.Type = run.StageTypeImplement
		other.RequiresApproval = true
		w := shipPRRequest(t, p.s, p.runRow.ID, other.ID, p.priv, body, "")
		assertNotInFlight(t, w)
		if w.Code != http.StatusCreated {
			t.Errorf("other stage = %d:\n%s", w.Code, w.Body.String())
		}
		release()
		awaitResponse(t, done, "A")
	})
	t.Run("other_head", func(t *testing.T) {
		p, _, done, release := live(t)
		w := p.post(t, runnerShapedSupplementalPRBody(t, prSeamOther, prSeamPath, "unchanged after rebase"))
		assertNotInFlight(t, w)
		if w.Code != http.StatusCreated {
			t.Errorf("other head = %d:\n%s", w.Code, w.Body.String())
		}
		release()
		awaitResponse(t, done, "A")
	})
	t.Run("other_attempt", func(t *testing.T) {
		p, body, done, release := live(t)
		p.retry()
		w := p.post(t, body)
		assertNotInFlight(t, w)
		if w.Code != http.StatusOK {
			t.Errorf("other attempt = %d:\n%s", w.Code, w.Body.String())
		}
		release()
		awaitResponse(t, done, "A")
		p.assertAdvancedOnce(t)
	})
	t.Run("failed_report_lands", func(t *testing.T) {
		p, _, done, release := live(t)
		failed, err := json.Marshal(map[string]any{"outcome": "failed", "category": "C", "reason": "gave up"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if w := p.post(t, failed); w.Code != http.StatusOK {
			t.Fatalf("failed report = %d:\n%s", w.Code, w.Body.String())
		}
		if got := p.state(t); got != run.StageStateFailed {
			t.Errorf("stage = %q, want failed", got)
		}
		release()
		awaitResponse(t, done, "A")
		if got := p.state(t); got != run.StageStateFailed {
			t.Errorf("stage after A's edge = %q, want it still failed", got)
		}
		if n := p.repo.advances.Load(); n != 0 {
			t.Errorf("successful edges = %d, want 0", n)
		}
	})
	for _, tc := range []struct {
		name string
		spec []byte
		body func(t *testing.T, p *prSeam) []byte
	}{
		{"advisory_unclaimed", specImplementAdvisoryReviewers, func(t *testing.T, p *prSeam) []byte { return p.body(t) }},
		{"no_exemptions_unclaimed", specImplementGatingReviewers, func(t *testing.T, p *prSeam) []byte { return p.plainBody(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPRSeam(t, tc.spec, approveStep)
			parking := newParkingArtifactRepo(t, p.art, "create")
			p.s.cfg.ArtifactRepo = parking
			body := tc.body(t, p)
			done := p.postAsync(t.Context(), body)
			waitFor(t, parking.reached, "setup: A parked at Create")
			assertNotInFlight(t, p.post(t, body))
			parking.open()
			assertNotInFlight(t, awaitResponse(t, done, "A"))
			// The advisory pass is detached; drain it before the seam's var
			// restores run, or its terminal write races them.
			p.s.waitBackgroundReviews()
		})
	}
}

// TestShutdown_GatingRoundInterrupted_NothingStranded_PullRequest is the pull-request
// arm of the nothing-stranded proof, over a real listener, a real Shutdown and a
// grounded review tree.
func TestShutdown_GatingRoundInterrupted_NothingStranded_PullRequest(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	repo, headSHA := gitFixtureRepo(t)
	started := make(chan struct{})
	reviewer := &groundingFakeReviewer{blockOnCtx: true, started: started}
	s, sf, _, rr, runRow, stage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	runRow.WorkingDir = repo
	stage.State = run.StageStateRunning
	copies := &stageCopyRepo{orchestratorRepo: rr}
	s.cfg.RunRepo = copies
	priv, _ := sf.issue(t, runRow.ID)
	body := runnerShapedSupplementalPRBody(t, headSHA, prSeamPath, "unchanged after rebase")
	code := servedUpload(t, s, fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", runRow.ID, stage.ID),
		hex.EncodeToString(ed25519.Sign(priv, signing.ComputeMessage(body))), "", body, started)
	if code != http.StatusServiceUnavailable {
		t.Errorf("client status = %d, want 503", code)
	}
	assertReviewerHeldExport(t, reviewer)
	if n := copies.advances.Load() + copies.fails.Load(); n != 0 {
		t.Errorf("terminal edges = %d, want 0", n)
	}
	got, err := copies.GetStage(t.Context(), stage.ID)
	if err != nil || got.State != run.StageStateRunning {
		t.Errorf("stage = %+v, %v, want running", got, err)
	}
	assertNothingStranded(t, s, runRow.ID, scratch)
	w := shipPRRequest(t, s, runRow.ID, stage.ID, priv, body, "")
	assertNotInFlight(t, w)
	assertInterrupted(t, w, http.StatusServiceUnavailable)
}

func TestShipPullRequest_SupplementalGatingLive_CompletionThenReplay(t *testing.T) {
	during := func(t *testing.T, p *prSeam, body []byte, started <-chan struct{}, release chan<- struct{}) {
		t.Helper()
		done := p.postAsync(t.Context(), body)
		waitFor(t, started, "setup: A's reviewer")
		assertCode(t, p.post(t, body), http.StatusServiceUnavailable, "review_round_in_flight")
		close(release)
		if w := awaitResponse(t, done, "A"); w.Code != http.StatusCreated {
			t.Fatalf("A = %d:\n%s", w.Code, w.Body.String())
		}
		w := p.post(t, body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"idempotent":true`) {
			t.Fatalf("B after = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
		if n := countAuditCategory(p.au, implementReviewedCategory); n != 1 {
			t.Errorf("verdict rows = %d, want 1", n)
		}
		if n := p.startRowsFor(t, p.attempt(t)); n != 1 {
			t.Errorf("start rows = %d, want 1", n)
		}
	}
	t.Run("approve", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep))
		during(t, p, p.body(t), started, release)
		p.assertAdvancedOnce(t)
	})
	t.Run("approve_gateless", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, approveStep))
		p.setGateless()
		during(t, p, p.body(t), started, release)
		if got := p.state(t); got != run.StageStateSucceeded {
			t.Errorf("stage = %q, want succeeded", got)
		}
		if n := p.repo.advances.Load(); n != 1 {
			t.Errorf("successful edges = %d, want 1", n)
		}
	})
	t.Run("reject", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, rejectStep))
		during(t, p, p.body(t), started, release)
		p.assertFailedB(t)
		if n := p.repo.fails.Load(); n != 1 {
			t.Errorf("failed edges = %d, want 1", n)
		}
	})
}

// parkingGetStageRepo parks the first GetStage after arming just after it captured
// its copy, so that handler resumes holding a stale stage.
type parkingGetStageRepo struct {
	*stageCopyRepo
	armed    atomic.Bool
	reached  chan struct{}
	release  chan struct{}
	openOnce sync.Once
}

func (r *parkingGetStageRepo) open() { r.openOnce.Do(func() { close(r.release) }) }

func (r *parkingGetStageRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	st, err := r.stageCopyRepo.GetStage(ctx, id)
	if r.armed.CompareAndSwap(true, false) {
		close(r.reached)
		<-r.release
	}
	return st, err
}

func TestShipPullRequest_SupplementalGatingLive_StaleStageRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final reviewStep
		check func(t *testing.T, p *prSeam)
	}{
		{"approve_gateless", approveStep, func(t *testing.T, p *prSeam) {
			if got := p.state(t); got != run.StageStateSucceeded {
				t.Errorf("stage = %q, want succeeded", got)
			}
		}},
		{"reject", rejectStep, func(t *testing.T, p *prSeam) { p.assertFailedB(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			p := newPRSeam(t, specImplementGatingReviewers, heldStep(started, release, tc.final), tc.final)
			p.setGateless()
			parking := &parkingGetStageRepo{stageCopyRepo: p.repo, reached: make(chan struct{}), release: make(chan struct{})}
			t.Cleanup(parking.open)
			p.s.cfg.RunRepo = parking
			body := p.body(t)
			a := p.postAsync(t.Context(), body)
			waitFor(t, started, "setup: A's reviewer")
			parking.armed.Store(true)
			b := p.postAsync(t.Context(), body)
			waitFor(t, parking.reached, "setup: B parked on its stale read")
			close(release)
			if w := awaitResponse(t, a, "A"); w.Code != http.StatusCreated {
				t.Fatalf("A = %d:\n%s", w.Code, w.Body.String())
			}
			parking.open()
			w := awaitResponse(t, b, "B")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"idempotent":true`) {
				t.Fatalf("B = %d:\n%s", w.Code, w.Body.String())
			}
			if n := p.repo.advances.Load() + p.repo.fails.Load(); n != 1 {
				t.Errorf("terminal transitions = %d, want 1", n)
			}
			if n := p.rev.calls.Load(); n != 1 {
				t.Errorf("reviewer calls = %d, want 1", n)
			}
			tc.check(t, p)
		})
	}
}

// --- ADR-140 (#5047): real-store route evidence -----------------------------------

// edgeParkRepo wraps the real run repository. It parks the next terminal store call
// of any shape at entry, after every outside-lock read and holding no lock, can fault
// one guarded edge, and counts every edge that landed.
type edgeParkRepo struct {
	run.Repository
	guarded  run.GuardedStageTransitioner
	cas      run.StageAttemptCASTransitioner
	stageCAS run.StageCASTransitioner
	armed    atomic.Bool
	reached  chan struct{}
	release  chan struct{}
	openOnce sync.Once
	fault    atomic.Pointer[error]
	advances atomic.Int32
	fails    atomic.Int32
	failedAt atomic.Pointer[string]
}

func newEdgeParkRepo(t *testing.T, inner run.Repository) *edgeParkRepo {
	t.Helper()
	guarded, ok := inner.(run.GuardedStageTransitioner)
	if !ok {
		t.Fatal("setup: the run repository lacks the guarded edge")
	}
	cas, ok := inner.(run.StageAttemptCASTransitioner)
	if !ok {
		t.Fatal("setup: the run repository lacks the attempt CAS")
	}
	stageCAS, ok := inner.(run.StageCASTransitioner)
	if !ok {
		t.Fatal("setup: the run repository lacks the stage CAS")
	}
	r := &edgeParkRepo{Repository: inner, guarded: guarded, cas: cas, stageCAS: stageCAS, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(r.open)
	return r
}

func (r *edgeParkRepo) open() { r.openOnce.Do(func() { close(r.release) }) }

// parkNext arms one park; the channels are single-use, so one test parks once.
func (r *edgeParkRepo) parkNext() { r.armed.Store(true) }

func (r *edgeParkRepo) failNext(err error) { r.fault.Store(&err) }

func (r *edgeParkRepo) count(to run.StageState) {
	switch to {
	case run.StageStateAwaitingApproval, run.StageStateSucceeded:
		r.advances.Add(1)
	case run.StageStateFailed:
		r.fails.Add(1)
	}
}

func (r *edgeParkRepo) park(ctx context.Context) {
	if r.armed.CompareAndSwap(true, false) {
		close(r.reached)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}
}

func (r *edgeParkRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	r.park(ctx)
	st, err := r.Repository.TransitionStage(ctx, id, to, completion)
	if err == nil {
		r.count(to)
	}
	return st, err
}

func (r *edgeParkRepo) TransitionStageFrom(ctx context.Context, id uuid.UUID, from, to run.StageState, completion *run.StageCompletion) (*run.Stage, error) {
	r.park(ctx)
	st, err := r.stageCAS.TransitionStageFrom(ctx, id, from, to, completion)
	if err == nil {
		r.count(to)
	}
	return st, err
}

func (r *edgeParkRepo) TransitionStageFromAttemptGuarded(ctx context.Context, id uuid.UUID, from, to run.StageState, expected string, completion *run.StageCompletion, guard run.StageEdgeGuard) (*run.Stage, error) {
	r.park(ctx)
	if err := r.fault.Swap(nil); err != nil {
		return nil, *err
	}
	st, err := r.guarded.TransitionStageFromAttemptGuarded(ctx, id, from, to, expected, completion, guard)
	if err == nil {
		r.count(to)
	}
	return st, err
}

func (r *edgeParkRepo) TransitionStageFromAttempt(ctx context.Context, id uuid.UUID, from, to run.StageState, expected string, completion *run.StageCompletion) (*run.Stage, error) {
	r.park(ctx)
	st, err := r.cas.TransitionStageFromAttempt(ctx, id, from, to, expected, completion)
	if err == nil {
		r.count(to)
		if to == run.StageStateFailed {
			r.failedAt.Store(&expected)
		}
	}
	return st, err
}

// pgAdvanceCounter is what the gateless Advance reads, so its walk is counted apart
// from every handler read.
type pgAdvanceCounter struct {
	run.Repository
	n *atomic.Int32
}

func (r pgAdvanceCounter) ListStagesForRun(ctx context.Context, id uuid.UUID) ([]*run.Stage, error) {
	r.n.Add(1)
	return r.Repository.ListStagesForRun(ctx, id)
}

// pgRouteSeam is one run's stores behind any number of real Servers: a real run,
// audit and artifact repository over one pool, one signing fake and one reviewer.
type pgRouteSeam struct {
	pool     *pgxpool.Pool
	realRuns run.Repository
	edge     *edgeParkRepo
	audits   audit.Repository
	arts     artifact.Repository
	sf       *signingFake
	priv     ed25519.PrivateKey
	runID    uuid.UUID
	stageID  uuid.UUID
	rev      *scriptedReviewer
	set      ReviewerSet
	closes   *closeRecorder
	notes    *prStatusRecorder
	boards   *atomic.Int32
	walks    *atomic.Int32
	gitHub   *githubclient.Client
	s        *Server
}

func newPGRouteSeam(t *testing.T, workflowSpec []byte, requiresApproval bool, steps ...reviewStep) *pgRouteSeam {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	realRuns := run.NewPostgresRepository(pool)
	arts := artifact.NewPostgresRepository(pool)
	inst := int64(12345)
	ref := "issue:7"
	created, err := realRuns.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/example", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerCLI, RunnerKind: run.RunnerKindLocal, WorkflowSpec: workflowSpec,
		InstallationID: &inst, TriggerRef: &ref,
	})
	if err != nil {
		t.Fatalf("setup: CreateRun: %v", err)
	}
	planStage, err := realRuns.CreateStage(ctx, run.CreateStageParams{RunID: created.ID, Sequence: 0, Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
	if err != nil {
		t.Fatalf("setup: plan stage: %v", err)
	}
	version := "standard_v1"
	if _, err := arts.Create(ctx, artifact.CreateParams{StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &version, Content: validPlanBytes(t), ContentHash: strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("setup: plan artifact: %v", err)
	}
	stage, err := realRuns.CreateStage(ctx, run.CreateStageParams{RunID: created.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: requiresApproval})
	if err != nil {
		t.Fatalf("setup: implement stage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := realRuns.TransitionStage(ctx, stage.ID, to, nil); err != nil {
			t.Fatalf("setup: transition to %s: %v", to, err)
		}
	}
	closes := &closeRecorder{}
	ghSrv := closes.reportedHeadAdmittingServer(t)
	boards := &atomic.Int32{}
	setVar(t, &conventionsLoader, func(context.Context, string) (workmgmt.Conventions, error) {
		boards.Add(1)
		return workmgmt.Conventions{}, errors.New("no conventions in this fixture")
	})
	setVar(t, &defaultReviewTerminalWriteConfig, reviewTerminalWriteConfig{opTimeout: time.Hour, settle: rtSteps(1, time.Millisecond)})
	p := &pgRouteSeam{
		pool: pool, realRuns: realRuns, edge: newEdgeParkRepo(t, realRuns), audits: audit.NewPostgresRepository(pool), arts: arts,
		sf: newSigningFake(), runID: created.ID, stageID: stage.ID, rev: &scriptedReviewer{steps: steps},
		closes: closes, notes: &prStatusRecorder{}, boards: boards, walks: &atomic.Int32{},
		gitHub: &githubclient.Client{BaseURL: ghSrv.URL, Tokens: &ghTokensStub{tok: "ghs_test"}, HTTP: &http.Client{Timeout: 5 * time.Second}, AppJWT: func() (string, error) { return "gha_app_jwt_test", nil }},
	}
	p.set = singleReviewerSet{p.rev}
	p.priv, _ = p.sf.issue(t, created.ID)
	p.s = p.newServer(p.audits)
	return p
}

// newServer is one process over the shared stores: its own ownership map and lifetime.
func (p *pgRouteSeam) newServer(auditRepo audit.Repository) *Server {
	s := New(Config{
		Addr: "127.0.0.1:0", SigningRepo: p.sf, TraceStore: newTraceStoreFake(), AuditRepo: auditRepo, RunRepo: p.edge,
		ArtifactRepo: p.arts, PlanReviewers: p.set, GitHub: p.gitHub,
		Orchestrator: &orchestrator.Orchestrator{Runs: pgAdvanceCounter{p.realRuns, p.walks}},
	})
	s.issueNotifier = p.notes
	return s
}

func (p *pgRouteSeam) body(t *testing.T) []byte {
	t.Helper()
	return runnerShapedSupplementalPRBody(t, prSeamHead, prSeamPath, "unchanged after rebase")
}

func (p *pgRouteSeam) postTo(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return shipPRRequest(t, s, p.runID, p.stageID, p.priv, body, "")
}

func (p *pgRouteSeam) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return p.postTo(t, p.s, body)
}

func (p *pgRouteSeam) postAsyncTo(s *Server, body []byte) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	url := fmt.Sprintf("/v0/runs/%s/pull-request?stage_id=%s", p.runID, p.stageID)
	sig := hex.EncodeToString(ed25519.Sign(p.priv, signing.ComputeMessage(body)))
	go func() {
		req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("X-Fishhawk-Signature", sig)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		done <- w
	}()
	return done
}

func (p *pgRouteSeam) postAsync(body []byte) <-chan *httptest.ResponseRecorder {
	return p.postAsyncTo(p.s, body)
}

func (p *pgRouteSeam) committed(t *testing.T) *run.Stage {
	t.Helper()
	st, err := p.realRuns.GetStage(context.Background(), p.stageID)
	if err != nil {
		t.Fatalf("read committed stage: %v", err)
	}
	return st
}

func (p *pgRouteSeam) attempt(t *testing.T) string {
	t.Helper()
	return run.StageAttemptToken(p.committed(t).DispatchedAt)
}

// retry takes the real recovery route: failed, retried and re-dispatched to running
// on a new attempt token.
func (p *pgRouteSeam) retry(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	before := p.attempt(t)
	if _, err := run.FailStage(ctx, p.realRuns, p.stageID, run.FailureC, "retried by the test"); err != nil {
		t.Fatalf("setup: fail stage: %v", err)
	}
	if _, err := run.RetryStage(ctx, p.realRuns, p.stageID, run.RetryOptions{}); err != nil {
		t.Fatalf("setup: retry stage: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := p.realRuns.TransitionStage(ctx, p.stageID, to, nil); err != nil {
			t.Fatalf("setup: transition to %s: %v", to, err)
		}
	}
	after := p.attempt(t)
	if after == before || after == "" {
		t.Fatalf("setup: the retry kept attempt %q", after)
	}
	return after
}

func (p *pgRouteSeam) appendRow(t *testing.T, stageID uuid.UUID, category string, payload []byte) {
	t.Helper()
	if _, err := p.audits.AppendChained(context.Background(), audit.ChainAppendParams{RunID: p.runID, StageID: &stageID, Timestamp: time.Now().UTC(), Category: category, Payload: payload}); err != nil {
		t.Fatalf("setup: append %s: %v", category, err)
	}
}

func (p *pgRouteSeam) verdictRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := p.audits.ListForRunByCategory(context.Background(), p.runID, implementReviewedCategory)
	if err != nil {
		t.Fatalf("list verdicts: %v", err)
	}
	return rows
}

func (p *pgRouteSeam) startRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := p.audits.ListForRunByCategory(context.Background(), p.runID, CategorySupplementalReviewStarted)
	if err != nil {
		t.Fatalf("list start rows: %v", err)
	}
	return rows
}

func (p *pgRouteSeam) closeCalls() int {
	p.closes.mu.Lock()
	defer p.closes.mu.Unlock()
	return p.closes.closeCalls
}

// assertNoEffects pins zero edges, closes, notifications, board moves and advances.
func (p *pgRouteSeam) assertNoEffects(t *testing.T) {
	t.Helper()
	if n := p.edge.advances.Load(); n != 0 {
		t.Errorf("successful edges = %d, want 0", n)
	}
	if n := p.edge.fails.Load(); n != 0 {
		t.Errorf("failed edges = %d, want 0", n)
	}
	if n := p.closeCalls(); n != 0 {
		t.Errorf("PR closes = %d, want 0", n)
	}
	p.assertNoNotification(t)
}

func (p *pgRouteSeam) assertNoNotification(t *testing.T) {
	t.Helper()
	if n := p.notes.n.Load(); n != 0 {
		t.Errorf("status comments = %d, want 0", n)
	}
	if n := p.boards.Load(); n != 0 {
		t.Errorf("board transitions = %d, want 0", n)
	}
	if n := p.walks.Load(); n != 0 {
		t.Errorf("orchestrator advances = %d, want 0", n)
	}
}

func (p *pgRouteSeam) assertRunning(t *testing.T) {
	t.Helper()
	if got := p.committed(t).State; got != run.StageStateRunning {
		t.Errorf("committed stage = %q, want running", got)
	}
}

// assertFailedBOn pins category B on the attempt's own token, one close and no
// successful edge.
func (p *pgRouteSeam) assertFailedBOn(t *testing.T, attempt string) {
	t.Helper()
	got := p.committed(t)
	if got.State != run.StageStateFailed || got.FailureCategory == nil || *got.FailureCategory != run.FailureB ||
		got.FailureReason == nil || !strings.HasPrefix(*got.FailureReason, implementReviewGatingRejectPrefix) {
		t.Fatalf("committed stage = %q %v %v, want failed category B with the gating-reject reason", got.State, got.FailureCategory, got.FailureReason)
	}
	if token := run.StageAttemptToken(got.DispatchedAt); token != attempt {
		t.Errorf("failed token = %q, want %q", token, attempt)
	}
	if n := p.edge.advances.Load(); n != 0 {
		t.Errorf("successful edges = %d, want 0", n)
	}
	if n := p.closeCalls(); n != 1 {
		t.Errorf("PR closes = %d, want 1", n)
	}
	if at := p.edge.failedAt.Load(); at == nil || *at != attempt {
		t.Errorf("failed edge pinned to %v, want %q", at, attempt)
	}
	p.assertNoNotification(t)
	state, err := p.s.supplementalRoundState(context.Background(), p.runID, p.stageID, prSeamHead, attempt)
	if err != nil || supplementalPassDischarged(state) {
		t.Errorf("round state = %+v, %v: a recorded reject never discharges", state, err)
	}
}

func (p *pgRouteSeam) assertAdvancedOnce(t *testing.T, want run.StageState) {
	t.Helper()
	if got := p.committed(t).State; got != want {
		t.Errorf("committed stage = %q, want %q", got, want)
	}
	if n := p.edge.advances.Load(); n != 1 {
		t.Errorf("successful edges = %d, want 1", n)
	}
	if n := p.closeCalls(); n != 0 {
		t.Errorf("PR closes = %d, want 0", n)
	}
}

// awaitCommittedReject waits for attempt 1's response, then proves its reject is
// committed by reading it on another connection.
func (p *pgRouteSeam) awaitCommittedReject(t *testing.T, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	w := awaitResponse(t, done, "setup: attempt 1")
	if w.Code != http.StatusCreated {
		t.Fatalf("setup: attempt 1 = %d:\n%s", w.Code, w.Body.String())
	}
	if p.committed(t).State != run.StageStateRunning {
		t.Fatalf("attempt 1's stale failed edge moved the newer attempt")
	}
	if n := p.closeCalls(); n != 0 {
		t.Fatalf("attempt 1's stale reject closed the PR (%d closes)", n)
	}
	if err := recordedRejectInRows(p.verdictRows(t)); err != nil {
		t.Fatal("setup: no committed reject at this head")
	}
}

// recordedRejectInRows reports a binding reject among rows.
func recordedRejectInRows(rows []*audit.Entry) error {
	for _, row := range rows {
		var payload planreview.ImplementReviewedPayload
		if json.Unmarshal(row.Payload, &payload) == nil && payload.Verdict == planreview.VerdictReject {
			return nil
		}
	}
	return errors.New("no reject")
}

func TestShipPullRequest_SupplementalRejectBeforeEdge_PG(t *testing.T) {
	// window parks attempt 2 past its final outside-lock decision with attempt 1's
	// rejecting reviewer still blocked, so the reject commits inside the window.
	window := func(t *testing.T, requiresApproval bool) (*pgRouteSeam, []byte, <-chan *httptest.ResponseRecorder, chan<- struct{}, string) {
		t.Helper()
		started, release := make(chan struct{}), make(chan struct{})
		p := newPGRouteSeam(t, specImplementGatingReviewers, requiresApproval, heldStep(started, release, rejectStep), approveStep)
		body := p.body(t)
		first := p.postAsync(body)
		waitFor(t, started, "setup: attempt 1's reviewer")
		return p, body, first, release, p.retry(t)
	}
	inWindow := func(t *testing.T, requiresApproval bool) {
		t.Helper()
		p, body, first, release, attempt := window(t, requiresApproval)
		p.edge.parkNext()
		second := p.postAsync(body)
		waitFor(t, p.edge.reached, "setup: attempt 2 parked at its edge")
		if n := p.rev.calls.Load(); n != 2 {
			t.Fatalf("setup: reviewer calls = %d, want attempt 2's approval too", n)
		}
		close(release)
		p.awaitCommittedReject(t, first)
		p.edge.open()
		if w := awaitResponse(t, second, "attempt 2"); w.Code != http.StatusOK {
			t.Fatalf("attempt 2 = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedBOn(t, attempt)
	}
	t.Run("prior_attempt_reject_in_window", func(t *testing.T) { inWindow(t, true) })
	t.Run("prior_attempt_reject_in_window_gateless", func(t *testing.T) { inWindow(t, false) })
	t.Run("replay_heal_in_window", func(t *testing.T) {
		p, body, first, release, attempt := window(t, true)
		fault := errors.New("forced store fault")
		p.edge.failNext(fault)
		assertStaticEdgeError(t, p.post(t, body), fault.Error())
		p.edge.parkNext()
		replay := p.postAsync(body)
		waitFor(t, p.edge.reached, "setup: the replay parked at its edge")
		close(release)
		p.awaitCommittedReject(t, first)
		p.edge.open()
		if w := awaitResponse(t, replay, "replay"); w.Code != http.StatusOK {
			t.Fatalf("replay = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedBOn(t, attempt)
	})
	t.Run("discharged_omission_in_window", func(t *testing.T) {
		p, body, first, release, attempt := window(t, true)
		fault := errors.New("forced store fault")
		p.edge.failNext(fault)
		assertStaticEdgeError(t, p.post(t, body), fault.Error())
		p.edge.parkNext()
		omitting := p.postAsync(mustPRBody(t, prSeamHead))
		waitFor(t, p.edge.reached, "setup: the omission parked at its edge")
		close(release)
		p.awaitCommittedReject(t, first)
		p.edge.open()
		if w := awaitResponse(t, omitting, "omission"); w.Code != http.StatusCreated {
			t.Fatalf("omission = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedBOn(t, attempt)
	})
	t.Run("current_approve_advances_once", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if n := p.notes.n.Load(); n != 1 {
			t.Errorf("status comments = %d, want one", n)
		}
	})
	t.Run("current_reject_fails_b", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, rejectStep)
		attempt := p.attempt(t)
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedBOn(t, attempt)
	})
	t.Run("prior_head_reject_excluded", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		p.appendRow(t, p.stageID, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(prSeamOther, p.attempt(t), planreview.VerdictReject)))
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("other_stage_reject_excluded", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		other, err := p.realRuns.CreateStage(context.Background(), run.CreateStageParams{RunID: p.runID, Sequence: 2, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		p.appendRow(t, other.ID, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(prSeamHead, p.attempt(t), planreview.VerdictReject)))
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("prior_attempt_approve_not_settling", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		p.appendRow(t, p.stageID, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(prSeamHead, p.attempt(t), planreview.VerdictApprove)))
		p.retry(t)
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want attempt 2's own", n)
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	stale := func(t *testing.T) (*pgRouteSeam, <-chan *httptest.ResponseRecorder, string, string) {
		t.Helper()
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		first := p.attempt(t)
		p.edge.parkNext()
		parked := p.postAsync(p.body(t))
		waitFor(t, p.edge.reached, "setup: parked at the edge")
		return p, parked, first, p.retry(t)
	}
	assertNewerUntouched := func(t *testing.T, p *pgRouteSeam, attempt string) {
		t.Helper()
		got := p.committed(t)
		if got.State != run.StageStateRunning || run.StageAttemptToken(got.DispatchedAt) != attempt || got.FailureCategory != nil {
			t.Errorf("newer attempt = %q token %q category %v, want it running untouched on %q", got.State, run.StageAttemptToken(got.DispatchedAt), got.FailureCategory, attempt)
		}
		p.assertNoEffects(t)
	}
	t.Run("stale_attempt_no_reject", func(t *testing.T) {
		p, parked, _, attempt := stale(t)
		p.edge.open()
		if w := awaitResponse(t, parked, "stale upload"); w.Code != http.StatusCreated {
			t.Fatalf("stale upload = %d:\n%s", w.Code, w.Body.String())
		}
		assertNewerUntouched(t, p, attempt)
	})
	t.Run("stale_attempt_with_reject", func(t *testing.T) {
		p, parked, first, attempt := stale(t)
		p.appendRow(t, p.stageID, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(prSeamHead, first, planreview.VerdictReject)))
		p.edge.open()
		if w := awaitResponse(t, parked, "stale upload"); w.Code != http.StatusCreated {
			t.Fatalf("stale upload = %d:\n%s", w.Code, w.Body.String())
		}
		assertNewerUntouched(t, p, attempt)
	})
	t.Run("verdict_row_undecodable", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		p.edge.parkNext()
		parked := p.postAsync(p.body(t))
		waitFor(t, p.edge.reached, "setup: parked at the edge")
		bad, err := json.Marshal(map[string]any{"origin": string(planreview.OriginBaseRebaseReinvoke), "head_sha": prSeamHead, "verdict": 123})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		p.appendRow(t, p.stageID, implementReviewedCategory, bad)
		p.edge.open()
		w := awaitResponse(t, parked, "upload")
		p.assertRunning(t)
		p.assertNoEffects(t)
		assertStaticEdgeError(t, w)
	})
	t.Run("guarded_read_error", func(t *testing.T) {
		// A boundary fault, labelled as such: real rows cannot fail the read on demand.
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		fault := errors.New("forced guarded read fault")
		p.edge.failNext(fault)
		body := p.body(t)
		assertStaticEdgeError(t, p.post(t, body), fault.Error())
		p.assertRunning(t)
		p.assertNoEffects(t)
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("replay = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("reject_after_terminal_commit", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		body := p.body(t)
		if w := p.post(t, body); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		attempt := p.attempt(t)
		p.appendRow(t, p.stageID, implementReviewedCategory, recoveryPayload(t, supplementalVerdict(prSeamHead, attempt, planreview.VerdictReject)))
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("replay = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
}

func TestShipPullRequest_SupplementalStartedIncomplete_PG(t *testing.T) {
	assertLost := func(t *testing.T, p *pgRouteSeam, w *httptest.ResponseRecorder) {
		t.Helper()
		p.assertRunning(t)
		p.assertNoEffects(t)
		assertInterrupted(t, w, http.StatusConflict)
		if n := len(p.startRows(t)); n != 1 {
			t.Errorf("start rows = %d, want 1", n)
		}
		state, err := p.s.supplementalRoundState(context.Background(), p.runID, p.stageID, prSeamHead, p.attempt(t))
		if err != nil || supplementalPassDischarged(state) {
			t.Errorf("round state = %+v, %v: an incomplete pass never discharges", state, err)
		}
	}
	t.Run("first_call_error_row", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, errorStep)
		assertLost(t, p, p.post(t, p.body(t)))
		rows, err := p.audits.ListForRunByCategory(context.Background(), p.runID, "implement_review_failed")
		if err != nil || len(rows) != 1 {
			t.Errorf("failed rows = %d, %v, want the reviewer's", len(rows), err)
		}
	})
	t.Run("skip_row", func(t *testing.T) {
		rev := &scriptedReviewer{steps: []reviewStep{approveStep}}
		p := newPGRouteSeam(t, specImplementHeterogeneousGating, true, approveStep)
		p.rev = rev
		p.set = fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": rev}, def: rev}
		p.s = p.newServer(p.audits)
		assertLost(t, p, p.post(t, p.body(t)))
		rows, err := p.audits.ListForRunByCategory(context.Background(), p.runID, "implement_review_skipped")
		if err != nil || len(rows) == 0 {
			t.Fatalf("setup: skip rows = %d, %v", len(rows), err)
		}
	})
	t.Run("replay_refused", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, errorStep)
		body := p.body(t)
		assertLost(t, p, p.post(t, body))
		calls := p.rev.calls.Load()
		assertLost(t, p, p.post(t, body))
		if n := p.rev.calls.Load(); n != calls {
			t.Errorf("replay invoked %d more reviewers", n-calls)
		}
	})
	t.Run("retry_reenters_once", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, errorStep, approveStep)
		body := p.body(t)
		assertLost(t, p, p.post(t, body))
		p.retry(t)
		if w := p.post(t, body); w.Code != http.StatusOK {
			t.Fatalf("retried upload = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 2 {
			t.Errorf("reviewer calls = %d, want one per attempt", n)
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("full_verdict_advances_once", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
}

// verdictFaultAudit forwards the real store, atomic start capability included, and
// faults verdict appends: before the commit, or after it as a lost acknowledgement.
type verdictFaultAudit struct {
	audit.Repository
	deduped audit.DedupedChainAppender
	lostAck bool
}

func (a verdictFaultAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	return a.deduped.AppendChainedDeduped(ctx, p, spec)
}

func (a verdictFaultAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category != implementReviewedCategory {
		return a.Repository.AppendChained(ctx, p)
	}
	if a.lostAck {
		if _, err := a.Repository.AppendChained(ctx, p); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("injected write failure")
}

func TestShipPullRequest_SupplementalEvidenceWriteFailed_PG(t *testing.T) {
	wrap := func(t *testing.T, p *pgRouteSeam, lostAck bool) {
		t.Helper()
		deduped, ok := p.audits.(audit.DedupedChainAppender)
		if !ok {
			t.Fatal("setup: the audit repository lacks the deduped append")
		}
		p.s = p.newServer(verdictFaultAudit{Repository: p.audits, deduped: deduped, lostAck: lostAck})
	}
	t.Run("exhausted_no_edge", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		wrap(t, p, false)
		assertStaticEdgeError(t, p.post(t, p.body(t)), "injected write failure")
		p.assertRunning(t)
		p.assertNoEffects(t)
		if n := len(p.startRows(t)); n != 1 {
			t.Errorf("start rows = %d, want 1", n)
		}
		if n := len(p.verdictRows(t)); n != 0 {
			t.Errorf("verdict rows = %d, want 0", n)
		}
	})
	t.Run("lost_ack_row_counts", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		wrap(t, p, true)
		if w := p.post(t, p.body(t)); w.Code != http.StatusCreated {
			t.Fatalf("upload = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
		if n := len(p.verdictRows(t)); n != 1 {
			t.Errorf("verdict rows = %d, want the committed original alone", n)
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
}

// barrierStartedAudit holds both handlers at allocation until both arrive, past
// their round-state read, so each has seen no start before either can write one.
// It covers the deduped append and a plain chained start append alike.
type barrierStartedAudit struct {
	audit.Repository
	deduped audit.DedupedChainAppender
	mu      sync.Mutex
	arrived int
	open    chan struct{}
	armed   bool
}

func newBarrierStartedAudit(t *testing.T, repo audit.Repository) *barrierStartedAudit {
	t.Helper()
	deduped, ok := repo.(audit.DedupedChainAppender)
	if !ok {
		t.Fatal("setup: the audit repository lacks the deduped append")
	}
	return &barrierStartedAudit{Repository: repo, deduped: deduped, open: make(chan struct{})}
}

func (a *barrierStartedAudit) arm() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.armed = true
}

func (a *barrierStartedAudit) hold(ctx context.Context) {
	a.mu.Lock()
	if !a.armed {
		a.mu.Unlock()
		return
	}
	a.arrived++
	if a.arrived == 2 {
		close(a.open)
	}
	a.mu.Unlock()
	select {
	case <-a.open:
	case <-ctx.Done():
	case <-time.After(boundedWait):
	}
}

func (a *barrierStartedAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	if p.Category == CategorySupplementalReviewStarted {
		a.hold(ctx)
	}
	return a.deduped.AppendChainedDeduped(ctx, p, spec)
}

func (a *barrierStartedAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategorySupplementalReviewStarted {
		a.hold(ctx)
	}
	return a.Repository.AppendChained(ctx, p)
}

// missBarrierArtifacts holds each artifact miss until two handlers have missed, so
// both go on to their own allocation rather than one seeing the other's artifact.
type missBarrierArtifacts struct {
	artifact.Repository
	mu      sync.Mutex
	arrived int
	open    chan struct{}
}

func newMissBarrierArtifacts(inner artifact.Repository) *missBarrierArtifacts {
	return &missBarrierArtifacts{Repository: inner, open: make(chan struct{})}
}

func (a *missBarrierArtifacts) GetByHash(ctx context.Context, stageID uuid.UUID, hash string) (*artifact.Artifact, error) {
	got, err := a.Repository.GetByHash(ctx, stageID, hash)
	if !errors.Is(err, artifact.ErrNotFound) {
		return got, err
	}
	a.mu.Lock()
	a.arrived++
	if a.arrived == 2 {
		close(a.open)
	}
	a.mu.Unlock()
	select {
	case <-a.open:
	case <-ctx.Done():
	case <-time.After(boundedWait):
	}
	return got, err
}

// failingStartAudit fails the atomic start append until cleared.
type failingStartAudit struct {
	audit.Repository
	deduped audit.DedupedChainAppender
	failing atomic.Bool
}

func (a *failingStartAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	if a.failing.Load() {
		return nil, errors.New("injected start append failure")
	}
	return a.deduped.AppendChainedDeduped(ctx, p, spec)
}

// TestSupplementalPassAllocation_TwoServers_PG is real-store component evidence, not
// split-mode deployment proof: Servers A and B each have their own ownership map over
// one pool's stores, so only the atomic start allocation can keep the second reviewer
// out.
func TestSupplementalPassAllocation_TwoServers_PG(t *testing.T) {
	two := func(t *testing.T, steps ...reviewStep) (*pgRouteSeam, *Server) {
		t.Helper()
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, steps...)
		return p, p.newServer(p.audits)
	}
	// oneBlocked lets A win and block in its reviewer.
	oneBlocked := func(t *testing.T, final reviewStep) (*pgRouteSeam, *Server, []byte, <-chan *httptest.ResponseRecorder, chan struct{}) {
		t.Helper()
		started, release := make(chan struct{}), make(chan struct{})
		// A second reviewer would be the defect, so its call counts rather than crashes.
		p, b := two(t, heldStep(started, release, final), approveStep)
		body := p.body(t)
		done := p.postAsync(body)
		waitFor(t, started, "setup: A's reviewer")
		return p, b, body, done, release
	}
	assertOneStart := func(t *testing.T, p *pgRouteSeam) {
		t.Helper()
		if n := len(p.startRows(t)); n != 1 {
			t.Errorf("start rows = %d, want 1", n)
		}
	}
	t.Run("simultaneous_start", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, heldStep(started, release, approveStep), approveStep)
		barrier := newBarrierStartedAudit(t, p.audits)
		barrier.arm()
		a, b := p.newServer(barrier), p.newServer(barrier)
		misses := newMissBarrierArtifacts(p.arts)
		a.cfg.ArtifactRepo, b.cfg.ArtifactRepo = misses, misses
		body := p.body(t)
		doneA, doneB := p.postAsyncTo(a, body), p.postAsyncTo(b, body)
		waitFor(t, started, "the winner's reviewer")
		var loser *httptest.ResponseRecorder
		var winner <-chan *httptest.ResponseRecorder
		select {
		case loser = <-doneA:
			winner = doneB
		case loser = <-doneB:
			winner = doneA
		case <-time.After(boundedWait):
			t.Fatal("no loser answered while the winner's reviewer blocked")
		}
		assertOneStart(t, p)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want the configured 1 in all", n)
		}
		p.assertRunning(t)
		assertInterrupted(t, loser, http.StatusConflict)
		close(release)
		if w := awaitResponse(t, winner, "winner"); w.Code != http.StatusCreated {
			t.Fatalf("winner = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("one_blocked_other_proceeds", func(t *testing.T) {
		// The artifact predates this attempt, so B reaches the dispatcher rather than
		// the same-attempt replay guard, and only A's committed start can stop it.
		started, release := make(chan struct{}), make(chan struct{})
		p, b := two(t, heldStep(started, release, approveStep), approveStep)
		body := p.body(t)
		if _, err := p.arts.Create(context.Background(), artifact.CreateParams{StageID: p.stageID, Kind: artifact.KindPullRequest, Content: json.RawMessage(body), ContentHash: sha256Hex(body)}); err != nil {
			t.Fatalf("setup: seed artifact: %v", err)
		}
		p.retry(t)
		done := p.postAsync(body)
		waitFor(t, started, "setup: A's reviewer")
		w := p.postTo(t, b, body)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want A's alone", n)
		}
		assertInterrupted(t, w, http.StatusConflict)
		p.assertRunning(t)
		assertCode(t, p.post(t, body), http.StatusServiceUnavailable, "review_round_in_flight")
		close(release)
		awaitResponse(t, done, "A")
		assertOneStart(t, p)
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("approve_then_replay_on_other", func(t *testing.T) {
		p, b, body, done, release := oneBlocked(t, approveStep)
		close(release)
		awaitResponse(t, done, "A")
		if w := p.postTo(t, b, body); w.Code != http.StatusOK {
			t.Fatalf("replay on B = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("reject_then_replay_on_other", func(t *testing.T) {
		p, b, body, done, release := oneBlocked(t, rejectStep)
		attempt := p.attempt(t)
		close(release)
		awaitResponse(t, done, "A")
		if w := p.postTo(t, b, body); w.Code != http.StatusOK {
			t.Fatalf("replay on B = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertFailedBOn(t, attempt)
	})
	// loserParked has both handlers miss the artifact, then parks B at its allocation
	// past its own read of an empty round while A wins and blocks in its reviewer.
	loserParked := func(t *testing.T, final reviewStep) (*pgRouteSeam, *Server, <-chan *httptest.ResponseRecorder, <-chan *httptest.ResponseRecorder, chan struct{}, *heldStartAudit) {
		t.Helper()
		started, release := make(chan struct{}), make(chan struct{})
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, heldStep(started, release, final), approveStep)
		held := newHeldStartAudit(t, p.audits)
		b := p.newServer(held)
		misses := newMissBarrierArtifacts(p.arts)
		p.s.cfg.ArtifactRepo, b.cfg.ArtifactRepo = misses, misses
		body := p.body(t)
		doneB := p.postAsyncTo(b, body)
		done := p.postAsync(body)
		waitFor(t, held.reached, "setup: B parked at allocation")
		waitFor(t, started, "setup: A's reviewer")
		return p, b, done, doneB, release, held
	}
	t.Run("loser_parked_winner_approves", func(t *testing.T) {
		p, _, doneA, doneB, release, held := loserParked(t, approveStep)
		close(release)
		awaitResponse(t, doneA, "A")
		held.open()
		if w := awaitResponse(t, doneB, "B"); w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("B = %d:\n%s", w.Code, w.Body.String())
		}
		assertOneStart(t, p)
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("loser_parked_winner_rejects", func(t *testing.T) {
		p, _, doneA, doneB, release, held := loserParked(t, rejectStep)
		attempt := p.attempt(t)
		close(release)
		awaitResponse(t, doneA, "A")
		held.open()
		awaitResponse(t, doneB, "B")
		assertOneStart(t, p)
		p.assertFailedBOn(t, attempt)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("loser_applies_for_parked_winner", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		b := p.newServer(p.audits)
		body := p.body(t)
		p.edge.parkNext()
		done := p.postAsync(body)
		waitFor(t, p.edge.reached, "setup: A parked before its edge with its set written")
		if w := p.postTo(t, b, body); w.Code != http.StatusOK {
			t.Fatalf("B = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		p.edge.open()
		if w := awaitResponse(t, done, "A"); w.Code != http.StatusCreated {
			t.Fatalf("A = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
	t.Run("committed_allocation_owner_lost", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		first := p.attempt(t)
		won, err := p.s.allocateSupplementalPass(context.Background(), p.runID, p.stageID, prSeamHead, first, 1, []string{prSeamPath})
		if err != nil || !won.won {
			t.Fatalf("setup: allocation = %+v, %v", won, err)
		}
		fresh := p.newServer(p.audits)
		body := p.body(t)
		assertInterrupted(t, p.postTo(t, fresh, body), http.StatusConflict)
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("reviewer calls = %d, want 0 on the lost attempt", n)
		}
		p.assertRunning(t)
		p.retry(t)
		if w := p.postTo(t, fresh, body); w.Code != http.StatusOK {
			t.Fatalf("new attempt = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want one pass", n)
		}
		if n := len(p.startRows(t)); n != 2 {
			t.Errorf("start rows = %d, want one per attempt", n)
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("allocation_write_fails", func(t *testing.T) {
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, approveStep)
		deduped, ok := p.audits.(audit.DedupedChainAppender)
		if !ok {
			t.Fatal("setup: the audit repository lacks the deduped append")
		}
		failing := &failingStartAudit{Repository: p.audits, deduped: deduped}
		failing.failing.Store(true)
		s := p.newServer(failing)
		body := p.body(t)
		assertStaticEdgeError(t, p.postTo(t, s, body), "injected start append failure")
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("reviewer calls = %d, want 0", n)
		}
		p.assertRunning(t)
		p.assertNoEffects(t)
		failing.failing.Store(false)
		// The artifact persisted before the fault, and no same-attempt replay ever
		// dispatches, so the one pass belongs to the stage retry's new attempt.
		assertInterrupted(t, p.postTo(t, s, body), http.StatusConflict)
		if n := p.rev.calls.Load(); n != 0 {
			t.Errorf("same-attempt replay invoked %d reviewers, want 0", n)
		}
		p.retry(t)
		if w := p.postTo(t, s, body); w.Code != http.StatusOK {
			t.Fatalf("cleared on the new attempt = %d:\n%s", w.Code, w.Body.String())
		}
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want one pass once cleared", n)
		}
		assertOneStart(t, p)
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
	})
	t.Run("stale_attempt_same_head", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		p := newPGRouteSeam(t, specImplementGatingReviewers, true, heldStep(started, release, approveStep), approveStep)
		b := p.newServer(p.audits)
		body := p.body(t)
		done := p.postAsync(body)
		waitFor(t, started, "setup: A's reviewer")
		first := p.attempt(t)
		second := p.retry(t)
		if w := p.postTo(t, b, body); w.Code != http.StatusOK {
			t.Fatalf("B = %d:\n%s", w.Code, w.Body.String())
		}
		starts := p.startRows(t)
		if len(starts) != 2 {
			t.Fatalf("start rows = %d, want one per attempt", len(starts))
		}
		var keys []string
		for _, row := range starts {
			var payload map[string]any
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				t.Fatalf("decode start: %v", err)
			}
			keys = append(keys, fmt.Sprint(payload[supplementalPassKeyField]))
		}
		sort.Strings(keys)
		want := []string{supplementalPassStartKey(first, prSeamHead), supplementalPassStartKey(second, prSeamHead)}
		sort.Strings(want)
		if !slices.Equal(keys, want) {
			t.Errorf("start keys = %v, want %v", keys, want)
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		close(release)
		if w := awaitResponse(t, done, "A"); w.Code != http.StatusCreated {
			t.Fatalf("A = %d:\n%s", w.Code, w.Body.String())
		}
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		state, err := p.s.supplementalRoundState(context.Background(), p.runID, p.stageID, prSeamHead, first)
		if err != nil || state.currentVerdicts != 1 {
			t.Errorf("attempt 1's verdicts = %+v, %v, want its own one", state, err)
		}
		state, err = p.s.supplementalRoundState(context.Background(), p.runID, p.stageID, prSeamHead, second)
		if err != nil || state.currentVerdicts != 1 {
			t.Errorf("attempt 2's verdicts = %+v, %v, want its own one", state, err)
		}
	})
	t.Run("legitimate_once", func(t *testing.T) {
		p, b := two(t, approveStep)
		body := p.body(t)
		if w := p.post(t, body); w.Code != http.StatusCreated {
			t.Fatalf("A = %d:\n%s", w.Code, w.Body.String())
		}
		if w := p.postTo(t, b, body); w.Code != http.StatusOK {
			t.Fatalf("replay on B = %d:\n%s", w.Code, w.Body.String())
		}
		assertOneStart(t, p)
		p.assertAdvancedOnce(t, run.StageStateAwaitingApproval)
		if n := p.rev.calls.Load(); n != 1 {
			t.Errorf("reviewer calls = %d, want 1", n)
		}
	})
}

// heldStartAudit parks one handler at its start allocation, past its round-state read.
type heldStartAudit struct {
	audit.Repository
	deduped  audit.DedupedChainAppender
	reached  chan struct{}
	release  chan struct{}
	openOnce sync.Once
	parked   atomic.Bool
}

func newHeldStartAudit(t *testing.T, repo audit.Repository) *heldStartAudit {
	t.Helper()
	deduped, ok := repo.(audit.DedupedChainAppender)
	if !ok {
		t.Fatal("setup: the audit repository lacks the deduped append")
	}
	a := &heldStartAudit{Repository: repo, deduped: deduped, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(a.open)
	return a
}

func (a *heldStartAudit) open() { a.openOnce.Do(func() { close(a.release) }) }

func (a *heldStartAudit) hold(category string) {
	if category == CategorySupplementalReviewStarted && a.parked.CompareAndSwap(false, true) {
		close(a.reached)
		<-a.release
	}
}

func (a *heldStartAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, spec audit.DedupeSpec) (*audit.Entry, error) {
	a.hold(p.Category)
	return a.deduped.AppendChainedDeduped(ctx, p, spec)
}

func (a *heldStartAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.hold(p.Category)
	return a.Repository.AppendChained(ctx, p)
}
