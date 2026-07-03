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
	// numbered type (e.g. existing ADR numbers), supplied so Apply can
	// allocate the next one. For the GitHub provider these are now DISCOVERED
	// server-side by the filing handler from the tracker when omitted (#1269);
	// a caller-supplied list is an optional override/hint that short-circuits
	// that discovery. Apply itself stays pure: it only reads this field. For a
	// numbered type an empty list still fails the apply closed rather than
	// allocating 1 (#1265) — allocateNumber remains the final fail-closed guard
	// for providers/paths where discovery did not run. A genuinely-first
	// numbered item is filed with a non-empty seed whose max is 0
	// (existing_numbers:[0] -> 1). Ignored for non-numbered types.
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
		if err := validateSections(req.Sections, itemType.BodySkeleton); err != nil {
			return WorkItem{}, 0, err
		}
		body = assembleBody(itemType, req.Sections)
	}

	labels := mergeLabels(itemType.DefaultLabels, req.Labels)
	labels, defaulted, missing := applyLabelCompleteness(itemType, labels)

	item := WorkItem{
		Type:  req.Type,
		Title: title,
		Body:  body,
		Classification: Classification{
			Labels:                 labels,
			Complexity:             complexity,
			DefaultedLabels:        defaulted,
			MissingLabelNamespaces: missing,
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
// (max(existing)+1), or 0 for an unnumbered type. Only the "sequential"
// scheme is supported in v0; any other scheme fails closed.
//
// For a numbered type the numbers already in use must be supplied via
// ExistingNumbers: an empty list fails closed with a *SemanticError rather
// than defaulting to 1, so a numbered filing can never silently ship a wrong
// number (#1265). For the GitHub provider the filing handler now DISCOVERS
// those numbers server-side from the tracker and seeds this field before Apply
// (#1269), so a caller need not pass them; this fail-closed guard remains the
// last line for providers/paths where discovery did not run (a provider
// without the NumberDiscoverer capability, or a discovery that was skipped).
// The empty case is keyed on len(existing)==0 because the MCP->backend hop's
// `omitempty` tag makes an omitted and an explicit-empty list indistinguishable
// at the backend. To file a genuinely-first numbered item, seed a non-empty
// list whose max is 0 — existing_numbers:[0] yields 1 and survives the
// omitempty hop (this is also the seed the handler's discovery uses for an
// empty result).
func allocateNumber(itemType ItemType, existing []int) (int, error) {
	if itemType.Numbering == nil {
		return 0, nil
	}
	if itemType.Numbering.Scheme != "sequential" {
		return 0, &SemanticError{Msg: fmt.Sprintf(
			"unsupported numbering scheme %q (only \"sequential\" is supported)", itemType.Numbering.Scheme)}
	}
	if len(existing) == 0 {
		// Carry structured Details so the handler surfaces the cause in the
		// 422 response (mirroring the renderTitle missing_placeholders
		// precedent above). The Msg states how to supply it.
		return 0, &SemanticError{
			Msg: fmt.Sprintf(
				"existing_numbers is required for the numbered type %q: pass the numbers already in use so the next sequential number can be allocated; for a genuinely-first numbered item pass a seed such as existing_numbers:[0] (which yields 1)",
				itemType.Numbering.Prefix),
			Details: map[string]any{
				"numbered_type":             itemType.Numbering.Prefix,
				"existing_numbers_required": true,
			},
		}
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
// with a literal `{epic}` in its title. pad zero-pads the {number}
// substitution to a minimum width; pad<=0 renders the bare integer (the
// "%0*d" width-from-arg form is identical to "%d" when pad<=0), so types
// that declare no numbering.pad are unchanged.
func renderTitle(format, summary string, number, pad int, vars map[string]string) (string, error) {
	if strings.TrimSpace(format) == "" {
		return summary, nil
	}
	subs := map[string]string{"summary": summary}
	if number > 0 {
		subs["number"] = fmt.Sprintf("%0*d", pad, number)
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
		missing = dedup(missing)
		// Carry the structured missing-placeholder list so the handler can
		// surface it in details.missing_placeholders (#1184) — the human Msg
		// is kept verbatim so existing substring assertions still pass.
		return "", &SemanticError{
			Msg: fmt.Sprintf(
				"title_format %q has unresolved placeholder(s): %s", format, strings.Join(missing, ", ")),
			Details: map[string]any{
				"missing_placeholders": missing,
				"title_format":         format,
			},
		}
	}
	return out, nil
}

// resolveRelations validates the caller's relations against the type's
// epic_link rule: "required" rejects a missing parent epic, "none"
// rejects a supplied one, "optional" (and empty) accepts either. It also
// format-validates each depends_on entry as a well-formed issue reference
// (`#N` or `N`, positive integer); existence and cycle checks are NOT done
// here (Apply is pure, and cycle detection needs the full assembled DAG) —
// they are deferred to campaign-assembly time (E25.3 / #1437).
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
	for _, dep := range rel.DependsOn {
		if !isWellFormedIssueRef(dep) {
			return Relations{}, &SemanticError{Msg: fmt.Sprintf(
				"depends_on entry %q is not a well-formed issue reference (expected #N or N, a positive integer)", dep)}
		}
	}
	return rel, nil
}

// issueRefRE matches a well-formed issue reference: an optional leading `#`
// then a positive integer, surrounding whitespace tolerated. It mirrors the
// github provider's parseIssueRef so a depends_on edge validates at file
// time the same way the parent-epic ref resolves at provider time.
var issueRefRE = regexp.MustCompile(`^\s*#?([1-9]\d*)\s*$`)

// isWellFormedIssueRef reports whether ref is a `#N` or `N` positive-integer
// issue reference.
func isWellFormedIssueRef(ref string) bool {
	return issueRefRE.MatchString(ref)
}

// assembleBody renders a markdown skeleton when the caller supplies no
// body: one `## Section` heading per body_skeleton entry, followed by the
// matching Sections content (empty when absent).
//
// A section named in the type's optional_sections is skipped entirely — no
// heading, no trailing blank block — when the Sections map has no key for it.
// Presence with an empty value is NOT absence: a present-but-empty key renders
// its heading in position exactly like a mandatory section. The blank-line
// separator keys on whether a section has already been written (not the loop
// index), so an omitted optional section produces output byte-identical to a
// skeleton that never listed it — the additive guarantee (#1615).
func assembleBody(it ItemType, sections map[string]string) string {
	optional := make(map[string]bool, len(it.OptionalSections))
	for _, s := range it.OptionalSections {
		optional[s] = true
	}
	var b strings.Builder
	wrote := false
	for _, section := range it.BodySkeleton {
		if _, ok := sections[section]; !ok && optional[section] {
			continue
		}
		if wrote {
			b.WriteString("\n")
		}
		wrote = true
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

// validateSections fails loud when the caller keys Sections off the type's
// body_skeleton (#1184). assembleBody renders only skeleton sections and
// looks each up by exact name, so a section keyed off-skeleton would be
// silently dropped — the caller's content vanishing with no error. This
// rejects that path: any Sections key matching no skeleton section returns
// a SemanticError naming the unknown key(s) and the expected skeleton
// names, with structured Details for the 422 response. Exact-match is
// deliberate (consistent with assembleBody's sections[section] lookup) so a
// near-miss like "Done means" vs skeleton "Done-means" is reported rather
// than rendered under the wrong heading. An empty Sections is a no-op.
func validateSections(sections map[string]string, skeleton []string) error {
	if len(sections) == 0 {
		return nil
	}
	known := make(map[string]bool, len(skeleton))
	for _, s := range skeleton {
		known[s] = true
	}
	var unknown []string
	for key := range sections {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &SemanticError{
		Msg: fmt.Sprintf(
			"sections key(s) %s do not match the type's body skeleton; expected one of: %s",
			strings.Join(unknown, ", "), strings.Join(skeleton, ", ")),
		Details: map[string]any{
			"unknown_sections":  unknown,
			"expected_sections": skeleton,
		},
	}
}

// applyLabelCompleteness runs the fail-open label-completeness pass over the
// already-merged label set (#1616). It NEVER returns an error — a filing is
// never rejected on labels alone.
//
//  1. For each label_defaults key (sorted), if the merged set has no label in
//     that namespace (no label with the "<key>:" prefix), the configured
//     default label is appended and recorded in defaulted.
//  2. For each required_label_namespaces entry (sorted), if the
//     default-augmented set still has no label in that namespace, the
//     namespace is recorded in missing.
//
// The namespace match is prefix-based, not exact-string: a caller-supplied
// label in the namespace (e.g. autonomy:high) suppresses the default entirely,
// and existing labels are never rewritten or reordered.
func applyLabelCompleteness(itemType ItemType, merged []string) (labels, defaulted, missing []string) {
	labels = merged
	for _, key := range sortedKeys(itemType.LabelDefaults) {
		if hasLabelInNamespace(labels, key) {
			continue
		}
		def := itemType.LabelDefaults[key]
		labels = append(labels, def)
		defaulted = append(defaulted, def)
	}
	for _, ns := range sortedStrings(itemType.RequiredLabelNamespaces) {
		if !hasLabelInNamespace(labels, ns) {
			missing = append(missing, ns)
		}
	}
	return labels, defaulted, missing
}

// hasLabelInNamespace reports whether any label carries the "<ns>:" prefix.
func hasLabelInNamespace(labels []string, ns string) bool {
	prefix := ns + ":"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// sortedStrings returns a sorted copy of in, so a completeness pass over a
// namespace list produces a stable order without mutating the shared config
// slice.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
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
