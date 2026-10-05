package upkeep_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// lines joins fixture lines with a trailing newline.
func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func TestManifestEcosystem(t *testing.T) {
	for _, tc := range []struct {
		path, eco, dir string
		ok             bool
	}{
		{"go.mod", upkeep.EcosystemGo, ".", true},
		{"backend/go.mod", upkeep.EcosystemGo, "backend", true},
		{"frontend/pnpm-lock.yaml", upkeep.EcosystemNPM, "frontend", true},
		{"./site/docs/pnpm-lock.yaml", upkeep.EcosystemNPM, "site/docs", true},
		// package.json holds ranges, never a resolved version.
		{"frontend/package.json", "", "", false},
		{"backend/go.sum", "", "", false},
		{"backend/main.go", "", "", false},
		{"../go.mod", "", "", false},
		{"/go.mod", "", "", false},
		{"", "", "", false},
	} {
		eco, dir, ok := upkeep.ManifestEcosystem(tc.path)
		if eco != tc.eco || dir != tc.dir || ok != tc.ok {
			t.Errorf("ManifestEcosystem(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.path, eco, dir, ok, tc.eco, tc.dir, tc.ok)
		}
	}
}

func TestSplitComparePatch(t *testing.T) {
	patch := lines(
		"preamble that is not a file",
		"diff --git a/backend/go.mod b/backend/go.mod",
		"@@ -1,1 +1,1 @@",
		"-x",
		"+y",
		"diff --git a/old/name.txt b/new/name.txt",
		"@@ -1,1 +1,2 @@",
		"+diff --git a/evil b/evil",
		" z",
	)
	got := upkeep.SplitComparePatch(patch)
	want := map[string]string{
		"backend/go.mod": lines("diff --git a/backend/go.mod b/backend/go.mod", "@@ -1,1 +1,1 @@", "-x", "+y"),
		// A rename is keyed by its b/ path, and a '+diff --git' hunk line
		// never starts a section.
		"new/name.txt": lines("diff --git a/old/name.txt b/new/name.txt", "@@ -1,1 +1,2 @@", "+diff --git a/evil b/evil", " z"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SplitComparePatch = %#v, want %#v", got, want)
	}
	if got := upkeep.SplitComparePatch(""); got == nil || len(got) != 0 {
		t.Errorf("SplitComparePatch(\"\") = %#v, want empty non-nil", got)
	}
}

func TestAddedVersions_Go(t *testing.T) {
	patch := lines(
		"diff --git a/go.mod b/go.mod",
		"--- a/go.mod",
		"+++ b/go.mod",
		"@@ -1,3 +1,9 @@",
		"+require golang.org/x/text v0.14.0",
		" require (",
		"+\tgolang.org/x/net v0.20.0",
		"+\tgithub.com/x/y v1.0.0 // indirect",
		"+\tgolang.org/x/old => golang.org/x/new v1.0.0",
		"+\tgolang.org/x/old v1.0.0 => golang.org/x/new v1.1.0",
		"+exclude golang.org/x/ex v0.1.0",
		"-\tgolang.org/x/gone v0.1.0",
		"+go 1.25.0",
		"+toolchain go1.25.6",
		"+require (",
		" )",
	)
	want := []upkeep.PackageVersion{
		{Package: "golang.org/x/text", Version: "v0.14.0"},
		{Package: "golang.org/x/net", Version: "v0.20.0"},
		{Package: "github.com/x/y", Version: "v1.0.0"},
	}
	if got := upkeep.AddedVersions(upkeep.EcosystemGo, patch); !reflect.DeepEqual(got, want) {
		t.Errorf("AddedVersions(go) = %#v, want %#v", got, want)
	}
	if got := upkeep.AddedVersions(upkeep.EcosystemGo, "@@ bogus\n+golang.org/x/net v0.20.0\n"); got == nil || len(got) != 0 {
		t.Errorf("AddedVersions(malformed hunk) = %#v, want empty non-nil", got)
	}
	if got := upkeep.AddedVersions("cargo", patch); got == nil || len(got) != 0 {
		t.Errorf("AddedVersions(unknown ecosystem) = %#v, want empty non-nil", got)
	}
}

func TestAddedVersions_NPM(t *testing.T) {
	patch := lines(
		"diff --git a/frontend/pnpm-lock.yaml b/frontend/pnpm-lock.yaml",
		"@@ -1,1 +1,9 @@",
		"+  foo@1.2.3:",
		"+  '@scope/pkg@2.0.0':",
		"+  bar@3.1.0(react@18.2.0):",
		"+  /old@0.4.1:",
		"+    version: 9.9.9",
		"+      baz@1.0.0:",
		"+  \"quoted@4.0.0\":",
		"+  nover:",
		"+  trailing@:",
		"+  inline@1.0.0: {}",
		" ",
	)
	want := []upkeep.PackageVersion{
		{Package: "foo", Version: "1.2.3"},
		{Package: "@scope/pkg", Version: "2.0.0"},
		{Package: "bar", Version: "3.1.0"},
		{Package: "old", Version: "0.4.1"},
		{Package: "quoted", Version: "4.0.0"},
	}
	if got := upkeep.AddedVersions(upkeep.EcosystemNPM, patch); !reflect.DeepEqual(got, want) {
		t.Errorf("AddedVersions(npm) = %#v, want %#v", got, want)
	}
}

func TestGoModRequires(t *testing.T) {
	content := lines(
		"module example.com/backend",
		"",
		"go 1.25.0",
		"",
		"require golang.org/x/text v0.14.0",
		"",
		"require (",
		"\tgolang.org/x/net v0.20.0",
		"\tgithub.com/c/d v1.2.0 // indirect",
		")",
		"",
		"replace (",
		"\tgolang.org/x/rep v1.0.0 => golang.org/x/rep v1.1.0",
		")",
		"",
		"exclude (",
		"\tgolang.org/x/ex v0.1.0",
		")",
		"",
		"retract (",
		"\tv1.0.0 // broken",
		"\t[v1.1.0, v1.2.0]",
		")",
		"",
		"exclude golang.org/x/ex2 v0.2.0",
	)
	got, err := upkeep.GoModRequires(content)
	if err != nil {
		t.Fatalf("GoModRequires: %v", err)
	}
	want := map[string]string{
		"golang.org/x/text": "v0.14.0",
		"golang.org/x/net":  "v0.20.0",
		"github.com/c/d":    "v1.2.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoModRequires = %#v, want %#v", got, want)
	}

	for name, bad := range map[string]string{
		"duplicate require":     lines("require golang.org/x/net v0.20.0", "require (", "\tgolang.org/x/net v0.24.0", ")"),
		"unterminated block":    lines("require (", "\tgolang.org/x/net v0.20.0"),
		"unmatched close":       lines(")"),
		"malformed block entry": lines("require (", "\tgolang.org/x/net", ")"),
		"malformed single line": lines("require golang.org/x/net"),
		"malformed block open":  lines("require weird (", ")"),
	} {
		if _, err := upkeep.GoModRequires(bad); err == nil {
			t.Errorf("GoModRequires(%s) = nil error, want an error", name)
		}
	}
}

func TestPnpmLockPackages(t *testing.T) {
	v9 := lines(
		"lockfileVersion: '9.0'",
		"",
		"importers:",
		"",
		"  .:",
		"    dependencies:",
		"      foo:",
		"        specifier: ^1.2.0",
		"        version: 1.2.4",
		"",
		"packages:",
		"",
		"  foo@1.2.4:",
		"    resolution: {integrity: sha512-x}",
		"",
		"  '@scope/pkg@2.0.0':",
		"    resolution: {integrity: sha512-y}",
		"",
		"snapshots:",
		"",
		"  foo@1.2.4(react@18.2.0):",
		"    dependencies:",
		"      react: 18.2.0",
		"",
		"  snaponly@9.9.9: {}",
	)
	got, err := upkeep.PnpmLockPackages(v9)
	if err != nil {
		t.Fatalf("PnpmLockPackages(v9): %v", err)
	}
	want := map[upkeep.PackageVersion]bool{
		{Package: "foo", Version: "1.2.4"}:        true,
		{Package: "@scope/pkg", Version: "2.0.0"}: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PnpmLockPackages(v9) = %#v, want %#v", got, want)
	}

	v6 := lines(
		"lockfileVersion: '6.0'",
		"",
		"packages:",
		"",
		"  /foo@1.2.4(react@17.0.0):",
		"    resolution: {integrity: sha512-a}",
		"",
		"  /@scope/pkg@2.0.0:",
		"    resolution: {integrity: sha512-b}",
	)
	got, err = upkeep.PnpmLockPackages(v6)
	if err != nil {
		t.Fatalf("PnpmLockPackages(v6): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PnpmLockPackages(v6) = %#v, want %#v", got, want)
	}

	if got, err := upkeep.PnpmLockPackages(lines("lockfileVersion: '9.0'")); err != nil || got == nil || len(got) != 0 {
		t.Errorf("PnpmLockPackages(no packages) = (%#v, %v), want empty non-nil, nil", got, err)
	}
	for name, bad := range map[string]string{
		"v5 refused":              lines("lockfileVersion: 5.4", "", "packages:", "", "  /foo/1.2.4:", "    resolution: {integrity: sha512-a}"),
		"missing lockfileVersion": lines("packages:", "  foo@1.2.4:", "    resolution: {}"),
		"packages not a mapping":  lines("lockfileVersion: '9.0'", "packages: [a, b]"),
		"not yaml":                "lockfileVersion: '9.0'\npackages: {\n",
	} {
		if _, err := upkeep.PnpmLockPackages(bad); err == nil {
			t.Errorf("PnpmLockPackages(%s) = nil error, want an error", name)
		}
	}
}

// goModHead is a head go.mod: x/net v0.20.0 is required at line 7.
var goModHead = lines(
	"module example.com/backend",          // 1
	"",                                    // 2
	"go 1.25.0",                           // 3
	"",                                    // 4
	"require (",                           // 5
	"\tgithub.com/a/b v1.0.0",             // 6
	"\tgolang.org/x/net v0.20.0",          // 7
	")",                                   // 8
	"",                                    // 9
	"require (",                           // 10
	"\tgithub.com/c/d v1.2.0 // indirect", // 11
	")",                                   // 12
)

// pnpmV9Head is a head v9 lockfile: foo@1.2.4 is a packages key at line 13
// and a snapshots key at line 21.
var pnpmV9Head = lines(
	"lockfileVersion: '9.0'",                // 1
	"",                                      // 2
	"importers:",                            // 3
	"",                                      // 4
	"  .:",                                  // 5
	"    dependencies:",                     // 6
	"      foo:",                            // 7
	"        specifier: ^1.2.0",             // 8
	"        version: 1.2.4",                // 9
	"",                                      // 10
	"packages:",                             // 11
	"",                                      // 12
	"  foo@1.2.4:",                          // 13
	"    resolution: {integrity: sha512-x}", // 14
	"",                                      // 15
	"  '@scope/pkg@2.0.0':",                 // 16
	"    resolution: {integrity: sha512-y}", // 17
	"",                                      // 18
	"snapshots:",                            // 19
	"",                                      // 20
	"  foo@1.2.4(react@18.2.0):",            // 21
	"    dependencies:",                     // 22
	"      react: 18.2.0",                   // 23
)

// TestDependencyChanges: each case is a run patch plus the manifest at the
// run's head. The "no change" cases each isolate one rule (named in the
// case), so deleting that rule alone turns the case into a change.
func TestDependencyChanges(t *testing.T) {
	xnet := func(v string) []upkeep.DependencyChange {
		return []upkeep.DependencyChange{{Ecosystem: upkeep.EcosystemGo, Directory: "backend", Manifest: "backend/go.mod", Package: "golang.org/x/net", Version: v}}
	}
	for _, tc := range []struct {
		name, path, patch, head string
		want                    []upkeep.DependencyChange
	}{
		{
			name: "go: a bump inside a require block is a change (positive control)",
			path: "backend/go.mod",
			patch: lines(
				"diff --git a/backend/go.mod b/backend/go.mod",
				"@@ -5,4 +5,4 @@",
				" require (",
				" \tgithub.com/a/b v1.0.0",
				"-\tgolang.org/x/net v0.19.0",
				"+\tgolang.org/x/net v0.20.0",
				" )",
			),
			head: goModHead,
			want: xnet("v0.20.0"),
		},
		{
			name: "go: a single-line require directive counts",
			path: "go.mod",
			patch: lines(
				"@@ -1,2 +1,2 @@",
				"",
				"-require golang.org/x/net v0.19.0",
				"+require golang.org/x/net v0.20.0",
			),
			head: lines("", "require golang.org/x/net v0.20.0"),
			want: []upkeep.DependencyChange{{Ecosystem: upkeep.EcosystemGo, Directory: ".", Manifest: "go.mod", Package: "golang.org/x/net", Version: "v0.20.0"}},
		},
		{
			// NET-ADD (removed half): the pair is removed from the indirect
			// block and re-added, `// indirect` dropped, at the same version.
			name: "go: a moved require line is not a change (net-add)",
			path: "backend/go.mod",
			patch: lines(
				"@@ -5,8 +5,8 @@",
				" require (",
				" \tgithub.com/a/b v1.0.0",
				"+\tgolang.org/x/net v0.20.0",
				" )",
				" ",
				" require (",
				" \tgithub.com/c/d v1.2.0 // indirect",
				"-\tgolang.org/x/net v0.20.0 // indirect",
				" )",
			),
			head: goModHead,
			want: []upkeep.DependencyChange{},
		},
		{
			name: "go: a tidy reshuffle is not a change (net-add)",
			path: "backend/go.mod",
			patch: lines(
				"@@ -5,4 +5,4 @@",
				" require (",
				"-\tgolang.org/x/net v0.20.0",
				" \tgithub.com/a/b v1.0.0",
				"+\tgolang.org/x/net v0.20.0",
				" )",
			),
			head: lines(
				"module example.com/backend",
				"",
				"go 1.25.0",
				"",
				"require (",
				"\tgithub.com/a/b v1.0.0",
				"\tgolang.org/x/net v0.20.0",
				")",
			),
			want: []upkeep.DependencyChange{},
		},
		{
			// REQUIRE POSITION: the line is shape-valid and nothing else
			// rejects it (the head requires a DIFFERENT version), so only
			// the head's require-directive classification does.
			name: "go: an exclude-block line is not a change (require position)",
			path: "backend/go.mod",
			patch: lines(
				"@@ -3,0 +4,4 @@",
				"+",
				"+exclude (",
				"+\tgolang.org/x/net v0.20.0",
				"+)",
			),
			head: lines(
				"require golang.org/x/net v0.24.0", // 1
				"",                                 // 2
				"go 1.25.0",                        // 3
				"",                                 // 4
				"exclude (",                        // 5
				"\tgolang.org/x/net v0.20.0",       // 6
				")",                                // 7
			),
			want: []upkeep.DependencyChange{},
		},
		{
			name: "go: an exclude-block addition of a pair the head already requires is not a change",
			path: "backend/go.mod",
			patch: lines(
				"@@ -1,1 +1,5 @@",
				" require golang.org/x/net v0.20.0",
				"+",
				"+exclude (",
				"+\tgolang.org/x/net v0.20.0",
				"+)",
			),
			head: lines(
				"require golang.org/x/net v0.20.0",
				"",
				"exclude (",
				"\tgolang.org/x/net v0.20.0",
				")",
			),
			want: []upkeep.DependencyChange{},
		},
		{
			name: "go: a single-line retract is not a change (require position)",
			path: "go.mod",
			patch: lines(
				"@@ -1,0 +2,1 @@",
				"+retract v1.0.0",
			),
			head: lines("module example.com/m", "retract v1.0.0"),
			want: []upkeep.DependencyChange{},
		},
		{
			// npm positive control: a packages key bump.
			name: "npm: a packages-key bump is a change (positive control)",
			path: "frontend/pnpm-lock.yaml",
			patch: lines(
				"@@ -9,1 +9,1 @@",
				"-        version: 1.2.3",
				"+        version: 1.2.4",
				"@@ -13,1 +13,1 @@",
				"-  foo@1.2.3:",
				"+  foo@1.2.4:",
				"@@ -21,1 +21,1 @@",
				"-  foo@1.2.3(react@18.2.0):",
				"+  foo@1.2.4(react@18.2.0):",
			),
			head: pnpmV9Head,
			want: []upkeep.DependencyChange{{Ecosystem: upkeep.EcosystemNPM, Directory: "frontend", Manifest: "frontend/pnpm-lock.yaml", Package: "foo", Version: "1.2.4"}},
		},
		{
			name: "npm: a snapshot-key churn for a version already resolved is not a change",
			path: "frontend/pnpm-lock.yaml",
			patch: lines(
				"@@ -21,1 +21,1 @@",
				"-  foo@1.2.4(react@18.0.0):",
				"+  foo@1.2.4(react@18.2.0):",
			),
			head: pnpmV9Head,
			want: []upkeep.DependencyChange{},
		},
		{
			name: "npm v6: a peer-suffix churn for a version already resolved is not a change (net-add)",
			path: "frontend/pnpm-lock.yaml",
			patch: lines(
				"@@ -5,1 +5,1 @@",
				"-  /foo@1.2.4(react@17.0.0):",
				"+  /foo@1.2.4(react@18.2.0):",
			),
			head: lines(
				"lockfileVersion: '6.0'",
				"",
				"packages:",
				"",
				"  /foo@1.2.4(react@18.2.0):",
				"    resolution: {integrity: sha512-a}",
			),
			want: []upkeep.DependencyChange{},
		},
		{
			// NET-ADD (unchanged half): a new peer variant of a version an
			// UNCHANGED packages key already resolves; nothing is removed.
			name: "npm v6: a new peer variant of an already-resolved version is not a change (net-add, unchanged line)",
			path: "frontend/pnpm-lock.yaml",
			patch: lines(
				"@@ -6,0 +7,3 @@",
				"+",
				"+  /foo@1.2.4(react@18.2.0):",
				"+    resolution: {integrity: sha512-b}",
			),
			head: lines(
				"lockfileVersion: '6.0'",                // 1
				"",                                      // 2
				"packages:",                             // 3
				"",                                      // 4
				"  /foo@1.2.4(react@17.0.0):",           // 5
				"    resolution: {integrity: sha512-a}", // 6
				"",                                      // 7
				"  /foo@1.2.4(react@18.2.0):",           // 8
				"    resolution: {integrity: sha512-b}", // 9
			),
			want: []upkeep.DependencyChange{},
		},
		{
			// YAML CONFIRMATION: the line scan sees a two-space key under
			// `packages:`, but the YAML structure nests it under another
			// key, so it is not a resolved package.
			name: "npm: a key the YAML packages mapping does not hold is not a change (head confirmation)",
			path: "frontend/pnpm-lock.yaml",
			patch: lines(
				"@@ -5,1 +5,3 @@",
				"  bar@1.0.0:",
				"+  foo@1.2.4:",
				"+    resolution: {integrity: sha512-x}",
			),
			head: lines(
				"lockfileVersion: '9.0'",
				"",
				"packages:",
				"",
				" bar@1.0.0:",
				"  foo@1.2.4:",
				"    resolution: {integrity: sha512-x}",
			),
			want: []upkeep.DependencyChange{},
		},
	} {
		got, err := upkeep.DependencyChanges(tc.path, tc.patch, tc.head)
		if err != nil {
			t.Errorf("%s: DependencyChanges error = %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: DependencyChanges = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// TestDependencyChanges_Errors: inputs that cannot be read fail toward no
// change by erroring.
func TestDependencyChanges_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, path, patch, head string
	}{
		{
			// HEAD CONFIRMATION: the patch claims v0.20.0 at line 7, the
			// head holds v0.24.0 there, inside the require block.
			name: "patch disagrees with the head content",
			path: "backend/go.mod",
			patch: lines(
				"@@ -7,1 +7,1 @@",
				"-\tgolang.org/x/net v0.19.0",
				"+\tgolang.org/x/net v0.20.0",
			),
			head: strings.Replace(goModHead, "golang.org/x/net v0.20.0", "golang.org/x/net v0.24.0", 1),
		},
		{
			name:  "added line past the head content",
			path:  "backend/go.mod",
			patch: lines("@@ -40,0 +41,1 @@", "+\tgolang.org/x/net v0.20.0"),
			head:  goModHead,
		},
		{
			name:  "go.mod requiring a module twice",
			path:  "go.mod",
			patch: lines("@@ -1,0 +2,1 @@", "+require golang.org/x/net v0.20.0"),
			head:  lines("require golang.org/x/net v0.24.0", "require golang.org/x/net v0.20.0"),
		},
		{
			name:  "pnpm lockfile below v6",
			path:  "pnpm-lock.yaml",
			patch: lines("@@ -1,0 +2,1 @@", "+  /foo/1.2.4:"),
			head:  lines("lockfileVersion: 5.4", "  /foo/1.2.4:"),
		},
		{
			name:  "malformed hunk header",
			path:  "go.mod",
			patch: lines("@@ nope @@", "+require golang.org/x/net v0.20.0"),
			head:  lines("require golang.org/x/net v0.20.0"),
		},
		{
			name:  "hunk header line number overflows",
			path:  "go.mod",
			patch: lines("@@ -1,1 +99999999999999999999999,1 @@", "+require golang.org/x/net v0.20.0"),
			head:  lines("require golang.org/x/net v0.20.0"),
		},
		{
			name:  "unexpected hunk line",
			path:  "go.mod",
			patch: lines("@@ -1,1 +1,1 @@", "?require golang.org/x/net v0.20.0"),
			head:  lines("require golang.org/x/net v0.20.0"),
		},
		{
			name:  "not a manifest",
			path:  "frontend/package.json",
			patch: lines("@@ -1,1 +1,1 @@", "+x"),
			head:  lines("x"),
		},
	} {
		if got, err := upkeep.DependencyChanges(tc.path, tc.patch, tc.head); err == nil {
			t.Errorf("%s: DependencyChanges = (%#v, nil), want an error", tc.name, got)
		}
	}
}

// matchBase is an advisory the change matchBaseChange is a positive match
// for; every rule case below differs from that pair in ONE field.
var matchBase = upkeep.InFlightAdvisory{
	FindingID:    "advisory:GHSA-aaaa-bbbb-cccc:example-lib",
	IDs:          []string{"GHSA-aaaa-bbbb-cccc"},
	Ecosystem:    upkeep.EcosystemNPM,
	Package:      "example-lib",
	Version:      "2.1.0",
	FixedVersion: "2.3.0",
	Directories:  []string{"frontend"},
	Severity:     "medium",
}

var matchBaseChange = upkeep.DependencyChange{
	Ecosystem: upkeep.EcosystemNPM, Directory: "frontend", Manifest: "frontend/pnpm-lock.yaml",
	Package: "example-lib", Version: "2.2.0",
}

func TestMatchInFlight_Rules(t *testing.T) {
	with := func(f func(*upkeep.DependencyChange)) upkeep.DependencyChange {
		c := matchBaseChange
		f(&c)
		return c
	}
	adv := func(f func(*upkeep.InFlightAdvisory)) upkeep.InFlightAdvisory {
		a := matchBase
		a.Directories = append([]string(nil), matchBase.Directories...)
		f(&a)
		return a
	}
	for _, tc := range []struct {
		name   string
		a      upkeep.InFlightAdvisory
		c      upkeep.DependencyChange
		wantOK bool
	}{
		{"positive control: below the fix on the in-use line", matchBase, matchBaseChange, true},
		{"ecosystem differs (same package string)", matchBase, with(func(c *upkeep.DependencyChange) { c.Ecosystem = upkeep.EcosystemGo }), false},
		{"package differs", matchBase, with(func(c *upkeep.DependencyChange) { c.Package = "example-lib2" }), false},
		{"directory not cited", matchBase, with(func(c *upkeep.DependencyChange) { c.Directory = "site" }), false},
		// Condition 5: in-use v2.1.0, fixed v2.3.0; the change v1.9.0 is
		// BELOW the fix, so only the version-line rule rejects it.
		{"another major line below the fix", matchBase, with(func(c *upkeep.DependencyChange) { c.Version = "1.9.0" }), false},
		{"at the fixed version", matchBase, with(func(c *upkeep.DependencyChange) { c.Version = "2.3.0" }), false},
		{"above the fixed version", matchBase, with(func(c *upkeep.DependencyChange) { c.Version = "2.4.0" }), false},
		{"unparseable change version", matchBase, with(func(c *upkeep.DependencyChange) { c.Version = "^2.2.0" }), false},
		{"unparseable fixed version", adv(func(a *upkeep.InFlightAdvisory) { a.FixedVersion = "2.3" }), matchBaseChange, false},
		{"0.x: another minor line", adv(func(a *upkeep.InFlightAdvisory) { a.Version, a.FixedVersion = "0.20.0", "0.23.0" }),
			with(func(c *upkeep.DependencyChange) { c.Version = "0.21.0" }), false},
		{"0.x: same minor below the fix", adv(func(a *upkeep.InFlightAdvisory) { a.Version, a.FixedVersion = "0.20.0", "0.23.0" }),
			with(func(c *upkeep.DependencyChange) { c.Version = "0.20.5" }), true},
		{"go pseudo-version below the fix", adv(func(a *upkeep.InFlightAdvisory) {
			a.Ecosystem, a.Version, a.FixedVersion = upkeep.EcosystemGo, "v0.23.0-0.20240101000000-abcdef123456", "v0.23.0"
		}), with(func(c *upkeep.DependencyChange) {
			c.Ecosystem, c.Version = upkeep.EcosystemGo, "v0.23.0-0.20240201000000-abcdef123456"
		}), true},
		{"no fix: below the in-use version", adv(func(a *upkeep.InFlightAdvisory) { a.FixedVersion = "" }),
			with(func(c *upkeep.DependencyChange) { c.Version = "2.0.0" }), false},
		{"no fix: at the in-use version", adv(func(a *upkeep.InFlightAdvisory) { a.FixedVersion = "" }),
			with(func(c *upkeep.DependencyChange) { c.Version = "2.1.0" }), true},
		{"no fix: above the in-use version", adv(func(a *upkeep.InFlightAdvisory) { a.FixedVersion = "" }),
			with(func(c *upkeep.DependencyChange) { c.Version = "2.5.0" }), true},
	} {
		got := upkeep.MatchInFlight([]upkeep.InFlightAdvisory{tc.a}, []upkeep.DependencyChange{tc.c})
		if gotOK := len(got) == 1; gotOK != tc.wantOK || len(got) > 1 {
			t.Errorf("%s: MatchInFlight = %#v, want matched=%v", tc.name, got, tc.wantOK)
		}
	}
}

func TestMatchInFlight_OrderAndDedupe(t *testing.T) {
	other := upkeep.InFlightAdvisory{IDs: []string{"GO-2024-0001"}, Ecosystem: upkeep.EcosystemGo, Package: "golang.org/x/net",
		Version: "v0.20.0", FixedVersion: "v0.23.0", Directories: []string{"runner", "backend"}}
	unmatched := upkeep.InFlightAdvisory{IDs: []string{"GO-2024-0002"}, Ecosystem: upkeep.EcosystemGo, Package: "golang.org/x/text",
		Version: "v0.14.0", FixedVersion: "v0.15.0", Directories: []string{"backend"}}
	runnerC := upkeep.DependencyChange{Ecosystem: upkeep.EcosystemGo, Directory: "runner", Manifest: "runner/go.mod", Package: "golang.org/x/net", Version: "v0.20.3"}
	backendC := upkeep.DependencyChange{Ecosystem: upkeep.EcosystemGo, Directory: "backend", Manifest: "backend/go.mod", Package: "golang.org/x/net", Version: "v0.20.3"}
	backendC2 := backendC
	backendC2.Version = "v0.20.1"
	got := upkeep.MatchInFlight(
		[]upkeep.InFlightAdvisory{other, unmatched, matchBase},
		[]upkeep.DependencyChange{runnerC, matchBaseChange, backendC, runnerC, backendC2},
	)
	want := []upkeep.InFlightMatch{
		{Advisory: other, Changes: []upkeep.DependencyChange{backendC2, backendC, runnerC}},
		{Advisory: matchBase, Changes: []upkeep.DependencyChange{matchBaseChange}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MatchInFlight = %#v, want %#v", got, want)
	}
	if got := upkeep.MatchInFlight(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("MatchInFlight(nil, nil) = %#v, want empty non-nil", got)
	}
}

func TestRenderInFlightFinding(t *testing.T) {
	const callerFn, callerFile = "SENTINELCALLERFUNC", "SENTINEL_CALLER_FILE.go"
	a := upkeep.InFlightAdvisory{
		FindingID:    "advisory:GO-2024-2687:golang.org/x/net",
		IDs:          []string{"GO-2024-2687", "CVE-2023-45288"},
		Ecosystem:    upkeep.EcosystemGo,
		Package:      "golang.org/x/net",
		Version:      "v0.20.0",
		FixedVersion: "v0.23.0",
		Directories:  []string{"backend"},
		Reachability: "called",
		Severity:     "high",
		CallPath: []upkeep.DisclosureFrame{
			{Package: "golang.org/x/net/http2", Function: "ReadFrame", Filename: "frame.go"},
			{Package: "example.com/backend/internal/srv", Function: callerFn, Receiver: "*Server", Filename: callerFile},
		},
	}
	m := upkeep.InFlightMatch{Advisory: a, Changes: []upkeep.DependencyChange{
		{Ecosystem: upkeep.EcosystemGo, Directory: "backend", Manifest: "backend/go.mod", Package: "golang.org/x/net", Version: "v0.21.0"},
	}}
	summary, detail, severity := upkeep.RenderInFlightFinding(m)
	if summary != upkeep.InFlightFindingSummary(a) {
		t.Errorf("summary = %q, want InFlightFindingSummary %q", summary, upkeep.InFlightFindingSummary(a))
	}
	if want := "Dependency advisory GO-2024-2687 (go golang.org/x/net) matches a version this run's dependency changes introduce"; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
	for _, want := range []string{
		"`GO-2024-2687`", "`CVE-2023-45288`", "Fixed version: `v0.23.0`",
		"- `backend` (`backend/go.mod`): `v0.21.0`", "In use on the default branch: `v0.20.0`",
		"Reachability: `called`", "Vulnerable symbol: `golang.org/x/net/http2.ReadFrame` (1 caller frame(s) omitted)",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	// Caller frames name this repository's own code: never rendered.
	for _, leak := range []string{callerFn, callerFile, "example.com/backend/internal/srv", "Server"} {
		if strings.Contains(summary+detail, leak) {
			t.Errorf("render discloses caller-frame token %q:\n%s", leak, detail)
		}
	}
	if severity != "high" {
		t.Errorf("severity = %q, want high", severity)
	}

	b := a
	b.FixedVersion = ""
	b.Severity = "critical"
	b.Package = "golang.org/x/net\n@everyone"
	b.CallPath = []upkeep.DisclosureFrame{{Package: "golang.org/x/net/http2"}}
	summary, detail, severity = upkeep.RenderInFlightFinding(upkeep.InFlightMatch{Advisory: b})
	for _, want := range []string{"Fixed version: no fix published", "Package: (withheld: not a plain token)", "Vulnerable symbol: `golang.org/x/net/http2`\n"} {
		if !strings.Contains(detail, want) {
			t.Errorf("no-fix detail lacks %q:\n%s", want, detail)
		}
	}
	if strings.Contains(summary+detail, "@everyone") {
		t.Errorf("a non-plain package leaked into the render:\n%s\n%s", summary, detail)
	}
	if severity != "" {
		t.Errorf("severity for an unknown value = %q, want empty", severity)
	}
	if _, detail, _ := upkeep.RenderInFlightFinding(upkeep.InFlightMatch{Advisory: upkeep.InFlightAdvisory{}}); !strings.Contains(detail, "Vulnerable symbol: none recorded") || !strings.Contains(detail, "Advisory IDs: none") {
		t.Errorf("empty-advisory detail = %q, want none markers", detail)
	}
	if s := upkeep.InFlightFindingSummary(upkeep.InFlightAdvisory{Ecosystem: "go", Package: "m"}); !strings.HasPrefix(s, "Dependency advisory advisory (go m)") {
		t.Errorf("summary with no ids = %q", s)
	}
	if _, detail, _ := upkeep.RenderInFlightFinding(upkeep.InFlightMatch{Advisory: upkeep.InFlightAdvisory{CallPath: []upkeep.DisclosureFrame{{}}}}); !strings.Contains(detail, "Vulnerable symbol: not resolved below the module\n") {
		t.Errorf("module-only frame detail = %q", detail)
	}
}
