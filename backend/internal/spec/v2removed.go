package spec

import (
	"fmt"
	"sort"
)

// Messages for the four back-compat surfaces workflow-v2 REMOVES (E52.3 /
// #2215) or RESHAPES (E52.6 / #2218). Raw JSON Schema would reject all four
// — the bare token falls out of the must_page_human enum, `agent` and
// `drive` become undeclared properties of an additionalProperties:false
// block, and a list-form `constraints` fails the object type — but none of
// those generic messages names the replacement surface, which is the whole
// point of the sweep. The CLI carries a byte-identical set in
// cli/internal/spec/validate.go; the two modules are deliberately separate
// (see that package's doc comment), so the strings are kept in lockstep by
// the message-content assertions on both sides rather than by a shared
// constant.
const (
	msgV2RemovedReviewerReject = `page event "reviewer_reject" was removed in workflow-v2: use "advisory_reviewer_reject" (an agent reject under advisory review authority) or "gating_reviewer_reject" (under gating authority)`
	msgV2RemovedReviewersAgent = `reviewers.agent was removed in workflow-v2: declare agent reviewers with reviewers.agents[] (one {provider, model?} entry per reviewer); the effective agent count is len(agents)`
	msgV2RenamedDrive          = `the workflow flag "drive" is spelled "auto_advance" in workflow-v2: rename the key; the semantics are unchanged (fishhawkd auto-advances mechanical transitions, judgment points still park), and v0/v1 keep the "drive" spelling`
	msgV2ReshapedConstraints   = `constraints is an OBJECT in workflow-v2, not a list: write the kinds as one object, e.g. constraints: {max_files_changed: 45, forbidden_paths: ["infra/**"]}; keys are unique, so the one-kind-per-entry list form is gone`
)

// checkV2RemovedForms sweeps a yaml.v3-decoded generic document for the
// four forms workflow-v2 removed or reshaped and returns the first match as
// a *SchemaError naming the replacement surface, or nil.
//
// It runs ONLY for a routed major >= 2 (see ParseBytes) and BEFORE schema
// validation, so its actionable message wins over the generic
// `additional properties 'agent' not allowed` / enum message the same
// document would otherwise produce.
//
// Matching contract — read this before changing the walk. The sweep
// matches by KEY NAME at any depth: any `must_page_human` array carrying
// the string "reviewer_reject", any `reviewers` map carrying an `agent`
// key, any `drive` key, and any `constraints` value that is a LIST. It is
// deliberately NOT position-aware, and it deliberately
// OVER-TRIGGERS in exchange for never missing a legacy form: a legacy
// form sitting in a subtree the v2 schema does not permit at all is still
// reported with the removed-form message, so in an already-invalid
// document the legacy-form message may PRECEDE the genuine structural
// error. That trade is intentional. A position-aware sweep would have to
// re-encode the schema's structural knowledge in Go, and E52 exists to
// restructure that schema — it would rot across the sibling children, and
// a missed position would silently fall back to the unhelpful generic
// message this sweep exists to prevent. Pinned by
// TestCheckV2RemovedForms_MatchesByKeyNameNotPosition.
//
// Nodes that are neither maps nor arrays are skipped, as is a
// `must_page_human` that is not an array, a `reviewers` that is not a map,
// or a `constraints` that is neither a list nor an object — those shapes
// are not a legacy form, and schema validation (which runs next) reports
// them.
func checkV2RemovedForms(raw any) *SchemaError {
	return walkV2RemovedForms(raw, "")
}

// walkV2RemovedForms is checkV2RemovedForms' recursion, carrying the JSON
// pointer of the node under inspection. Map keys are visited in sorted
// order so the reported first match is deterministic across runs (Go's
// map iteration order is randomized).
func walkV2RemovedForms(node any, ptr string) *SchemaError {
	switch v := node.(type) {
	case map[string]any:
		if err := checkV2RemovedAtNode(v, ptr); err != nil {
			return err
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := walkV2RemovedForms(v[k], ptr+"/"+k); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := walkV2RemovedForms(item, fmt.Sprintf("%s/%d", ptr, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkV2RemovedAtNode reports a legacy form declared directly on this
// map node. The four forms are checked in a FIXED order — page event,
// reviewers.agent, then the two E52.6 reshapes (drive, list-form
// constraints) — so a document carrying several always reports the same one.
func checkV2RemovedAtNode(m map[string]any, ptr string) *SchemaError {
	if events, ok := m["must_page_human"].([]any); ok {
		for i, ev := range events {
			if s, ok := ev.(string); ok && s == PageEventReviewerReject {
				return &SchemaError{
					Path:    fmt.Sprintf("%s/must_page_human/%d", ptr, i),
					Message: msgV2RemovedReviewerReject,
				}
			}
		}
	}
	if reviewers, ok := m["reviewers"].(map[string]any); ok {
		if _, ok := reviewers["agent"]; ok {
			return &SchemaError{
				Path:    ptr + "/reviewers/agent",
				Message: msgV2RemovedReviewersAgent,
			}
		}
	}
	if _, ok := m["drive"]; ok {
		return &SchemaError{
			Path:    ptr + "/drive",
			Message: msgV2RenamedDrive,
		}
	}
	if _, ok := m["constraints"].([]any); ok {
		return &SchemaError{
			Path:    ptr + "/constraints",
			Message: msgV2ReshapedConstraints,
		}
	}
	return nil
}
