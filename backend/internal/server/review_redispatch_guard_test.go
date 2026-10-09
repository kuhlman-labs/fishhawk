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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
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
	// The allowRedispatch *Mode chain: the terminate-only wrappers pass a
	// literal false, only the …ForBoot wrapper passes a literal true, and the
	// chain between them passes the parameter through.
	"reconcileRunOrphanedReviewsMode": {byArg: true, sites: []string{
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
			src:   plantedHandler(`_, _ = s.reconcileRunOrphanedReviewsMode(r.Context(), uuid.Nil, true)`),
			site:  "planted_handler.go:handlePlanted(true)",
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
		const from = "return s.reconcileRunOrphanedReviewsMode(ctx, runID, false)"
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if n := strings.Count(string(src), from); n != 1 {
			t.Fatalf("%q occurs %d times in %s, want exactly 1: the mutation would not land", from, n, file)
		}
		mutated := strings.Replace(string(src), from, "return s.reconcileRunOrphanedReviewsMode(ctx, runID, true)", 1)
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
