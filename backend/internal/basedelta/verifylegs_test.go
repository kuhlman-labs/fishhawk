package basedelta

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
)

// This file pins ADR-096 D3 to the real verify gate: every leg `scripts/test
// verify` runs, and every `_verify_gate_harnesses` entry, must be registered
// below with its inputs, and every input a leg reads as a whole-tree input
// must be covered by this repository's verdict_transfer declaration's
// verdict-wide (global_surface) or doc-check class — never by an inert one
// (thisRepoSurface: the live block once the operator lands it, the committed
// example until then). A new leg or harness without a row, a
// line of cmd_verify / _verify_gate_harnesses this file cannot classify, or a
// `_verify_*` leg invoking an undeclared script, fails the test by name.

type coverage int

const (
	// global: the input must be covered by the declaration's verdict-wide or
	// doc-check class (a change to it on either side forces a verify pass).
	global coverage = iota
	// goReach: the leg consumes the input only by compiling, linting or
	// testing Go packages, which rule (c)'s import graph already tracks.
	// Only .go files may be declared goReach.
	goReach
)

type legInput struct {
	glob string
	cov  coverage
}

type legSpec struct {
	// scripts are the scripts/<x> a `_verify_*` leg's body invokes; checked
	// two-way against the body.
	scripts []string
	inputs  []legInput
}

type legTable struct {
	verify          map[string]legSpec // cmd_verify legs
	harnesses       map[string]legSpec // _verify_gate_harnesses entries
	verifyPlumbing  []string           // exact cmd_verify lines that run no leg
	harnessPlumbing []string           // exact _verify_gate_harnesses lines
}

func g(glob string) legInput { return legInput{glob, global} }
func r(glob string) legInput { return legInput{glob, goReach} }

var goTestInputs = []legInput{r("**/*.go"), g("go.work"), g("**/go.mod"), g("**/go.sum")}

// hermetic is a harness that reads nothing outside scripts/ (its fixtures are
// temp dirs).
var hermetic = legSpec{inputs: []legInput{g("scripts/**")}}

var gateImagePinInputs = []legInput{
	g("deploy/gate-image/Dockerfile"), g("go.work"), g("*/go.mod"), g(".github/workflows/*.yml"),
}

var versionFiles = []legInput{
	g("backend/internal/version/version.go"), g("runner/internal/version/version.go"), g("cli/internal/version/version.go"),
}

// The checker reads these sources and tests existence of these linked targets.
// Keep the list aligned with the routes; unlinked documentation stays inert.
var agentContextInputs = []legInput{
	g("AGENTS*.md"),
	g("CLAUDE*.md"),
	g("docs/agents/**"),
	g(".agents/**"),
	g(".claude/**"),
	g("backend/internal/agenteval/README.md"),
	g("backend/internal/basedelta/README.md"),
	g("backend/internal/decisionrecord/README.md"),
	g("backend/internal/repodoc/README.md"),
	g("backend/internal/testanchor/README.md"),
	g("backend/internal/timescale/README.md"),
	g("backend/internal/wirecontract/README.md"),
	g("cli/README.md"),
	g("deploy/gate-image/README.md"),
	g("docs/ARCHITECTURE.md"),
	g("docs/BRAND_FOUNDATIONS.md"),
	g("docs/GROOMING_RUNBOOK.md"),
	g("docs/METHODOLOGY.md"),
	g("docs/MVP_SPEC.md"),
	g("docs/OPERATING_MODEL.md"),
	g("docs/adr/README.md"),
	g("docs/api/v0.md"),
	g("docs/board-capability-matrix.md"),
	g("docs/compliance/planreview-catchrate-evidence.md"),
	g("docs/compliance/prompt-injection-evidence.md"),
	g("docs/deploy/kubernetes.md"),
	g("docs/deploy/self-hosted.md"),
	g("docs/issue-comment-surfaces.md"),
	g("docs/spec/workflow-v2.md"),
	g("frontend/README.md"),
	g("runner/README.md"),
	g("runner/internal/gateiso/README.md"),
	g("scripts/README.md"),
	g("site/README.md"),
}

var realLegTable = legTable{
	verify: map[string]legSpec{
		// cmd_lint's host-toolchain precondition (#4245) runs
		// scripts/check-gate-image --host against the Dockerfile pins.
		"cmd_lint": {inputs: []legInput{
			r("**/*.go"), g(".golangci.yml"), g("go.work"), g("**/go.mod"),
			g("deploy/gate-image/Dockerfile"), g("scripts/check-gate-image"),
		}},
		"_verify_doc_line_budget": {inputs: []legInput{g("docs/ARCHITECTURE.md")}},
		"_verify_schema_sync": {scripts: []string{"sync-schemas"}, inputs: []legInput{
			g("docs/spec/**"),
			g("backend/internal/plan/schemas/**"), g("backend/internal/spec/schemas/**"),
			g("backend/internal/crewmessage/schemas/**"), g("backend/internal/specialistjob/schemas/**"),
			g("backend/internal/operatorrole/schemas/**"),
			g("backend/internal/workmgmt/schemas/**"), g("backend/internal/operatorrole/defaults/**"),
			g("backend/internal/workmgmt/defaults/**"), g("backend/internal/spec/presets/**"),
			g("cli/internal/spec/schemas/**"), g("cli/internal/spec/presets/**"), g("runner/internal/plan/schemas/**"),
			g("backend/internal/spec/testdata/*-fixtures.json"), g("cli/internal/spec/testdata/*-fixtures.json"),
			g("backend/internal/server/testdata/acceptance-verdict-fixtures.json"),
			g("runner/cmd/fishhawk-runner/testdata/acceptance-verdict-fixtures.json"),
		}},
		"_verify_gate_harnesses":       {},
		"_verify_site_voice":           {scripts: []string{"check-site-voice"}, inputs: []legInput{g("site/src/content/docs/**")}},
		"_verify_site_ia":              {scripts: []string{"check-site-ia"}, inputs: []legInput{g("site/src/content/docs/**"), g("site/astro.config.mjs")}},
		"_verify_adr_records":          {scripts: []string{"check-adr"}, inputs: []legInput{g("docs/adr/**")}},
		"_verify_gate_image_pins":      {scripts: []string{"check-gate-image"}, inputs: gateImagePinInputs},
		"_verify_pipefail_grep":        {scripts: []string{"check-pipefail-grep"}, inputs: []legInput{g("scripts/*")}},
		"_verify_agent_context":        {scripts: []string{"check-agent-context"}, inputs: agentContextInputs},
		"_verify_scope_buckets":        {inputs: []legInput{g("go.work")}},
		"cmd_test":                     {inputs: goTestInputs},
		"cmd_test_scoped":              {inputs: goTestInputs},
		"cmd_test_with_patch_coverage": {inputs: goTestInputs},
		"_verify_patch_coverage":       {scripts: []string{"check-coverage.py"}, inputs: []legInput{g("scripts/**")}},
	},
	harnesses: map[string]legSpec{
		"test-agent-context":       hermetic,
		"test-check-coverage":      hermetic,
		"test-patch-coverage":      hermetic,
		"test-container-lease":     hermetic,
		"test-verify-scope":        hermetic,
		"test-is-run-agent":        hermetic,
		"test-review-prompt-eval":  hermetic,
		"test-check-pipefail-grep": hermetic,
		"test-site-voice":          hermetic,
		"test-site-ia":             hermetic,
		"test-adr":                 hermetic,
		"test-dev": {inputs: append(append([]legInput{}, versionFiles...),
			g("backend/Dockerfile"), g("Makefile"), g("testdata/wire/restart_blockers.json"),
			r("runner/cmd/fishhawk-runner/*.go"))},
		"test-helm-render": {inputs: []legInput{g("deploy/helm/fishhawk/**"), g("backend/cmd/fishhawkd/**")}},
		"test-site-reference": {inputs: []legInput{
			g("site/src/content/docs/reference/*.md"), g("docs/spec/*.schema.json"), g("docs/spec/examples/*.yaml"),
			g("docs/api/v0.openapi.yaml"), g("cli/cmd/fishhawk-docgen/**"), g("cli/internal/docgen/**"),
			g("cli/internal/cmdinfo/**"), g("go.work"), g("cli/go.mod"),
		}},
		"test-check-gate-image": {inputs: gateImagePinInputs},
		"test-in-gate-image":    {inputs: []legInput{g("deploy/gate-image/Dockerfile")}},
		"test-release-ldflags": {inputs: append(append([]legInput{}, versionFiles...),
			r("backend/cmd/fishhawkd/*.go"), r("backend/cmd/fishhawk-mcp/*.go"), r("backend/cmd/fishhawk-mcp-shim/*.go"),
			r("runner/cmd/fishhawk-runner/*.go"), r("cli/cmd/fishhawk/*.go"), g("go.work"))},
	},
	verifyPlumbing: []string{
		`_acquire_verify_lock || exit "$?"`,
		`fi`,
		`if [ "$VERIFY_NO_TESTS" = "1" ]; then`,
		`echo "scripts/test: verify --no-tests — the go test loop and the patch-coverage gate are SKIPPED (the daemon-dependent tests need full gate isolation, #2137); the full verify still runs them" >&2`,
		`return 0`,
		`local buckets=""`,
		`echo "scripts/test: scoped verify (--packages $VERIFY_PACKAGES) — the patch-coverage gate runs in the full committed-tree gate" >&2`,
		`local base="" mods=""`,
		`if [ -n "${FISHHAWK_SKIP_PATCH_COVERAGE:-}" ]; then`,
		`echo "scripts/test: FISHHAWK_SKIP_PATCH_COVERAGE set — skipping the patch-coverage gate" >&2`,
		`elif ! base="$(_patch_cov_base)"; then`,
		`echo "scripts/test: no usable diff base (git absent, not a work tree, or unresolvable ref) — skipping the patch-coverage gate" >&2`,
		`elif ! mods="$(_patch_cov_changed_modules "$base")"; then`,
		`echo "scripts/test: could not bucket changed Go files (no merge base, or go/jq unavailable) — skipping the patch-coverage gate" >&2`,
		`elif [ -z "$mods" ]; then`,
		`echo "scripts/test: no changed Go packages vs $base — skipping the patch-coverage gate" >&2`,
		`else`,
	},
	harnessPlumbing: []string{
		`local h`,
		`if [ ! -x "$ROOT/scripts/$h" ]; then`,
		`echo "scripts/test: scripts/$h missing or not executable — skipping that harness" >&2`,
		`continue`,
		`fi`,
		`if [ "$h" = "test-dev" ] && ! command -v zsh >/dev/null 2>&1; then`,
		`echo "scripts/test: scripts/test-dev needs zsh, absent from PATH — skipping that harness" >&2`,
		`printf '>>> scripts/%s\n' "$h"`,
		`( unset FISHHAWK_VERIFY_LOCK_PATH FISHHAWK_VERIFY_LOCK_OWNER; "$ROOT/scripts/$h" )`,
		`done`,
	},
}

// Anti-vacuity floors: a parser collapse must go RED, not pass empty.
const (
	minVerifyLegs       = 8
	minHarnesses        = 12
	minSyncDestinations = 10
)

var (
	// legToken is word-anchored: `_acquire_verify_lock` yields itself, never
	// `_verify_lock`.
	legToken   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])((?:cmd_|_)[a-z0-9_]+)`)
	scriptRef  = regexp.MustCompile(`\$(?:ROOT|\{ROOT\})/scripts/([A-Za-z0-9._-]+)|\$(?:SCRIPTS_DIR|\{SCRIPTS_DIR\})/([A-Za-z0-9._-]+)`)
	harnessFor = regexp.MustCompile(`^for h in (.+); do$`)
)

// funcBody returns the lines between `name() {` and the first column-0 `}`.
func funcBody(script, name string) ([]string, bool) {
	var out []string
	in := false
	for _, l := range strings.Split(script, "\n") {
		if !in {
			if strings.HasPrefix(l, name+"() {") {
				in = true
			}
			continue
		}
		if l == "}" {
			return out, true
		}
		out = append(out, l)
	}
	return nil, false
}

// codeLines drops blank and comment lines and trims the rest.
func codeLines(body []string) []string {
	var out []string
	for _, l := range body {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, t)
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// checkVerifyLegs is the pure checker behind the pin. files are repo-relative
// slash paths of the tree the inputs are matched against; s is the
// declaration whose verdict-wide or doc-check class must cover every global
// input (an inert class never does; a nil s covers nothing but the workflow
// spec).
func checkVerifyLegs(script string, tbl legTable, files []string, s *Surface) []error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	covered := func(f string) bool { return s.matchVerdictWide(f) || s.matchDocCheck(f) }

	// cmd_verify: every code line is exact plumbing or a registered-leg line.
	body, ok := funcBody(script, "cmd_verify")
	if !ok {
		return []error{fmt.Errorf("scripts/test has no cmd_verify() body")}
	}
	plumbing := toSet(tbl.verifyPlumbing)
	found := map[string]bool{}
	for _, line := range codeLines(body) {
		if plumbing[line] {
			continue
		}
		var toks []string
		for _, m := range legToken.FindAllStringSubmatch(line, -1) {
			toks = append(toks, m[1])
		}
		unregistered := ""
		for _, tok := range toks {
			if _, ok := tbl.verify[tok]; !ok {
				unregistered = tok
				break
			}
		}
		switch {
		case len(toks) == 0:
			fail("cmd_verify line is neither a registered leg nor allow-listed plumbing: %q", line)
		case unregistered != "":
			fail("cmd_verify calls unregistered leg %s (register it in realLegTable and cover its inputs): %q", unregistered, line)
		case strings.Contains(line, "scripts/") || strings.Contains(line, "SCRIPTS_DIR"):
			fail("cmd_verify leg line also invokes a script inline: %q", line)
		default:
			for _, tok := range toks {
				found[tok] = true
			}
		}
	}
	for leg := range found {
		if _, ok := tbl.verify[leg]; !ok {
			fail("cmd_verify leg %s is not registered", leg)
		}
	}
	for _, leg := range sortedKeys(tbl.verify) {
		if !found[leg] {
			fail("registered leg %s no longer appears in cmd_verify (stale table entry)", leg)
		}
	}
	if len(found) < minVerifyLegs || !found["cmd_lint"] || !found["_verify_gate_harnesses"] {
		fail("cmd_verify yielded %d legs (want >= %d including cmd_lint and _verify_gate_harnesses): parser collapse?", len(found), minVerifyLegs)
	}

	// _verify_gate_harnesses: exactly one `for h in ...; do` list plus plumbing.
	hbody, ok := funcBody(script, "_verify_gate_harnesses")
	if !ok {
		fail("scripts/test has no _verify_gate_harnesses() body")
	}
	hplumbing := toSet(tbl.harnessPlumbing)
	var names []string
	forLines := 0
	for _, line := range codeLines(hbody) {
		if m := harnessFor.FindStringSubmatch(line); m != nil {
			forLines++
			names = append(names, strings.Fields(m[1])...)
			continue
		}
		if !hplumbing[line] {
			fail("_verify_gate_harnesses line is neither the harness list nor allow-listed plumbing: %q", line)
		}
	}
	if forLines != 1 {
		fail("_verify_gate_harnesses has %d `for h in ...; do` lines, want 1", forLines)
	}
	hset := toSet(names)
	for _, h := range names {
		if _, ok := tbl.harnesses[h]; !ok {
			fail("harness %s runs in _verify_gate_harnesses but is not registered (register it and cover its inputs)", h)
		}
		if !covered("scripts/" + h) {
			fail("harness scripts/%s is not covered by the declaration", h)
		}
	}
	for _, h := range sortedKeys(tbl.harnesses) {
		if !hset[h] {
			fail("registered harness %s no longer runs in _verify_gate_harnesses (stale table entry)", h)
		}
	}
	if len(hset) < minHarnesses {
		fail("_verify_gate_harnesses yielded %d harnesses (want >= %d): parser collapse?", len(hset), minHarnesses)
	}

	// Each registered _verify_* leg's script invocations match its row.
	for _, leg := range sortedKeys(tbl.verify) {
		if !strings.HasPrefix(leg, "_verify_") {
			continue
		}
		lbody, ok := funcBody(script, leg)
		if !ok {
			fail("registered leg %s has no %s() body in scripts/test", leg, leg)
			continue
		}
		got := map[string]bool{}
		for _, line := range codeLines(lbody) {
			for _, m := range scriptRef.FindAllStringSubmatch(line, -1) {
				got[m[1]+m[2]] = true
			}
		}
		want := toSet(tbl.verify[leg].scripts)
		for s := range got {
			if !want[s] {
				fail("leg %s invokes scripts/%s, not declared in its row", leg, s)
			}
		}
		for _, s := range sortedKeys(want) {
			if !got[s] {
				fail("leg %s declares scripts/%s but its body never invokes it", leg, s)
			}
			if !covered("scripts/" + s) {
				fail("leg %s's scripts/%s is not covered by the declaration", leg, s)
			}
		}
	}

	// Every declared input matches files, and each match is covered.
	check := func(owner string, spec legSpec) {
		for _, in := range spec.inputs {
			n := 0
			for _, f := range files {
				ok, err := doublestar.Match(in.glob, f)
				if err != nil {
					fail("%s: input glob %q: %v", owner, in.glob, err)
					break
				}
				if !ok {
					continue
				}
				n++
				switch in.cov {
				case global:
					if !covered(f) {
						fail("%s reads %s (input %q) but it is not covered by the declaration's verdict-wide (global_surface) or doc-check class", owner, f, in.glob)
					}
				case goReach:
					if !strings.HasSuffix(f, ".go") {
						fail("%s: go_reach input %q matches non-Go file %s", owner, in.glob, f)
					}
				}
			}
			if n == 0 {
				fail("%s: input glob %q matches no file", owner, in.glob)
			}
		}
	}
	for _, leg := range sortedKeys(tbl.verify) {
		check("leg "+leg, tbl.verify[leg])
	}
	for _, h := range sortedKeys(tbl.harnesses) {
		check("harness "+h, tbl.harnesses[h])
	}
	return errs
}

// repoFiles lists the repository's files, repo-relative and slash-separated.
func repoFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot()
	skip := map[string]bool{".git": true, "node_modules": true, "bin": true, "dist": true, ".astro": true}
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip[d.Name()] && p != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestVerifyLegsCoveredByDeclaration(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "scripts", "test"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range checkVerifyLegs(string(raw), realLegTable, repoFiles(t), thisRepoSurface(t)) {
		t.Error(e)
	}
}

var syncDest = regexp.MustCompile(`"\$REPO_ROOT/([^"]+)"`)

func TestSchemaSyncDestinationsCoveredByDeclaration(t *testing.T) {
	t.Parallel()
	surface := thisRepoSurface(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "scripts", "sync-schemas"))
	if err != nil {
		t.Fatal(err)
	}
	files := repoFiles(t)
	dests := map[string]bool{}
	for _, m := range syncDest.FindAllStringSubmatch(string(raw), -1) {
		d := m[1]
		if strings.HasPrefix(d, "docs/spec/") {
			continue // a source, not a mirror
		}
		dests[d] = true
	}
	if len(dests) < minSyncDestinations {
		t.Fatalf("scripts/sync-schemas yielded %d destinations (want >= %d): parser collapse?", len(dests), minSyncDestinations)
	}
	for _, d := range sortedKeys(dests) {
		prefix, exact := d, true
		if dir, ok := strings.CutSuffix(d, "/$base"); ok {
			prefix, exact = dir+"/", false
		}
		n := 0
		for _, f := range files {
			if (exact && f == prefix) || (!exact && strings.HasPrefix(f, prefix)) {
				n++
				if !surface.matchVerdictWide(f) && !surface.matchDocCheck(f) {
					t.Errorf("sync-schemas mirror %s (destination %s) is not covered by the declaration's verdict-wide or doc-check class", f, d)
				}
			}
		}
		if n == 0 {
			t.Errorf("sync-schemas destination %s matches no file", d)
		}
	}
}

// --- synthetic checker cases -------------------------------------------------

const synthLegCount = 8

func synthTable() legTable {
	tbl := legTable{verify: map[string]legSpec{}, harnesses: map[string]legSpec{}}
	tbl.verify["cmd_lint"] = legSpec{inputs: []legInput{r("**/*.go")}}
	tbl.verify["_verify_gate_harnesses"] = legSpec{}
	for i := 0; i < synthLegCount-2; i++ {
		tbl.verify[fmt.Sprintf("_verify_leg_%c", 'a'+i)] = legSpec{scripts: []string{"check-x"}, inputs: []legInput{g("docs/ARCHITECTURE.md")}}
	}
	for i := 0; i < minHarnesses; i++ {
		tbl.harnesses[fmt.Sprintf("test-h%02d", i)] = hermetic
	}
	tbl.verifyPlumbing = []string{`if ! _acquire_verify_lock; then`, `exit 1`, `fi`}
	tbl.harnessPlumbing = []string{`local h`, `"$ROOT/scripts/$h"`, `done`}
	return tbl
}

func synthScript(extraVerify, extraHarness string, dropLeg string) string {
	var b strings.Builder
	b.WriteString("cmd_verify() {\n  if ! _acquire_verify_lock; then\n    exit 1\n  fi\n  # a comment\n\n  cmd_lint\n")
	for i := 0; i < synthLegCount-2; i++ {
		leg := fmt.Sprintf("_verify_leg_%c", 'a'+i)
		if leg != dropLeg {
			b.WriteString("  " + leg + "\n")
		}
	}
	b.WriteString("  _verify_gate_harnesses\n" + extraVerify + "}\n\n")
	b.WriteString("_verify_gate_harnesses() {\n  local h\n  for h in")
	for i := 0; i < minHarnesses; i++ {
		fmt.Fprintf(&b, " test-h%02d", i)
	}
	b.WriteString(extraHarness + "; do\n    \"$ROOT/scripts/$h\"\n  done\n}\n\n")
	for i := 0; i < synthLegCount-2; i++ {
		fmt.Fprintf(&b, "_verify_leg_%c() {\n  \"$ROOT/scripts/check-x\" --root \"$ROOT\"\n}\n\n", 'a'+i)
	}
	return b.String()
}

var synthFiles = []string{"docs/ARCHITECTURE.md", "backend/a.go", "scripts/check-x", "README.md", "frontend/x.json"}

// synthSurface covers the synthetic legs' inputs verdict-wide and declares
// frontend/ inert only (H9).
var synthSurface = &Surface{Global: []string{"scripts/**", "docs/ARCHITECTURE.md"}, Inert: []string{"frontend/**"}}

func wantErr(t *testing.T, errs []error, needle string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), needle) {
			return
		}
	}
	t.Fatalf("no error containing %q; got %v", needle, errs)
}

func TestCheckVerifyLegs_Synthetic(t *testing.T) {
	t.Parallel()
	if errs := checkVerifyLegs(synthScript("", "", ""), synthTable(), synthFiles, synthSurface); len(errs) != 0 {
		t.Fatalf("clean synthetic script: %v", errs)
	}
	// H1: a new cmd_verify leg.
	wantErr(t, checkVerifyLegs(synthScript("  _verify_new_leg\n", "", ""), synthTable(), synthFiles, synthSurface), "unregistered leg _verify_new_leg")
	// H2: a new harness.
	wantErr(t, checkVerifyLegs(synthScript("", " test-new", ""), synthTable(), synthFiles, synthSurface), "harness test-new runs")
	// H3: a global input matching a non-global file.
	h3 := synthTable()
	h3.verify["_verify_leg_a"] = legSpec{scripts: []string{"check-x"}, inputs: []legInput{g("README.md")}}
	wantErr(t, checkVerifyLegs(synthScript("", "", ""), h3, synthFiles, synthSurface), "reads README.md")
	// H4: a go_reach glob matching a non-Go file.
	h4 := synthTable()
	h4.verify["cmd_lint"] = legSpec{inputs: []legInput{r("frontend/*")}}
	wantErr(t, checkVerifyLegs(synthScript("", "", ""), h4, synthFiles, synthSurface), "matches non-Go file frontend/x.json")
	// H5: a stale table entry.
	wantErr(t, checkVerifyLegs(synthScript("", "", "_verify_leg_a"), synthTable(), synthFiles, synthSurface), "_verify_leg_a no longer appears")
	// H6: an input glob matching zero files.
	h6 := synthTable()
	h6.verify["_verify_leg_b"] = legSpec{scripts: []string{"check-x"}, inputs: []legInput{g("docs/nothing/**")}}
	wantErr(t, checkVerifyLegs(synthScript("", "", ""), h6, synthFiles, synthSurface), `"docs/nothing/**" matches no file`)
	// H7: parser collapse (fewer legs than the floor).
	h7 := synthTable()
	delete(h7.verify, "_verify_leg_a")
	delete(h7.verify, "_verify_leg_b")
	collapsed := strings.Replace(synthScript("", "", ""), "  _verify_leg_b\n", "", 1)
	collapsed = strings.Replace(collapsed, "  _verify_leg_a\n", "", 1)
	wantErr(t, checkVerifyLegs(collapsed, h7, synthFiles, synthSurface), "parser collapse")
	// H8: an inline script invocation, and an unregistered helper call.
	wantErr(t, checkVerifyLegs(synthScript("  \"$ROOT/scripts/check-new\"\n", "", ""), synthTable(), synthFiles, synthSurface),
		`neither a registered leg nor allow-listed plumbing: "\"$ROOT/scripts/check-new\""`)
	wantErr(t, checkVerifyLegs(synthScript("  _check_new\n", "", ""), synthTable(), synthFiles, synthSurface), "unregistered leg _check_new")
	// A leg line that also runs a script inline.
	wantErr(t, checkVerifyLegs(synthScript("  _verify_leg_a; \"$ROOT/scripts/check-new\"\n", "", ""), synthTable(), synthFiles, synthSurface), "invokes a script inline")
	// A _verify_* leg body invoking an undeclared script.
	undeclared := strings.Replace(synthScript("", "", ""), "_verify_leg_c() {\n", "_verify_leg_c() {\n  \"$ROOT/scripts/check-y\"\n", 1)
	wantErr(t, checkVerifyLegs(undeclared, synthTable(), synthFiles, synthSurface), "leg _verify_leg_c invokes scripts/check-y")
	// An unclassified harness-body line.
	wantErr(t, checkVerifyLegs(strings.Replace(synthScript("", "", ""), "  local h\n", "  local h\n  rm -rf x\n", 1), synthTable(), synthFiles, synthSurface),
		`_verify_gate_harnesses line is neither the harness list nor allow-listed plumbing: "rm -rf x"`)
	// H9: an input covered only by an inert class is not covered.
	h9 := synthTable()
	h9.verify["_verify_leg_a"] = legSpec{scripts: []string{"check-x"}, inputs: []legInput{g("frontend/x.json")}}
	wantErr(t, checkVerifyLegs(synthScript("", "", ""), h9, synthFiles, synthSurface),
		"reads frontend/x.json (input \"frontend/x.json\") but it is not covered by the declaration's verdict-wide (global_surface) or doc-check class")
	// The doc-check class does cover an input.
	docCheck := &Surface{Global: []string{"scripts/**"}, DocCheck: []string{"docs/ARCHITECTURE.md"}}
	if errs := checkVerifyLegs(synthScript("", "", ""), synthTable(), synthFiles, docCheck); len(errs) != 0 {
		t.Fatalf("doc-check-covered input: %v", errs)
	}
	// H10: an undeclared (nil) surface covers no global input.
	h10 := checkVerifyLegs(synthScript("", "", ""), synthTable(), synthFiles, nil)
	wantErr(t, h10, "reads docs/ARCHITECTURE.md")
	wantErr(t, h10, "harness scripts/test-h00 is not covered by the declaration")
}

func TestLegTokenWordAnchored(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		`if ! _acquire_verify_lock; then x="$(cmd_test_scoped)" $VERIFY_NO_TESTS`: "_acquire_verify_lock,cmd_test_scoped",
		// An unanchored extractor pulls the leg-shaped tokens cmd_lint and
		// _verify_lock out of these words.
		`xcmd_lint`:           "",
		`acquire_verify_lock`: "",
		`run xcmd_lint && my_verify_lock; cmd_lint`: "cmd_lint",
	} {
		var toks []string
		for _, m := range legToken.FindAllStringSubmatch(line, -1) {
			toks = append(toks, m[1])
		}
		if got := strings.Join(toks, ","); got != want {
			t.Errorf("tokens of %q = %q, want %q", line, got, want)
		}
	}
}
