package upkeep_test

import (
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// golangciInstallStep is a real-repo-shaped workflow step installing
// golangci-lint at tag (lines 5 and 6 carry the URL tag and the sh argument).
func golangciInstallStep(tag string) string {
	return "jobs:\n" +
		"  lint:\n" +
		"    steps:\n" +
		"      - run: |\n" +
		"          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/" + tag + "/install.sh \\\n" +
		"            | sh -s -- -b \"$(go env GOPATH)/bin\" " + tag + "\n"
}

// findFamily returns the drift for family, or fails the test.
func findFamily(t *testing.T, drifts []upkeep.PinDrift, family string) upkeep.PinDrift {
	t.Helper()
	for _, d := range drifts {
		if d.Family == family {
			return d
		}
	}
	t.Fatalf("no %s drift in %+v", family, drifts)
	return upkeep.PinDrift{}
}

// TestDetectPinDrift_GolangciInstallTagAcrossTwoWorkflows (issue AC1): two
// workflows installing v2.8.0 and v2.9.0 yield exactly ONE golangci-lint
// finding listing every path:line:value.
//
// Counterfactual: the drift predicate (pinFamilyDrifts) returning false drops
// the finding — the fixture's two tags only become a finding through it.
func TestDetectPinDrift_GolangciInstallTagAcrossTwoWorkflows(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml":              golangciInstallStep("v2.8.0"),
		".github/workflows/backend-release.yml": golangciInstallStep("v2.9.0"),
	}
	got := upkeep.DetectPinDrift(files)
	want := []upkeep.PinDrift{{
		Family: upkeep.PinFamilyGolangciLint,
		Occurrences: []upkeep.PinOccurrence{
			{Path: ".github/workflows/backend-release.yml", Line: 5, Value: "v2.9.0"},
			{Path: ".github/workflows/backend-release.yml", Line: 6, Value: "v2.9.0"},
			{Path: ".github/workflows/ci.yml", Line: 5, Value: "v2.8.0"},
			{Path: ".github/workflows/ci.yml", Line: 6, Value: "v2.8.0"},
		},
		DistinctValues: []string{"v2.8.0", "v2.9.0"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DetectPinDrift =\n%+v\nwant\n%+v", got, want)
	}
}

// realRepoGoFiles is the repository's own go-family shape: go.work 1.25.0,
// cli/go.mod 1.25, backend/go.mod 1.25.0 and ci.yml go-version '1.25'.
func realRepoGoFiles() map[string]string {
	return map[string]string{
		"go.work":        "// workspace\n\ngo 1.25.0\n\nuse (\n\t./backend\n\t./cli\n)\n",
		"cli/go.mod":     "module github.com/kuhlman-labs/fishhawk/cli\n\ngo 1.25\n",
		"backend/go.mod": "module github.com/kuhlman-labs/fishhawk/backend\n\ngo 1.25.0\n\ntoolchain go1.25.6\n",
		".github/workflows/ci.yml": "jobs:\n  go:\n    steps:\n" +
			"      - uses: actions/setup-go@v5\n" +
			"        with:\n" +
			"          go-version: '1.25'\n",
	}
}

// TestDetectPinDrift_GoMajorMinor: a .golangci.yml run.go "1.22" drifts from
// the 1.25 family and the finding names that line; the companion fixture of
// only 1.25 / 1.25.0 / '1.25' yields NO drift.
//
// Counterfactual: grouping on the raw value instead of MAJOR.MINOR makes 1.25
// and 1.25.0 two keys, so the companion fixture drifts spuriously — RED.
func TestDetectPinDrift_GoMajorMinor(t *testing.T) {
	agree := realRepoGoFiles()
	if got := upkeep.DetectPinDrift(agree); len(got) != 0 {
		t.Fatalf("1.25 / 1.25.0 / '1.25' must agree, got drift %+v", got)
	}

	drifting := realRepoGoFiles()
	drifting[".golangci.yml"] = "version: \"2\"\n\nrun:\n  timeout: 5m\n  go: \"1.22\"\n\nlinters:\n  default: standard\n"
	got := upkeep.DetectPinDrift(drifting)
	d := findFamily(t, got, upkeep.PinFamilyGo)
	if want := []string{"1.22", "1.25"}; !reflect.DeepEqual(d.DistinctValues, want) {
		t.Errorf("DistinctValues = %v, want %v", d.DistinctValues, want)
	}
	wantOccs := []upkeep.PinOccurrence{
		{Path: ".github/workflows/ci.yml", Line: 6, Value: "1.25"},
		{Path: ".golangci.yml", Line: 5, Value: "1.22"},
		{Path: "backend/go.mod", Line: 3, Value: "1.25.0"},
		{Path: "cli/go.mod", Line: 3, Value: "1.25"},
		{Path: "go.work", Line: 3, Value: "1.25.0"},
	}
	if !reflect.DeepEqual(d.Occurrences, wantOccs) {
		t.Errorf("Occurrences =\n%+v\nwant\n%+v", d.Occurrences, wantOccs)
	}
}

// upkeepFactCharset is the prompt renderer's fact charset (approach step 5:
// upkeepFact renders a value only if it matches this, else withholds it). The
// renderer itself lives in backend/internal/prompt, which this package does
// not import; the DetectPinDrift → renderer round trip is asserted server-side.
var upkeepFactCharset = regexp.MustCompile(`^[A-Za-z0-9._/@:+-]{1,256}$`)

// TestDetectPinDrift_QuotedPinsYieldUnquotedValues (approval condition 1): the
// real repo's quoted go pins — ci.yml '1.25' and .golangci.yml "1.22" — come
// back as the UNQUOTED scalars 1.25 / 1.22, which conform to the renderer's
// fact charset (a quote character does not).
//
// Counterfactual: yamlScalar returning its trimmed input with the quotes kept
// makes both values fail MAJOR.MINOR normalization, so they are skipped and
// the drift disappears — RED.
func TestDetectPinDrift_QuotedPinsYieldUnquotedValues(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": "      - uses: actions/setup-go@v5\n        with:\n          go-version: '1.25' # pinned\n",
		".golangci.yml":            "run:\n  go: \"1.22\"\n",
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGo)
	want := []upkeep.PinOccurrence{
		{Path: ".github/workflows/ci.yml", Line: 3, Value: "1.25"},
		{Path: ".golangci.yml", Line: 2, Value: "1.22"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Fatalf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
	for _, o := range d.Occurrences {
		if !upkeepFactCharset.MatchString(o.Value) {
			t.Errorf("Value %q would render withheld (charset %s)", o.Value, upkeepFactCharset)
		}
	}
}

// TestDetectPinDrift_RootGoModAndGolangciYAML (approval condition 3): a ROOT
// go.mod and the .golangci.yaml spelling are both read.
//
// Counterfactual: dropping the `p == "go.mod"` arm of isGoModFile (or the
// .yaml arm of isGolangciConfig) leaves one occurrence and no drift — RED.
func TestDetectPinDrift_RootGoModAndGolangciYAML(t *testing.T) {
	files := map[string]string{
		"go.mod":         "module example.test/m\n\ngo 1.24\n",
		".golangci.yaml": "run:\n  go: '1.25'\n",
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGo)
	want := []upkeep.PinOccurrence{
		{Path: ".golangci.yaml", Line: 2, Value: "1.25"},
		{Path: "go.mod", Line: 3, Value: "1.24"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Errorf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
}

// TestDetectPinDrift_GolangciRunGoScoping: only the `go:` key DIRECTLY under
// the top-level run: block is a pin. A go: under another top-level key, or
// nested deeper under run:, is not; and a NESTED golangci config is not read.
//
// Counterfactuals: dropping the inRun check reads linters-settings' go 1.10;
// dropping the childIndent check reads run.build.go 1.11 — each adds a second
// key and RED.
func TestDetectPinDrift_GolangciRunGoScoping(t *testing.T) {
	files := map[string]string{
		"go.work": "go 1.25.0\n",
		".golangci.yml": "linters-settings:\n  go: \"1.10\"\n" +
			"run:\n  timeout: 5m\n  build:\n    go: \"1.11\"\n  go: \"1.25\"\n" +
			"output:\n  go: \"1.12\"\n",
		"backend/.golangci.yml": "run:\n  go: \"1.13\"\n",
	}
	if got := upkeep.DetectPinDrift(files); len(got) != 0 {
		t.Errorf("only run.go (1.25) is a pin, got drift %+v", got)
	}
}

// TestDetectPinDrift_UnnormalizableGoVersionSkipped (approval condition 3): a
// go-version literal that is not MAJOR.MINOR[.PATCH] — `stable`, '1.25.x',
// '>=1.25', a flow list, an expression, a block list, an unterminated quote —
// is a floating, multi-valued or malformed selector, not a pin: it contributes
// NO occurrence, so it can neither raise nor hide a drift. The real '1.24' pin
// still drifts.
//
// Counterfactual: normalizeGoVersion returning the raw literal as its own key
// (instead of ok=false) adds those lines as occurrences — RED.
func TestDetectPinDrift_UnnormalizableGoVersionSkipped(t *testing.T) {
	files := map[string]string{
		"go.work": "go 1.25.0\n",
		".github/workflows/matrix.yml": "jobs:\n  t:\n    steps:\n" +
			"      - with:\n" +
			"          go-version: stable\n" +
			"          go-version: '1.25.x'\n" +
			"          go-version: '>=1.25'\n" +
			"          go-version: [1.24, 1.25]\n" +
			"          go-version: ${{ matrix.go }}\n" +
			"          go-version:\n" +
			"            - 1.23\n" +
			"          go-version: '1.24'\n" +
			"          go-version: '1.23\n",
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGo)
	want := []upkeep.PinOccurrence{
		{Path: ".github/workflows/matrix.yml", Line: 12, Value: "1.24"},
		{Path: "go.work", Line: 1, Value: "1.25.0"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Errorf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
}

// TestDetectPinDrift_UnquotedScalarTrailingComment: an unquoted YAML scalar's
// trailing comment, after a space or a tab, is not part of the value.
//
// Counterfactual: dropping either comment cut leaves "1.24 # floor" or
// "1.25\t# lint" as the value, which fails normalization and is skipped, so
// the drift disappears — RED.
func TestDetectPinDrift_UnquotedScalarTrailingComment(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": "        with:\n          go-version: 1.24 # floor\n",
		".golangci.yml":            "run:\n  go: 1.25\t# lint\n",
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGo)
	want := []upkeep.PinOccurrence{
		{Path: ".github/workflows/ci.yml", Line: 2, Value: "1.24"},
		{Path: ".golangci.yml", Line: 2, Value: "1.25"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Errorf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
}

// TestDetectPinDrift_CommentedPinIgnored: a commented-out pin in a YAML file
// is not executed, so it is not a pin.
//
// Counterfactual: dropping the YAML comment skip reads the commented install
// tag v2.7.0 as a second golangci-lint key — RED. (The commented go-version
// line is ALSO excluded by workflowGoVersionRE's line anchor, so the
// golangci-lint line is what isolates the skip.)
func TestDetectPinDrift_CommentedPinIgnored(t *testing.T) {
	files := map[string]string{
		"go.work": "go 1.25.0\n",
		".github/workflows/ci.yml": golangciInstallStep("v2.8.0") +
			"      # - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.7.0/install.sh | sh -s -- v2.7.0\n" +
			"      # go-version: '1.20'\n" +
			"      #   npx -y @redocly/cli@1.0.0 lint\n",
		".github/workflows/docs.yml": "      - run: npx -y @redocly/cli@2.31.5 lint\n",
	}
	if got := upkeep.DetectPinDrift(files); len(got) != 0 {
		t.Errorf("commented pins must be ignored, got drift %+v", got)
	}
}

// TestDetectPinDrift_ShArgLastTokenInOneFile: the sh argument is the LAST
// vX.Y.Z after `sh -s --`, and a drift inside ONE file is still reported.
func TestDetectPinDrift_ShArgLastTokenInOneFile(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": "      - run: |\n" +
			"          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh \\\n" +
			"            | sh -s -- -b /tools/v1.0.0/bin v2.9.0\n",
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGolangciLint)
	want := []upkeep.PinOccurrence{
		{Path: ".github/workflows/ci.yml", Line: 2, Value: "v2.8.0"},
		{Path: ".github/workflows/ci.yml", Line: 3, Value: "v2.9.0"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Errorf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
}

// TestDetectPinDrift_ShArgNeedsGolangciContext: another tool's
// `curl … | sh -s -- vX.Y.Z` installer is not a golangci-lint pin.
//
// Counterfactuals: dropping the golangci-lint context check in golangciShArg
// reads v9.9.9 as a second golangci-lint key; searching the WHOLE line instead
// of only the text after `sh -s --` reads v2.7.0 — each RED.
func TestDetectPinDrift_ShArgNeedsGolangciContext(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": golangciInstallStep("v2.8.0") +
			"      - run: |\n" +
			"          curl -sSfL https://example.test/other-tool/install.sh \\\n" +
			"            | sh -s -- -b ./bin v9.9.9\n" +
			// golangci-lint context but no version argument after `sh -s --`:
			// the URL tag only — the v2.7.0 token BEFORE the marker is not the
			// sh argument.
			"      - run: curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh | TMPDIR=/tmp/v2.7.0 sh -s -- -b ./bin\n",
	}
	if got := upkeep.DetectPinDrift(files); len(got) != 0 {
		t.Errorf("an unrelated installer's sh argument must be ignored, got drift %+v", got)
	}
}

// TestDetectPinDrift_SameLineSameValueIsOneOccurrence: a one-line install whose
// URL tag and sh argument agree is ONE occurrence, not two.
//
// Counterfactual: dropping the exact-duplicate skip in sortDedupePinHits
// lists ci.yml:2:v2.8.0 twice — RED.
func TestDetectPinDrift_SameLineSameValueIsOneOccurrence(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": "      - run: |\n" +
			"          curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.8.0/install.sh | sh -s -- -b ./bin v2.8.0\n",
		".github/workflows/release.yml": golangciInstallStep("v2.9.0"),
	}
	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGolangciLint)
	want := []upkeep.PinOccurrence{
		{Path: ".github/workflows/ci.yml", Line: 2, Value: "v2.8.0"},
		{Path: ".github/workflows/release.yml", Line: 5, Value: "v2.9.0"},
		{Path: ".github/workflows/release.yml", Line: 6, Value: "v2.9.0"},
	}
	if !reflect.DeepEqual(d.Occurrences, want) {
		t.Errorf("Occurrences = %+v, want %+v", d.Occurrences, want)
	}
}

// TestDetectPinDrift_RedoclyAcrossWorkflowAgentsAndDocsAPI: the @redocly/cli
// family reads a workflow, the root AGENTS.md and docs/api/ files (markdown
// lines all scanned), and nothing else.
func TestDetectPinDrift_RedoclyAcrossWorkflowAgentsAndDocsAPI(t *testing.T) {
	files := map[string]string{
		".github/workflows/ci.yml": "      - run: |\n          npx -y @redocly/cli@2.31.5 lint \\\n",
		"AGENTS.md":                "Lint with `npx -y @redocly/cli@2.31.5 lint --config docs/api/redocly.yaml`.\n",
		"docs/api/README.md":       "# API\n\n```sh\nnpx -y @redocly/cli@2.30.0 lint\nnpx -y @redocly/cli@2.30.0 preview-docs\n```\n",
		// Not in the family's file set:
		"site/README.md":         "npx -y @redocly/cli@1.0.0 lint\n",
		"docs/api/nested/x.md":   "npx -y @redocly/cli@1.0.1 lint\n",
		"backend/AGENTS.md":      "npx -y @redocly/cli@1.0.2 lint\n",
		".github/workflows/x.sh": "npx -y @redocly/cli@1.0.3 lint\n",
	}
	got := upkeep.DetectPinDrift(files)
	want := []upkeep.PinDrift{{
		Family: upkeep.PinFamilyRedocly,
		Occurrences: []upkeep.PinOccurrence{
			{Path: ".github/workflows/ci.yml", Line: 2, Value: "2.31.5"},
			{Path: "AGENTS.md", Line: 1, Value: "2.31.5"},
			{Path: "docs/api/README.md", Line: 4, Value: "2.30.0"},
			{Path: "docs/api/README.md", Line: 5, Value: "2.30.0"},
		},
		DistinctValues: []string{"2.30.0", "2.31.5"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DetectPinDrift =\n%+v\nwant\n%+v", got, want)
	}
}

// TestDetectPinDrift_GoVersionFileIgnored: `go-version-file:` names a file,
// not a version, and is never read as a pin.
func TestDetectPinDrift_GoVersionFileIgnored(t *testing.T) {
	files := map[string]string{
		"go.work": "go 1.25.0\n",
		".github/workflows/price-drift.yml": "      - uses: actions/setup-go@v5\n" +
			"        with:\n          go-version-file: 1.20\n",
	}
	if got := upkeep.DetectPinDrift(files); len(got) != 0 {
		t.Errorf("go-version-file must be ignored, got drift %+v", got)
	}
}

// TestDetectPinDrift_OccurrenceCapKeepsEveryValue (approval condition 4): a
// family with more than PinMaxOccurrences occurrences is cut to the cap, the
// remainder is counted in OmittedOccurrences, and every disagreeing value
// keeps at least one occurrence even when it sorts last.
//
// Counterfactuals: dropping the cap lists all 41 occurrences with
// OmittedOccurrences 0; dropping the representative pass in capPinHits keeps
// the first 32 by sort order and loses zz/go.mod's 1.24 — each RED.
func TestDetectPinDrift_OccurrenceCapKeepsEveryValue(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("m%02d/go.mod", i)] = "go 1.25.0\n"
	}
	files["zz/go.mod"] = "go 1.24\n"

	d := findFamily(t, upkeep.DetectPinDrift(files), upkeep.PinFamilyGo)
	if len(d.Occurrences) != upkeep.PinMaxOccurrences {
		t.Errorf("len(Occurrences) = %d, want %d", len(d.Occurrences), upkeep.PinMaxOccurrences)
	}
	if want := 41 - upkeep.PinMaxOccurrences; d.OmittedOccurrences != want {
		t.Errorf("OmittedOccurrences = %d, want %d", d.OmittedOccurrences, want)
	}
	if want := []string{"1.24", "1.25"}; !reflect.DeepEqual(d.DistinctValues, want) {
		t.Errorf("DistinctValues = %v, want %v", d.DistinctValues, want)
	}
	last := d.Occurrences[len(d.Occurrences)-1]
	if want := (upkeep.PinOccurrence{Path: "zz/go.mod", Line: 1, Value: "1.24"}); last != want {
		t.Errorf("last occurrence = %+v, want %+v (the minority value must survive the cap)", last, want)
	}
	for i := 1; i < len(d.Occurrences); i++ {
		if d.Occurrences[i-1].Path >= d.Occurrences[i].Path {
			t.Fatalf("occurrences not sorted at %d: %+v", i, d.Occurrences)
		}
	}

	// More distinct values than the cap: the cap still holds, and
	// DistinctValues still names every value.
	wide := map[string]string{}
	for i := 0; i < 34; i++ {
		wide[fmt.Sprintf("m%02d/go.mod", i)] = fmt.Sprintf("go 1.%d\n", i)
	}
	w := findFamily(t, upkeep.DetectPinDrift(wide), upkeep.PinFamilyGo)
	if len(w.Occurrences) != upkeep.PinMaxOccurrences || w.OmittedOccurrences != 34-upkeep.PinMaxOccurrences || len(w.DistinctValues) != 34 {
		t.Errorf("wide: len(Occurrences) = %d OmittedOccurrences = %d len(DistinctValues) = %d, want %d, %d, 34",
			len(w.Occurrences), w.OmittedOccurrences, len(w.DistinctValues), upkeep.PinMaxOccurrences, 34-upkeep.PinMaxOccurrences)
	}
}

// TestDetectPinDrift_NoDriftIsEmptyNonNil: agreeing pins and irrelevant files
// yield a non-nil empty slice.
func TestDetectPinDrift_NoDriftIsEmptyNonNil(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"nil map":    nil,
		"irrelevant": {"README.md": "go 1.20\n@redocly/cli@1.0.0\n", "main.go": "package main\n"},
		"agreeing":   realRepoGoFiles(),
	} {
		if got := upkeep.DetectPinDrift(files); got == nil || len(got) != 0 {
			t.Errorf("%s: DetectPinDrift = %#v, want a non-nil empty slice", name, got)
		}
	}
}

// TestDetectPinDrift_Deterministic: the same map yields deep-equal output
// across repeated calls (Go randomizes map iteration per range).
func TestDetectPinDrift_Deterministic(t *testing.T) {
	files := realRepoGoFiles()
	files[".golangci.yml"] = "run:\n  go: \"1.22\"\n"
	files[".github/workflows/a.yml"] = golangciInstallStep("v2.8.0") + "      - run: npx -y @redocly/cli@2.31.5 lint\n"
	files[".github/workflows/b.yml"] = golangciInstallStep("v2.9.0")
	files["docs/api/v0.md"] = "npx -y @redocly/cli@2.30.0 lint\n"
	for i := 0; i < 8; i++ {
		files[fmt.Sprintf("mod%d/go.mod", i)] = "go 1.25\n"
	}

	first := upkeep.DetectPinDrift(files)
	if len(first) != 3 {
		t.Fatalf("want 3 drifting families, got %+v", first)
	}
	for i, fam := range []string{upkeep.PinFamilyRedocly, upkeep.PinFamilyGo, upkeep.PinFamilyGolangciLint} {
		if first[i].Family != fam {
			t.Errorf("family[%d] = %q, want %q (sorted by family)", i, first[i].Family, fam)
		}
	}
	for i := 0; i < 20; i++ {
		if got := upkeep.DetectPinDrift(files); !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d differs:\n%+v\nvs\n%+v", i, got, first)
		}
	}
}
