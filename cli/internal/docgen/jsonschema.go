package docgen

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file walks a JSON Schema (parsed as YAML so declaration order is
// preserved — encoding/json into a map loses it) and renders Markdown
// field tables, plus the machinery the accounting test needs: the set of
// ACCOUNTABLE nodes (every top-level property, every $defs entry, and
// every nested property of each $defs entry, addressed by JSON pointer)
// and the set of pointers a render actually emitted. The accounting test
// requires the two to agree modulo DeliberateExclusions.

// SchemaRender is the output of rendering one schema: the Markdown body
// and the JSON pointers it emitted a row/section for.
type SchemaRender struct {
	Markdown string
	Rendered []string // sorted JSON pointers
}

// DeliberateExclusions names accountable nodes that are intentionally
// NOT rendered as a plain field row (e.g. a node summarized elsewhere),
// keyed by schema logical name then JSON pointer -> reason. It lives in
// production code so an exclusion is reviewable, and every entry must
// still RESOLVE in its schema — a stale exclusion is itself a failure
// (TestSchemaAccountingIsExhaustive). Empty means "every accountable
// node is rendered", which is the stronger and current posture.
var DeliberateExclusions = map[string]map[string]string{
	"workflow-v0":      {},
	"workflow-v1":      {},
	"workflow-v2":      {},
	"plan-standard-v1": {},
}

// rootMapping parses schema bytes (JSON or YAML) into the root mapping
// node, preserving key order.
func rootMapping(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("docgen: empty or non-document schema")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("docgen: schema root is not a mapping")
	}
	return root, nil
}

// mapVal returns the value node for key in a mapping node, or nil.
func mapVal(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mapKeys returns the keys of a mapping node in declaration order.
func mapKeys(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keys = append(keys, n.Content[i].Value)
	}
	return keys
}

// seqValues returns the scalar string values of a sequence node.
func seqValues(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, scalarString(c))
	}
	return out
}

// scalarString renders a node compactly for a table cell: a scalar as
// its value, a sequence as [a, b], a mapping as {…}.
func scalarString(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.SequenceNode:
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			parts = append(parts, scalarString(c))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case yaml.MappingNode:
		return "{…}"
	default:
		return ""
	}
}

// typeDesc renders the "Type" column for a property node: the type,
// including a $ref target name, combined types joined with " or ", and
// array element types.
func typeDesc(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if ref := mapVal(n, "$ref"); ref != nil {
		return codeSpan(refName(ref.Value))
	}
	if tn := mapVal(n, "type"); tn != nil {
		switch tn.Kind {
		case yaml.ScalarNode:
			t := tn.Value
			if t == "array" {
				if items := mapVal(n, "items"); items != nil {
					if ir := mapVal(items, "$ref"); ir != nil {
						return "array of " + codeSpan(refName(ir.Value))
					}
					if it := mapVal(items, "type"); it != nil && it.Kind == yaml.ScalarNode {
						return "array of " + it.Value
					}
				}
				return "array"
			}
			return t
		case yaml.SequenceNode:
			return strings.Join(seqValues(tn), " or ")
		}
	}
	// Composed schemas (oneOf/anyOf/allOf) with no direct type.
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if mapVal(n, k) != nil {
			return k
		}
	}
	if mapVal(n, "enum") != nil {
		return "enum"
	}
	if mapVal(n, "properties") != nil {
		return "object"
	}
	return ""
}

// refName extracts the terminal name from a JSON pointer $ref like
// "#/$defs/stage" -> "stage".
func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// constraintDesc renders the "Constraints" column: enum members,
// default, const, format, and numeric/length/item bounds — the shape
// facets, not the description.
func constraintDesc(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	var parts []string
	if e := mapVal(n, "enum"); e != nil {
		vals := seqValues(e)
		coded := make([]string, 0, len(vals))
		for _, v := range vals {
			coded = append(coded, codeSpan(v))
		}
		parts = append(parts, "enum: "+strings.Join(coded, ", "))
	}
	if c := mapVal(n, "const"); c != nil {
		parts = append(parts, "const: "+codeSpan(scalarString(c)))
	}
	if d := mapVal(n, "default"); d != nil {
		parts = append(parts, "default: "+codeSpan(scalarString(d)))
	}
	if f := mapVal(n, "format"); f != nil {
		parts = append(parts, "format: "+f.Value)
	}
	for _, b := range []struct{ key, label string }{
		{"minimum", "min"}, {"maximum", "max"},
		{"minLength", "minLength"}, {"maxLength", "maxLength"},
		{"minItems", "minItems"}, {"maxItems", "maxItems"},
		{"pattern", "pattern"},
	} {
		if v := mapVal(n, b.key); v != nil {
			parts = append(parts, b.label+": "+codeSpan(v.Value))
		}
	}
	return strings.Join(parts, "; ")
}

// requiredSet returns the set of required property names for an object
// node.
func requiredSet(n *yaml.Node) map[string]bool {
	set := map[string]bool{}
	for _, r := range seqValues(mapVal(n, "required")) {
		set[r] = true
	}
	return set
}

// orderedProps returns the property names of an object node in
// REQUIRED-FIRST then declaration order. Required names keep their
// relative declaration order; optional names follow in declaration
// order. This is the ordering TestFieldOrderIsRequiredFirstThenDocumentOrder
// pins against a fixture whose optional field is declared BEFORE its
// required ones.
func orderedProps(props *yaml.Node, required map[string]bool) []string {
	keys := mapKeys(props)
	var req, opt []string
	for _, k := range keys {
		if required[k] {
			req = append(req, k)
		} else {
			opt = append(opt, k)
		}
	}
	return append(req, opt...)
}

// renderPropertiesTable renders one properties table and returns the
// Markdown plus the JSON pointers it emitted (pointerBase+"/properties/"+key).
func renderPropertiesTable(props *yaml.Node, required map[string]bool, pointerBase string) (string, []string) {
	var b strings.Builder
	var pointers []string
	b.WriteString("| Field | Type | Required | Constraints | Description |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, key := range orderedProps(props, required) {
		v := mapVal(props, key)
		reqTxt := "optional"
		if required[key] {
			reqTxt = "required"
		}
		desc := ""
		if d := mapVal(v, "description"); d != nil {
			desc = d.Value
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			codeSpan(key), proseCell(typeDesc(v)), reqTxt,
			proseCell(constraintDesc(v)), proseCell(desc))
		pointers = append(pointers, pointerBase+"/properties/"+key)
	}
	return b.String(), pointers
}

// RenderSchemaDoc renders a full schema: a top-level fields table, then
// a Definitions section with one subsection per $defs entry. It returns
// the Markdown and the sorted set of JSON pointers rendered.
func RenderSchemaDoc(title string, data []byte) (SchemaRender, error) {
	root, err := rootMapping(data)
	if err != nil {
		return SchemaRender{}, err
	}
	var b strings.Builder
	var rendered []string

	if props := mapVal(root, "properties"); props != nil {
		fmt.Fprintf(&b, "#### %s — top-level fields\n\n", title)
		tbl, ptrs := renderPropertiesTable(props, requiredSet(root), "")
		b.WriteString(tbl)
		b.WriteString("\n")
		rendered = append(rendered, ptrs...)
	}

	if defs := mapVal(root, "$defs"); defs != nil {
		fmt.Fprintf(&b, "#### %s — definitions\n\n", title)
		for _, name := range mapKeys(defs) {
			d := mapVal(defs, name)
			ptr := "/$defs/" + name
			rendered = append(rendered, ptr)
			fmt.Fprintf(&b, "##### `%s`\n\n", name)
			if desc := mapVal(d, "description"); desc != nil {
				b.WriteString(proseCell(desc.Value) + "\n\n")
			}
			if dp := mapVal(d, "properties"); dp != nil {
				tbl, ptrs := renderPropertiesTable(dp, requiredSet(d), ptr)
				b.WriteString(tbl)
				b.WriteString("\n")
				rendered = append(rendered, ptrs...)
			} else {
				// A composed/scalar definition (oneOf/enum/…): the
				// heading itself accounts for the node; there are no
				// direct properties to enumerate.
				if t := typeDesc(d); t != "" {
					b.WriteString("Type: " + proseCell(t) + ". " + proseCell(constraintDesc(d)) + "\n\n")
				}
			}
		}
	}

	sort.Strings(rendered)
	return SchemaRender{Markdown: b.String(), Rendered: rendered}, nil
}

// AccountableNodes returns the sorted set of JSON pointers that MUST be
// rendered or excluded: every top-level property, every $defs entry, and
// every nested property of each $defs entry that has a properties block.
func AccountableNodes(data []byte) ([]string, error) {
	root, err := rootMapping(data)
	if err != nil {
		return nil, err
	}
	var out []string
	if props := mapVal(root, "properties"); props != nil {
		for _, k := range mapKeys(props) {
			out = append(out, "/properties/"+k)
		}
	}
	if defs := mapVal(root, "$defs"); defs != nil {
		for _, name := range mapKeys(defs) {
			out = append(out, "/$defs/"+name)
			if dp := mapVal(mapVal(defs, name), "properties"); dp != nil {
				for _, p := range mapKeys(dp) {
					out = append(out, "/$defs/"+name+"/properties/"+p)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// PointerResolves reports whether a JSON pointer of the shapes this
// package accounts for resolves to a node in the schema. Used to reject
// a stale DeliberateExclusions entry.
func PointerResolves(data []byte, pointer string) (bool, error) {
	root, err := rootMapping(data)
	if err != nil {
		return false, err
	}
	segs := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	n := root
	for _, s := range segs {
		n = mapVal(n, s)
		if n == nil {
			return false, nil
		}
	}
	return true, nil
}
