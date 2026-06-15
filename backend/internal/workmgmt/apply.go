package workmgmt

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FilingRequest is the raw caller input to Apply — what the MCP tool,
// CLI verb, and filing endpoint collect before the repo's conventions
// are applied. Apply turns it into a conventions-complete WorkItem.
type FilingRequest struct {
	// Type names the work-item type; must be a key in the conventions'
	// types map (bug/feature/chore/adr/…).
	Type string
	// Summary fills the {summary} placeholder in title_format and is the
	// mandatory one-line description.
	Summary string
	// Body is a caller-assembled body. When set it is used verbatim;
	// when empty Apply assembles a skeleton from the type's body_skeleton
	// and Sections.
	Body string
	// Sections supplies per-skeleton-section content keyed by section
	// name; consulted only when Body is empty.
	Sections map[string]string
	// TitleVars supplies any title_format placeholders beyond {summary}
	// and {number} (e.g. {epic}, {n} for a feature). An unresolved
	// placeholder fails the apply closed.
	TitleVars map[string]string
	// Labels are caller-supplied labels merged on top of the type's
	// default_labels.
	Labels []string
	// Complexity overrides the type's default complexity; must be one of
	// the conventions' complexity_levels when those are declared.
	Complexity string
	// Status overrides the type's default board status.
	Status string
	// Relations links the item to other work; validated against the
	// type's epic_link rule.
	Relations Relations
	// ExistingNumbers are the sequential numbers already in use for a
	// numbered type (e.g. existing ADR numbers parsed from the board),
	// supplied so Apply can allocate the next one. Ignored for
	// non-numbered types.
	ExistingNumbers []int
}

// placeholderRE matches a `{name}` title_format placeholder.
var placeholderRE = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// Apply resolves a FilingRequest against the repo's conventions into a
// conventions-complete WorkItem plus the allocated sequential number (0
// when the type is not numbered). It renders the title, assembles or
// passes through the body, merges labels, resolves complexity and board
// placement, allocates the numbering, and validates the relations against
// the type's epic_link rule. It is pure — provider I/O (querying existing
// numbers, the actual create/link) lives in the provider — so the
// conventions layer, the load-bearing value, is fully unit-testable.
//
// Errors are *SemanticError: an unknown type, a missing mandatory field
// (Summary / complexity), an unresolved title placeholder, an unknown
// complexity level, or an epic_link rule violation.
func Apply(req FilingRequest, conv Conventions) (WorkItem, int, error) {
	itemType, ok := conv.Types[req.Type]
	if !ok {
		return WorkItem{}, 0, &SemanticError{Msg: fmt.Sprintf(
			"unknown work-item type %q; known types: %s",
			req.Type, strings.Join(sortedTypeNames(conv.Types), ", "))}
	}

	if strings.TrimSpace(req.Summary) == "" {
		return WorkItem{}, 0, &SemanticError{Msg: "Summary is required"}
	}

	complexity, err := resolveComplexity(req.Complexity, itemType.DefaultFields.Complexity, conv.ComplexityLevels)
	if err != nil {
		return WorkItem{}, 0, err
	}

	number, err := allocateNumber(itemType, req.ExistingNumbers)
	if err != nil {
		return WorkItem{}, 0, err
	}

	pad := 0
	if itemType.Numbering != nil {
		pad = itemType.Numbering.Pad
	}
	title, err := renderTitle(itemType.TitleFormat, req.Summary, number, pad, req.TitleVars)
	if err != nil {
		return WorkItem{}, 0, err
	}

	relations, err := resolveRelations(req.Type, itemType.EpicLink, req.Relations)
	if err != nil {
		return WorkItem{}, 0, err
	}

	body := req.Body
	if strings.TrimSpace(body) == "" {
		body = assembleBody(itemType.BodySkeleton, req.Sections)
	}

	item := WorkItem{
		Type:  req.Type,
		Title: title,
		Body:  body,
		Classification: Classification{
			Labels:     mergeLabels(itemType.DefaultLabels, req.Labels),
			Complexity: complexity,
		},
		BoardPlacement: BoardPlacement{
			Status:      firstNonEmpty(req.Status, itemType.DefaultFields.Status),
			BoardColumn: itemType.DefaultFields.BoardColumn,
		},
		Relations: relations,
	}
	return item, number, nil
}

// resolveComplexity picks the override or the type default and validates
// it against the declared complexity_levels (when any are declared). The
// mandatory trio requires complexity, so an unresolved value fails.
func resolveComplexity(override, typeDefault string, levels map[string]string) (string, error) {
	c := firstNonEmpty(override, typeDefault)
	if c == "" {
		return "", &SemanticError{Msg: "complexity is required (no value supplied and the type declares no default)"}
	}
	if len(levels) > 0 {
		if _, ok := levels[c]; !ok {
			return "", &SemanticError{Msg: fmt.Sprintf(
				"unknown complexity %q; known levels: %s", c, strings.Join(sortedKeys(levels), ", "))}
		}
	}
	return c, nil
}

// allocateNumber returns the next sequential number for a numbered type
// (max(existing)+1, or 1 when none exist), or 0 for an unnumbered type.
// Only the "sequential" scheme is supported in v0; any other scheme fails
// closed.
func allocateNumber(itemType ItemType, existing []int) (int, error) {
	if itemType.Numbering == nil {
		return 0, nil
	}
	if itemType.Numbering.Scheme != "sequential" {
		return 0, &SemanticError{Msg: fmt.Sprintf(
			"unsupported numbering scheme %q (only \"sequential\" is supported)", itemType.Numbering.Scheme)}
	}
	max := 0
	for _, n := range existing {
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

// renderTitle substitutes {summary}, {number} (for numbered types), and
// any caller-supplied TitleVars into title_format. An empty title_format
// yields the bare summary. Any placeholder left unresolved fails closed,
// so a feature missing its {epic}/{n} vars is rejected rather than filed
// with a literal `{epic}` in its title.
func renderTitle(format, summary string, number, pad int, vars map[string]string) (string, error) {
	if strings.TrimSpace(format) == "" {
		return summary, nil
	}
	subs := map[string]string{"summary": summary}
	if number > 0 {
		if pad > 0 {
			subs["number"] = fmt.Sprintf("%0*d", pad, number)
		} else {
			subs["number"] = fmt.Sprintf("%d", number)
		}
	}
	for k, v := range vars {
		subs[k] = v
	}

	var missing []string
	out := placeholderRE.ReplaceAllStringFunc(format, func(m string) string {
		key := m[1 : len(m)-1]
		if v, ok := subs[key]; ok {
			return v
		}
		missing = append(missing, key)
		return m
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", &SemanticError{Msg: fmt.Sprintf(
			"title_format %q has unresolved placeholder(s): %s", format, strings.Join(dedup(missing), ", "))}
	}
	return out, nil
}

// resolveRelations validates the caller's relations against the type's
// epic_link rule: "required" rejects a missing parent epic, "none"
// rejects a supplied one, "optional" (and empty) accepts either.
func resolveRelations(typeName, epicLink string, rel Relations) (Relations, error) {
	hasEpic := strings.TrimSpace(rel.ParentEpic) != ""
	switch epicLink {
	case "required":
		if !hasEpic {
			return Relations{}, &SemanticError{Msg: fmt.Sprintf("type %q requires a parent epic relation", typeName)}
		}
	case "none":
		if hasEpic {
			return Relations{}, &SemanticError{Msg: fmt.Sprintf("type %q does not take a parent epic relation", typeName)}
		}
	}
	return rel, nil
}

// assembleBody renders a markdown skeleton when the caller supplies no
// body: one `## Section` heading per body_skeleton entry, followed by the
// matching Sections content (empty when absent).
func assembleBody(skeleton []string, sections map[string]string) string {
	var b strings.Builder
	for i, section := range skeleton {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## ")
		b.WriteString(section)
		b.WriteString("\n\n")
		if content, ok := sections[section]; ok {
			b.WriteString(strings.TrimRight(content, "\n"))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// mergeLabels concatenates default + extra labels, deduplicating while
// preserving order (defaults first).
func mergeLabels(defaults, extra []string) []string {
	seen := make(map[string]bool, len(defaults)+len(extra))
	var out []string
	for _, l := range append(append([]string{}, defaults...), extra...) {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
