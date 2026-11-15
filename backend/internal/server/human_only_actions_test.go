package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestHumanOnlyActions_EnforcedSetPinned pins the ADR-143 D2 enforced set: a
// mark is set only by an action's enabling change and cleared only by an
// ADR-143 amendment, so changing this set is a visible change to this test.
//
// COUNTERFACTUAL (hand-run, #5228): `Enforced: true,` → `Enforced: false,` in
// human_only_actions.go reddened this test (enforced set {} ≠
// {server_check_clear}) and every bearer arm of
// TestServerCheckClearSession_RefusesBearerKinds.
func TestHumanOnlyActions_EnforcedSetPinned(t *testing.T) {
	var enforced []string
	for _, a := range humanOnlyActions {
		if a.Enforced {
			enforced = append(enforced, a.ID)
		}
	}
	if want := []string{humanOnlyActionServerCheckClear}; !reflect.DeepEqual(enforced, want) {
		t.Fatalf("enforced D2 set = %v, want %v: an enforced mark is cleared only by an ADR-143 amendment", enforced, want)
	}
	if !humanOnlyActionEnforced(humanOnlyActionServerCheckClear) {
		t.Errorf("humanOnlyActionEnforced(%q) = false, want true", humanOnlyActionServerCheckClear)
	}
	if humanOnlyActionEnforced("no_such_action") {
		t.Errorf("humanOnlyActionEnforced(unknown id) = true, want false")
	}
	wantVerbs := []string{clearVerbWaive, clearVerbBulkWaive, clearVerbDefer, clearVerbResolve}
	if !reflect.DeepEqual(serverCheckClearAction.Verbs, wantVerbs) {
		t.Errorf("server_check_clear verbs = %v, want %v", serverCheckClearAction.Verbs, wantVerbs)
	}
	// The mark is one literal line, so a reviewer sees exactly one switch.
	src, err := os.ReadFile("human_only_actions.go")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if n := strings.Count(string(src), "Enforced: true,"); n != 1 {
		t.Errorf("human_only_actions.go carries %d `Enforced: true,` literals, want exactly 1", n)
	}
}

// completenessNonRefusing lists functions the completeness scan flags (they
// call an agent classifier or read a delegated flag) that are NOT a refusal of
// a caller, with the reason. Every entry must still be flagged, so the table
// cannot go stale silently.
var completenessNonRefusing = map[string]string{
	"server.isAgentSubject":          "the shared agent classifier itself",
	"server.actorKindForSubject":     "audit actor-kind attribution only; refuses nothing",
	"server.captureDelegationShadow": "records a delegation shadow for non-delegated human actions; refuses nothing",
	"server.escapeAppendBirth":       "records agent_recorded on an escape birth row; the birth is admitted either way",
}

// completenessCloseVetoes are the NON-HUMAN close paths that retire a concern
// without a clearing verb (C10). They call no agent classifier, so the scan
// above cannot see them; this table names each one and the server_check fact
// it rests on, so a future non-human clear of a server-check concern cannot
// slip past the registry unnoticed. Each entry: the function must exist; when
// mustVeto is true its body must test IsServerCheck; when callers is set, the
// package functions calling it must be exactly that set ("file:func"), so a
// new caller of the close primitive goes RED here; and when reRaisedBy is set,
// each named server-check raiser must still de-duplicate on IsOpen and must
// not read StateSuperseded, so a superseded row never suppresses the re-raise
// that protects a server_check row on that path.
var completenessCloseVetoes = []struct {
	file, fn   string
	mustVeto   bool
	callers    []string
	reRaisedBy []string
	reason     string
}{
	{file: "trace.go", fn: "vetoReason", mustVeto: true,
		reason: "reviewer resolutions (confirm, supersede) and the clean-round auto-close: vetoServerCheckRequiresHuman is evaluated FIRST, so no reviewer verdict retires a server_check row"},
	{file: "concern_rollup.go", fn: "supersedeRolledUpCopies",
		reason: "supersedes a decomposition parent's COPY only after its child source was superseded by supersedeOpenImplementConcerns (a stage retry discarding the pass); it is not a clearing verb and carries no provenance veto of its own"},
	{file: "trace.go", fn: "supersedeOpenImplementConcerns",
		callers: []string{"retry.go:retryStageAs", "trace.go:ingestImplementReview"},
		reRaisedBy: []string{
			"counterfactual_observed.go:raiseControlUnpinnedConcern",
			"diff_secrets.go:raiseDiffSecretConcerns",
			"permission_drift.go:raisePermissionDrift",
		},
		reason: "RESIDUAL, not a veto: a stage retry (retry.go retryStageAs, bearer-reachable and delegable) supersedes EVERY open implement concern of the re-opened stage with no IsServerCheck test, server_check rows included " +
			"(the ingestImplementReview post-persist re-check matches only the rows its own verdict minted). What protects a server_check row is the re-raise when the retried pass re-runs its check: " +
			"each raiser's dedupe skips only open, waived and deferred rows, so a credential or widening still present is raised again, and a control_unpinned row returns only if the retried pass observes GREEN again"},
}

// TestHumanOnlyActions_CompletenessOverAgentRefusals is the ADR-143 D2
// completeness test. It parses every non-test .go file in
// backend/internal/server, backend/internal/captain and
// backend/internal/delegationconfirm and flags every function that calls
// isAgentSubject, operatorrole.IsTokenSubject or captainActorIsAgent, or that
// reads a delegated refusal flag (ActorIsDelegated, refusedActorDelegated).
// Each flagged function must appear in EXACTLY one place: the D2 entry's
// sites, one exclusion's sites, or completenessNonRefusing. Every listed site
// must still be flagged. Every refuseNonHumanServerCheckClear call must pass a
// verb in the D2 entry's verbs.
//
// COUNTERFACTUAL (hand-run, #5228): deleting exclusion (vii)'s seat sites
// (server.authorizeSeatCaptainAct, server.handleAttachFirstOfficerSeat,
// server.handleRetireFirstOfficerSeat) from human_only_actions.go reddened
// this test, naming each of the three as unclassified. The retry-supersession
// row (fix-up, each observed RED and restored): (a) adding `|| row.State ==
// concern.StateSuperseded` to raiseDiffSecretConcerns' dedupe → RED "raiser
// diff_secrets.go:raiseDiffSecretConcerns reads StateSuperseded"; (b)
// replacing retryStageAs' supersedeOpenImplementConcerns call with `_ =
// reason` → RED "callers = [trace.go:ingestImplementReview], want
// [retry.go:retryStageAs trace.go:ingestImplementReview]"; (c) adding a new
// caller (attention.go zzExtraClose) → RED naming attention.go:zzExtraClose.
func TestHumanOnlyActions_CompletenessOverAgentRefusals(t *testing.T) {
	flagged := map[string]bool{}
	var verbsPassed []string
	for _, dir := range []string{".", "../captain", "../delegationconfirm"} {
		pkg, fset := parseNonTestPackage(t, dir)
		for _, f := range pkg {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := f.Name.Name + "." + fd.Name.Name
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.CallExpr:
						switch callee := calleeName(x.Fun); callee {
						case "isAgentSubject", "operatorrole.IsTokenSubject", "captainActorIsAgent":
							flagged[name] = true
						case "s.refuseNonHumanServerCheckClear":
							if len(x.Args) == 6 {
								if id, ok := x.Args[5].(*ast.Ident); ok {
									verbsPassed = append(verbsPassed, fset.Position(x.Pos()).String()+"="+id.Name)
								} else {
									t.Errorf("%s: refuseNonHumanServerCheckClear verb is not a clearVerb* constant", fset.Position(x.Pos()))
								}
							}
						}
					case *ast.SelectorExpr:
						if x.Sel.Name == "ActorIsDelegated" {
							flagged[name] = true
						}
					case *ast.Ident:
						if x.Name == "refusedActorDelegated" {
							flagged[name] = true
						}
					}
					return true
				})
			}
		}
	}

	where := map[string][]string{}
	for _, site := range serverCheckClearAction.Sites {
		where[site] = append(where[site], "D2 "+serverCheckClearAction.ID)
	}
	for _, ex := range humanOnlyExclusions {
		for _, site := range ex.Sites {
			where[site] = append(where[site], "exclusion "+ex.Clause)
		}
	}
	for site := range completenessNonRefusing {
		where[site] = append(where[site], "non-refusing classifier")
	}

	var names []string
	for n := range flagged {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		switch got := where[n]; len(got) {
		case 0:
			t.Errorf("%s refuses on an agent classifier or a delegated flag but is in neither the ADR-143 D2 list nor its exclusions: "+
				"add it to human_only_actions.go (a D2 site, or the exclusion whose clause covers it) — extending D2 needs an ADR-143 amendment", n)
		case 1:
		default:
			t.Errorf("%s is listed %d times (%v); each flagged function belongs in exactly one place", n, len(got), got)
		}
	}
	var listed []string
	for site := range where {
		listed = append(listed, site)
	}
	sort.Strings(listed)
	for _, site := range listed {
		if !flagged[site] {
			t.Errorf("%s is listed (%v) but no longer calls an agent classifier or reads a delegated flag: remove the stale entry", site, where[site])
		}
	}

	allowed := map[string]bool{}
	for _, v := range []struct{ name, val string }{
		{"clearVerbWaive", clearVerbWaive}, {"clearVerbBulkWaive", clearVerbBulkWaive},
		{"clearVerbDefer", clearVerbDefer}, {"clearVerbResolve", clearVerbResolve},
	} {
		for _, ev := range serverCheckClearAction.Verbs {
			if ev == v.val {
				allowed[v.name] = true
			}
		}
	}
	if len(verbsPassed) != 4 {
		t.Errorf("refuseNonHumanServerCheckClear call sites = %d (%v), want 4 (waive, bulk waive, defer, resolve)", len(verbsPassed), verbsPassed)
	}
	for _, vp := range verbsPassed {
		verb := vp[strings.LastIndex(vp, "=")+1:]
		if !allowed[verb] {
			t.Errorf("refuseNonHumanServerCheckClear call %s passes a verb not in the D2 entry's verbs %v", vp, serverCheckClearAction.Verbs)
		}
	}

	// C10: the non-human close paths, which the classifier scan cannot see.
	pkg, _ := parseNonTestPackage(t, ".")
	for _, cv := range completenessCloseVetoes {
		f := pkg[cv.file]
		if f == nil {
			t.Errorf("close path %s.%s: file not found (%s)", cv.file, cv.fn, cv.reason)
			continue
		}
		found := funcDecl(f, cv.fn)
		if found == nil {
			t.Errorf("close path %s in %s no longer exists: re-establish how a server_check row is protected on that path (%s)", cv.fn, cv.file, cv.reason)
			continue
		}
		if cv.mustVeto && !bodyNames(found, "IsServerCheck") {
			t.Errorf("close path %s in %s no longer tests IsServerCheck: a non-human close of a server_check concern is now possible (%s)", cv.fn, cv.file, cv.reason)
		}
		if cv.callers != nil {
			if got := callersOf(pkg, cv.fn); !reflect.DeepEqual(got, cv.callers) {
				t.Errorf("close path %s callers = %v, want %v: a new caller is a new non-human close of a server_check concern — classify it here, with what protects a server_check row on it (%s)", cv.fn, got, cv.callers, cv.reason)
			}
		}
		for _, r := range cv.reRaisedBy {
			file, fn, _ := strings.Cut(r, ":")
			raiser := funcDecl(pkg[file], fn)
			switch {
			case raiser == nil:
				t.Errorf("close path %s: server-check raiser %s no longer exists, so nothing re-raises a server_check row it supersedes (%s)", cv.fn, r, cv.reason)
			case !bodyNames(raiser, "IsOpen"):
				t.Errorf("close path %s: raiser %s no longer de-duplicates on IsOpen (%s)", cv.fn, r, cv.reason)
			case bodyNames(raiser, "StateSuperseded"):
				t.Errorf("close path %s: raiser %s reads StateSuperseded, so a superseded server_check row can suppress its re-raise and %s becomes a silent non-human clear (%s)", cv.fn, r, cv.fn, cv.reason)
			}
		}
	}
}

// funcDecl returns the top-level function named fn in f, or nil.
func funcDecl(f *ast.File, fn string) *ast.FuncDecl {
	if f == nil {
		return nil
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Body != nil {
			return fd
		}
	}
	return nil
}

// bodyNames reports whether fd's body references a selector or identifier
// named name.
func bodyNames(fd *ast.FuncDecl, name string) bool {
	hit := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			hit = hit || x.Sel.Name == name
		case *ast.Ident:
			hit = hit || x.Name == name
		}
		return !hit
	})
	return hit
}

// callersOf lists, sorted, the "file:func" of every package function whose
// body calls fn (as fn or recv.fn).
func callersOf(pkg map[string]*ast.File, fn string) []string {
	var out []string
	for file, f := range pkg {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			calls := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					name := calleeName(c.Fun)
					calls = calls || name == fn || strings.HasSuffix(name, "."+fn)
				}
				return !calls
			})
			if calls {
				out = append(out, file+":"+fd.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// parseNonTestPackage parses every non-test .go file in dir, keyed by base
// file name.
func parseNonTestPackage(t *testing.T, dir string) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		out[filepath.Base(p)] = f
	}
	if len(out) == 0 {
		t.Fatalf("no Go files under %s", dir)
	}
	return out, fset
}

// calleeName renders a call's function expression as "name" or "x.name".
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	}
	return ""
}

// d2ClearRoutes are the four REST routes of the ADR-143 D2 server_check_clear
// action, keyed by the handler handlers.go registers them on.
var d2ClearRoutes = map[string]string{
	"handleWaiveConcern":      "POST /v0/concerns/{concern_id}/waive",
	"handleBulkWaiveConcerns": "POST /v0/runs/{run_id}/concerns/waive",
	"handleDeferConcern":      "POST /v0/concerns/{concern_id}/defer",
	"handleResolveConcerns":   "POST /v0/runs/{run_id}/concerns/resolve",
}

// TestHumanOnlyActions_NoCLIClearVerb pins ADR-143 D3: the CLI is not a human
// channel for the D2 actions, so no `fishhawk` command clears a concern. It
// pins the four D2 routes in handlers.go (every one carries a "concerns" path
// segment), then fails if any non-test Go file under cli/ holds a string
// literal that could address one ("concerns/", "/concerns" or "concerns"), or
// if the CLI command registry (cli/internal/cmdinfo, which the CLI's own
// TestInventoryCoversDispatch ties to the dispatch tables) names a command or
// subcommand word in {concern, concerns, waive, defer, resolve}. Adding such a
// verb needs an ADR-143 amendment adopting a proof of presence, so a failure
// here names that, not a test fix.
//
// COUNTERFACTUAL (hand-run, #5228 fix-up, both observed RED and restored):
// (1) appending `const cfConcernRoute = "/v0/concerns/%s/waive"` to
// cli/cmd/fishhawk/run.go (it still vets) → RED "cli/cmd/fishhawk/run.go:
// string literal \"/v0/concerns/%s/waive\" addresses a concern route"; (2)
// adding the subcommand "waive" to the "run" group in
// cli/internal/cmdinfo/cmdinfo.go → RED "CLI command registry names
// \"waive\"".
func TestHumanOnlyActions_NoCLIClearVerb(t *testing.T) {
	pkg, _ := parseNonTestPackage(t, ".")
	got := map[string]string{}
	ast.Inspect(pkg["handlers.go"], func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != "mux.HandleFunc" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		ast.Inspect(call.Args[1], func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok {
				if _, d2 := d2ClearRoutes[sel.Sel.Name]; d2 {
					got[sel.Sel.Name] = pattern
				}
			}
			return true
		})
		return true
	})
	if !reflect.DeepEqual(got, d2ClearRoutes) {
		t.Fatalf("handlers.go D2 clearing routes = %v, want %v: re-derive this test's CLI needles from the new routes", got, d2ClearRoutes)
	}
	for _, route := range d2ClearRoutes {
		if !strings.Contains(route, "/concerns/") {
			t.Fatalf("D2 route %q has no concerns path segment: re-derive this test's CLI needles", route)
		}
	}

	cliRoot := filepath.Join("..", "..", "..", "cli")
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(cliRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		scanned++
		rel, _ := filepath.Rel(filepath.Join("..", "..", ".."), p)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, _ := strconv.Unquote(lit.Value)
			if strings.Contains(v, "concerns/") || strings.HasSuffix(v, "/concerns") || v == "concerns" {
				t.Errorf("%s: string literal %q addresses a concern route: ADR-143 D3 admits no CLI path to waive, bulk waive, defer or resolve a concern until an amendment adopts a proof of presence", filepath.ToSlash(rel), v)
			}
			return true
		})
		if filepath.Base(p) == "cmdinfo.go" {
			checkCLIRegistry(t, f)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", cliRoot, err)
	}
	if scanned == 0 {
		t.Fatalf("no CLI Go files under %s", cliRoot)
	}
}

// checkCLIRegistry fails on a cmdinfo Key, Name or Subcommands literal naming
// a concern-clearing word.
func checkCLIRegistry(t *testing.T, f *ast.File) {
	t.Helper()
	banned := map[string]bool{"concern": true, "concerns": true, "waive": true, "defer": true, "resolve": true}
	seen := 0
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || (key.Name != "Key" && key.Name != "Name" && key.Name != "Subcommands") {
			return true
		}
		ast.Inspect(kv.Value, func(m ast.Node) bool {
			lit, ok := m.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, _ := strconv.Unquote(lit.Value)
			seen++
			for _, w := range strings.Fields(v) {
				if banned[w] {
					t.Errorf("CLI command registry names %q (%s): ADR-143 D3 admits no CLI verb that clears a concern", w, v)
				}
			}
			return true
		})
		return true
	})
	if seen == 0 {
		t.Errorf("cli/internal/cmdinfo/cmdinfo.go: no Key, Name or Subcommands literals found; re-point this registry check")
	}
}
