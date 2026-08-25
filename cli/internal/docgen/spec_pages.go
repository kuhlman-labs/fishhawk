package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Schema source paths, repo-relative.
const (
	schemaV0Rel   = "docs/spec/workflow-v0.schema.json"
	schemaV1Rel   = "docs/spec/workflow-v1.schema.json"
	schemaV2Rel   = "docs/spec/workflow-v2.schema.json"
	planSchemaRel = "docs/spec/plan-standard-v1.schema.json"

	exampleReuseRel    = "docs/spec/examples/workflow-v2-reuse.yaml"
	exampleGroomingRel = "docs/spec/examples/workflow-v2-backlog-grooming.yaml"
)

// Primitive is one of the MVP_SPEC §4.1 primitives, bound to a schema
// anchor and an example key.
type Primitive struct {
	Name         string // display name
	SchemaAnchor string // $defs name rendered as `#####` anchor
	ExampleKey   string // YAML key that appears in a shipped example
	ExampleName  string // which example demonstrates it
}

// Section4Primitives is the closed six-primitive set (MVP_SPEC §4.1,
// "Six primitives, no more"). TestSection4PrimitivesAreDemonstrated
// asserts each is present in the generated reference at both its schema
// anchor and its example key.
var Section4Primitives = []Primitive{
	{Name: "workflow", SchemaAnchor: "workflow", ExampleKey: "workflows:", ExampleName: "same-document reuse"},
	{Name: "stage", SchemaAnchor: "stage", ExampleKey: "stages:", ExampleName: "same-document reuse"},
	{Name: "gate", SchemaAnchor: "gate", ExampleKey: "gates:", ExampleName: "backlog grooming"},
	{Name: "constraint", SchemaAnchor: "constraint", ExampleKey: "constraints:", ExampleName: "same-document reuse"},
	{Name: "approver", SchemaAnchor: "approvals", ExampleKey: "approvals:", ExampleName: "backlog grooming"},
	{Name: "artifact", SchemaAnchor: "produces", ExampleKey: "produces:", ExampleName: "same-document reuse"},
}

func readSource(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, rel))
}

// fenceYAML wraps raw YAML in a fenced code block, widening the fence
// past any internal backtick run.
func fenceYAML(raw string) string {
	longest, cur := 0, 0
	for _, r := range raw {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	width := 3
	if longest+1 > width {
		width = longest + 1
	}
	fence := strings.Repeat("`", width)
	return fence + "yaml\n" + strings.TrimRight(raw, "\n") + "\n" + fence + "\n"
}

func generateWorkflowSpecPage(root string) (string, error) {
	v2, err := readSource(root, schemaV2Rel)
	if err != nil {
		return "", err
	}
	v1, err := readSource(root, schemaV1Rel)
	if err != nil {
		return "", err
	}
	v0, err := readSource(root, schemaV0Rel)
	if err != nil {
		return "", err
	}
	reuse, err := readSource(root, exampleReuseRel)
	if err != nil {
		return "", err
	}
	grooming, err := readSource(root, exampleGroomingRel)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(genPreamble())
	b.WriteString("\n")
	b.WriteString(crossLinkBlock())
	b.WriteString("\n")

	b.WriteString("## Field reference\n\n")
	for _, s := range []struct {
		title, note string
		data        []byte
	}{
		{"workflow-v2", "The live major — write new specs here.", v2},
		{"workflow-v1", "Frozen. Accepted and validated forever; migrate with `fishhawk migrate-spec`.", v1},
		{"workflow-v0", "Frozen. Accepted and validated forever.", v0},
	} {
		fmt.Fprintf(&b, "### `%s`\n\n%s\n\n", s.title, s.note)
		sr, err := RenderSchemaDoc(s.title, s.data)
		if err != nil {
			return "", err
		}
		b.WriteString(sr.Markdown)
		b.WriteString("\n")
	}

	b.WriteString("## What changed between majors\n\n")
	b.WriteString("Shape deltas only — a field whose description changed but whose type, " +
		"requiredness, enum members and default did not is NOT listed here.\n\n")
	for _, d := range []struct {
		title    string
		old, new []byte
	}{
		{"workflow-v1 → workflow-v2", v1, v2},
		{"workflow-v0 → workflow-v2", v0, v2},
	} {
		b.WriteString("### " + d.title + "\n\n")
		delta, err := renderDelta(d.old, d.new)
		if err != nil {
			return "", err
		}
		b.WriteString(delta)
		b.WriteString("\n")
	}

	b.WriteString("## The six primitives\n\n")
	b.WriteString("MVP_SPEC §4.1 declares six workflow primitives, no more. Each is defined " +
		"in the schema and demonstrated in a shipped example:\n\n")
	b.WriteString("| Primitive | Schema anchor | Example key | Demonstrated in |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, p := range Section4Primitives {
		fmt.Fprintf(&b, "| %s | %s | %s | %s example |\n",
			p.Name, codeSpan(p.SchemaAnchor), codeSpan(p.ExampleKey), p.ExampleName)
	}
	b.WriteString("\n### Example — same-document reuse\n\n")
	b.WriteString(fenceYAML(string(reuse)))
	b.WriteString("\n### Example — backlog grooming (gates and approvals)\n\n")
	b.WriteString(fenceYAML(string(grooming)))

	return b.String(), nil
}

func generatePlanSchemaPage(root string) (string, error) {
	data, err := readSource(root, planSchemaRel)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(genPreamble())
	b.WriteString("\n")
	b.WriteString(crossLinkBlock())
	b.WriteString("\n")
	b.WriteString("## Field reference — `standard_v1`\n\n")
	sr, err := RenderSchemaDoc("standard_v1", data)
	if err != nil {
		return "", err
	}
	b.WriteString(sr.Markdown)
	return b.String(), nil
}

// -------------------------------------------------------------------------
// Shape-based delta between two schema majors.
// -------------------------------------------------------------------------

type fieldShape struct {
	Type     string
	Required bool
	Enum     string
	Default  string
}

func (s fieldShape) sig() string {
	req := "optional"
	if s.Required {
		req = "required"
	}
	return s.Type + "|" + req + "|" + s.Enum + "|" + s.Default
}

func nodeShape(n *yaml.Node, required bool) fieldShape {
	fs := fieldShape{Type: typeDesc(n), Required: required}
	if e := mapVal(n, "enum"); e != nil {
		fs.Enum = strings.Join(seqValues(e), ",")
	}
	if d := mapVal(n, "default"); d != nil {
		fs.Default = scalarString(d)
	}
	return fs
}

// schemaShapes flattens a schema into path -> shape for the accountable
// node shapes (top-level properties, $defs entries, $defs nested props).
func schemaShapes(data []byte) (map[string]fieldShape, error) {
	root, err := rootMapping(data)
	if err != nil {
		return nil, err
	}
	out := map[string]fieldShape{}
	if props := mapVal(root, "properties"); props != nil {
		req := requiredSet(root)
		for _, k := range mapKeys(props) {
			out["/properties/"+k] = nodeShape(mapVal(props, k), req[k])
		}
	}
	if defs := mapVal(root, "$defs"); defs != nil {
		for _, name := range mapKeys(defs) {
			d := mapVal(defs, name)
			out["/$defs/"+name] = nodeShape(d, false)
			if dp := mapVal(d, "properties"); dp != nil {
				req := requiredSet(d)
				for _, p := range mapKeys(dp) {
					out["/$defs/"+name+"/properties/"+p] = nodeShape(mapVal(dp, p), req[p])
				}
			}
		}
	}
	return out, nil
}

func renderDelta(oldData, newData []byte) (string, error) {
	oldS, err := schemaShapes(oldData)
	if err != nil {
		return "", err
	}
	newS, err := schemaShapes(newData)
	if err != nil {
		return "", err
	}
	paths := map[string]bool{}
	for p := range oldS {
		paths[p] = true
	}
	for p := range newS {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var rows []string
	for _, p := range sorted {
		o, inOld := oldS[p]
		n, inNew := newS[p]
		switch {
		case inOld && !inNew:
			rows = append(rows, fmt.Sprintf("| %s | removed | present in the older major, absent at the newer |", codeSpan(p)))
		case !inOld && inNew:
			rows = append(rows, fmt.Sprintf("| %s | added | new at the newer major |", codeSpan(p)))
		case o.sig() != n.sig():
			rows = append(rows, fmt.Sprintf("| %s | changed | %s |", codeSpan(p), proseCell(facetDiff(o, n))))
		}
	}
	if len(rows) == 0 {
		return "_No shape differences._\n", nil
	}
	var b strings.Builder
	b.WriteString("| Field | Change | Detail |\n|---|---|---|\n")
	for _, r := range rows {
		b.WriteString(r + "\n")
	}
	return b.String(), nil
}

// facetDiff names which shape facets differ between two shapes.
func facetDiff(o, n fieldShape) string {
	var parts []string
	if o.Type != n.Type {
		parts = append(parts, fmt.Sprintf("type %q→%q", o.Type, n.Type))
	}
	if o.Required != n.Required {
		parts = append(parts, fmt.Sprintf("requiredness %v→%v", o.Required, n.Required))
	}
	if o.Enum != n.Enum {
		parts = append(parts, "enum members differ")
	}
	if o.Default != n.Default {
		parts = append(parts, fmt.Sprintf("default %q→%q", o.Default, n.Default))
	}
	return strings.Join(parts, "; ")
}
