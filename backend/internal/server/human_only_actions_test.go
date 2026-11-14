package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
// slip past the registry unnoticed. Each entry: the function must exist, and
// when mustVeto is true its body must test IsServerCheck.
var completenessCloseVetoes = []struct {
	file, fn string
	mustVeto bool
	reason   string
}{
	{"trace.go", "vetoReason", true,
		"reviewer resolutions (confirm, supersede) and the clean-round auto-close: vetoServerCheckRequiresHuman is evaluated FIRST, so no reviewer verdict retires a server_check row"},
	{"concern_rollup.go", "supersedeRolledUpCopies", false,
		"supersedes a decomposition parent's COPY only after its child source was superseded by supersedeOpenImplementConcerns (a stage retry discarding the pass); it is not a clearing verb and carries no provenance veto of its own"},
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
// this test, naming each of the three as unclassified.
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
		var found *ast.FuncDecl
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == cv.fn {
				found = fd
			}
		}
		if found == nil {
			t.Errorf("close path %s in %s no longer exists: re-establish how a server_check row is protected on that path (%s)", cv.fn, cv.file, cv.reason)
			continue
		}
		if !cv.mustVeto {
			continue
		}
		vetoes := false
		ast.Inspect(found.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "IsServerCheck" {
				vetoes = true
			}
			return true
		})
		if !vetoes {
			t.Errorf("close path %s in %s no longer tests IsServerCheck: a non-human close of a server_check concern is now possible (%s)", cv.fn, cv.file, cv.reason)
		}
	}
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
