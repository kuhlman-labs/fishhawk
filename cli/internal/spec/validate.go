package spec

import "fmt"

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
