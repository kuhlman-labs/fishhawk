package spec_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The done-means gate for E52.9 (#2221): `docs/spec/workflow-v2.md` must be a
// COMPLETE, STANDALONE grammar reference, not a delta on v1 that hands the
// reader off to `workflow-v0.md` / `workflow-v1.md`. Prose alone cannot hold
// that property through a 474 -> ~1200 line rewrite, so the four helpers below
// assert it mechanically against the SHIPPED artifact:
//
//  1. missingSchemaFieldNames  — every field name the canonical v2 schema
//     declares is mentioned in PROSE in the reference body.
//  2. delegationSentences      — no "go read the other major" sentence survives.
//  3. undocumentedStageReadings— each of the five stage types carries BOTH its
//     code-change and its general reading.
//  4. missingRequiredHeadings  — the section titles other documents cite BY
//     NAME still exist under those names.
//
// Each helper is pure over injected inputs so every failure mode gets its own
// table case; TestWorkflowV2DocIsStandalone then runs all four against the real
// doc + the real canonical schema.

const (
	v2DocPath    = "../../../docs/spec/workflow-v2.md"
	v2SchemaPath = "../../../docs/spec/workflow-v2.schema.json"

	// v2DocAppendixHeading opens the RETAINED historical appendix. Everything
	// above it is the reference body; everything below it is the per-E52-child
	// divergence record, kept as history. The completeness check measures the
	// BODY only: a field whose prose section was forgotten must not show green
	// because its name appears in an appendix YAML stanza.
	v2DocAppendixHeading = "Appendix: what changed from v0/v1"
)

// v2DocCitedHeadings are section titles other documents cite BY NAME (with no
// anchor link to grep for), so a retitling would silently orphan the citation:
// `backend/internal/spec/README.md` cites the first two, and the appendix
// heading is what bounds the reference body above.
var v2DocCitedHeadings = []string{
	"Reuse: defaults and extends",
	"Autonomy: tier shorthand and action matrix",
	v2DocAppendixHeading,
}

// v2DocDelegationPhrases are the sentences that make the page a DELTA rather
// than a reference. Each one hands the reader to another major for a v2 field.
var v2DocDelegationPhrases = []string{
	"For the full reference see",
	"the v1 reference is authoritative",
}

// v2StageTypes is the closed stage-type enum. Each must be documented in both
// its code-change and its general reading (ADR-067 §2 retains the v0 names, so
// the doc carries both readings rather than renaming).
var v2StageTypes = []string{"plan", "implement", "review", "deploy", "acceptance"}

var (
	fencedBlockRE = regexp.MustCompile("(?ms)^[ \t]*```.*?^[ \t]*```[ \t]*$")
	headingRE     = regexp.MustCompile(`(?m)^(#{1,6})[ \t]+(.*)$`)
)

// stripFencedBlocks removes every fenced code block. A field name that appears
// only inside an example does not count as documented.
func stripFencedBlocks(text string) string {
	return fencedBlockRE.ReplaceAllString(text, "\n")
}

// v2ReferenceBody returns the text above the historical appendix heading. When
// the heading is absent the whole document is returned — the appendix heading
// itself is asserted separately by missingRequiredHeadings, so its loss fails
// loudly there instead of silently widening this window.
func v2ReferenceBody(docText string) string {
	for _, m := range headingRE.FindAllStringSubmatchIndex(docText, -1) {
		if normalizeHeadingTitle(docText[m[4]:m[5]]) == v2DocAppendixHeading {
			return docText[:m[0]]
		}
	}
	return docText
}

// normalizeHeadingTitle strips markdown code spans and surrounding emphasis so
// a heading rendered as "Reuse: `defaults` and `extends`" compares equal to the
// bare title other documents cite.
func normalizeHeadingTitle(title string) string {
	title = strings.ReplaceAll(title, "`", "")
	title = strings.ReplaceAll(title, "*", "")
	return strings.TrimSpace(title)
}

// mentionsIdentifier reports whether name appears in text as a whole
// identifier — bounded by a non-identifier byte on each side — so `agent` is
// NOT satisfied by `agent_self_retry` or by `reviewers.agents`.
func mentionsIdentifier(text, name string) bool {
	for i := 0; ; {
		idx := strings.Index(text[i:], name)
		if idx < 0 {
			return false
		}
		start := i + idx
		end := start + len(name)
		if !isIdentByte(text, start-1) && !isIdentByte(text, end) {
			return true
		}
		i = start + 1
	}
}

func isIdentByte(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return false
	}
	c := text[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// schemaDeclaredFieldNames collects every field name the schema declares under
// a `properties` / `patternProperties` map, at any depth, including inside
// `$defs`.
func schemaDeclaredFieldNames(schemaJSON []byte) ([]string, error) {
	var root any
	if err := json.Unmarshal(schemaJSON, &root); err != nil {
		return nil, fmt.Errorf("decode schema: %w", err)
	}
	names := map[string]struct{}{}
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, key := range []string{"properties", "patternProperties"} {
				if sub, ok := n[key].(map[string]any); ok {
					for name, child := range sub {
						if key == "properties" {
							names[name] = struct{}{}
						}
						walk(child)
					}
				}
			}
			for key, child := range n {
				if key == "properties" || key == "patternProperties" {
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(root)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// missingSchemaFieldNames returns every field name the schema declares that the
// doc's REFERENCE BODY never mentions in prose (fenced examples and the
// historical appendix both excluded). It is a completeness FLOOR, not proof of
// quality: names like `mode` and `count` are common words, so a green result
// means "a sentence mentions this field", not "this field is documented well".
func missingSchemaFieldNames(schemaJSON []byte, docText string) ([]string, error) {
	names, err := schemaDeclaredFieldNames(schemaJSON)
	if err != nil {
		return nil, err
	}
	prose := stripFencedBlocks(v2ReferenceBody(docText))
	var missing []string
	for _, name := range names {
		if !mentionsIdentifier(prose, name) {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// delegationSentences returns the delta-delegation phrases still present
// anywhere in the doc, in v2DocDelegationPhrases order.
func delegationSentences(docText string) []string {
	var found []string
	for _, phrase := range v2DocDelegationPhrases {
		if strings.Contains(docText, phrase) {
			found = append(found, phrase)
		}
	}
	return found
}

// undocumentedStageReadings returns each stage type whose own subsection is
// missing, or which is missing one of its two readings. A type's subsection is
// the first heading whose title names it in a code span; the readings are
// labelled "Code-change reading" and "General reading".
func undocumentedStageReadings(docText string) []string {
	body := stripFencedBlocks(v2ReferenceBody(docText))
	var missing []string
	for _, stageType := range v2StageTypes {
		section, ok := headingSection(body, "`"+stageType+"`")
		if !ok || !strings.Contains(section, "Code-change reading") || !strings.Contains(section, "General reading") {
			missing = append(missing, stageType)
		}
	}
	return missing
}

// headingSection returns the text of the first heading whose title contains
// needle, up to the next heading at the same or a higher level.
func headingSection(text, needle string) (string, bool) {
	matches := headingRE.FindAllStringSubmatchIndex(text, -1)
	for i, m := range matches {
		title := text[m[4]:m[5]]
		if !strings.Contains(title, needle) {
			continue
		}
		level := m[3] - m[2]
		end := len(text)
		for _, next := range matches[i+1:] {
			if next[3]-next[2] <= level {
				end = next[0]
				break
			}
		}
		return text[m[0]:end], true
	}
	return "", false
}

// missingRequiredHeadings returns each wanted section title with no heading in
// the doc, so a retitling fails in-loop rather than orphaning the inbound
// citations that name these sections (they carry no anchor link to grep for).
func missingRequiredHeadings(docText string, want []string) []string {
	have := map[string]struct{}{}
	for _, m := range headingRE.FindAllStringSubmatch(docText, -1) {
		have[normalizeHeadingTitle(m[2])] = struct{}{}
	}
	var missing []string
	for _, title := range want {
		if _, ok := have[title]; !ok {
			missing = append(missing, title)
		}
	}
	return missing
}

func TestMissingSchemaFieldNames(t *testing.T) {
	const schemaJSON = `{
	  "properties": {"version": {"type": "string"}, "workflows": {
	     "properties": {"zonk": {"type": "string"}}
	  }},
	  "$defs": {"stage": {"properties": {"deep_field": {"type": "string"}}}}
	}`

	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "field mentioned nowhere is reported by name",
			doc:  "# Doc\n\nThe `version` and `workflows` keys, plus `deep_field`.\n",
			want: []string{"zonk"},
		},
		{
			name: "backticked prose mention counts",
			doc:  "# Doc\n\n`version`, `workflows`, `zonk` and `deep_field` are documented.\n",
			want: nil,
		},
		{
			name: "bare prose mention counts",
			doc:  "# Doc\n\nversion, workflows, zonk and deep_field are documented.\n",
			want: nil,
		},
		{
			name: "mention only inside a fenced example does not count",
			doc:  "# Doc\n\n`version`, `workflows`, `deep_field`.\n\n```yaml\nzonk: true\n```\n",
			want: []string{"zonk"},
		},
		{
			name: "mention only in an appendix YAML stanza does not count",
			doc: "# Doc\n\n`version`, `workflows`, `deep_field`.\n\n" +
				"## Appendix: what changed from v0/v1\n\n```yaml\nzonk: true\n```\n",
			want: []string{"zonk"},
		},
		{
			name: "mention only in appendix prose does not count",
			doc: "# Doc\n\n`version`, `workflows`, `deep_field`.\n\n" +
				"## Appendix: what changed from v0/v1\n\nv1 spelled this `zonk`.\n",
			want: []string{"zonk"},
		},
		{
			name: "a longer identifier does not satisfy a shorter field name",
			doc:  "# Doc\n\n`version`, `workflows`, `deep_field`, `zonk_legacy`.\n",
			want: []string{"zonk"},
		},
		{
			name: "every declared name missing is reported, sorted",
			doc:  "# Doc\n\nNothing here.\n",
			want: []string{"deep_field", "version", "workflows", "zonk"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := missingSchemaFieldNames([]byte(schemaJSON), tc.doc)
			if err != nil {
				t.Fatalf("missingSchemaFieldNames: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("missing names = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMissingSchemaFieldNamesRejectsUndecodableSchema(t *testing.T) {
	if _, err := missingSchemaFieldNames([]byte("{not json"), "# Doc\n"); err == nil {
		t.Fatal("want an error for an undecodable schema, got nil")
	}
}

func TestDelegationSentences(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "full-reference handoff is reported by name",
			doc:  "# Doc\n\nFor the full reference see `workflow-v1.md`.\n",
			want: []string{"For the full reference see"},
		},
		{
			name: "authoritative-v1 handoff is reported by name",
			doc:  "# Doc\n\nFor every other member, the v1 reference is authoritative.\n",
			want: []string{"the v1 reference is authoritative"},
		},
		{
			name: "both handoffs are reported",
			doc:  "# Doc\n\nFor the full reference see v1; the v1 reference is authoritative.\n",
			want: v2DocDelegationPhrases,
		},
		{
			name: "a standalone reference reports none",
			doc:  "# Doc\n\nThis page documents every v2 member; `workflow-v1.md` is history.\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := delegationSentences(tc.doc)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("delegation sentences = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUndocumentedStageReadings(t *testing.T) {
	section := func(stageType string, readings ...string) string {
		s := "### `" + stageType + "` — a stage\n\n"
		for _, r := range readings {
			s += r + ": prose about " + stageType + ".\n\n"
		}
		return s
	}
	bothAll := func() string {
		var b strings.Builder
		b.WriteString("# Doc\n\n## Stage types\n\n")
		for _, st := range v2StageTypes {
			b.WriteString(section(st, "Code-change reading", "General reading"))
		}
		return b.String()
	}

	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "all five types with both readings reports none",
			doc:  bothAll(),
			want: nil,
		},
		{
			name: "a type with only the code-change reading is reported",
			doc:  strings.Replace(bothAll(), section("implement", "Code-change reading", "General reading"), section("implement", "Code-change reading"), 1),
			want: []string{"implement"},
		},
		{
			name: "a type with only the general reading is reported",
			doc:  strings.Replace(bothAll(), section("acceptance", "Code-change reading", "General reading"), section("acceptance", "General reading"), 1),
			want: []string{"acceptance"},
		},
		{
			name: "a type with no subsection at all is reported",
			doc:  strings.Replace(bothAll(), section("deploy", "Code-change reading", "General reading"), "", 1),
			want: []string{"deploy"},
		},
		{
			name: "readings that live only in a fenced example do not count",
			doc: "# Doc\n\n## Stage types\n\n### `plan` — a stage\n\n```\nCode-change reading: x\nGeneral reading: y\n```\n" +
				section("implement", "Code-change reading", "General reading") +
				section("review", "Code-change reading", "General reading") +
				section("deploy", "Code-change reading", "General reading") +
				section("acceptance", "Code-change reading", "General reading"),
			want: []string{"plan"},
		},
		{
			name: "readings below the appendix heading do not count",
			doc: "# Doc\n\n## Appendix: what changed from v0/v1\n\n" +
				section("plan", "Code-change reading", "General reading") +
				section("implement", "Code-change reading", "General reading") +
				section("review", "Code-change reading", "General reading") +
				section("deploy", "Code-change reading", "General reading") +
				section("acceptance", "Code-change reading", "General reading"),
			want: v2StageTypes,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := undocumentedStageReadings(tc.doc)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("undocumented stage readings = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMissingRequiredHeadings(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "both cited headings present, code spans and all",
			doc: "# Doc\n\n## Reuse: `defaults` and `extends`\n\n" +
				"## Autonomy: tier shorthand and action matrix\n",
			want: nil,
		},
		{
			name: "a retitled reuse section is reported by name",
			doc: "# Doc\n\n## Same-document reuse\n\n" +
				"## Autonomy: tier shorthand and action matrix\n",
			want: []string{"Reuse: defaults and extends"},
		},
		{
			name: "a retitled autonomy section is reported by name",
			doc:  "# Doc\n\n## Reuse: `defaults` and `extends`\n\n## Autonomy\n",
			want: []string{"Autonomy: tier shorthand and action matrix"},
		},
		{
			name: "a title mentioned only in body prose is not a heading",
			doc:  "# Doc\n\nSee Reuse: defaults and extends below.\n\n## Autonomy: tier shorthand and action matrix\n",
			want: []string{"Reuse: defaults and extends"},
		},
		{
			name: "both missing are reported in want order",
			doc:  "# Doc\n\n## Something else\n",
			want: []string{"Reuse: defaults and extends", "Autonomy: tier shorthand and action matrix"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRequiredHeadings(tc.doc, []string{
				"Reuse: defaults and extends",
				"Autonomy: tier shorthand and action matrix",
			})
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("missing headings = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkflowV2DocIsStandalone(t *testing.T) {
	docBytes, err := os.ReadFile(v2DocPath)
	if err != nil {
		t.Fatalf("read %s: %v", v2DocPath, err)
	}
	schemaBytes, err := os.ReadFile(v2SchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", v2SchemaPath, err)
	}
	doc := string(docBytes)

	if missing := missingRequiredHeadings(doc, v2DocCitedHeadings); len(missing) > 0 {
		t.Errorf("%s is missing section headings cited by name from other documents: %v\n"+
			"These titles are referenced without an anchor link, so a retitling orphans the citation. Restore them verbatim.",
			v2DocPath, missing)
	}

	missing, err := missingSchemaFieldNames(schemaBytes, doc)
	if err != nil {
		t.Fatalf("missingSchemaFieldNames: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%s never mentions %d field name(s) the canonical schema declares: %v\n"+
			"A v2 field must be documented in PROSE in the reference body (above %q, outside fenced examples) — "+
			"add a section or table row for each, or the reader has to read the schema instead.",
			v2DocPath, len(missing), missing, v2DocAppendixHeading)
	}

	if found := delegationSentences(doc); len(found) > 0 {
		t.Errorf("%s still delegates to another major: %v\n"+
			"v2 is a standalone reference: links to workflow-v0.md / workflow-v1.md survive as history only, never as the reader's path to a v2 field.",
			v2DocPath, found)
	}

	if undocumented := undocumentedStageReadings(doc); len(undocumented) > 0 {
		t.Errorf("%s does not document both readings of stage type(s) %v\n"+
			"ADR-067 §2 retains the v0 type names, so each type needs a subsection giving its \"Code-change reading\" AND its \"General reading\".",
			v2DocPath, undocumented)
	}
}
