package spec

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
)

// Warning is a non-fatal model-validation finding (#1339). The spec package is
// hard-errors-only otherwise; this is the one advisory channel. Code is a
// stable machine token (today only WarningCodeModelUnverifiable); Path is a
// JSON-Pointer-shaped location into the document; Message is human-readable.
//
// This slice surfaces warnings via the structured logger only — no HTTP
// response field — because with the wired no-data oracle every model is
// unverifiable, so a response warning would fire on every submit. The
// user-facing warning surface lands with #1335.
type Warning struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// WarningCodeModelUnverifiable is emitted when a model could not be verified
// against an authoritative snapshot — the oracle is nil, reported no snapshot
// (ok=false), or reported a stale one (fresh=false). The model is accepted
// (fail-open); the warning records that verification did not happen.
const WarningCodeModelUnverifiable = "model_unverifiable"

// ValidateModels checks every model id named in the spec against the snapshot
// oracle (#1339). It walks each workflow's stages and, for every non-empty
// model field, resolves (field path, provider, model):
//
//   - executor.model — keyed by the provider derived from executor.agent.
//   - reviewers.agents[j].model — keyed by that reviewer's explicit provider.
//
// Severity routing follows the binding contract:
//
//   - oracle == nil, ok == false, or fresh == false → FAIL OPEN: append a
//     WarningCodeModelUnverifiable warning and accept.
//   - fresh && ok && model present in the snapshot → accept silently.
//   - fresh && ok && model ABSENT → HARD ERROR (*ValidationError) naming the
//     field path, the rejected model, a did-you-mean suggestion, and the
//     available set.
//
// All warnings are returned regardless of whether a hard error fires; err is
// the FIRST hard error encountered (deterministic stage/field order), or nil.
// A deprecated/sunset model and a typo both manifest as absence-from-a-fresh
// list and therefore both reject — the minimal Snapshot contract has no
// deprecation channel (deferred to #1335).
func ValidateModels(s *Spec, oracle modeloracle.ModelOracle) (warnings []Warning, err error) {
	if s == nil {
		return nil, nil
	}
	ctx := context.Background()

	// Deterministic workflow order so the first hard error and the warning
	// list are stable across runs (map iteration is randomized).
	workflowIDs := make([]string, 0, len(s.Workflows))
	for id := range s.Workflows {
		workflowIDs = append(workflowIDs, id)
	}
	sort.Strings(workflowIDs)

	check := func(path, provider, model string) {
		if strings.TrimSpace(model) == "" {
			return
		}
		// The verdict routing (unverifiable → warn, verified → accept, rejected →
		// hard error) and the reject-message rendering both live in
		// modeloracle.Verify / Verdict.RejectMessage now (#3578), so the run
		// path, the reviewer-set resolution, and the doctor rung all answer the
		// same question. The Warning message and *ValidationError text below are
		// byte-identical to the pre-refactor inline logic.
		switch v := modeloracle.Verify(ctx, oracle, provider, model); v.Status {
		case modeloracle.ModelUnverifiable:
			// Fail open: no oracle, no snapshot, or a stale one — accept with a
			// warning. Absence from a non-authoritative list cannot reject.
			warnings = append(warnings, Warning{
				Path:    path,
				Code:    WarningCodeModelUnverifiable,
				Message: fmt.Sprintf("model %q (provider %q) could not be verified against a live model snapshot; accepting (fail-open)", model, provider),
			})
		case modeloracle.ModelVerified:
			// Present in a fresh, authoritative snapshot — accept silently.
		case modeloracle.ModelRejected:
			// Authoritative absence → hard reject. Record only the first.
			if err == nil {
				err = &ValidationError{
					Path:    path,
					Message: v.RejectMessage(),
				}
			}
		}
	}

	for _, wid := range workflowIDs {
		wf := s.Workflows[wid]
		for si, st := range wf.Stages {
			base := fmt.Sprintf("/workflows/%s/stages/%d", wid, si)
			check(base+"/executor/model", providerForExecutorAgent(st.Executor.Agent), st.Executor.Model)
			if st.Reviewers != nil {
				for ai, a := range st.Reviewers.Agents {
					check(fmt.Sprintf("%s/reviewers/agents/%d/model", base, ai), a.Provider, a.Model)
				}
			}
		}
	}
	return warnings, err
}

// providerForExecutorAgent maps an executor.agent id to the provider key the
// oracle is queried by. The executor names an agent id ("claude-code" | "codex";
// the runner default is "claude-code") while the reviewer/provider vocabulary
// uses adapter names ("claudecode" | "codex" | "anthropic"). An empty/absent
// agent maps to "claudecode" (the default spawn's adapter); an unrecognized id
// passes through verbatim so a future agent keys its own provider rather than
// silently colliding. Mirrors the server's adapterForImplementAgent so the
// validity layer and the allow-list gate agree on the key. Because the wired
// no-data oracle ignores the key, today's correctness does not depend on the
// exact mapping; this is the thin seam to reconcile with #1335's namespace.
func providerForExecutorAgent(agent string) string {
	switch strings.TrimSpace(agent) {
	case "", "claude-code", "claudecode":
		return "claudecode"
	case "codex":
		return "codex"
	default:
		return strings.TrimSpace(agent)
	}
}

// The model-membership, reject-message rendering, did-you-mean suggestion, and
// Levenshtein helpers moved to modeloracle (verify.go) in #3578 so the
// reviewer-set resolution and the doctor rung reuse the SAME verdict logic this
// validation drives. ValidateModels now routes through modeloracle.Verify +
// Verdict.RejectMessage above; the error and warning text are unchanged.
