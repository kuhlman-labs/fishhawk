package upkeep

// Toolchain pin drift (#3922): the deterministic detector the upkeep scan's
// toolchain_drift source runs over the pin-bearing files the server read at the
// run's recorded base commit. It is pure — the caller hands it path → content,
// and it reads nothing itself.

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// The pin families. A family string is the finding SUBJECT, so a drift finding's
// id is `toolchain_drift:<family>`, which the upkeep_report_v1 id pattern
// (`^(flake|toolchain_drift|deprecation):[^\s<>]+$`) accepts for all three.
const (
	// PinFamilyGo is the Go language version: the go.work / go.mod `go`
	// directive, the root golangci config's run.go, and literal workflow
	// go-version values. Grouped on MAJOR.MINOR.
	PinFamilyGo = "go"
	// PinFamilyGolangciLint is the golangci-lint release a workflow installs:
	// the install.sh URL tag and the version argument handed to it. Exact.
	PinFamilyGolangciLint = "golangci-lint"
	// PinFamilyRedocly is the @redocly/cli version pinned in an npx command,
	// in a workflow, the root AGENTS.md or a docs/api/ file. Exact.
	PinFamilyRedocly = "@redocly/cli"
)

// PinMaxOccurrences caps the occurrences one PinDrift carries. The rest are
// counted in OmittedOccurrences, so a truncated finding says so. The cap keeps
// every distinct value AND at least two distinct paths: every value keeps an
// occurrence unless the values alone fill the cap from one path, when the
// latest yields its slot to a second path (see capPinHits).
const PinMaxOccurrences = 32

// PinOccurrence is one pin read from one line of one file.
type PinOccurrence struct {
	// Path is the repo-relative path exactly as the caller keyed it.
	Path string
	// Line is the 1-based line number.
	Line int
	// Value is the pinned literal as written, UNQUOTED: a YAML `'1.25'` or
	// `"1.22"` scalar yields 1.25 / 1.22. For the go family it is the raw
	// literal (1.25.0 stays 1.25.0); grouping uses the normalized MAJOR.MINOR.
	Value string
}

// PinDrift is one family whose pins disagree.
type PinDrift struct {
	// Family is one of the PinFamily* constants.
	Family string
	// Occurrences are the family's pins, sorted by (Path, Line, Value), at most
	// PinMaxOccurrences of them.
	Occurrences []PinOccurrence
	// OmittedOccurrences counts the occurrences past the cap.
	OmittedOccurrences int
	// DistinctValues are the family's distinct GROUPING keys, sorted: the
	// normalized MAJOR.MINOR for go, the exact value otherwise. Always two or
	// more. Computed over every occurrence, including the omitted ones.
	DistinctValues []string
}

var (
	// goDirectiveRE matches the column-0 `go` directive of a go.mod / go.work.
	goDirectiveRE = regexp.MustCompile(`^go[ \t]+(\S+)`)
	// goVersionRE is the only go-version shape the go family accepts:
	// MAJOR.MINOR or MAJOR.MINOR.PATCH. Anything else — `stable`, `1.25.x`,
	// `>=1.25`, a `${{ matrix.go }}` expression, a flow or block list — is not
	// a single pin and contributes NO occurrence (see normalizeGoVersion).
	goVersionRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.[0-9]+)?$`)
	// workflowGoVersionRE matches a `go-version:` key (never
	// `go-version-file:`), optionally as a block-sequence entry.
	workflowGoVersionRE = regexp.MustCompile(`^[ \t]*(?:-[ \t]+)?go-version:(?:[ \t]+(.*))?$`)
	// golangciRunKeyRE matches the top-level `run:` key of a golangci config.
	golangciRunKeyRE = regexp.MustCompile(`^run:[ \t]*(?:#.*)?$`)
	// golangciGoKeyRE matches a `go:` key with its leading indent removed.
	golangciGoKeyRE = regexp.MustCompile(`^go:(?:[ \t]+(.*))?$`)
	// golangciInstallRE matches the tag in golangci-lint's install.sh URL.
	golangciInstallRE = regexp.MustCompile(`golangci-lint/(v[0-9]+\.[0-9]+\.[0-9]+)/install\.sh`)
	// semverTagRE matches a vX.Y.Z token.
	semverTagRE = regexp.MustCompile(`\bv[0-9]+\.[0-9]+\.[0-9]+\b`)
	// redoclyRE matches an exact @redocly/cli@X.Y.Z pin.
	redoclyRE = regexp.MustCompile(`@redocly/cli@([0-9]+\.[0-9]+\.[0-9]+)\b`)
)

// shArgMarker introduces the arguments piped into an install script.
const shArgMarker = "sh -s --"

// pinHit is one occurrence with its family and grouping key.
type pinHit struct {
	family string
	key    string
	occ    PinOccurrence
}

// DetectPinDrift returns one PinDrift per family whose pins disagree, sorted by
// Family. It never returns nil: no drift is a non-nil empty slice.
//
// files maps a repo-relative path to its content. A path is read only if it
// belongs to a family's file set; every other entry is ignored:
//
//   - go: `go.work`, a root `go.mod` and any `*/go.mod` (the column-0 `go`
//     directive); the ROOT `.golangci.yml` / `.golangci.yaml` (the `go:` key
//     directly under the top-level `run:` block); `.github/workflows/*.yml|
//     *.yaml` (a literal `go-version:` value — `go-version-file:` is ignored).
//     Values are grouped on MAJOR.MINOR, so 1.25 and 1.25.0 agree. A value
//     that is not MAJOR.MINOR[.PATCH] is NOT a pin and is skipped.
//   - golangci-lint: workflow files only — the `golangci-lint/vX.Y.Z/install.sh`
//     tag, and the LAST vX.Y.Z after `sh -s --` on a line that, or whose
//     previous non-comment line, mentions golangci-lint. Exact.
//   - @redocly/cli: `@redocly/cli@X.Y.Z` in workflow files, the root AGENTS.md
//     and any file directly under docs/api/. Exact.
//
// In YAML files a line whose first non-space rune is `#` is skipped: a
// commented-out pin is not executed. Markdown lines are all scanned — a
// command in prose is the command people run.
//
// The map is iterated in sorted key order and every output list is sorted, so
// the same input always yields deep-equal output.
func DetectPinDrift(files map[string]string) []PinDrift {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	byFamily := map[string][]pinHit{}
	for _, p := range paths {
		for _, h := range scanPinFile(p, files[p]) {
			byFamily[h.family] = append(byFamily[h.family], h)
		}
	}

	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	sort.Strings(families)

	out := make([]PinDrift, 0, len(families))
	for _, fam := range families {
		hits := sortDedupePinHits(byFamily[fam])
		keys := distinctPinKeys(hits)
		if !pinFamilyDrifts(keys) {
			continue
		}
		kept, omitted := capPinHits(hits, PinMaxOccurrences)
		occs := make([]PinOccurrence, len(kept))
		for i, h := range kept {
			occs[i] = h.occ
		}
		out = append(out, PinDrift{
			Family:             fam,
			Occurrences:        occs,
			OmittedOccurrences: omitted,
			DistinctValues:     keys,
		})
	}
	return out
}

// pinFamilyDrifts is the drift predicate: a family drifts when its pins carry
// more than one distinct grouping key.
func pinFamilyDrifts(distinctKeys []string) bool {
	return len(distinctKeys) > 1
}

// scanPinFile returns every pin p's content carries, in line order.
func scanPinFile(p, content string) []pinHit {
	clean := path.Clean(p)
	workflow := isWorkflowFile(clean)
	goMod := isGoModFile(clean)
	golangci := isGolangciConfig(clean)
	redocly := workflow || isRedoclyDoc(clean)
	if !workflow && !goMod && !golangci && !redocly {
		return nil
	}
	yaml := isYAMLFile(clean)

	var hits []pinHit
	add := func(family, key, value string, line int) {
		hits = append(hits, pinHit{family: family, key: key, occ: PinOccurrence{Path: p, Line: line, Value: value}})
	}
	addGo := func(raw string, line int) {
		if key, ok := normalizeGoVersion(raw); ok {
			add(PinFamilyGo, key, raw, line)
		}
	}

	run := golangciRunTracker{childIndent: -1}
	prev := ""
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		n := i + 1
		if yaml && isYAMLComment(line) {
			continue
		}

		if goMod {
			if m := goDirectiveRE.FindStringSubmatch(line); m != nil {
				v, _, _ := strings.Cut(m[1], "//")
				addGo(v, n)
			}
		}
		if golangci {
			if v, ok := run.feed(line); ok {
				addGo(v, n)
			}
		}
		if workflow {
			if m := workflowGoVersionRE.FindStringSubmatch(line); m != nil {
				addGo(yamlScalar(m[1]), n)
			}
			for _, m := range golangciInstallRE.FindAllStringSubmatch(line, -1) {
				add(PinFamilyGolangciLint, m[1], m[1], n)
			}
			if v, ok := golangciShArg(line, prev); ok {
				add(PinFamilyGolangciLint, v, v, n)
			}
		}
		if redocly {
			for _, m := range redoclyRE.FindAllStringSubmatch(line, -1) {
				add(PinFamilyRedocly, m[1], m[1], n)
			}
		}

		if strings.TrimSpace(line) != "" {
			prev = line
		}
	}
	return hits
}

// golangciShArg returns the last vX.Y.Z after `sh -s --` on line, when line or
// the previous non-blank, non-comment line mentions golangci-lint. The context
// requirement keeps another tool's `curl … | sh -s -- vX.Y.Z` installer from
// being read as a golangci-lint pin.
func golangciShArg(line, prev string) (string, bool) {
	idx := strings.Index(line, shArgMarker)
	if idx < 0 {
		return "", false
	}
	if !strings.Contains(line, "golangci-lint") && !strings.Contains(prev, "golangci-lint") {
		return "", false
	}
	tags := semverTagRE.FindAllString(line[idx+len(shArgMarker):], -1)
	if len(tags) == 0 {
		return "", false
	}
	return tags[len(tags)-1], true
}

// golangciRunTracker follows a golangci config line by line and reports the
// `go:` key sitting DIRECTLY under the top-level `run:` block. The block ends
// at the next column-0 key; comment lines never reach it (skipped first).
type golangciRunTracker struct {
	inRun       bool
	childIndent int // indent of the run block's first key; -1 until seen
}

func (g *golangciRunTracker) feed(line string) (string, bool) {
	if strings.TrimSpace(line) == "" {
		return "", false
	}
	body := strings.TrimLeft(line, " \t")
	indent := len(line) - len(body)
	if indent == 0 {
		g.inRun = golangciRunKeyRE.MatchString(line)
		g.childIndent = -1
		return "", false
	}
	if !g.inRun {
		return "", false
	}
	if g.childIndent < 0 {
		g.childIndent = indent
	}
	if indent != g.childIndent {
		return "", false
	}
	m := golangciGoKeyRE.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return yamlScalar(m[1]), true
}

// normalizeGoVersion returns the MAJOR.MINOR grouping key of a go version
// literal, or ok=false when the literal is not MAJOR.MINOR[.PATCH]. An
// unnormalizable literal (`stable`, `1.25.x`, `>=1.25`, an expression, a list)
// is a floating or multi-valued selector, not a pin: it is skipped rather than
// compared, so it can neither raise nor hide a drift.
func normalizeGoVersion(v string) (string, bool) {
	m := goVersionRE.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	return m[1] + "." + m[2], true
}

// yamlScalar returns the UNQUOTED value of a single-line YAML scalar: the
// contents of a '…' or "…" quoted scalar, otherwise the text before a ` #`
// comment, trimmed. A flow collection (`[…]`, `{…}`) comes back as written and
// fails every family's value shape.
func yamlScalar(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if q := s[0]; q == '\'' || q == '"' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : 1+end]
		}
		return s
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "\t#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// sortDedupePinHits sorts hits by (Path, Line, Value) and drops exact
// duplicates — one line naming the same value twice (an install.sh tag and its
// matching sh argument on a single line) is one occurrence.
func sortDedupePinHits(hits []pinHit) []pinHit {
	sorted := make([]pinHit, len(hits))
	copy(sorted, hits)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].occ, sorted[j].occ
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Value < b.Value
	})
	out := sorted[:0]
	for i, h := range sorted {
		if i > 0 && h.occ == sorted[i-1].occ {
			continue
		}
		out = append(out, h)
	}
	return out
}

// distinctPinKeys returns the sorted distinct grouping keys of hits.
func distinctPinKeys(hits []pinHit) []string {
	seen := map[string]bool{}
	keys := []string{}
	for _, h := range hits {
		if !seen[h.key] {
			seen[h.key] = true
			keys = append(keys, h.key)
		}
	}
	sort.Strings(keys)
	return keys
}

// capPinHits keeps at most limit of the sorted hits and returns how many it
// dropped. A finding must show the disagreement AND name at least two distinct
// paths (upkeep_report_v1 semantic rule (g)), so the slots are filled in tiers,
// each in hit order:
//
//	(a) one occurrence per distinct grouping key, so every disagreeing value
//	    keeps a path:line;
//	(b) when the slots kept so far cover fewer than two paths while the input
//	    spans two or more, one occurrence from an uncovered path — added if a
//	    slot is free, otherwise REPLACING the latest-added tier-(a) slot;
//	(c) one occurrence per distinct (key, path) pair;
//	(d) the remaining hits.
//
// The bound tier (b) trades away: a value can lose its only occurrence ONLY
// when the distinct values alone fill every slot from a single path, and then
// exactly one value (the latest in hit order) yields its slot so the finding
// keeps its two-path minimum. DistinctValues, computed over every occurrence,
// still names it. The kept hits are returned in the input's (sorted) order.
func capPinHits(hits []pinHit, limit int) ([]pinHit, int) {
	if len(hits) <= limit {
		return hits, 0
	}
	keep := make([]bool, len(hits))
	n := 0
	covered := map[string]bool{}
	mark := func(i int) {
		keep[i] = true
		n++
		covered[hits[i].occ.Path] = true
	}

	// (a) one per distinct key.
	lastA := -1
	seenKey := map[string]bool{}
	for i, h := range hits {
		if n == limit {
			break
		}
		if !seenKey[h.key] {
			seenKey[h.key] = true
			mark(i)
			lastA = i
		}
	}

	// (b) the two-path minimum. A limit below 2 cannot hold two paths, and a
	// replaced slot leaves its path covered: tier (a) put every kept slot on
	// that one path, and limit >= 2 slots are kept.
	if limit >= 2 && len(covered) < 2 && distinctPinPaths(hits) >= 2 {
		for i, h := range hits {
			if covered[h.occ.Path] {
				continue
			}
			if n == limit {
				keep[lastA] = false
				n--
			}
			mark(i)
			break
		}
	}

	// (c) one per distinct (key, path) pair not already kept.
	type pinPair struct{ key, path string }
	seenPair := map[pinPair]bool{}
	for i, h := range hits {
		if keep[i] {
			seenPair[pinPair{h.key, h.occ.Path}] = true
		}
	}
	for i, h := range hits {
		if n == limit {
			break
		}
		p := pinPair{h.key, h.occ.Path}
		if !keep[i] && !seenPair[p] {
			seenPair[p] = true
			mark(i)
		}
	}

	// (d) fill.
	for i := range hits {
		if n == limit {
			break
		}
		if !keep[i] {
			mark(i)
		}
	}

	kept := make([]pinHit, 0, n)
	for i, h := range hits {
		if keep[i] {
			kept = append(kept, h)
		}
	}
	return kept, len(hits) - n
}

// distinctPinPaths counts the distinct paths hits name.
func distinctPinPaths(hits []pinHit) int {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.occ.Path] = true
	}
	return len(seen)
}

// isGoModFile reports go.work, a root go.mod, or any nested go.mod.
func isGoModFile(p string) bool {
	return p == "go.work" || p == "go.mod" || strings.HasSuffix(p, "/go.mod")
}

// isGolangciConfig reports the ROOT golangci config, in either extension.
func isGolangciConfig(p string) bool {
	return p == ".golangci.yml" || p == ".golangci.yaml"
}

// isWorkflowFile reports a YAML file directly under .github/workflows/.
func isWorkflowFile(p string) bool {
	return path.Dir(p) == ".github/workflows" && isYAMLFile(p)
}

// isRedoclyDoc reports the root AGENTS.md or a file directly under docs/api/.
func isRedoclyDoc(p string) bool {
	return p == "AGENTS.md" || path.Dir(p) == "docs/api"
}

func isYAMLFile(p string) bool {
	ext := path.Ext(p)
	return ext == ".yml" || ext == ".yaml"
}

// isYAMLComment reports a line whose first non-space rune is `#`.
func isYAMLComment(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " \t"), "#")
}
