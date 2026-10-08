package permdrift

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

func side(s string) FileSide { return FileSide{Content: []byte(s), Exists: true} }

var absentSide = FileSide{}

// detectAll resolves path's surfaces ONLY through MatchSurfaces over
// surfaces and aggregates Detect across them.
func detectAll(surfaces []Surface, path string, base, head FileSide) (widened, narrowed []Change, unevaluable []string) {
	for _, s := range MatchSurfaces(surfaces, path) {
		r := Detect(s, base, head)
		widened = append(widened, r.Widened...)
		narrowed = append(narrowed, r.Narrowed...)
		if r.Unevaluable != "" {
			unevaluable = append(unevaluable, s.ID+":"+r.Unevaluable)
		}
	}
	return widened, narrowed, unevaluable
}

func TestDefaultSurfaces(t *testing.T) {
	if SurfacesVersion != 1 {
		t.Errorf("SurfacesVersion = %d, want 1", SurfacesVersion)
	}
	seen := map[string]bool{}
	for _, s := range DefaultSurfaces() {
		if !surfaceIDPattern.MatchString(s.ID) || seen[s.ID] {
			t.Errorf("surface id %q malformed or duplicated", s.ID)
		}
		seen[s.ID] = true
		if !knownKinds[s.Kind] {
			t.Errorf("%s: unknown kind %q", s.ID, s.Kind)
		}
		// The ratified severity map: high everywhere, medium for App events.
		want := SeverityHigh
		if s.Kind == KindAppEventsJSON || s.Kind == KindAppEventsGo {
			want = SeverityMedium
		}
		if s.Severity != want {
			t.Errorf("%s: severity %q, want %q", s.ID, s.Severity, want)
		}
		if len(s.Paths) == 0 || s.Description == "" {
			t.Errorf("%s: empty paths or description", s.ID)
		}
		for _, p := range s.Paths {
			if !doublestar.ValidatePattern(p) {
				t.Errorf("%s: invalid glob %q", s.ID, p)
			}
		}
	}
	if len(seen) != 13 {
		t.Errorf("DefaultSurfaces has %d surfaces, want 13", len(seen))
	}
	// A fresh slice per call: a caller's append never reaches the next caller.
	a := DefaultSurfaces()
	a[0].ID = "mutated"
	if DefaultSurfaces()[0].ID == "mutated" {
		t.Error("DefaultSurfaces aliases its backing array")
	}
}

func TestMatchSurfaces(t *testing.T) {
	ids := func(ss []Surface) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return out
	}
	cases := map[string][]string{
		".github/workflows/ci.yml":               {"gha-workflow-permissions"},
		".github/workflows/release.yaml":         {"gha-workflow-permissions"},
		".github/workflows/nested/ci.yml":        nil,
		"backend/internal/server/manifest.go":    {"github-app-permissions-go", "github-app-events-go"},
		"docs/github-app/manifest.template.json": {"github-app-permissions-template", "github-app-events-template"},
		".fishhawk/workflows.yaml": {"fishhawk-spec-forbidden-paths", "fishhawk-spec-escalations",
			"fishhawk-spec-autonomy", "fishhawk-spec-stage-permissions"},
		RepoSurfacesPath: {SurfaceIDSurfaceDeclarations},
		"README.md":      nil,
	}
	for path, want := range cases {
		if got := ids(MatchSurfaces(DefaultSurfaces(), path)); !reflect.DeepEqual(got, want) {
			t.Errorf("MatchSurfaces(%q) = %v, want %v", path, got, want)
		}
	}
}

const actionsPerms = `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
%PERMS%
    steps:
      - run: echo
`

func actionsDoc(perms string) string { return strings.Replace(actionsPerms, "%PERMS%", perms, 1) }

// TestDetect_DefaultSurfaces_AcceptanceFixtures is the issue's acceptance
// table: each fixture is resolved ONLY through MatchSurfaces(DefaultSurfaces(),
// path) and aggregated across every matched surface.
//
// COUNTERFACTUAL (the issue's own): delete the fishhawk-spec-forbidden-paths
// entry from DefaultSurfaces. The "forbidden_paths entry removed" fixture's
// only change is the removed glob ".fishhawk/**"; no remaining surface has
// the forbidden_paths kind, so no Change is produced and the "exactly one
// widening" assertion goes RED.
func TestDetect_DefaultSurfaces_AcceptanceFixtures(t *testing.T) {
	widening := []struct {
		name, path, base, head string
		want                   Change
	}{
		{"workflow gains contents: write", ".github/workflows/ci.yml",
			actionsDoc("      contents: read"), actionsDoc("      contents: write"),
			Change{Key: "jobs.build.contents", Before: "read", After: "write", Direction: Widened}},
		{"token scope added to a stage type", "backend/internal/server/mcptoken.go",
			tokenSrc(), tokenSrc("%PRED%", " || st == run.StageTypeImplement"),
			Change{Key: "run_token.scopeWriteMessages@implement", Before: Absent, After: Present, Direction: Widened}},
		{"forbidden_paths entry removed", ".fishhawk/workflows.yaml",
			specDoc(), specDoc("%FORBIDDEN%", `[".github/workflows/**"]`),
			Change{Key: "workflows.feature_change.stages.implement.forbidden_paths[.fishhawk/**]", Before: Present, After: Absent, Direction: Widened}},
		{"escalation max_autonomy raised", ".fishhawk/workflows.yaml",
			specDoc(), specDoc("%ESCALATIONS%", `    escalations:
      - match:
          paths: ["a/**", "b/**"]
        require:
          max_autonomy: medium`),
			Change{Key: `workflows.feature_change.escalations[{"paths":["a/**","b/**"]}].max_autonomy`, Before: "low", After: "medium", Direction: Widened}},
	}
	for _, c := range widening {
		t.Run(c.name, func(t *testing.T) {
			w, _, unev := detectAll(DefaultSurfaces(), c.path, side(c.base), side(c.head))
			if len(unev) > 0 {
				t.Fatalf("unevaluable: %v", unev)
			}
			if len(w) != 1 || w[0] != c.want {
				t.Errorf("widenings = %+v, want exactly [%+v]", w, c.want)
			}
		})
	}
	t.Run("reordered forbidden_paths is no widening", func(t *testing.T) {
		w, n, unev := detectAll(DefaultSurfaces(), ".fishhawk/workflows.yaml",
			side(specDoc()), side(specDoc("%FORBIDDEN%", `[".fishhawk/**", ".github/workflows/**"]`)))
		if len(w)+len(n)+len(unev) != 0 {
			t.Errorf("widened %+v narrowed %+v unevaluable %v; want nothing", w, n, unev)
		}
	})
	t.Run("removed permission is exactly one narrowing", func(t *testing.T) {
		w, n, unev := detectAll(DefaultSurfaces(), ".github/workflows/ci.yml",
			side(actionsDoc("      contents: read\n      issues: write")), side(actionsDoc("      contents: read")))
		want := []Change{{Key: "jobs.build.issues", Before: "write", After: Absent, Direction: Narrowed}}
		if len(w)+len(unev) != 0 || !reflect.DeepEqual(n, want) {
			t.Errorf("widened %+v narrowed %+v unevaluable %v; want narrowed %+v only", w, n, unev, want)
		}
	})
}

// TestDetect_GoSurfaceShapeLostIsUnevaluable pins the shape guard, one arm
// per clause, each asserting Unevaluable shape_unrecognized with NO changes
// (never a narrowing). Each arm's fixture isolates one clause of
// shapeProblem, so deleting that clause (body mutation) turns exactly that
// arm RED:
//
//   - unresolved clause: "append moved into a helper" and "anyOf built by a
//     call" keep the anchor and every other entry at head, so only the
//     unresolved construct marks them; without the clause the helper case
//     reads as the message grants narrowing and the anyOf case as a tool's
//     grants vanishing.
//   - anchor clause: "env-allow var made a call" keeps BaseAllow and
//     ClaudeAllow (head has entries) and records nothing unresolved (a var
//     whose value is a non-append call is not a string-slice var), so only
//     the missing env_allow.CodexAllow anchor marks it.
//   - zero-entries clause: "table emptied" keeps the mcpToolScopes anchor and
//     resolves cleanly, so only "base had entries, head has none" marks it.
//   - package-var write sweep (#3939 F6, an unresolved-clause feeder): "a
//     package var mutated in init" keeps every declaration literal, anchor
//     and grant unchanged, so only the sweep's Unresolved marks it; with
//     sweepPackageVarWrites returning immediately (body mutation) the arm
//     reads as evaluable with no change and goes RED.
func TestDetect_GoSurfaceShapeLostIsUnevaluable(t *testing.T) {
	surface := func(id string) Surface {
		for _, s := range DefaultSurfaces() {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no surface %q", id)
		return Surface{}
	}
	cases := []struct {
		name, surface, base, head string
	}{
		{"mcpToolScopes renamed", "mcp-tool-scopes",
			mcpScopes(), strings.Replace(mcpScopes(), "var mcpToolScopes", "var mcpToolScopeTable", 1)},
		{"table emptied", "mcp-tool-scopes",
			mcpScopes(), "package server\n\nvar mcpToolScopes = map[string]mcpToolScopeRule{}\n"},
		{"anyOf built by a call", "mcp-tool-scopes",
			mcpScopes(), mcpScopes("%START%", `{anyOf: scopesFor("start")}`)},
		{"append moved into a helper", "run-token-scope-grants",
			tokenSrc(), tokenSrc("%MESSAGES%", `	scopes = addMessageScope(scopes, stageType)`) +
				"\nfunc addMessageScope(s []string, st run.StageType) []string {\n\tif stageTypeMayMessage(st) {\n\t\treturn append(s, scopeWriteMessages)\n\t}\n\treturn s\n}\n"},
		{"token-scope function lost its scopes literal", "run-token-scope-grants",
			tokenSrc(), "package server\n\nfunc (s *Server) handleIssueMCPToken() {\n\tscopes := mintScopes(s)\n\ts.issue(scopes)\n}\n"},
		{"env-allow var made a call", "reviewer-env-allowlist",
			envSrc(), envSrc("%CODEX%", "loadCodexAllow()")},
		{"manifest literal lost default_events", "github-app-events-go",
			manifestGo(`"contents": "write",`, `"push",`),
			strings.Replace(manifestGo(`"contents": "write",`, `"push",`), `"default_events"`, `"events"`, 1)},
		{"a package var mutated in init", "reviewer-env-allowlist",
			envSrc(), envSrc() + "\nfunc init() { BaseAllow[0] = \"GITHUB_TOKEN\" }\n"},
		{"manifest events built by a call", "github-app-events-go",
			manifestGo(`"contents": "write",`, `"push",`),
			strings.Replace(manifestGo(`"contents": "write",`, `"push",`), "[]string{\n\"push\",\n\t\t}", "events()", 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Detect(surface(c.surface), side(c.base), side(c.head))
			if r.Unevaluable != ReasonShapeUnrecognized || len(r.Widened)+len(r.Narrowed) != 0 {
				t.Errorf("Detect = %+v; want Unevaluable %q and no changes", r, ReasonShapeUnrecognized)
			}
			if r.Detail == "" {
				t.Error("want a structural detail")
			}
		})
	}
	t.Run("a deleted Go surface file is unevaluable, not an all-narrowing", func(t *testing.T) {
		r := Detect(surface("mcp-tool-scopes"), side(mcpScopes()), absentSide)
		if r.Unevaluable != ReasonShapeUnrecognized || len(r.Narrowed) != 0 {
			t.Errorf("Detect = %+v; want shape_unrecognized", r)
		}
	})
}

func TestDetect_ManifestPartsAreIndependent(t *testing.T) {
	var perms, events Surface
	for _, s := range DefaultSurfaces() {
		switch s.ID {
		case "github-app-permissions-go":
			perms = s
		case "github-app-events-go":
			events = s
		}
	}
	base := manifestGo(`"contents": "read",`, `"push",`)
	head := manifestGo(`"contents": "read",`, "\"push\",\n\"workflow_run\",")
	// COUNTERFACTUAL: make GoExtraction.withPrefix return x unfiltered (body
	// mutation). The fixture's only change is an added EVENT, so the
	// permissions surface then reports the events widening too — RED.
	if r := Detect(perms, side(base), side(head)); len(r.Widened)+len(r.Narrowed) != 0 || r.Unevaluable != "" {
		t.Errorf("permissions surface = %+v; want nothing (the change is an event)", r)
	}
	if r := Detect(events, side(base), side(head)); len(r.Widened) != 1 || r.Widened[0].Key != "default_events.workflow_run" {
		t.Errorf("events surface = %+v; want one widening default_events.workflow_run", r)
	}
	// An unresolved events value fails only the events part.
	bad := strings.Replace(base, "[]string{\n\"push\",\n\t\t}", "events()", 1)
	if bad == base {
		t.Fatal("fixture substitution did not apply")
	}
	if r := Detect(perms, side(base), side(bad)); r.Unevaluable != "" {
		t.Errorf("permissions surface = %+v; want evaluable", r)
	}
	if r := Detect(events, side(base), side(bad)); r.Unevaluable != ReasonShapeUnrecognized {
		t.Errorf("events surface = %+v; want shape_unrecognized", r)
	}
	// The JSON template splits the same way.
	jb := `{"default_permissions": {"contents": "read"}, "default_events": ["push"]}`
	jh := `{"default_permissions": {"contents": "write"}, "default_events": ["push"]}`
	for _, s := range DefaultSurfaces() {
		r := Detect(s, side(jb), side(jh))
		switch s.ID {
		case "github-app-permissions-template":
			if len(r.Widened) != 1 {
				t.Errorf("%s = %+v; want one widening", s.ID, r)
			}
		case "github-app-events-template":
			if len(r.Widened)+len(r.Narrowed) != 0 {
				t.Errorf("%s = %+v; want nothing", s.ID, r)
			}
		}
	}
}

// TestDetect_ManifestPackageVarWriteFailsBothParts: manifest.go carries TWO
// surfaces (App permissions and App events), and withPrefix filters
// Unresolved by prefix, so a package-var write the sweep finds must be
// recorded under BOTH prefixes or one surface would read it as clean. The
// synthetic manifest plus a package var written by init() is
// shape_unrecognized on both surfaces; the same source without the init()
// is evaluable on both (the control: the var alone is no write).
//
// COUNTERFACTUAL: change the ExtractGoManifest call site to pass only
// ManifestPermissionPrefix — the github-app-events-go arm goes RED while the
// permissions arm stays green, so each prefix is pinned independently.
func TestDetect_ManifestPackageVarWriteFailsBothParts(t *testing.T) {
	base := manifestGo(`"contents": "read",`, `"push",`) + "\nvar appName = \"Fishhawk\"\n"
	head := base + "func init() { appName = \"Fishhawk (patched)\" }\n"
	for _, id := range []string{"github-app-permissions-go", "github-app-events-go"} {
		t.Run(id, func(t *testing.T) {
			s := surfaceByID(t, id)
			if r := Detect(s, side(base), side(base)); r.Unevaluable != "" || len(r.Widened)+len(r.Narrowed) != 0 {
				t.Fatalf("control without init(): Detect = %+v; want evaluable with no change", r)
			}
			r := Detect(s, side(base), side(head))
			if r.Unevaluable != ReasonShapeUnrecognized || len(r.Widened)+len(r.Narrowed) != 0 ||
				!strings.Contains(r.Detail, "write to package var appName outside its declaration") {
				t.Fatalf("Detect = %+v; want shape_unrecognized naming the appName write", r)
			}
		})
	}
}

func TestDetect_ParseErrorAndAbsentSides(t *testing.T) {
	gha := MatchSurfaces(DefaultSurfaces(), ".github/workflows/ci.yml")[0]
	if r := Detect(gha, side(actionsDoc("      contents: read")), side("jobs: [")); r.Unevaluable != ReasonParseError || len(r.Narrowed) != 0 {
		t.Errorf("unparseable head = %+v; want parse_error and no changes", r)
	}
	if r := Detect(gha, side("jobs: ["), side(actionsDoc("      contents: read"))); r.Unevaluable != ReasonParseError {
		t.Errorf("unparseable base = %+v; want parse_error", r)
	}
	mcp := MatchSurfaces(DefaultSurfaces(), "backend/internal/server/mcpscopes.go")[0]
	if r := Detect(mcp, side(mcpScopes()), side("package server\nvar {")); r.Unevaluable != ReasonParseError {
		t.Errorf("unparseable Go head = %+v; want parse_error", r)
	}
	// An added file's grants are widenings; a both-absent file is nothing.
	if r := Detect(gha, absentSide, side(actionsDoc("      contents: write"))); len(r.Widened) != 1 {
		t.Errorf("added workflow = %+v; want one widening", r)
	}
	if r := Detect(gha, absentSide, absentSide); !reflect.DeepEqual(r, Result{}) {
		t.Errorf("both absent = %+v; want zero Result", r)
	}
	// A deleted forbidden_paths spec drops its restrictions: widenings.
	fp := MatchSurfaces(DefaultSurfaces(), ".fishhawk/workflows.yaml")[0]
	if r := Detect(fp, side(specDoc()), absentSide); len(r.Widened) != 2 {
		t.Errorf("deleted spec = %+v; want two forbidden_paths widenings", r)
	}
	if r := Detect(Surface{ID: "x", Kind: "nope"}, side("a"), side("b")); r.Unevaluable != ReasonParseError {
		t.Errorf("unknown kind = %+v; want parse_error", r)
	}
}

const repoSurfaces = `version: 1
surfaces:
  - id: infra-app
    kind: github_app_permissions_json
    paths: ["infra/app.json"]
    severity: medium
    description: infra app manifest
%MORE%`

// TestDetect_SurfaceDeclarationsRemovalIsWidening: each declared id and path
// is a Restriction, so removing one widens. COUNTERFACTUAL: flip the
// `surfaces[<id>]` entry's Polarity to Grant in ExtractSurfaceDeclarations
// (body mutation) — the fixture removes the whole declaration, and a vanished
// Grant is a narrowing, so the widening assertion on surfaces[infra-app] goes
// RED.
func TestDetect_SurfaceDeclarationsRemovalIsWidening(t *testing.T) {
	s := MatchSurfaces(DefaultSurfaces(), RepoSurfacesPath)[0]
	base := strings.Replace(repoSurfaces, "%MORE%", "", 1)
	r := Detect(s, side(base), side("version: 1\nsurfaces: []\n"))
	got := map[string]Direction{}
	for _, c := range r.Widened {
		got[c.Key] = c.Direction
	}
	for _, k := range []string{"surfaces[infra-app]", "surfaces[infra-app].paths[infra/app.json]",
		"surfaces[infra-app].kind[github_app_permissions_json]", "surfaces[infra-app].severity"} {
		if got[k] != Widened {
			t.Errorf("%s not widened (widened %+v)", k, r.Widened)
		}
	}
	if len(r.Narrowed) != 0 {
		t.Errorf("narrowed %+v; want none", r.Narrowed)
	}
	// Lowering a severity is a widening; raising it a narrowing.
	r = Detect(s, side(base), side(strings.Replace(base, "severity: medium", "severity: low", 1)))
	if len(r.Widened) != 1 || r.Widened[0].Key != "surfaces[infra-app].severity" {
		t.Errorf("severity lowered = %+v; want one widening", r)
	}
	// An entry made invalid at head vanishes: a widening.
	r = Detect(s, side(base), side(strings.Replace(base, "github_app_permissions_json", "bogus_kind", 1)))
	if len(r.Widened) == 0 {
		t.Errorf("entry made invalid = %+v; want widenings", r)
	}
}

func TestParseRepoSurfaces(t *testing.T) {
	t.Run("accepted entry", func(t *testing.T) {
		acc, rej, err := ParseRepoSurfaces([]byte(strings.Replace(repoSurfaces, "%MORE%", "", 1)))
		if err != nil || len(rej) != 0 {
			t.Fatalf("err %v rejected %v", err, rej)
		}
		want := []Surface{{ID: "infra-app", Kind: KindAppPermissionsJSON, Paths: []string{"infra/app.json"},
			Severity: SeverityMedium, Description: "infra app manifest"}}
		if !reflect.DeepEqual(acc, want) {
			t.Errorf("accepted %+v, want %+v", acc, want)
		}
	})
	// COUNTERFACTUAL: delete the product-id-collision case from
	// ParseRepoSurfaces (body mutation). The fixture's colliding entry is
	// otherwise valid (known kind, paths, severity), so it is then ACCEPTED
	// and the product_id_collision row goes RED.
	t.Run("each rejection is named, never fatal", func(t *testing.T) {
		more := `  - id: Bad_ID
    kind: gha_workflow_permissions
    paths: ["x.yml"]
  - id: infra-app
    kind: gha_workflow_permissions
    paths: ["x.yml"]
  - id: mcp-tool-scopes
    kind: gha_workflow_permissions
    paths: ["x.yml"]
  - id: unknown-kind
    kind: nope
    paths: ["x.yml"]
  - id: empty-paths
    kind: gha_workflow_permissions
    paths: []
  - id: bad-path
    kind: gha_workflow_permissions
    paths: ["a/[b"]
  - id: bad-severity
    kind: gha_workflow_permissions
    paths: ["x.yml"]
    severity: critical
  - id: default-severity
    kind: gha_workflow_permissions
    paths: ["x.yml"]
`
		acc, rej, err := ParseRepoSurfaces([]byte(strings.Replace(repoSurfaces, "%MORE%", more, 1)))
		if err != nil {
			t.Fatal(err)
		}
		want := []Rejection{
			{Index: 1, ID: "Bad_ID", Reason: RejectBadID},
			{Index: 2, ID: "infra-app", Reason: RejectDuplicateID},
			{Index: 3, ID: "mcp-tool-scopes", Reason: RejectProductIDCollision},
			{Index: 4, ID: "unknown-kind", Reason: RejectUnknownKind},
			{Index: 5, ID: "empty-paths", Reason: RejectEmptyPaths},
			{Index: 6, ID: "bad-path", Reason: RejectBadPath},
			{Index: 7, ID: "bad-severity", Reason: RejectBadSeverity},
		}
		if !reflect.DeepEqual(rej, want) {
			t.Errorf("rejected %+v, want %+v", rej, want)
		}
		if len(acc) != 2 || acc[1].ID != "default-severity" || acc[1].Severity != SeverityHigh {
			t.Errorf("accepted %+v; want infra-app and default-severity (high)", acc)
		}
	})
	for name, doc := range map[string]string{
		"unknown top-level key": "version: 1\nsurfaces: []\nextra: true\n",
		"unknown entry key":     "version: 1\nsurfaces:\n  - id: a\n    kind: gha_workflow_permissions\n    paths: [x]\n    color: red\n",
		"bad version":           "version: 2\nsurfaces: []\n",
		"missing version":       "surfaces: []\n",
		"unparseable":           "version: [\n",
	} {
		t.Run(name+" is an error", func(t *testing.T) {
			if _, _, err := ParseRepoSurfaces([]byte(doc)); err == nil {
				t.Error("want an error")
			}
		})
	}
	t.Run("empty and comment-only are no surfaces", func(t *testing.T) {
		for _, doc := range []string{"", "  \n", "# nothing here\n"} {
			if acc, rej, err := ParseRepoSurfaces([]byte(doc)); err != nil || acc != nil || rej != nil {
				t.Errorf("%q = %v %v %v; want nothing", doc, acc, rej, err)
			}
		}
	})
}

func TestMergeSurfaces(t *testing.T) {
	product := DefaultSurfaces()
	ext := []Surface{{ID: "infra-app", Kind: KindAppPermissionsJSON, Paths: []string{"infra/app.json"}, Severity: SeverityHigh},
		{ID: "mcp-tool-scopes", Kind: KindActionsWorkflow, Paths: []string{"x"}, Severity: SeverityLow}}
	got := MergeSurfaces(product, ext)
	if len(got) != len(product)+1 || got[len(got)-1].ID != "infra-app" {
		t.Errorf("merged %d surfaces (last %q); want product + infra-app only", len(got), got[len(got)-1].ID)
	}
	if m := MatchSurfaces(got, "infra/app.json"); len(m) != 1 || m[0].ID != "infra-app" {
		t.Errorf("infra/app.json matches %+v; want the extension surface", m)
	}
}

// TestCheckKey_Injective: COUNTERFACTUAL — make escapeKeyField return s
// unchanged (body mutation). The first pair below differs only in where a
// '|' sits (inside the key vs between key and after), so the unescaped keys
// collide and the test goes RED.
func TestCheckKey_Injective(t *testing.T) {
	type in struct{ surface, path, key, after string }
	long := strings.Repeat("x", 1024)
	inputs := []in{
		{"s", "p", "k|x", "a"},
		{"s", "p", "k", "x|a"},
		{"s", "p|k", "x", "a"},
		{"s", "p", "k%7Cx", "a"},
		{"s", "p", "k%x", "a"},
		{"s", "p", "k%25x", "a"},
		{"s|p", "k", "x", "a"},
		// Control runes (approval condition 3): a raw newline and its
		// already-escaped spelling must not collide.
		{"s", "p", "k\nx", "a"},
		{"s", "p", "k%0Ax", "a"},
		// A file-derived dotted segment carrying '.' (keySegment-escaped by
		// the extractors) cannot forge a segment boundary: job "a.b" + scope
		// "c" vs job "a" + scope "b.c".
		{"s", "p", "jobs." + keySegment("a.b") + ".c", "a"},
		{"s", "p", "jobs.a." + keySegment("b.c"), "a"},
		// Overlong fields are digested: two 1 KB fields differing in the last
		// byte stay distinct, and a raw field spelled like a digest cannot
		// collide with one.
		{"s", "p", long + "a", "a"},
		{"s", "p", long + "b", "a"},
		{"s", "p", "%Habc", "a"},
		{"s", "p", keyFieldDigestPrefix + strings.Repeat("0", 64), "a"},
	}
	seen := map[string]in{}
	for _, i := range inputs {
		k := CheckKey(i.surface, i.path, i.key, i.after)
		if prev, dup := seen[k]; dup {
			t.Errorf("CheckKey collision %q for %+v and %+v", k, prev, i)
		}
		seen[k] = i
		if strings.Count(k, "|") != 4 || !strings.HasPrefix(k, CheckName+"|") {
			t.Errorf("CheckKey %q does not have exactly 5 fields", k)
		}
	}
	u := UnevaluableKey("s", "p", "abc123")
	if u != "permission_drift|s|p|unevaluable@abc123" || seen[u] != (in{}) {
		t.Errorf("UnevaluableKey = %q", u)
	}
	if UnevaluableKey("s|p", "x", "h") == UnevaluableKey("s", "p|x", "h") {
		t.Error("UnevaluableKey is not injective")
	}
	// COUNTERFACTUAL (drop the head from UnevaluableKey's body): two heads then
	// share one key, so a waived row at head A would suppress head B → RED.
	if UnevaluableKey("s", "p", "aaaa") == UnevaluableKey("s", "p", "bbbb") {
		t.Error("UnevaluableKey does not distinguish heads")
	}
	// A four-field unevaluable key never equals a five-field CheckKey, even
	// when the key field is spelled like the unevaluable marker.
	if UnevaluableKey("s", "p", "h") == CheckKey("s", "p", "unevaluable@h", "") {
		t.Error("UnevaluableKey collides with a CheckKey")
	}
}

// TestCheckKey_ControlFreeAndBounded (approval condition 3, item 10).
//
// COUNTERFACTUAL (escapeKeyField's control-rune clause mutated out): the
// newline, the bidi override and U+2028 survive into the key → RED.
// COUNTERFACTUAL (the digest branch mutated out): a 10 KB after value yields
// a key far over the bound → RED.
func TestCheckKey_ControlFreeAndBounded(t *testing.T) {
	for _, field := range []string{"a\nb", "a\u202eb", "a\u2028b", "a\u200bb", "a\x00b", "a\xffb"} {
		k := CheckKey("s", "p", field, field)
		for _, r := range k {
			if isDisplayHostile(r) || r == utf8.RuneError {
				t.Errorf("CheckKey(%q) = %q carries a control/format rune %U", field, k, r)
			}
		}
		if u := UnevaluableKey("s", field, field); strings.ContainsFunc(u, isDisplayHostile) {
			t.Errorf("UnevaluableKey(%q) = %q carries a control/format rune", field, u)
		}
	}
	k := CheckKey("s", "p", "k", strings.Repeat("v", 10*1024))
	if len(k) > len(CheckName)+4*(maxKeyFieldBytes+1) {
		t.Errorf("CheckKey length = %d, want bounded", len(k))
	}
	if last := k[strings.LastIndex(k, "|")+1:]; !strings.HasPrefix(last, keyFieldDigestPrefix) || len(last) != len(keyFieldDigestPrefix)+64 {
		t.Errorf("CheckKey overlong field not digested: %q", last)
	}
}

// TestDisplay (approval condition 4, item 10).
//
// COUNTERFACTUAL (Display's hostile-rune replacement mutated out): the
// newline, bidi override, zero-width space and U+2028/U+2029 survive → RED.
// COUNTERFACTUAL (the length cap mutated out): a 10 KB value renders whole →
// RED.
func TestDisplay(t *testing.T) {
	for _, in := range []string{"a\nb", "a\rb", "a\u202eb", "a\u200bb", "a\ufeffb", "a\u2028b", "a\u2029b", "a\xffb"} {
		out := Display(in)
		if strings.ContainsFunc(out, isDisplayHostile) || !utf8.ValidString(out) {
			t.Errorf("Display(%q) = %q, want no control/format rune and valid UTF-8", in, out)
		}
		if !strings.HasPrefix(out, "a") || !strings.HasSuffix(out, "b") {
			t.Errorf("Display(%q) = %q, want the printable runes kept", in, out)
		}
	}
	if got := Display("jobs.build.contents"); got != "jobs.build.contents" {
		t.Errorf("Display(plain) = %q, want unchanged", got)
	}
	out := Display(strings.Repeat("é", 10*1024))
	if len(out) > maxDisplayBytes+len(displayTruncationMarker) || !strings.HasSuffix(out, displayTruncationMarker) || !utf8.ValidString(out) {
		t.Errorf("Display(10 KB) = %d bytes, want <= %d ending in the marker on a rune boundary", len(out), maxDisplayBytes+len(displayTruncationMarker))
	}
}

func TestNote(t *testing.T) {
	s := DefaultSurfaces()[0]
	c := Change{Key: "jobs.build.contents", Before: "read", After: "write", Direction: Widened}
	n := Note(s, ".github/workflows/ci.yml", c)
	for _, want := range []string{s.ID, ".github/workflows/ci.yml", c.Key, "from read to write",
		"deterministic permission-drift check, not by a model reviewer", "Only a human can waive it, with a reason"} {
		if !strings.Contains(n, want) {
			t.Errorf("Note missing %q: %s", want, n)
		}
	}
	for reason := range unevaluableWhy {
		u := NoteUnevaluable(s, "p.yml", reason)
		if !strings.Contains(u, unevaluableWhy[reason]) || !strings.Contains(u, "fails closed") ||
			!strings.Contains(u, "Only a human can waive it") {
			t.Errorf("NoteUnevaluable(%s) = %s", reason, u)
		}
	}
	// #3939: the server's run-base resolution failure has its own class and
	// says the extension was not read (iterating the map above would stay
	// green with the entry deleted).
	if u := NoteUnevaluable(s, RepoSurfacesPath, ReasonSurfacesRefUnresolved); ReasonSurfacesRefUnresolved != "surfaces_ref_unresolved" ||
		!strings.Contains(u, "recorded base commit could not be resolved") || !strings.Contains(u, "surface extension was not read") {
		t.Errorf("NoteUnevaluable(%s) = %s; want the run-base explanation", ReasonSurfacesRefUnresolved, u)
	}
	if u := NoteUnevaluable(s, "", "other_reason"); !strings.Contains(u, "other_reason") || strings.Contains(u, " in ") {
		t.Errorf("NoteUnevaluable(unknown reason, no path) = %s", u)
	}
	// Injection (item 10, approval condition 4). COUNTERFACTUAL (the Display
	// calls in Note mutated out): the key's newline + markdown heading, the
	// bidi override and the U+2028 survive into the note, and the 10 KB after
	// value makes it unbounded → RED.
	evil := Change{Key: "jobs.build.contents\n## injected\u202e\u200b", Before: "read\u2028x", After: strings.Repeat("w", 10*1024), Direction: Widened}
	n = Note(s, "ci.yml\u2029", evil)
	if strings.ContainsFunc(n, isDisplayHostile) {
		t.Errorf("Note carries a control/format rune: %q", n)
	}
	if len(n) > 2000 {
		t.Errorf("Note length = %d, want bounded", len(n))
	}
	if u := NoteUnevaluable(s, "p\n## injected\u202e", ReasonFetchFailed); strings.ContainsFunc(u, isDisplayHostile) {
		t.Errorf("NoteUnevaluable carries a control/format rune: %q", u)
	}
}
