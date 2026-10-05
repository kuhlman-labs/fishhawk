package upkeep

// Dependabot coverage and advisory filing (#3750): the pure half of the
// upkeep scan's `advisory` source. MarkCovered decides which advisory
// findings an open Dependabot pull request already fixes, so the apply step
// skips filing them; RenderAdvisoryTitle and RenderAdvisoryFacts render an
// advisory filing from structured fields only, so agent prose never reaches
// the tracker. Like dedupe.go this file takes neutral inputs and imports no
// plan, forge or server package; the server adapts a validated report and a
// forge pull-request listing into them. Long-form contract: README.md
// § "Dependabot coverage and advisory filing (#3750)".

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// DependabotAuthor is the REST `user.login` of a pull request Dependabot
// opened. Only a pull request carrying exactly this author can cover a
// finding.
const DependabotAuthor = "dependabot[bot]"

// Advisory ecosystems. The values equal upkeep_report_v1's advisory
// `ecosystem` enum, so the server copies the report value verbatim.
const (
	EcosystemGo  = "go"
	EcosystemNPM = "npm"
)

// dependabotEcosystemPrefixes maps a Dependabot head-ref prefix to the
// advisory ecosystem it updates.
var dependabotEcosystemPrefixes = []struct {
	prefix    string
	ecosystem string
}{
	{"dependabot/go_modules/", EcosystemGo},
	{"dependabot/npm_and_yarn/", EcosystemNPM},
}

// AdvisoryProposal is one advisory finding reduced to what coverage reads.
type AdvisoryProposal struct {
	// FindingID is the report's `advisory:<id>:<package>` finding id.
	FindingID string
	// Ecosystem is EcosystemGo or EcosystemNPM.
	Ecosystem string
	// Package is the affected dependency: the Go MODULE path, or the npm
	// package name.
	Package string
	// Version is the in-use version the finding names. A bump covers only
	// when its From is on the SAME version line (sameVersionLine): a
	// lockfile can hold several versions of one package, and a bump of
	// another line leaves this one vulnerable. "" never covers.
	Version string
	// FixedVersion is the lowest fixed version; "" means no fix is published,
	// and such a finding is never covered.
	FixedVersion string
	// Directories are the directories of the finding's cited MANIFESTS ("."
	// for the repository root, else a slash path such as "backend"). Every
	// one must be covered; a finding with none is never covered.
	Directories []string
}

// PullRequest is one open pull request, reduced to what coverage reads.
type PullRequest struct {
	Number  int
	URL     string
	Title   string
	Body    string
	Author  string
	HeadRef string
	// BaseRef is the branch the pull request targets and DefaultBranch the
	// repository's default branch, both from the listing. A pull request
	// covers only when both are known and equal: the scan reads the default
	// branch, so a Dependabot `target-branch` pull request fixes another
	// tree.
	BaseRef       string
	DefaultBranch string
}

// Bump is one dependency update a Dependabot pull request makes.
type Bump struct {
	Package string
	From    string
	To      string
	// Directory is the updated manifest directory, normalized like
	// AdvisoryProposal.Directories. "" means the pull request does not name
	// exactly one directory, and such a bump never covers.
	Directory string
}

// CoveringPull is the pull request that covers one of a finding's
// directories.
type CoveringPull struct {
	Number    int    `json:"number"`
	URL       string `json:"url,omitempty"`
	Directory string `json:"directory"`
	BumpsTo   string `json:"bumps_to"`
}

// Covered marks one advisory finding as fixed by open Dependabot pull
// requests: one CoveringPull per cited manifest directory, in the finding's
// directory order.
type Covered struct {
	FindingID string         `json:"finding_id"`
	Package   string         `json:"package"`
	Pulls     []CoveringPull `json:"pulls"`
}

// DependabotEcosystem returns the advisory ecosystem a Dependabot head ref
// updates, or "" when the ref is not a recognized Dependabot branch.
func DependabotEcosystem(headRef string) string {
	for _, p := range dependabotEcosystemPrefixes {
		if strings.HasPrefix(headRef, p.prefix) {
			return p.ecosystem
		}
	}
	return ""
}

var (
	// commitPrefixRe strips a leading commit-style prefix such as
	// `deps(backend)(deps): ` or `chore!: `, and a `[Security] ` tag.
	commitPrefixRe = regexp.MustCompile(`^(?:\[[^\]]*\]\s*)?(?:[A-Za-z0-9_.-]+(?:\([^)]*\))*!?:\s*)?`)
	// singleBumpRe: `bump <pkg> from <a> to <b>[ in /<dir>]`.
	singleBumpRe = regexp.MustCompile(`(?i)^bump (\S+) from (\S+) to (\S+?)(?: in (/\S*))?$`)
	// titleDirRe: the `in /<dir>` of a multi-dependency or group title,
	// optionally followed by ` with N update(s)`.
	titleDirRe = regexp.MustCompile(`(?i) in (/\S*)(?: with \d+ updates?)?$`)
	// groupTitleRe: a group title, `bump the <g> group …`.
	groupTitleRe = regexp.MustCompile(`(?i)^bump the \S+ group\b`)
	// detailsRe: the first `<details>` block, where Dependabot embeds
	// upstream release notes, changelogs and commit lists.
	detailsRe = regexp.MustCompile(`(?i)<details`)
	// headingLineRe: a markdown ATX heading line.
	headingLineRe = regexp.MustCompile(`^#{1,6}(?:\s|$)`)
	// updatesLineRe: a body line `Updates `<pkg>` from <a> to <b>`.
	updatesLineRe = regexp.MustCompile("(?m)^\\s*Updates `([^`]+)` from (\\S+) to (\\S+?)\\.?\\s*$")
	// bumpsLineRe: a body line `Bumps [<pkg>](<url>) from <a> to <b>.`.
	bumpsLineRe = regexp.MustCompile(`(?m)^\s*Bumps \[([^\]]+)\]\([^)]*\) from (\S+) to (\S+?)\.?\s*$`)
)

// DependabotBumps parses the dependency updates a Dependabot pull request
// makes from its title and body. It never returns nil.
//
//   - A single-dependency title — `bump <pkg> from <a> to <b> in /<dir>`,
//     after any commit-style prefix, case-insensitive — yields exactly that
//     bump. A title with no `in /<dir>` yields an UNKNOWN directory: the
//     directory is not stated, so it is not guessed.
//   - Any other `bump …` title (a group or a multi-dependency update) yields
//     one bump per line `Updates `<pkg>` from <a> to <b>` or
//     `Bumps [<pkg>](<url>) from <a> to <b>.` in the body's LEADING SUMMARY
//     only (dependabotSummary), with the directory taken from the title's
//     `in /<dir>`. A line counts only when the title is a group form
//     (`bump the <g> group …`) or the title names its package as a word. A
//     title naming no `in /<dir>` — the `across N directories` group form,
//     even N = 1 — yields UNKNOWN directories.
//   - A title that is not a bump yields nothing.
//
// The summary-only read is a TRUST BOUNDARY, not a parse convenience: a
// Dependabot body embeds release notes, changelogs and commit lists written
// by the dependency's upstream maintainers, and an `Updates …` line inside
// them would otherwise cover a package the pull request does not update.
// The cost is fewer bumps for a grouped body (only the update lines ahead
// of the first embedded block are read), which fails toward NOT covered.
//
// The title and body formats are not a documented contract; every parse
// failure yields fewer bumps, which fails toward NOT covered.
func DependabotBumps(pr PullRequest) []Bump {
	out := []Bump{}
	title := strings.TrimSpace(pr.Title)
	title = strings.TrimSpace(title[len(commitPrefixRe.FindString(title)):])
	if m := singleBumpRe.FindStringSubmatch(title); m != nil {
		return append(out, Bump{Package: m[1], From: m[2], To: m[3], Directory: normalizeDependabotDir(m[4])})
	}
	if !strings.HasPrefix(strings.ToLower(title), "bump ") {
		return out
	}
	// A group spanning `across N directories` (even N = 1) carries no
	// `in /<dir>`, so its bumps keep an unknown directory.
	dir := ""
	if m := titleDirRe.FindStringSubmatch(title); m != nil {
		dir = normalizeDependabotDir(m[1])
	}
	group := groupTitleRe.MatchString(title)
	summary := dependabotSummary(pr.Body)
	for _, re := range []*regexp.Regexp{updatesLineRe, bumpsLineRe} {
		for _, m := range re.FindAllStringSubmatch(summary, -1) {
			if !group && !titleNamesPackage(title, m[1]) {
				continue
			}
			out = append(out, Bump{Package: m[1], From: m[2], To: m[3], Directory: dir})
		}
	}
	return out
}

// dependabotSummary returns the leading summary of a Dependabot pull-request
// body: the text before the first `<details>`, the first line starting
// `Release notes`, `Changelog` or `Commits` (case-insensitive), or the
// first markdown heading, whichever comes first. Everything from there on
// may carry upstream-authored text and is never read.
func dependabotSummary(body string) string {
	if loc := detailsRe.FindStringIndex(body); loc != nil {
		body = body[:loc[0]]
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		t := strings.TrimSpace(line)
		lt := strings.ToLower(t)
		if headingLineRe.MatchString(t) || strings.HasPrefix(lt, "release notes") ||
			strings.HasPrefix(lt, "changelog") || strings.HasPrefix(lt, "commits") {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// titleNamesPackage reports whether title carries pkg as one
// whitespace-separated word (a trailing comma trimmed).
func titleNamesPackage(title, pkg string) bool {
	for _, w := range strings.Fields(title) {
		if strings.TrimRight(w, ",") == pkg {
			return true
		}
	}
	return false
}

// normalizeDependabotDir turns a Dependabot `/<dir>` into the manifest
// directory form: "/" -> ".", "/backend" -> "backend". Anything that is not
// an absolute in-repository path is "" (unknown).
func normalizeDependabotDir(d string) string {
	if !strings.HasPrefix(d, "/") {
		return ""
	}
	c := path.Clean(d)
	if c == "/" {
		return "."
	}
	return strings.TrimPrefix(c, "/")
}

// MarkCovered returns, in input order, one Covered for each advisory finding
// open Dependabot pull requests already fix, and nothing for the rest. It
// never returns nil.
//
// A finding is covered iff it has a fixed version AND at least one manifest
// directory AND, for EVERY one of its directories, some pull request whose
// Author is DependabotAuthor, whose head ref names the finding's ecosystem,
// whose BaseRef is known and equals the known DefaultBranch, and that bumps
// the same package in that exact (known) directory FROM a version on the
// finding's in-use version line (sameVersionLine) TO a version
// VersionAtLeast reports comparable and at least the fixed version. The
// first such pull request, in input order, is recorded per directory.
//
// Every rule fails toward NOT covered: a covered finding is not filed, so a
// false cover would silently drop a real advisory.
func MarkCovered(advisories []AdvisoryProposal, prs []PullRequest) []Covered {
	type parsedPull struct {
		pr        PullRequest
		ecosystem string
		bumps     []Bump
	}
	pulls := make([]parsedPull, 0, len(prs))
	for _, pr := range prs {
		if pr.Author != DependabotAuthor {
			continue
		}
		eco := DependabotEcosystem(pr.HeadRef)
		if eco == "" {
			continue
		}
		if pr.BaseRef == "" || pr.BaseRef != pr.DefaultBranch {
			continue
		}
		pulls = append(pulls, parsedPull{pr: pr, ecosystem: eco, bumps: DependabotBumps(pr)})
	}

	out := []Covered{}
	for _, a := range advisories {
		if a.FixedVersion == "" || len(a.Directories) == 0 {
			continue
		}
		covering := make([]CoveringPull, 0, len(a.Directories))
		for _, dir := range a.Directories {
			if dir == "" {
				break
			}
			found := false
			for _, p := range pulls {
				if p.ecosystem != a.Ecosystem {
					continue
				}
				if b, ok := coveringBump(p.bumps, a, dir); ok {
					covering = append(covering, CoveringPull{Number: p.pr.Number, URL: p.pr.URL, Directory: dir, BumpsTo: b.To})
					found = true
					break
				}
			}
			if !found {
				break
			}
		}
		if len(covering) != len(a.Directories) {
			continue
		}
		out = append(out, Covered{FindingID: a.FindingID, Package: a.Package, Pulls: covering})
	}
	return out
}

// coveringBump returns the first bump of a's package in dir that moves a's
// in-use version line and reaches a's fixed version.
func coveringBump(bumps []Bump, a AdvisoryProposal, dir string) (Bump, bool) {
	for _, b := range bumps {
		if b.Package != a.Package || b.Directory != dir {
			continue
		}
		if !sameVersionLine(b.From, a.Version) {
			continue
		}
		if atLeast, comparable := VersionAtLeast(b.To, a.FixedVersion); atLeast && comparable {
			return b, true
		}
	}
	return Bump{}, false
}

// sameVersionLine reports whether from and inUse are on the same version
// line: the same major, and for a 0.x major also the same minor (semver
// §4: 0.y.z makes no compatibility promise across minors). Either side not
// parsing is false, so an unknown line never covers.
func sameVersionLine(from, inUse string) bool {
	f, ok := parseSemver(from)
	if !ok {
		return false
	}
	u, ok := parseSemver(inUse)
	if !ok {
		return false
	}
	if f.core[0] != u.core[0] {
		return false
	}
	return f.core[0] != 0 || f.core[1] == u.core[1]
}

// semver is a parsed MAJOR.MINOR.PATCH[-PRERELEASE] version. Build metadata
// is dropped at parse time.
type semver struct {
	core [3]uint64
	pre  []string
}

// parseSemver parses v with an optional leading `v`. Go pseudo-versions
// (vX.0.0-yyyymmddhhmmss-abcdef123456, vX.Y.Z-pre.0.yyyymmddhhmmss-…,
// vX.Y.(Z+1)-0.yyyymmddhhmmss-…) are valid semver with a prerelease and
// parse. Ranges, partial versions, leading zeros and empty identifiers do
// not.
func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	coreStr, preStr, hasPre := strings.Cut(v, "-")
	parts := strings.Split(coreStr, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var s semver
	for i, p := range parts {
		if !isNumericIdent(p) {
			return semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return semver{}, false
		}
		s.core[i] = n
	}
	if !hasPre {
		return s, true
	}
	for _, id := range strings.Split(preStr, ".") {
		if id == "" || strings.Trim(id, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-") != "" {
			return semver{}, false
		}
		if isAllDigits(id) && !isNumericIdent(id) {
			return semver{}, false
		}
		s.pre = append(s.pre, id)
	}
	return s, true
}

// isAllDigits reports whether s is non-empty and only ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isNumericIdent reports whether s is a semver numeric identifier: digits
// with no leading zero (other than "0" itself).
func isNumericIdent(s string) bool {
	return isAllDigits(s) && (s == "0" || s[0] != '0')
}

// comparePrereleaseIdent orders two prerelease identifiers by semver §11.4:
// numeric identifiers numerically, alphanumeric ones in ASCII order, and a
// numeric identifier below an alphanumeric one.
func comparePrereleaseIdent(a, b string) int {
	an, bn := isAllDigits(a), isAllDigits(b)
	switch {
	case an && bn:
		// No leading zeros (parseSemver refuses them), so the longer
		// number is the larger; equal lengths compare lexically. This
		// never overflows, unlike a ParseUint of a long identifier.
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// compareSemver orders a and b by semver §11 precedence.
func compareSemver(a, b semver) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			if a.core[i] < b.core[i] {
				return -1
			}
			return 1
		}
	}
	// A release outranks every prerelease of the same core.
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePrereleaseIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	// Equal so far: the shorter identifier set is the lower.
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// VersionAtLeast reports whether have >= want by semver §11 precedence
// (https://semver.org/#spec-item-11), with a leading `v` trimmed and build
// metadata ignored. comparable is false when either side does not parse — a
// range, a partial version, an empty string — and the caller must then treat
// the comparison as unknown (MarkCovered: NOT covered). Go pseudo-versions
// are comparable: they are semver prereleases.
func VersionAtLeast(have, want string) (atLeast, comparable bool) {
	h, ok := parseSemver(have)
	if !ok {
		return false, false
	}
	w, ok := parseSemver(want)
	if !ok {
		return false, false
	}
	return compareSemver(h, w) >= 0, true
}

// AdvisoryFacts are the structured fields of an advisory finding the server
// renders a filing from. Every value is agent-asserted (copied from the
// scanner output) but bounded by upkeep_report_v1; the renderers add a
// second bound — a value that is not a plain token is withheld — so no free
// text reaches the tracker through a structured field.
type AdvisoryFacts struct {
	// IDs are the advisory identifiers; index 0 is the primary id.
	IDs       []string
	Ecosystem string
	Package   string
	Version   string
	// FixedVersion "" renders as "no fix published".
	FixedVersion string
	Reachability string
	Severity     string
	// Manifests are the cited manifest file paths.
	Manifests []string
}

// advisoryTitleMax is GitHub's issue-title length limit.
const advisoryTitleMax = 256

// withheldFact replaces a structured value that is not a plain token.
const withheldFact = "(withheld: not a plain token)"

var (
	// idTokenRe mirrors upkeep_report_v1's advisory id pattern.
	idTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$`)
	// packageTokenRe admits Go module paths and npm package names.
	packageTokenRe = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._~/@+-]{0,213}$`)
	// versionTokenRe admits semver, Go pseudo-versions and other bare
	// version strings; no spaces, slashes or markdown.
	versionTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	// wordTokenRe admits the enum values (ecosystem, reachability,
	// severity).
	wordTokenRe = regexp.MustCompile(`^[a-z_]{1,32}$`)
	// pathTokenRe admits repository-relative manifest paths.
	pathTokenRe = regexp.MustCompile(`^[A-Za-z0-9._@+-][A-Za-z0-9._/@+-]{0,511}$`)
)

// plainToken returns v when it matches re, else the withheld marker.
func plainToken(re *regexp.Regexp, v string) string {
	if re.MatchString(v) {
		return v
	}
	return withheldFact
}

// codeToken renders v as an inline code span when it is a plain token. The
// token charsets exclude the backtick, so a span cannot be broken out of,
// and markdown, mentions and issue references inside a span are inert.
func codeToken(re *regexp.Regexp, v string) string {
	if re.MatchString(v) {
		return "`" + v + "`"
	}
	return withheldFact
}

// codeList renders each value as a code span, comma-separated, or none when
// empty.
func codeList(re *regexp.Regexp, vs []string, none string) string {
	if len(vs) == 0 {
		return none
	}
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, codeToken(re, v))
	}
	return strings.Join(parts, ", ")
}

// RenderAdvisoryTitle renders an advisory filing's issue title from
// structured fields only: `<primary id>: <package> <version> (<severity>
// severity)`, capped at GitHub's 256-character title limit. A value that is
// not a plain token is withheld.
func RenderAdvisoryTitle(f AdvisoryFacts) string {
	primary := "advisory"
	if len(f.IDs) > 0 {
		primary = plainToken(idTokenRe, f.IDs[0])
	}
	title := primary + ": " + plainToken(packageTokenRe, f.Package) + " " +
		plainToken(versionTokenRe, f.Version) + " (" + plainToken(wordTokenRe, f.Severity) + " severity)"
	if len(title) > advisoryTitleMax {
		// Every token is ASCII, so a byte cut is a character cut.
		title = title[:advisoryTitleMax]
	}
	return title
}

// RenderAdvisoryFacts renders the server-rendered facts block of an advisory
// filing: a `### Advisory facts (server-rendered)` heading and one line per
// field, with `Fixed version: no fix published` when FixedVersion is "". It
// deliberately takes no call path — caller frames name the repository's own
// code and are never filed — and no agent prose.
func RenderAdvisoryFacts(f AdvisoryFacts) string {
	fixed := "no fix published"
	if f.FixedVersion != "" {
		fixed = codeToken(versionTokenRe, f.FixedVersion)
	}
	var b strings.Builder
	b.WriteString("### Advisory facts (server-rendered)\n\n")
	b.WriteString("- Advisory IDs: " + codeList(idTokenRe, f.IDs, "none") + "\n")
	b.WriteString("- Ecosystem: " + codeToken(wordTokenRe, f.Ecosystem) + "\n")
	b.WriteString("- Package: " + codeToken(packageTokenRe, f.Package) + "\n")
	b.WriteString("- In-use version: " + codeToken(versionTokenRe, f.Version) + "\n")
	b.WriteString("- Fixed version: " + fixed + "\n")
	b.WriteString("- Reachability: " + codeToken(wordTokenRe, f.Reachability) + "\n")
	b.WriteString("- Severity: " + codeToken(wordTokenRe, f.Severity) + "\n")
	b.WriteString("- Manifests: " + codeList(pathTokenRe, f.Manifests, "none cited") + "\n")
	return b.String()
}

// DisclosureFrame is one call-path frame reduced to the tokens that could
// disclose the repository's own call path.
type DisclosureFrame struct {
	Package  string
	Function string
	Receiver string
	Filename string
}

// CallPathDisclosure reports whether body quotes a CALLER frame of the call
// path, returning the first token found. Frames at index >= 1 are callers;
// index 0 is the vulnerable dependency symbol, already public in the
// advisory, and is never a token. Per frame the tokens are a non-empty
// Filename, `Package.Function`, and `Receiver.Function` (a leading `*`
// trimmed) when both parts are set. Matching is a case-sensitive substring
// test, so a paraphrase evades it.
//
// It is an UNWIRED helper: no production path calls it. The disclosure
// control is the server-rendered advisory filing, which files no agent
// prose at all, so there is no agent text for it to scan; it is kept for a
// future surface that must screen agent prose (approval condition 1 permits
// keeping or dropping it).
func CallPathDisclosure(body string, frames []DisclosureFrame) (string, bool) {
	for i := 1; i < len(frames); i++ {
		f := frames[i]
		recv := strings.TrimPrefix(f.Receiver, "*")
		tokens := []string{f.Filename}
		if f.Package != "" && f.Function != "" {
			tokens = append(tokens, f.Package+"."+f.Function)
		}
		if recv != "" && f.Function != "" {
			tokens = append(tokens, recv+"."+f.Function)
		}
		for _, t := range tokens {
			if t != "" && strings.Contains(body, t) {
				return t, true
			}
		}
	}
	return "", false
}
