package reviewsandbox

// Hermetic tests for the two per-adapter read bounds (#2522).
//
// EVERY test here drives an INJECTED HostPaths whose HomeDir/CodexHome/TempDir
// are t.TempDir()s and whose Canonical is a table lookup. Nothing in this file
// reads, writes, or litters the operator's real ~/.codex, and nothing depends on
// the developer's real home layout — a plain `go test` must never touch a live
// credential.
//
// Scope note for the exact-file-set assertion below: it pins only what the
// BUILDER writes. It cannot observe codex-cli writing sessions/, history.jsonl
// or log/ into CODEX_HOME at runtime — that observation lives in the opt-in live
// harness (live_confinement_test.go), which inspects the synthesized home before
// cleanup on a real run.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureHostPaths builds an injectable HostPaths over temp dirs. canon maps a
// raw path to its canonical form; anything absent from the map is returned
// unchanged (an identity canonicalizer for paths the test does not care about).
func fixtureHostPaths(t *testing.T, canon map[string]string, missing ...string) HostPaths {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir codex home: %v", err)
	}
	absent := make(map[string]struct{}, len(missing))
	for _, m := range missing {
		absent[m] = struct{}{}
	}
	return HostPaths{
		HomeDir:   home,
		CodexHome: codexHome,
		TempDir:   t.TempDir(),
		Canonical: func(p string) (string, error) {
			if _, gone := absent[p]; gone {
				return "", &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
			}
			if c, ok := canon[p]; ok {
				return c, nil
			}
			return p, nil
		},
	}
}

// darwinFixtureRoots / linuxFixtureRoots mirror the shipped platform tables with
// every path present, so the deterministic rule-count assertion is host-independent.
func darwinFixtureRoots(hp HostPaths) []string {
	return platformDenyRoots(hp, "darwin")
}

// ---------------------------------------------------------------------------
// Failure mode 1 — errDenyRootUnresolvable (fail-closed arm).
// ---------------------------------------------------------------------------

func TestClaudeDenyRules_UnresolvableApplicableRootFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	export := t.TempDir()
	hp.Canonical = func(p string) (string, error) {
		if p == "/etc" {
			return "", errors.New("permission denied")
		}
		return p, nil
	}

	rules, err := claudeDenyRules(hp, export, []string{hp.HomeDir, "/etc"})
	if !errors.Is(err, errDenyRootUnresolvable) {
		t.Fatalf("err = %v, want errDenyRootUnresolvable", err)
	}
	if len(rules) != 0 {
		t.Errorf("rules = %v, want none on a fail-closed build", rules)
	}
}

func TestClaudeDenyRules_EmptyHomeFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	hp.HomeDir = ""
	if _, err := ClaudeDenyRules(hp, t.TempDir(), "linux"); !errors.Is(err, errDenyRootUnresolvable) {
		t.Fatalf("err = %v, want errDenyRootUnresolvable for an empty HOME", err)
	}
}

// ---------------------------------------------------------------------------
// Failure mode 2 — absent/inapplicable roots skip SILENTLY, rules still produced.
//
// Asserted as a PAIR with mode 1 so neither arm can be satisfied by making the
// other vacuous: an implementation that failed closed on everything would fail
// this test, and one that skipped everything would fail mode 1.
// ---------------------------------------------------------------------------

func TestClaudeDenyRules_AbsentRootSkippedRulesStillProduced(t *testing.T) {
	// /root does not exist on darwin; /private/etc does not exist on linux.
	hp := fixtureHostPaths(t, nil, "/root")
	export := t.TempDir()

	rules, err := claudeDenyRules(hp, export, []string{hp.HomeDir, "/etc", "/root"})
	if err != nil {
		t.Fatalf("an ABSENT root must be skipped, not fail: %v", err)
	}
	for _, r := range rules {
		if strings.Contains(r, "//root") {
			t.Errorf("absent root emitted a rule: %q", r)
		}
	}
	if !slices.Contains(rules, "Read(//"+strings.TrimPrefix(hp.HomeDir, "/")+"/**)") {
		t.Errorf("rules for the applicable roots were not produced: %v", rules)
	}
	if !slices.Contains(rules, "Read(//etc/**)") {
		t.Errorf("rules for /etc were not produced: %v", rules)
	}
}

func TestPlatformDenyRoots_TableIsPlatformSelected(t *testing.T) {
	hp := HostPaths{HomeDir: "/home/op"}
	darwin := platformDenyRoots(hp, "darwin")
	linux := platformDenyRoots(hp, "linux")

	wantDarwin := []string{"/home/op", "/etc", "/private/etc", "/var/root", "/private/var/root"}
	wantLinux := []string{"/home/op", "/etc", "/root"}
	if !slices.Equal(darwin, wantDarwin) {
		t.Errorf("darwin roots = %v, want %v", darwin, wantDarwin)
	}
	if !slices.Equal(linux, wantLinux) {
		t.Errorf("linux roots = %v, want %v", linux, wantLinux)
	}
	if slices.Contains(darwin, "/root") {
		t.Error("/root is absent on darwin and must not be in its table")
	}
	if slices.Contains(linux, "/private/etc") || slices.Contains(linux, "/private/var/root") {
		t.Error("the /private forms are darwin-only and must not be in the linux table")
	}
}

// ---------------------------------------------------------------------------
// Failure mode 3 — errDenyRuleMetacharacter, on EVERY emitted spelling.
// ---------------------------------------------------------------------------

func TestClaudeDenyRules_MetacharacterInCanonicalFailsClosed(t *testing.T) {
	for _, bad := range []string{"/opt/lib (x)", "/opt/li*b", "/opt/a,b", "/opt/a b"} {
		hp := fixtureHostPaths(t, map[string]string{"/opt/raw": bad})
		if _, err := claudeDenyRules(hp, t.TempDir(), []string{"/opt/raw"}); !errors.Is(err, errDenyRuleMetacharacter) {
			t.Errorf("canonical %q: err = %v, want errDenyRuleMetacharacter", bad, err)
		}
	}
}

// TestClaudeDenyRules_MetacharacterInRawSpellingFailsClosed is the condition-3
// case: the RAW spelling carries a metacharacter while its CANONICAL form is
// perfectly clean. Both spellings are emitted into --disallowed-tools, so a
// guard that checked only the canonical one would ship the misparsing rule.
func TestClaudeDenyRules_MetacharacterInRawSpellingFailsClosed(t *testing.T) {
	rawHome := "/Users/op (work)"
	hp := fixtureHostPaths(t, map[string]string{rawHome: "/Users/op-work"})
	hp.HomeDir = rawHome

	_, err := claudeDenyRules(hp, t.TempDir(), []string{hp.HomeDir})
	if !errors.Is(err, errDenyRuleMetacharacter) {
		t.Fatalf("err = %v, want errDenyRuleMetacharacter for a raw spelling with a clean canonical form", err)
	}
	if !strings.Contains(err.Error(), rawHome) {
		t.Errorf("error must name the RAW spelling that offended: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Failure mode 4 — errDenyOverlapsExport, and its component-wise sibling case.
// ---------------------------------------------------------------------------

func TestClaudeDenyRules_RootCoveringExportFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	export := filepath.Join(hp.HomeDir, "export")
	if _, err := claudeDenyRules(hp, export, []string{hp.HomeDir}); !errors.Is(err, errDenyOverlapsExport) {
		t.Fatalf("err = %v, want errDenyOverlapsExport", err)
	}
}

// TestPathCovers_ComponentWiseNotStringPrefix pins the sibling case a
// strings.HasPrefix implementation gets WRONG: /var/root does not cover
// /var/rootless, and denying it there would be a false positive that fails an
// otherwise fine review.
func TestPathCovers_ComponentWiseNotStringPrefix(t *testing.T) {
	if pathCovers("/var/root", "/var/rootless") {
		t.Error("/var/root must NOT cover /var/rootless (component-wise, not string prefix)")
	}
	if !pathCovers("/var/root", "/var/root/x/y") {
		t.Error("/var/root must cover a descendant")
	}
	if !pathCovers("/var/root", "/var/root") {
		t.Error("a path covers itself")
	}
	if pathCovers("/var/root/x", "/var/root") {
		t.Error("a descendant must not cover its ancestor")
	}
}

func TestClaudeDenyRules_SiblingExportIsNotOverlap(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	hp.HomeDir = "/var/root"
	if _, err := claudeDenyRules(hp, "/var/rootless/export", []string{hp.HomeDir}); err != nil {
		t.Fatalf("a SIBLING export must not trip the overlap guard: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Both rule FORMS, and the deterministic counts.
// ---------------------------------------------------------------------------

func TestClaudeDenyRules_EmitsBareAndRecursiveFormsForEveryTool(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	hp.HomeDir = "/home/op"
	rules, err := claudeDenyRules(hp, t.TempDir(), []string{hp.HomeDir})
	if err != nil {
		t.Fatalf("claudeDenyRules: %v", err)
	}
	for _, want := range []string{
		"Read(//home/op)", "Read(//home/op/**)",
		"Grep(//home/op)", "Grep(//home/op/**)",
		"Glob(//home/op)", "Glob(//home/op/**)",
	} {
		if !slices.Contains(rules, want) {
			t.Errorf("missing rule %q in %v", want, rules)
		}
	}
}

// TestClaudeDenyRules_DeterministicRuleCount pins the exact rule counts so a
// future root addition is a DELIBERATE edit rather than silent growth back
// toward the ~666,000-rule / ~40 MB argv the abandoned ancestor-sibling
// enumeration would have produced against a ~1 MiB ARG_MAX.
func TestClaudeDenyRules_DeterministicRuleCount(t *testing.T) {
	// darwin: 5 candidate roots, of which /etc and /var/root each contribute a
	// SECOND spelling (their canonical /private form is already a separate root,
	// so the union dedupes to 5 distinct spellings) → 5 x 6 = 30.
	hp := fixtureHostPaths(t, map[string]string{
		"/etc":      "/private/etc",
		"/var/root": "/private/var/root",
	})
	hp.HomeDir = "/Users/op"
	rules, err := claudeDenyRules(hp, t.TempDir(), darwinFixtureRoots(hp))
	if err != nil {
		t.Fatalf("darwin: %v", err)
	}
	if len(rules) != 30 {
		t.Errorf("darwin rule count = %d, want exactly 30:\n%v", len(rules), rules)
	}
	// Both the raw AND the canonical spelling must be present: a deny rule
	// matches the path as the reviewer ASKS for it, so a rule naming only
	// /private/etc does not cover a read of /etc/passwd.
	for _, want := range []string{"Read(//etc/**)", "Read(//private/etc/**)"} {
		if !slices.Contains(rules, want) {
			t.Errorf("darwin rules missing %q", want)
		}
	}

	linuxHP := fixtureHostPaths(t, nil)
	linuxHP.HomeDir = "/home/op"
	linuxRules, err := claudeDenyRules(linuxHP, t.TempDir(), platformDenyRoots(linuxHP, "linux"))
	if err != nil {
		t.Fatalf("linux: %v", err)
	}
	if len(linuxRules) != 18 {
		t.Errorf("linux rule count = %d, want exactly 18:\n%v", len(linuxRules), linuxRules)
	}
}

// ---------------------------------------------------------------------------
// NON-VACUITY / HONESTY controls. These are what stop the change from being
// self-congratulatory.
// ---------------------------------------------------------------------------

// TestClaudeDenyRules_InTreeReadStillPermitted: a path INSIDE the export is
// matched by NO deny rule. Without this, a rule set that denied everything would
// pass every assertion above while blinding the reviewer entirely.
func TestClaudeDenyRules_InTreeReadStillPermitted(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	export := t.TempDir()
	rules, err := claudeDenyRules(hp, export, platformDenyRoots(hp, "linux"))
	if err != nil {
		t.Fatalf("claudeDenyRules: %v", err)
	}
	inTree := filepath.Join(export, "pkg", "a.go")
	if root, matched := firstMatchingRoot(rules, inTree); matched {
		t.Errorf("in-tree path %q is denied by a rule naming %q", inTree, root)
	}
}

// TestClaudeDenyRules_OutsideDeniedRootsStillPermitted is the machine-asserted
// ADMISSION that claude gets a BLOCKLIST, not confinement: an arbitrary path
// outside the named roots stays readable. It exists so the docs cannot quietly
// drift into claiming a bound the mechanism does not provide.
func TestClaudeDenyRules_OutsideDeniedRootsStillPermitted(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	rules, err := claudeDenyRules(hp, t.TempDir(), platformDenyRoots(hp, "linux"))
	if err != nil {
		t.Fatalf("claudeDenyRules: %v", err)
	}
	for _, outside := range []string{
		filepath.Join(hp.TempDir, "sentinel.txt"),
		"/opt/secrets/token",
		"/srv/data/creds.json",
	} {
		if root, matched := firstMatchingRoot(rules, outside); matched {
			t.Errorf("this mechanism is a blocklist: %q was unexpectedly denied by %q "+
				"— if the root set genuinely grew, update the honesty docs too", outside, root)
		}
	}
}

// firstMatchingRoot reports whether any emitted rule's path component-wise
// covers candidate. It decodes the shipped rule text (`Tool(//path)` /
// `Tool(//path/**)`) rather than re-deriving the roots, so a rule that names the
// wrong path is caught.
func firstMatchingRoot(rules []string, candidate string) (string, bool) {
	for _, r := range rules {
		open := strings.IndexByte(r, '(')
		if open < 0 || !strings.HasSuffix(r, ")") {
			continue
		}
		p := strings.TrimSuffix(r[open+1:], ")")
		p = strings.TrimPrefix(p, "//")
		p = strings.TrimSuffix(p, "/**")
		if pathCovers("/"+p, candidate) {
			return p, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Failure mode 5 — errConfinedHomeOutsideDeniedRoot, plus its positive pair.
// ---------------------------------------------------------------------------

func TestCodexConfinedHome_RelocatedCodexHomeRefused(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	// The operator relocated CODEX_HOME under the temp root — the one region the
	// claude blocklist deliberately does not deny. Placing a copy of auth.json
	// there would move a live credential OUT of every denied root.
	hp.CodexHome = filepath.Join(hp.TempDir, "codex")

	_, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if !errors.Is(err, errConfinedHomeOutsideDeniedRoot) {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatalf("err = %v, want errConfinedHomeOutsideDeniedRoot", err)
	}
	if cleanup != nil {
		t.Error("a refused build must return no cleanup func")
	}
	// And nothing was created there.
	if entries, rerr := os.ReadDir(hp.CodexHome); rerr == nil && len(entries) != 0 {
		t.Errorf("refused build left %d entries in the relocated CODEX_HOME", len(entries))
	}
}

func TestCodexConfinedHome_EmptyCodexHomeRefused(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	hp.CodexHome = ""
	if _, _, err := CodexConfinedHome(hp, t.TempDir(), "", "linux"); !errors.Is(err, errConfinedHomeOutsideDeniedRoot) {
		t.Fatalf("err = %v, want errConfinedHomeOutsideDeniedRoot for an empty CODEX_HOME", err)
	}
}

// TestCodexConfinedHome_PlacedInsideADeniedRoot is the POSITIVE pair: the
// placement is proven by running the SHIPPED claude rule generator over the SAME
// HostPaths and asserting it covers the synthesized home — not asserted in prose.
func TestCodexConfinedHome_PlacedInsideADeniedRoot(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	export := t.TempDir()
	home, cleanup, err := CodexConfinedHome(hp, export, "", "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	defer func() { _ = cleanup() }()

	if !strings.HasPrefix(home, hp.CodexHome) {
		t.Errorf("synthesized home %q is not inside the real CODEX_HOME %q", home, hp.CodexHome)
	}
	info, serr := os.Stat(home)
	if serr != nil {
		t.Fatalf("stat synthesized home: %v", serr)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("synthesized home perm = %04o, want 0700", perm)
	}

	rules, rerr := claudeDenyRules(hp, export, platformDenyRoots(hp, "linux"))
	if rerr != nil {
		t.Fatalf("claudeDenyRules: %v", rerr)
	}
	if _, matched := firstMatchingRoot(rules, filepath.Join(home, "auth.json")); !matched {
		t.Errorf("the copied credential at %q is NOT covered by any claude deny rule", home)
	}
	if _, matched := firstMatchingRoot(rules, filepath.Join(export, "a.go")); matched {
		t.Error("the export must not overlap the synthesized home's denied root")
	}
}

// ---------------------------------------------------------------------------
// CONDITION 1 — the credential must NOT be inside the reviewer-readable set.
// ---------------------------------------------------------------------------

// TestCodexConfinedHome_CredentialNotGrantedToToolLayer asserts that neither the
// synthesized home nor auth.json appears as a filesystem grant in either profile
// file, while the export (and the schema) DO. The grant set is what codex-cli
// enforces as an OS-level allowlist, so a home-wide grant would hand the copied
// credential straight to the reviewer's tool layer — the exact egress path this
// issue describes.
func TestCodexConfinedHome_CredentialNotGrantedToToolLayer(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	export := t.TempDir()
	schema := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schema, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	home, cleanup, err := CodexConfinedHome(hp, export, schema, "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	defer func() { _ = cleanup() }()

	for _, name := range []string{"config.toml", ConfinedProfileName + ".config.toml"} {
		body := readFile(t, filepath.Join(home, name))
		grants := filesystemGrants(body)
		for _, g := range grants {
			if pathCovers(g, filepath.Join(home, "auth.json")) {
				t.Errorf("%s grants %q, which covers the copied credential", name, g)
			}
		}
		if !slices.Contains(grants, export) {
			t.Errorf("%s does not grant the export %q: grants=%v", name, export, grants)
		}
		if !slices.Contains(grants, schema) {
			t.Errorf("%s does not grant the schema %q: grants=%v", name, schema, grants)
		}
		if !strings.Contains(body, `":minimal" = "read"`) {
			t.Errorf("%s omits \":minimal\" — every shell command then aborts with SIGABRT/134", name)
		}
	}
}

// filesystemGrants extracts the quoted path keys from the
// [permissions.confined.filesystem] table, skipping the ":minimal" pseudo-entry.
func filesystemGrants(body string) []string {
	var out []string
	inTable := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			inTable = t == "[permissions."+ConfinedProfileName+".filesystem]"
			continue
		}
		if !inTable || !strings.HasPrefix(t, `"`) {
			continue
		}
		key, _, ok := strings.Cut(strings.TrimPrefix(t, `"`), `"`)
		if !ok || key == ":minimal" {
			continue
		}
		out = append(out, key)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Failure mode 6 — errOperatorConfigDeclaresPermissions.
// ---------------------------------------------------------------------------

func TestCodexConfinedHome_OperatorConfigDeclaringPermissionsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"top-level default_permissions":  "model = \"gpt-5\"\ndefault_permissions = \"workspace\"\n",
		"a [permissions.confined] table": "model = \"gpt-5\"\n\n[permissions.confined.filesystem]\n\"/\" = \"read\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			hp := fixtureHostPaths(t, nil)
			if err := os.WriteFile(filepath.Join(hp.CodexHome, "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatalf("seed operator config: %v", err)
			}
			_, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
			if cleanup != nil {
				_ = cleanup()
			}
			if !errors.Is(err, errOperatorConfigDeclaresPermissions) {
				t.Fatalf("err = %v, want errOperatorConfigDeclaresPermissions", err)
			}
		})
	}
}

// TestCodexConfinedHome_OperatorConfigCopiedVerbatim: model_provider/base_url/
// model must survive into the synthesized base config, or a grounded review
// silently talks to a different endpoint than an ungrounded one.
func TestCodexConfinedHome_OperatorConfigCopiedVerbatim(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	operator := "model = \"gpt-5-codex\"\nmodel_provider = \"acme\"\n\n[model_providers.acme]\nbase_url = \"https://acme.example/v1\"\n"
	if err := os.WriteFile(filepath.Join(hp.CodexHome, "config.toml"), []byte(operator), 0o600); err != nil {
		t.Fatalf("seed operator config: %v", err)
	}
	home, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	defer func() { _ = cleanup() }()

	base := readFile(t, filepath.Join(home, "config.toml"))
	for _, want := range []string{`model_provider = "acme"`, `base_url = "https://acme.example/v1"`, `model = "gpt-5-codex"`} {
		if !strings.Contains(base, want) {
			t.Errorf("synthesized base config dropped %q:\n%s", want, base)
		}
	}
	// TOML ordering: every top-level key must precede the first table header.
	firstTable := strings.Index(base, "\n[")
	permKey := strings.Index(base, "default_permissions")
	if firstTable >= 0 && permKey > firstTable {
		t.Errorf("default_permissions lands after the first table header (invalid TOML):\n%s", base)
	}
}

// TestCodexConfinedHome_BuilderWritesExactlyThreeFiles pins what the BUILDER
// writes — deliberately NOT a claim about the home's contents during a real run.
// codex-cli writes sessions/, history.jsonl and log/ into CODEX_HOME at runtime,
// which this hermetic test structurally cannot observe; that observation lives in
// live_confinement_test.go.
func TestCodexConfinedHome_BuilderWritesExactlyThreeFiles(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	if err := os.WriteFile(filepath.Join(hp.CodexHome, "auth.json"), []byte(`{"t":"a"}`), 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	home, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	defer func() { _ = cleanup() }()

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("read synthesized home: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	want := []string{"auth.json", "config.toml", ConfinedProfileName + ".config.toml"}
	if !slices.Equal(names, want) {
		t.Errorf("builder wrote %v, want exactly %v", names, want)
	}
	if info, serr := os.Stat(filepath.Join(home, "auth.json")); serr == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("copied auth.json perm = %04o, want 0600", perm)
		}
	}
}

func TestCodexConfinedHome_MissingAuthIsNotFatal(t *testing.T) {
	hp := fixtureHostPaths(t, nil) // no auth.json seeded
	home, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if err != nil {
		t.Fatalf("a missing auth.json must not be fatal (OPENAI_API_KEY deployments have none): %v", err)
	}
	if werr := cleanup(); werr != nil {
		t.Errorf("cleanup warned with no credential to copy back: %v", werr)
	}
	if _, serr := os.Stat(home); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("cleanup must remove the synthesized home; stat err = %v", serr)
	}
}

// ---------------------------------------------------------------------------
// Failure mode 7 — the credential copy-back.
// ---------------------------------------------------------------------------

// seedConfined builds a confined home with a seeded credential and returns the
// source path, the synthesized home and its cleanup.
func seedConfined(t *testing.T, hp HostPaths, original string) (src, home string, cleanup func() error) {
	t.Helper()
	src = filepath.Join(hp.CodexHome, "auth.json")
	if err := os.WriteFile(src, []byte(original), 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	home, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	return src, home, cleanup
}

// TestCredentialCopyBack_CleanSourceIsWrittenBack is the POSITIVE pair for the
// skip branch below: without it, a copy-back that never worked at all would
// satisfy every skip assertion.
func TestCredentialCopyBack_CleanSourceIsWrittenBack(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)

	// The reviewer's codex refreshed the credential inside the confined home.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"token":"refreshed"}`), 0o600); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if werr := cleanup(); werr != nil {
		t.Fatalf("cleanup warned on an unchanged source: %v", werr)
	}
	// State read AFTER the call returns — the control's effect is COMMITTED
	// STATE, so asserting only an error identity would not see it.
	if got := readFile(t, src); got != `{"token":"refreshed"}` {
		t.Errorf("source = %q, want the refreshed credential", got)
	}
}

// TestCredentialCopyBack_IndependentlyChangedSourceIsSkipped: the operator's
// newest bytes always win.
func TestCredentialCopyBack_IndependentlyChangedSourceIsSkipped(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)

	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"token":"reviewer"}`), 0o600); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Another party rotated the operator's credential while the review ran.
	if err := os.WriteFile(src, []byte(`{"token":"operator-newer"}`), 0o600); err != nil {
		t.Fatalf("mutate source: %v", err)
	}

	werr := cleanup()
	// COMMITTED STATE first: this control's effect is what is left on disk, and a
	// version that fired and then wrote anyway would still return a byte-identical
	// warning. Read the state AFTER the call returns.
	if got := readFile(t, src); got != `{"token":"operator-newer"}` {
		t.Errorf("source = %q, want the operator's newer credential UNCHANGED", got)
	}
	if werr == nil {
		t.Fatal("cleanup must return a warning naming the skip")
	}
	if !strings.Contains(werr.Error(), "skipped credential copy-back") {
		t.Errorf("warning does not name the skip: %v", werr)
	}
}

// TestCredentialCopyBack_MutualExclusionDuringTheWindow is the condition-2 case:
// it mutates DURING the cleanup window (the hook fires inside the lock, after the
// source hash is re-verified and before the rename) and asserts a LOCK-RESPECTING
// peer is excluded there. That is the guarantee the protocol actually provides.
//
// It also records the honest RESIDUAL: an EXTERNAL writer that does not take this
// lock is NOT excluded. See TestCredentialCopyBack_ExternalWriterResidual.
func TestCredentialCopyBack_MutualExclusionDuringTheWindow(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src := filepath.Join(hp.CodexHome, "auth.json")

	var peerErr error
	peerRan := false
	hp.duringCredentialCopyBack = func() {
		peerRan = true
		peerErr = withCredentialLock(src+".fishhawk-lock", func() error {
			return os.WriteFile(src, []byte(`{"token":"peer"}`), 0o600)
		})
	}
	// Shorten the peer's wait: it is expected to fail, and the default is 1s.
	src2, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)
	if src2 != src {
		t.Fatalf("source path drift: %q vs %q", src2, src)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"token":"reviewer"}`), 0o600); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if werr := cleanup(); werr != nil {
		t.Fatalf("cleanup: %v", werr)
	}
	if !peerRan {
		t.Fatal("the during-window hook never fired — the test proves nothing")
	}
	if !errors.Is(peerErr, errCredentialLockUnavailable) {
		t.Errorf("a lock-respecting peer inside the window got err = %v, want errCredentialLockUnavailable", peerErr)
	}
	if got := readFile(t, src); got != `{"token":"reviewer"}` {
		t.Errorf("source = %q, want the reviewer's refreshed credential", got)
	}
}

// TestCredentialCopyBack_ExternalWriterResidual documents, as an executable
// assertion rather than prose, the limit of what this protocol guarantees: POSIX
// offers no compare-and-swap on a file, so a writer that does NOT take the lock
// (codex-cli refreshing the credential itself) can land between the re-verify and
// the rename and BE OVERWRITTEN. If this test ever goes red because the write
// survived, the protocol got stronger and the README's residual should be
// narrowed to match.
func TestCredentialCopyBack_ExternalWriterResidual(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src := filepath.Join(hp.CodexHome, "auth.json")
	hp.duringCredentialCopyBack = func() {
		// An external, non-lock-respecting writer.
		_ = os.WriteFile(src, []byte(`{"token":"external"}`), 0o600)
	}
	_, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"token":"reviewer"}`), 0o600); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if werr := cleanup(); werr != nil {
		t.Fatalf("cleanup: %v", werr)
	}
	// The outcome is DETERMINISTIC, so it is asserted rather than logged: the
	// hook fires inside the lock AFTER the re-verify and BEFORE the rename, so
	// the rename always lands on top of the external writer's bytes. A t.Logf
	// here would pass whether the write was clobbered or preserved, which is
	// exactly the vacuity that lets a README residual drift out of truth.
	if got := readFile(t, src); got != `{"token":"reviewer"}` {
		t.Errorf("external in-window write SURVIVED (source = %q, want %q) — the copy-back "+
			"protocol changed and the README's named residual now OVERSTATES the exposure; "+
			"narrow the residual text to match", got, `{"token":"reviewer"}`)
	}
}

// ---------------------------------------------------------------------------
// Failure mode 8 — canonicalization is load-bearing at every emitting site.
// ---------------------------------------------------------------------------

// TestCodexConfinedHome_GrantsTheCanonicalExport: with the injected canonicalizer
// rewriting a /var alias to its /private real path, the emitted grant carries the
// CANONICAL form. A raw grant under a canonical cwd BLINDS the reviewer silently
// rather than failing loudly, which is why this is asserted rather than trusted.
func TestCodexConfinedHome_GrantsTheCanonicalExport(t *testing.T) {
	rawExport := "/var/folders/zz/export"
	canonExport := "/private/var/folders/zz/export"
	hp := fixtureHostPaths(t, map[string]string{rawExport: canonExport})

	home, cleanup, err := CodexConfinedHome(hp, rawExport, "", "linux")
	if err != nil {
		t.Fatalf("CodexConfinedHome: %v", err)
	}
	defer func() { _ = cleanup() }()

	body := readFile(t, filepath.Join(home, ConfinedProfileName+".config.toml"))
	if !strings.Contains(body, `"`+canonExport+`" = "read"`) {
		t.Errorf("grant does not carry the CANONICAL export %q:\n%s", canonExport, body)
	}
	if strings.Contains(body, `"`+rawExport+`" = "read"`) {
		t.Errorf("grant carries the RAW /var alias, which does not cover the child's real cwd:\n%s", body)
	}
}

func TestTOMLQuote_EscapesQuoteAndBackslash(t *testing.T) {
	if got := tomlQuote(`/a"b\c`); got != `"/a\"b\\c"` {
		t.Errorf("tomlQuote = %s, want an escaped basic string", got)
	}
}

// ---------------------------------------------------------------------------
// Remaining error/degrade branches. Each is reachable through the shipped
// builders and each is asserted behaviourally rather than left as an untested
// defensive limb.
// ---------------------------------------------------------------------------

func TestDefaultHostPaths_HonoursCodexHomeEnv(t *testing.T) {
	t.Setenv("CODEX_HOME", "/somewhere/codex")
	hp := DefaultHostPaths()
	if hp.CodexHome != "/somewhere/codex" {
		t.Errorf("CodexHome = %q, want the CODEX_HOME override", hp.CodexHome)
	}
	if hp.Canonical == nil || hp.TempDir == "" {
		t.Error("DefaultHostPaths must populate Canonical and TempDir")
	}

	t.Setenv("CODEX_HOME", "")
	hp = DefaultHostPaths()
	if hp.HomeDir != "" && hp.CodexHome != filepath.Join(hp.HomeDir, ".codex") {
		t.Errorf("CodexHome = %q, want <home>/.codex when CODEX_HOME is unset", hp.CodexHome)
	}
}

// TestHostPaths_NilCanonicalDefaultsToEvalSymlinks: the nil-Canonical branch is
// the PRODUCTION path (adapters that inject no seam), so it must be exercised.
func TestHostPaths_NilCanonicalDefaultsToEvalSymlinks(t *testing.T) {
	dir := t.TempDir()
	hp := HostPaths{}
	got, err := hp.canonical(dir)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("canonical = %q, want EvalSymlinks %q", got, want)
	}
	if _, err := hp.canonical(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("canonical of an absent path err = %v, want fs.ErrNotExist", err)
	}
}

func TestSplitPath_RootAndRelative(t *testing.T) {
	if got := splitPath("/"); got != nil {
		t.Errorf("splitPath(/) = %v, want nil", got)
	}
	if got := splitPath("a/b"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("splitPath(a/b) = %v", got)
	}
}

func TestTOMLQuote_EscapesControlCharacters(t *testing.T) {
	if got := tomlQuote("a\nb\rc\td"); got != `"a\nb\rc\td"` {
		t.Errorf("tomlQuote = %s, want escaped control characters", got)
	}
}

func TestComposeBaseConfig_BlockWithNoTable(t *testing.T) {
	// The no-table fallback: composeBaseConfig must still emit both halves
	// rather than silently dropping the operator's config or the block.
	got := composeBaseConfig([]byte("model = \"x\"\n"), "default_permissions = \"confined\"\n")
	if !strings.Contains(got, `model = "x"`) || !strings.Contains(got, "default_permissions") {
		t.Errorf("composeBaseConfig dropped a half:\n%s", got)
	}
}

// TestClaudeDenyRules_UnresolvableExportFailsClosed: the export itself cannot be
// canonicalized, so the overlap guard cannot decide and no rules are emitted.
func TestClaudeDenyRules_UnresolvableExportFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	absent := filepath.Join(hp.TempDir, "absent-export")
	hp = fixtureHostPaths(t, nil, absent)
	if _, err := ClaudeDenyRules(hp, absent, "linux"); err == nil {
		t.Fatal("an unresolvable export must fail closed")
	}
}

// TestCodexConfinedHome_UnresolvableExportFailsClosed / _UnresolvableSchema:
// the codex grants are written in CANONICAL form, so a path that cannot be
// resolved must refuse rather than emit a grant that covers nothing.
func TestCodexConfinedHome_UnresolvablePathsFailClosed(t *testing.T) {
	t.Run("export", func(t *testing.T) {
		hp := fixtureHostPaths(t, nil, "/absent-export")
		_, cleanup, err := CodexConfinedHome(hp, "/absent-export", "", "linux")
		if cleanup != nil {
			_ = cleanup()
		}
		if err == nil {
			t.Fatal("an unresolvable export must fail closed")
		}
	})
	t.Run("schema", func(t *testing.T) {
		hp := fixtureHostPaths(t, nil, "/absent-schema.json")
		_, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "/absent-schema.json", "linux")
		if cleanup != nil {
			_ = cleanup()
		}
		if err == nil {
			t.Fatal("an unresolvable schema path must fail closed")
		}
	})
}

// TestCodexConfinedHome_UnreadableOperatorConfigFailsClosed: a config.toml that
// exists but cannot be READ is NOT the missing-file case. Treating it as missing
// would silently drop the operator's model_provider/base_url and point a grounded
// review at a different endpoint than an ungrounded one.
func TestCodexConfinedHome_UnreadableOperatorConfigFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	// A DIRECTORY named config.toml: os.ReadFile returns EISDIR, not ENOENT.
	if err := os.Mkdir(filepath.Join(hp.CodexHome, "config.toml"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("an unreadable operator config must fail closed, not be treated as absent")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, must NOT be conflated with the missing-file degrade", err)
	}
}

// TestCodexConfinedHome_UnreadableAuthFailsClosed: same distinction for the
// credential. Absent is fine (an OPENAI_API_KEY deployment has none); present
// but unreadable is not, because a confined home missing an auth the operator
// DOES have would fail the review with an opaque auth error.
func TestCodexConfinedHome_UnreadableAuthFailsClosed(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	if err := os.Mkdir(filepath.Join(hp.CodexHome, "auth.json"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	home, cleanup, err := CodexConfinedHome(hp, t.TempDir(), "", "linux")
	if cleanup != nil {
		_ = cleanup()
	}
	if err == nil {
		t.Fatal("an unreadable auth.json must fail closed")
	}
	// And the half-built home is removed rather than left as litter.
	if home != "" {
		if _, serr := os.Stat(home); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("a failed build left %q behind", home)
		}
	}
}

// TestCredentialCopyBack_ConfinedAuthRemovedIsANoOp: codex removed the confined
// copy. There is nothing to write back and that is not an error.
func TestCredentialCopyBack_ConfinedAuthRemovedIsANoOp(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatalf("remove confined auth: %v", err)
	}
	if werr := cleanup(); werr != nil {
		t.Errorf("a removed confined auth.json must be a no-op, got %v", werr)
	}
	if got := readFile(t, src); got != `{"token":"old"}` {
		t.Errorf("source = %q, want it untouched", got)
	}
}

// TestCredentialCopyBack_VanishedSourceIsSkipped: the operator deleted auth.json
// during the review. Re-creating it from a stale in-review copy would resurrect a
// credential they removed on purpose, so the copy-back SKIPS.
func TestCredentialCopyBack_VanishedSourceIsSkipped(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"token":"new"}`), 0o600); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	werr := cleanup()
	if _, serr := os.Stat(src); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("a deleted source must NOT be resurrected; stat err = %v", serr)
	}
	if werr == nil || !strings.Contains(werr.Error(), "skipped credential copy-back") {
		t.Errorf("warning = %v, want a named copy-back skip", werr)
	}
}

// TestWithCredentialLock_ExhaustsRetriesOnAHeldLock pins the fail-SAFE arm: a
// lock left behind by a SIGKILLed process wedges the copy-back permanently
// rather than clobbering the operator's bytes.
func TestWithCredentialLock_ExhaustsRetriesOnAHeldLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "auth.json.fishhawk-lock")
	if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	ran := false
	err := withCredentialLock(lock, func() error { ran = true; return nil })
	if !errors.Is(err, errCredentialLockUnavailable) {
		t.Fatalf("err = %v, want errCredentialLockUnavailable", err)
	}
	if ran {
		t.Error("the critical section must NOT run without the lock")
	}
	// The stale lock is left in place — breaking it would defeat the exclusion.
	if _, serr := os.Stat(lock); serr != nil {
		t.Errorf("a held lock must not be removed by a failed acquisition: %v", serr)
	}
}

// TestWithCredentialLock_NonExistFailureIsNotRetried: an OpenFile error that is
// NOT "lock held" (an unwritable directory) must surface immediately rather than
// spinning the full retry budget on a condition that cannot clear.
func TestWithCredentialLock_NonExistFailureIsNotRetried(t *testing.T) {
	err := withCredentialLock(filepath.Join(t.TempDir(), "absent-dir", "lock"), func() error { return nil })
	if !errors.Is(err, errCredentialLockUnavailable) {
		t.Fatalf("err = %v, want errCredentialLockUnavailable", err)
	}
}

// ---------------------------------------------------------------------------
// Copy-back content validation — the NEW bytes, not just the source.
// ---------------------------------------------------------------------------

// TestCredentialCopyBack_MalformedNewContentRefused: the reviewer child is the
// process that ingests the untrusted diff, and the copy-back writes whatever it
// left behind into the operator's real credential. The source is UNCHANGED here
// (so the hash gate passes and cannot account for the refusal) and the confined
// copy holds non-JSON bytes — only guardCredentialShape can refuse it.
func TestCredentialCopyBack_MalformedNewContentRefused(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)

	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	werr := cleanup()

	// COMMITTED STATE first: the refusal's effect is what is left on disk.
	if got := readFile(t, src); got != `{"token":"old"}` {
		t.Errorf("source = %q, want the operator's credential UNTOUCHED", got)
	}
	if !errors.Is(werr, errCredentialShapeChanged) {
		t.Fatalf("cleanup warning = %v, want errCredentialShapeChanged", werr)
	}
	if !strings.Contains(werr.Error(), "not valid JSON") {
		t.Errorf("warning does not name the refusal reason: %v", werr)
	}
}

// TestCredentialCopyBack_ChangedTopLevelKeySetRefused: the swap case. The bytes
// are perfectly valid JSON — a json.Valid-only guard would pass them — but the
// top-level key set differs from the credential that was copied in, so they are
// not a refresh of it.
func TestCredentialCopyBack_ChangedTopLevelKeySetRefused(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `{"token":"old"}`)

	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"attacker_token":"swap"}`), 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	werr := cleanup()

	if got := readFile(t, src); got != `{"token":"old"}` {
		t.Errorf("source = %q, want the operator's credential UNTOUCHED", got)
	}
	if !errors.Is(werr, errCredentialShapeChanged) {
		t.Fatalf("cleanup warning = %v, want errCredentialShapeChanged", werr)
	}
	if !strings.Contains(werr.Error(), "top-level keys") {
		t.Errorf("warning does not name the refusal reason: %v", werr)
	}
}

// TestCredentialCopyBack_NonObjectSourceHeldToJSONValidityOnly: a source that
// was never a JSON object is held to json validity alone rather than to a key
// set it never had — the guard must not refuse a legitimate refresh of a shape
// it cannot reason about.
func TestCredentialCopyBack_NonObjectSourceHeldToJSONValidityOnly(t *testing.T) {
	hp := fixtureHostPaths(t, nil)
	src, home, cleanup := seedConfined(t, hp, `"bare-string-credential"`)

	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`"refreshed-string"`), 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if werr := cleanup(); werr != nil {
		t.Fatalf("cleanup refused a valid-JSON refresh of a non-object source: %v", werr)
	}
	if got := readFile(t, src); got != `"refreshed-string"` {
		t.Errorf("source = %q, want the refreshed credential", got)
	}
}

// TestGuardCredentialShape_NullIsNotAnObject pins the edge a json.Unmarshal into
// a map would silently accept: `null` decodes into a NIL map with no error,
// which would look like an object carrying no keys — so a source object with no
// keys would accept a `null` write-back.
func TestGuardCredentialShape_NullIsNotAnObject(t *testing.T) {
	empty, ok := topLevelKeys([]byte(`{}`))
	if !ok || len(empty) != 0 {
		t.Fatalf("topLevelKeys(`{}`) = %v, %v; want empty set, true", empty, ok)
	}
	if _, ok := topLevelKeys([]byte(`null`)); ok {
		t.Error("topLevelKeys reported `null` as a JSON object")
	}
	snapshot := credentialSnapshot{object: true, keys: nil}
	if err := guardCredentialShape(snapshot, []byte(`null`)); !errors.Is(err, errCredentialShapeChanged) {
		t.Errorf("guardCredentialShape(`null`) = %v, want errCredentialShapeChanged", err)
	}
}
