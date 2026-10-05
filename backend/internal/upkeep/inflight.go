package upkeep

// In-flight advisory match (#3763): the pure half of the pass that tells an
// open run its own dependency changes introduce a version an advisory-watch
// finding affects. The server reads a run's diff through the forge compare
// seam and the changed manifests at the run's head; this file splits that
// diff per file, extracts the (package, version) pairs the run NET-ADDS
// inside a require directive (go.mod) or the packages section
// (pnpm-lock.yaml), matches them against advisory findings, and renders the
// crew-message finding from structured fields only. Like advisory.go it
// imports no plan, forge or server package. Long-form contract: README.md
// § "In-flight advisory match (#3763)".

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest basenames the in-flight pass reads. package.json is deliberately
// absent: it holds ranges, never a resolved version.
const (
	goModManifest    = "go.mod"
	pnpmLockManifest = "pnpm-lock.yaml"
)

// pnpmMinLockfileMajor is the oldest pnpm lockfile major whose `packages`
// keys are `name@version` (v6 with a leading `/`). v5's `/name/version`
// form is refused.
const pnpmMinLockfileMajor = 6

// PackageVersion is one (package, version) pair: a Go module path and its
// required version, or an npm package name and its resolved version.
type PackageVersion struct {
	Package string
	Version string
}

// DependencyChange is one (package, version) pair a run's own diff
// introduces in one manifest.
type DependencyChange struct {
	// Ecosystem is EcosystemGo or EcosystemNPM.
	Ecosystem string
	// Directory is the manifest's directory in the manifest form ("." for
	// the repository root), comparable with InFlightAdvisory.Directories.
	Directory string
	// Manifest is the repository-relative manifest path.
	Manifest string
	Package  string
	Version  string
}

// ManifestEcosystem reports whether p is a manifest the in-flight pass reads
// and, if so, its ecosystem and directory: go.mod -> EcosystemGo,
// pnpm-lock.yaml -> EcosystemNPM, with dir "." for the repository root else
// the cleaned slash path (the plan.UpkeepAdvisoryManifestDirs form).
// package.json, any other file, and a path that is absolute or escapes the
// root are !ok.
func ManifestEcosystem(p string) (ecosystem, dir string, ok bool) {
	c := path.Clean(p)
	if p == "" || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", "", false
	}
	switch path.Base(c) {
	case goModManifest:
		return EcosystemGo, path.Dir(c), true
	case pnpmLockManifest:
		return EcosystemNPM, path.Dir(c), true
	}
	return "", "", false
}

// diffHeaderPrefix starts the synthetic per-file header the forge compare
// seam writes ahead of each changed file's hunks.
const diffHeaderPrefix = "diff --git a/"

// SplitComparePatch splits a forge compare patch (forge.ComparePatchResult
// .Patch) into one section per changed file, keyed by the header's `b/`
// path (the post-change path, so a rename is keyed by its new name). A
// section starts at a COLUMN-0 `diff --git a/<p> b/<q>` header and includes
// it. Every hunk line begins with ' ', '+', '-', '\' or `@@`, so a header
// cannot be forged from inside a hunk. Text before the first header is
// dropped. It never returns nil.
func SplitComparePatch(patch string) map[string]string {
	out := map[string]string{}
	key := ""
	var b strings.Builder
	flush := func() {
		if key != "" {
			out[key] = b.String()
		}
		b.Reset()
	}
	for _, line := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(line, diffHeaderPrefix) {
			flush()
			rest := strings.TrimRight(line[len(diffHeaderPrefix):], "\r\n")
			key = ""
			if i := strings.LastIndex(rest, " b/"); i >= 0 {
				key = rest[i+len(" b/"):]
			}
		}
		if key != "" {
			b.WriteString(line)
		}
	}
	flush()
	return out
}

// diffLine is one hunk line of a file patch. head is the 1-based line number
// in the post-change (head) file for a context or added line, 0 for a
// removed line.
type diffLine struct {
	op   byte
	text string
	head int
}

// hunkHeaderRe matches a unified-diff hunk header `@@ -a[,b] +c[,d] @@`.
var hunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseFilePatch reads one file section's hunks. Lines before the first
// hunk header (the `diff --git`, `index`, `---`, `+++` headers) are skipped;
// `\ No newline at end of file` markers are skipped; any other line inside
// a hunk that is not ' ', '+' or '-' is an error. An empty line inside a
// hunk reads as an empty context line.
func parseFilePatch(section string) ([]diffLine, error) {
	var out []diffLine
	inHunk := false
	head := 0
	for _, line := range strings.Split(strings.TrimSuffix(section, "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "@@") {
			m := hunkHeaderRe.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("malformed hunk header %q", line)
			}
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("malformed hunk header %q: %w", line, err)
			}
			inHunk, head = true, n
			continue
		}
		if !inHunk {
			continue
		}
		if line == "" {
			out = append(out, diffLine{op: ' ', head: head})
			head++
			continue
		}
		switch line[0] {
		case ' ', '+':
			out = append(out, diffLine{op: line[0], text: line[1:], head: head})
			head++
		case '-':
			out = append(out, diffLine{op: '-', text: line[1:]})
		case '\\':
		default:
			return nil, fmt.Errorf("unexpected patch line %q", line)
		}
	}
	return out, nil
}

// linePair parses one manifest line's (package, version) by ecosystem shape.
func linePair(ecosystem, text string) (PackageVersion, bool) {
	switch ecosystem {
	case EcosystemGo:
		return goLinePair(text)
	case EcosystemNPM:
		return pnpmKeyPair(text)
	}
	return PackageVersion{}, false
}

// goLinePair parses a go.mod require-entry SHAPE: after a trailing `//`
// comment is stripped and an optional leading `require` keyword dropped,
// exactly two fields, the second `v`-prefixed. A replace line
// (`a => b v`), an exclude single-line directive (three fields), and the
// go/toolchain directives never qualify.
func goLinePair(text string) (PackageVersion, bool) {
	if i := strings.Index(text, "//"); i >= 0 {
		text = text[:i]
	}
	f := strings.Fields(text)
	if len(f) > 0 && f[0] == "require" {
		f = f[1:]
	}
	if len(f) != 2 || !strings.HasPrefix(f[1], "v") {
		return PackageVersion{}, false
	}
	return PackageVersion{Package: f[0], Version: f[1]}, true
}

// pnpmKeyPair parses a pnpm-lock.yaml entry-key SHAPE: a line with exactly
// two leading spaces whose trimmed text is a mapping key `name@version:`
// (optionally quoted), with a v6 leading `/` and a peer suffix `(...)`
// stripped. A deeper-indented importer `version:` line never qualifies.
func pnpmKeyPair(text string) (PackageVersion, bool) {
	if len(text) < 3 || !strings.HasPrefix(text, "  ") || text[2] == ' ' || text[2] == '\t' {
		return PackageVersion{}, false
	}
	t := strings.TrimSpace(text)
	if !strings.HasSuffix(t, ":") {
		return PackageVersion{}, false
	}
	key := t[:len(t)-1]
	if n := len(key); n >= 2 && (key[0] == '\'' && key[n-1] == '\'' || key[0] == '"' && key[n-1] == '"') {
		key = key[1 : n-1]
	}
	return splitPnpmKey(key)
}

// splitPnpmKey normalizes a pnpm packages key (`/` prefix and peer suffix
// stripped) and splits it at the LAST '@' past index 0, so a scoped name
// keeps its leading '@'.
func splitPnpmKey(key string) (PackageVersion, bool) {
	key = strings.TrimPrefix(key, "/")
	if i := strings.IndexByte(key, '('); i >= 0 {
		key = key[:i]
	}
	at := strings.LastIndexByte(key, '@')
	if at <= 0 || at == len(key)-1 {
		return PackageVersion{}, false
	}
	return PackageVersion{Package: key[:at], Version: key[at+1:]}, true
}

// AddedVersions returns the (package, version) SHAPES on a file patch's
// added lines, in patch order, deduped: only '+' lines inside a hunk count,
// so a `+++` header never does. It is the shape layer only — it applies no
// require-directive, net-add or head check (DependencyChanges does). A
// patch that does not parse yields none. It never returns nil.
func AddedVersions(ecosystem, filePatch string) []PackageVersion {
	out := []PackageVersion{}
	lines, err := parseFilePatch(filePatch)
	if err != nil {
		return out
	}
	seen := map[PackageVersion]bool{}
	for _, l := range lines {
		if l.op != '+' {
			continue
		}
		if pv, ok := linePair(ecosystem, l.text); ok && !seen[pv] {
			seen[pv] = true
			out = append(out, pv)
		}
	}
	return out
}

// splitLines splits file content into lines, dropping the empty element a
// trailing newline leaves.
func splitLines(content string) []string {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// goModScan is a block-aware go.mod read. It returns the require entries by
// 1-based line number: `require m v` single lines and the lines of a
// `require ( ... )` block. Every other directive and block (replace,
// exclude, retract, tool, godebug, ...) is tracked and ignored. A malformed
// require entry, an unterminated or unmatched block, or a module required
// twice is an error.
func goModScan(lines []string) (map[int]PackageVersion, error) {
	out := map[int]PackageVersion{}
	seen := map[string]int{}
	record := func(n int, mod, ver string) error {
		if prev, dup := seen[mod]; dup {
			return fmt.Errorf("go.mod requires %s twice (lines %d and %d)", mod, prev, n)
		}
		seen[mod] = n
		out[n] = PackageVersion{Package: mod, Version: ver}
		return nil
	}
	block := ""
	for i, raw := range lines {
		n := i + 1
		t := raw
		if j := strings.Index(t, "//"); j >= 0 {
			t = t[:j]
		}
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if block != "" {
			if t == ")" {
				block = ""
				continue
			}
			if block != "require" {
				continue
			}
			f := strings.Fields(t)
			if len(f) != 2 || !strings.HasPrefix(f[1], "v") {
				return nil, fmt.Errorf("go.mod line %d: malformed require entry", n)
			}
			if err := record(n, f[0], f[1]); err != nil {
				return nil, err
			}
			continue
		}
		if t == ")" {
			return nil, fmt.Errorf("go.mod line %d: unmatched )", n)
		}
		if strings.HasSuffix(t, "(") {
			kw := strings.TrimSpace(strings.TrimSuffix(t, "("))
			if kw == "" || strings.ContainsAny(kw, " \t") {
				return nil, fmt.Errorf("go.mod line %d: malformed block", n)
			}
			block = kw
			continue
		}
		if f := strings.Fields(t); f[0] == "require" {
			if len(f) != 3 || !strings.HasPrefix(f[2], "v") {
				return nil, fmt.Errorf("go.mod line %d: malformed require directive", n)
			}
			if err := record(n, f[1], f[2]); err != nil {
				return nil, err
			}
		}
	}
	if block != "" {
		return nil, fmt.Errorf("go.mod: unterminated %s block", block)
	}
	return out, nil
}

// GoModRequires returns the go.mod's required module -> version, from
// require directives only (single-line and block). replace, exclude, retract
// and tool directives are ignored. Since Go 1.17 module graph pruning a
// go.mod lists every module providing a package to the main module's
// build, indirect ones included, so go.sum is not read.
func GoModRequires(content string) (map[string]string, error) {
	entries, err := goModScan(splitLines(content))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, pv := range entries {
		out[pv.Package] = pv.Version
	}
	return out, nil
}

// PnpmLockPackages returns the set of resolved (name, version) pairs keyed
// under a pnpm lockfile's top-level `packages:` mapping, decoded with a YAML
// parser (not the line scan). A lockfileVersion major below 6 (the
// `/name/version` key form) is an error, as are a missing lockfileVersion
// and a `packages` value that is not a mapping. Keys that do not split into
// name@version are skipped.
func PnpmLockPackages(content string) (map[PackageVersion]bool, error) {
	var doc struct {
		LockfileVersion yaml.Node `yaml:"lockfileVersion"`
		Packages        yaml.Node `yaml:"packages"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("pnpm lockfile: %w", err)
	}
	majorStr, _, _ := strings.Cut(doc.LockfileVersion.Value, ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return nil, fmt.Errorf("pnpm lockfile: unreadable lockfileVersion %q", doc.LockfileVersion.Value)
	}
	if major < pnpmMinLockfileMajor {
		return nil, fmt.Errorf("pnpm lockfile: lockfileVersion %q is below %d (unsupported key form)", doc.LockfileVersion.Value, pnpmMinLockfileMajor)
	}
	out := map[PackageVersion]bool{}
	switch doc.Packages.Kind {
	case 0:
		return out, nil
	case yaml.MappingNode:
	default:
		return nil, errors.New("pnpm lockfile: packages is not a mapping")
	}
	for i := 0; i+1 < len(doc.Packages.Content); i += 2 {
		if pv, ok := splitPnpmKey(doc.Packages.Content[i].Value); ok {
			out[pv] = true
		}
	}
	return out, nil
}

// pnpmScan is the line scan of a pnpm lockfile: the entry keys
// (pnpmKeyPair) of the top-level `packages:` section by 1-based line
// number. A top-level section starts at a column-0 line; only one that is
// exactly `packages:` opens the packages section.
func pnpmScan(lines []string) map[int]PackageVersion {
	out := map[int]PackageVersion{}
	inPackages := false
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if line != "" && line[0] != ' ' && line[0] != '\t' && line[0] != '#' {
			inPackages = strings.TrimSpace(line) == "packages:"
			continue
		}
		if !inPackages {
			continue
		}
		if pv, ok := pnpmKeyPair(line); ok {
			out[i+1] = pv
		}
	}
	return out
}

// DependencyChanges returns the (package, version) pairs a run's file patch
// INTRODUCES in the manifest at path, given that manifest's content at the
// run's head. An added pair counts only when ALL hold:
//
//  1. the patch applies to the head content: every context and added line
//     equals the head line its hunk header places it at (otherwise the
//     inputs disagree and it is an error);
//  2. its added line sits, in the head's own parse, INSIDE a require
//     directive (go.mod, single-line or block; exclude/replace/retract never
//     count) or as an entry key of the `packages:` section (pnpm-lock.yaml;
//     a snapshots or importers line never counts);
//  3. it is a NET add: neither a removed line of the same patch (a moved or
//     reshuffled line, a peer-suffix churn) nor an unchanged head line of
//     the same section carries the same pair — either means the base
//     already had it;
//  4. pnpm only: the YAML-decoded `packages` keys hold it
//     (PnpmLockPackages), which also refuses a lockfile below v6.
//
// Output is in head-line order, deduped, never nil. A path that is not a
// manifest ManifestEcosystem knows, an unparseable patch or manifest, or a
// go.mod requiring one module twice is an error. Every rule fails toward
// NO change.
func DependencyChanges(manifestPath, filePatch, headContent string) ([]DependencyChange, error) {
	eco, dir, ok := ManifestEcosystem(manifestPath)
	if !ok {
		return nil, fmt.Errorf("%s is not a manifest the in-flight match reads", manifestPath)
	}
	diff, err := parseFilePatch(filePatch)
	if err != nil {
		return nil, fmt.Errorf("%s patch: %w", manifestPath, err)
	}
	headLines := splitLines(headContent)
	var section map[int]PackageVersion
	var resolved map[PackageVersion]bool
	switch eco {
	case EcosystemGo:
		if section, err = goModScan(headLines); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
	case EcosystemNPM:
		if resolved, err = PnpmLockPackages(headContent); err != nil {
			return nil, fmt.Errorf("%s: %w", manifestPath, err)
		}
		section = pnpmScan(headLines)
	}

	added := map[int]bool{}
	base := map[PackageVersion]bool{}
	for _, l := range diff {
		if l.op == '-' {
			if pv, ok := linePair(eco, l.text); ok {
				base[pv] = true
			}
			continue
		}
		if l.head < 1 || l.head > len(headLines) || strings.TrimSuffix(headLines[l.head-1], "\r") != l.text {
			return nil, fmt.Errorf("%s: patch does not match the head content at line %d", manifestPath, l.head)
		}
		if l.op == '+' {
			added[l.head] = true
		}
	}
	for n, pv := range section {
		if !added[n] {
			base[pv] = true
		}
	}

	out := []DependencyChange{}
	seen := map[PackageVersion]bool{}
	for _, l := range diff {
		if l.op != '+' {
			continue
		}
		pv, ok := section[l.head]
		if !ok || base[pv] || seen[pv] {
			continue
		}
		if eco == EcosystemNPM && !resolved[pv] {
			continue
		}
		seen[pv] = true
		out = append(out, DependencyChange{Ecosystem: eco, Directory: dir, Manifest: path.Clean(manifestPath), Package: pv.Package, Version: pv.Version})
	}
	return out, nil
}

// InFlightAdvisory is one recorded advisory finding reduced to what the
// in-flight match and render read.
type InFlightAdvisory struct {
	// FindingID is the report's `advisory:<id>:<package>` finding id.
	FindingID string
	// IDs are the advisory identifiers; index 0 is the primary id.
	IDs       []string
	Ecosystem string
	Package   string
	// Version is the in-use version on the default branch.
	Version string
	// FixedVersion "" means no fix is published.
	FixedVersion string
	// Directories are the cited manifest directories
	// (plan.UpkeepAdvisoryManifestDirs).
	Directories  []string
	Reachability string
	Severity     string
	// CallPath is the advisory's call path. Index 0 is the vulnerable
	// dependency symbol; index >= 1 are CALLER frames naming this
	// repository's own code, which RenderInFlightFinding never renders.
	CallPath []DisclosureFrame
}

// InFlightMatch is one advisory and the run's changes it affects.
type InFlightMatch struct {
	Advisory InFlightAdvisory
	Changes  []DependencyChange
}

// MatchInFlight returns one InFlightMatch per advisory that at least one of
// the run's changes matches, in advisory input order, never nil. A change
// matches an advisory iff ALL hold:
//
//   - the ecosystem is equal and the package is equal (exact);
//   - the change's directory is one the advisory cites;
//   - the change's version is on the advisory's in-use version line
//     (sameVersionLine: same major, and for 0.x the same minor);
//   - with a fixed version: the change's version is comparable with it and
//     BELOW it; with no fix published: comparable with the in-use version
//     and AT OR ABOVE it (only versions at or past the known-affected one
//     on its line are presumed affected).
//
// Every rule fails toward NO match. A match's changes are deduped and
// sorted by directory, then manifest, then version.
func MatchInFlight(advisories []InFlightAdvisory, changes []DependencyChange) []InFlightMatch {
	out := []InFlightMatch{}
	for _, a := range advisories {
		var hit []DependencyChange
		seen := map[DependencyChange]bool{}
		for _, c := range changes {
			if !inFlightAffects(a, c) || seen[c] {
				continue
			}
			seen[c] = true
			hit = append(hit, c)
		}
		if len(hit) == 0 {
			continue
		}
		sort.Slice(hit, func(i, j int) bool {
			if hit[i].Directory != hit[j].Directory {
				return hit[i].Directory < hit[j].Directory
			}
			if hit[i].Manifest != hit[j].Manifest {
				return hit[i].Manifest < hit[j].Manifest
			}
			return hit[i].Version < hit[j].Version
		})
		out = append(out, InFlightMatch{Advisory: a, Changes: hit})
	}
	return out
}

// inFlightAffects is MatchInFlight's per-change rule.
func inFlightAffects(a InFlightAdvisory, c DependencyChange) bool {
	if c.Ecosystem != a.Ecosystem || c.Package != a.Package {
		return false
	}
	if !containsString(a.Directories, c.Directory) {
		return false
	}
	if !sameVersionLine(c.Version, a.Version) {
		return false
	}
	if a.FixedVersion != "" {
		atLeast, comparable := VersionAtLeast(c.Version, a.FixedVersion)
		return comparable && !atLeast
	}
	atLeast, comparable := VersionAtLeast(c.Version, a.Version)
	return comparable && atLeast
}

// containsString reports whether vs holds v.
func containsString(vs []string, v string) bool {
	for _, s := range vs {
		if s == v {
			return true
		}
	}
	return false
}

// inFlightSeverities are the report severities passed through.
var inFlightSeverities = map[string]bool{"high": true, "medium": true, "low": true}

// InFlightFindingSummary is the server-rendered one-line summary of an
// in-flight finding AND its dedupe key: it depends only on the primary
// advisory id, the ecosystem and the package, each a plain token or the
// withheld marker.
func InFlightFindingSummary(a InFlightAdvisory) string {
	primary := "advisory"
	if len(a.IDs) > 0 {
		primary = plainToken(idTokenRe, a.IDs[0])
	}
	return "Dependency advisory " + primary + " (" + plainToken(wordTokenRe, a.Ecosystem) + " " +
		plainToken(packageTokenRe, a.Package) + ") matches a version this run's dependency changes introduce"
}

// RenderInFlightFinding renders an in-flight finding from structured fields
// only: the summary (InFlightFindingSummary), a detail block, and the
// severity (the report's high|medium|low, else ""). Every value renders as
// a code span when it is a plain token for its field, else the withheld
// marker. The call path contributes ONLY its vulnerable symbol (index 0, as
// package.function, or the package alone) and a count of the caller frames
// omitted: caller frames name this repository's own code and are never
// rendered.
func RenderInFlightFinding(m InFlightMatch) (summary, detail, severity string) {
	a := m.Advisory
	fixed := "no fix published"
	if a.FixedVersion != "" {
		fixed = codeToken(versionTokenRe, a.FixedVersion)
	}
	var b strings.Builder
	b.WriteString("Advisory IDs: " + codeList(idTokenRe, a.IDs, "none") + "\n")
	b.WriteString("Ecosystem: " + codeToken(wordTokenRe, a.Ecosystem) + "\n")
	b.WriteString("Package: " + codeToken(packageTokenRe, a.Package) + "\n")
	b.WriteString("Introduced by this run:\n")
	for _, c := range m.Changes {
		b.WriteString("- " + codeToken(pathTokenRe, c.Directory) + " (" + codeToken(pathTokenRe, c.Manifest) + "): " +
			codeToken(versionTokenRe, c.Version) + "\n")
	}
	b.WriteString("Fixed version: " + fixed + "\n")
	b.WriteString("In use on the default branch: " + codeToken(versionTokenRe, a.Version) + "\n")
	b.WriteString("Reachability: " + codeToken(wordTokenRe, a.Reachability) + "\n")
	b.WriteString("Vulnerable symbol: " + vulnerableSymbol(a.CallPath) + "\n")
	if inFlightSeverities[a.Severity] {
		severity = a.Severity
	}
	return InFlightFindingSummary(a), b.String(), severity
}

// vulnerableSymbol renders call path frame 0 and the omitted caller count.
func vulnerableSymbol(frames []DisclosureFrame) string {
	if len(frames) == 0 {
		return "none recorded"
	}
	f := frames[0]
	sym := "not resolved below the module"
	switch {
	case f.Package != "" && f.Function != "":
		sym = codeToken(packageTokenRe, f.Package+"."+f.Function)
	case f.Package != "":
		sym = codeToken(packageTokenRe, f.Package)
	}
	if n := len(frames) - 1; n > 0 {
		sym += fmt.Sprintf(" (%d caller frame(s) omitted)", n)
	}
	return sym
}
