package spec

import (
	"fmt"
	"sort"
)

// validateAgentVersions is the CLI's one semantic check beyond JSON Schema
// (E32.13 / #1743): the agent_version compatibility range is a plain string
// to the schema, so a malformed range like ">=abc" passes schema validation
// but is a spec authoring error. This sweep walks the raw decoded document —
// workflows -> stages -> executor.agent_version and reviewers.agents[].agent_version
// — and validates each declared range via ValidAgentVersionRange, so
// `fishhawk validate` catches a bad range locally instead of deferring it to
// the backend at dispatch (the same first-line-of-defense role the schema
// validation plays). It operates on the yaml.v3-decoded map[string]any /
// []any tree (never structs — this package is schema-only), tolerating any
// shape mismatch by skipping: the schema layer already rejected genuinely
// malformed structure, so a non-map/non-string node here is simply not an
// agent_version to check.
func validateAgentVersions(raw any) error {
	var errs []ValidationErrorEntry
	root, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	workflows, ok := root["workflows"].(map[string]any)
	if !ok {
		return nil
	}
	for wfName, wfRaw := range workflows {
		wf, ok := wfRaw.(map[string]any)
		if !ok {
			continue
		}
		stages, ok := wf["stages"].([]any)
		if !ok {
			continue
		}
		for i, stRaw := range stages {
			st, ok := stRaw.(map[string]any)
			if !ok {
				continue
			}
			base := fmt.Sprintf("/workflows/%s/stages/%d", wfName, i)
			checkExecutorAgentVersion(st, base, &errs)
			checkReviewerAgentVersions(st, base, &errs)
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// checkExecutorAgentVersion validates a stage's executor.agent_version range.
func checkExecutorAgentVersion(stage map[string]any, base string, errs *[]ValidationErrorEntry) {
	executor, ok := stage["executor"].(map[string]any)
	if !ok {
		return
	}
	appendRangeError(executor["agent_version"], base+"/executor/agent_version", errs)
}

// checkReviewerAgentVersions validates each reviewers.agents[].agent_version range.
func checkReviewerAgentVersions(stage map[string]any, base string, errs *[]ValidationErrorEntry) {
	reviewers, ok := stage["reviewers"].(map[string]any)
	if !ok {
		return
	}
	agents, ok := reviewers["agents"].([]any)
	if !ok {
		return
	}
	for j, agentRaw := range agents {
		agent, ok := agentRaw.(map[string]any)
		if !ok {
			continue
		}
		appendRangeError(agent["agent_version"], fmt.Sprintf("%s/reviewers/agents/%d/agent_version", base, j), errs)
	}
}

// appendRangeError validates a single agent_version node when it is a
// non-empty string, appending a ValidationErrorEntry on a malformed range.
// A non-string or empty value is skipped — absence is not an error, and a
// non-string shape was already rejected by the schema layer.
func appendRangeError(node any, path string, errs *[]ValidationErrorEntry) {
	s, ok := node.(string)
	if !ok || s == "" {
		return
	}
	if err := ValidAgentVersionRange(s); err != nil {
		*errs = append(*errs, ValidationErrorEntry{Path: path, Message: err.Error()})
	}
}

// Messages for the five back-compat surfaces workflow-v2 REMOVES (E52.3 /
// #2215), RESHAPES (E52.6 / #2218) or RENAMES (E52.5 / #2217). Byte-identical
// to backend/internal/spec/v2removed.go's set: `fishhawk validate` is where a
// spec author most often meets these errors, so the CLI must not degrade to
// the generic schema message, and the two surfaces must not drift. The Go
// modules are deliberately separate (see this package's doc comment), so
// the duplication is by design; the message-content assertions on both
// sides are what keep the strings in lockstep.
const (
	msgV2RemovedReviewerReject          = `page event "reviewer_reject" was removed in workflow-v2: use "advisory_reviewer_reject" (an agent reject under advisory review authority) or "gating_reviewer_reject" (under gating authority)`
	msgV2RemovedReviewersAgent          = `reviewers.agent was removed in workflow-v2: declare agent reviewers with reviewers.agents[] (one {provider, model?} entry per reviewer); the effective agent count is len(agents)`
	msgV2RenamedDrive                   = `the workflow flag "drive" is spelled "auto_advance" in workflow-v2: rename the key; the semantics are unchanged (fishhawkd auto-advances mechanical transitions, judgment points still park), and v0/v1 keep the "drive" spelling`
	msgV2ReshapedConstraints            = `constraints is an OBJECT in workflow-v2, not a list: write the kinds as one object, e.g. constraints: {max_files_changed: 45, forbidden_paths: ["infra/**"]}; keys are unique, so the one-kind-per-entry list form is gone`
	msgV2RenamedBudgetMaxRuntimeMinutes = `budget.max_runtime_minutes was renamed to budget.max_runtime in workflow-v2: write the same value as a Go duration string parsed by time.ParseDuration — max_runtime_minutes: 15 becomes max_runtime: 15m; v0/v1 keep the max_runtime_minutes spelling`
)

// legacyPageEventReviewerReject is the bare page-event token workflow-v2
// removed. v0/v1 still accept it; this package only names it to detect it.
const legacyPageEventReviewerReject = "reviewer_reject"

// validateV2RemovedForms sweeps a yaml.v3-decoded generic document for the
// five forms workflow-v2 removed, reshaped or renamed and returns the first
// match as a *ValidationError naming the replacement surface, or nil. It
// mirrors the backend's checkV2RemovedForms exactly, including the ordering
// contract: it runs ONLY for a routed major >= 2 and BEFORE schema validation,
// so the actionable message wins over the generic
// `additional properties 'agent' not allowed` / enum / type message.
//
// Matching contract — read this before changing the walk. The sweep
// matches by KEY NAME at any depth: any `must_page_human` array carrying
// "reviewer_reject", any `reviewers` map carrying an `agent` key, any
// `drive` key, any `constraints` value that is a LIST, and any `budget`
// map carrying a `max_runtime_minutes` key. It is
// deliberately NOT position-aware and deliberately OVER-TRIGGERS in
// exchange for never missing a legacy form, so in an already-invalid
// document the legacy-form message may PRECEDE the genuine structural
// error. A position-aware sweep would re-encode the schema's structural
// knowledge in Go, which E52 is actively restructuring. Nodes that are
// neither maps nor arrays are skipped, as is a non-array
// `must_page_human`, a non-map `reviewers` or `budget`, or a `constraints`
// that is neither a list nor an object — those are not legacy forms, and
// schema validation (which runs next) reports them.
func validateV2RemovedForms(raw any) error {
	if e := walkV2RemovedForms(raw, ""); e != nil {
		return &ValidationError{Errors: []ValidationErrorEntry{*e}}
	}
	return nil
}

// walkV2RemovedForms is validateV2RemovedForms' recursion, carrying the
// JSON pointer of the node under inspection. Map keys are visited in
// sorted order so the reported first match is deterministic across runs.
func walkV2RemovedForms(node any, ptr string) *ValidationErrorEntry {
	switch v := node.(type) {
	case map[string]any:
		if e := checkV2RemovedAtNode(v, ptr); e != nil {
			return e
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if e := walkV2RemovedForms(v[k], ptr+"/"+k); e != nil {
				return e
			}
		}
	case []any:
		for i, item := range v {
			if e := walkV2RemovedForms(item, fmt.Sprintf("%s/%d", ptr, i)); e != nil {
				return e
			}
		}
	}
	return nil
}

// checkV2RemovedAtNode reports a legacy form declared directly on this map
// node. The five forms are checked in a FIXED order — page event,
// reviewers.agent, the two E52.6 reshapes (drive, list-form constraints),
// then the E52.5 budget.max_runtime_minutes rename LAST — so a document
// carrying several always reports the same one.
func checkV2RemovedAtNode(m map[string]any, ptr string) *ValidationErrorEntry {
	if events, ok := m["must_page_human"].([]any); ok {
		for i, ev := range events {
			if s, ok := ev.(string); ok && s == legacyPageEventReviewerReject {
				return &ValidationErrorEntry{
					Path:    fmt.Sprintf("%s/must_page_human/%d", ptr, i),
					Message: msgV2RemovedReviewerReject,
				}
			}
		}
	}
	if reviewers, ok := m["reviewers"].(map[string]any); ok {
		if _, ok := reviewers["agent"]; ok {
			return &ValidationErrorEntry{
				Path:    ptr + "/reviewers/agent",
				Message: msgV2RemovedReviewersAgent,
			}
		}
	}
	if _, ok := m["drive"]; ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/drive",
			Message: msgV2RenamedDrive,
		}
	}
	if _, ok := m["constraints"].([]any); ok {
		return &ValidationErrorEntry{
			Path:    ptr + "/constraints",
			Message: msgV2ReshapedConstraints,
		}
	}
	if budget, ok := m["budget"].(map[string]any); ok {
		if _, ok := budget["max_runtime_minutes"]; ok {
			return &ValidationErrorEntry{
				Path:    ptr + "/budget/max_runtime_minutes",
				Message: msgV2RenamedBudgetMaxRuntimeMinutes,
			}
		}
	}
	return nil
}
