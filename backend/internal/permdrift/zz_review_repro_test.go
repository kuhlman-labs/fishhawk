package permdrift

// Review-finding repros for the E80.4 permission-drift check (#3934). Each
// test asserts that a REAL widening is reported. A FAIL confirms the finding
// (the widening goes undetected); a PASS refutes it.

import (
	"os"
	"strings"
	"testing"
)

func reproSurface(t *testing.T, id string) Surface {
	t.Helper()
	for _, s := range DefaultSurfaces() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no surface %q", id)
	return Surface{}
}

func reproRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func reproReplaceOnce(t *testing.T, src, old, repl string) string {
	t.Helper()
	if strings.Count(src, old) != 1 {
		t.Fatalf("anchor %q occurs %d times; want exactly 1", old, strings.Count(src, old))
	}
	return strings.Replace(src, old, repl, 1)
}

// requireWidening fails unless Detect is evaluable and reports a widening on
// wantKey.
func requireWidening(t *testing.T, r Result, wantKey string) {
	t.Helper()
	if r.Unevaluable != "" {
		t.Fatalf("Detect unevaluable %q (%s); the check failed closed, so the finding does not hold as stated", r.Unevaluable, r.Detail)
	}
	for _, c := range r.Widened {
		if c.Key == wantKey {
			return
		}
	}
	t.Errorf("widening on %q NOT reported.\n  widened:  %+v\n  narrowed: %+v", wantKey, r.Widened, r.Narrowed)
}

// F1: a gate whose class flips from "workflow auto, gate gated" (Restriction)
// to "workflow gated, gate auto" (Grant) keeps the SAME key at the SAME rank,
// so Compare sees no change at the gate.
func TestReviewRepro_F1_GateAutonomyPolarityFlip(t *testing.T) {
	base := specDoc("%ESCALATIONS%", "",
		"%AUTONOMY%", "    autonomy: high",
		"%GATE%", "            actions:\n              merge:\n                mode: gated")
	head := specDoc("%ESCALATIONS%", "",
		"%AUTONOMY%", "    autonomy: high\n    actions:\n      merge:\n        mode: gated",
		"%GATE%", "            actions:\n              merge:\n                mode: auto\n                when: gates_resolved_ci_green")
	// Sanity: the extractor really sees the gate as restricted at base and
	// granting at head under one key.
	bg, err := ExtractSpecAutonomy([]byte(base))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	hg, err := ExtractSpecAutonomy([]byte(head))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	key := "workflows.feature_change.stages.plan.gates[0].actions.merge"
	t.Logf("base %s = %+v; head %s = %+v", key, bg[key], key, hg[key])

	r := Detect(reproSurface(t, "fishhawk-spec-autonomy"), side(base), side(head))
	requireWidening(t, r, key)
}

// F4: a job with no permissions block is a jobs.<id>.* write wildcard, which
// subsumes a later explicit `id-token: write` — a scope GitHub's default
// token never carries.
func TestReviewRepro_F4_IDTokenAddedToBlocklessJob(t *testing.T) {
	const tmpl = `name: ci
on: push
jobs:
  build:
    runs-on: ubuntu-latest
%PERMS%
    steps:
      - run: echo
`
	base := strings.Replace(tmpl, "%PERMS%", "", 1)
	head := strings.Replace(tmpl, "%PERMS%", "    permissions:\n      id-token: write", 1)
	r := Detect(reproSurface(t, "gha-workflow-permissions"), side(base), side(head))
	requireWidening(t, r, "jobs.build.id-token")
}

// F5: copy(scopes, ...) overwrites an element of the token scope slice in
// place; checkStrayUses inspects &scopes, append-not-assigned, and unvisited
// assignments, but not a builtin call writing through the slice.
func TestReviewRepro_F5_CopyIntoScopes(t *testing.T) {
	base := reproRead(t, "../server/mcptoken.go")
	head := reproReplaceOnce(t, base,
		"\tscopes := []string{\"mcp:read\"}\n",
		"\tscopes := []string{\"mcp:read\"}\n\tcopy(scopes, []string{\"admin:write\"})\n")
	r := Detect(reproSurface(t, "run-token-scope-grants"), side(base), side(head))
	if r.Unevaluable == ReasonShapeUnrecognized {
		t.Logf("caught by the shape guard: %s", r.Detail)
		return
	}
	requireWidening(t, r, "run_token.admin:write@any_stage")
}

// F6: the env allow-lists and mcpToolScopes are read only from their
// package-level declaration literal; an init() mutation is invisible.
func TestReviewRepro_F6_InitMutation(t *testing.T) {
	t.Run("env allow-list appended in init", func(t *testing.T) {
		base := reproRead(t, "../reviewsandbox/env.go")
		head := base + "\nfunc init() { BaseAllow = append(BaseAllow, \"AWS_SECRET_ACCESS_KEY\") }\n"
		r := Detect(reproSurface(t, "reviewer-env-allowlist"), side(base), side(head))
		if r.Unevaluable == ReasonShapeUnrecognized {
			t.Logf("caught by the shape guard: %s", r.Detail)
			return
		}
		requireWidening(t, r, "env_allow.BaseAllow.AWS_SECRET_ACCESS_KEY")
	})
	t.Run("mcpToolScopes entry relaxed in init", func(t *testing.T) {
		base := reproRead(t, "../server/mcpscopes.go")
		head := base + "\nfunc init() { mcpToolScopes[\"fishhawk_merge_run\"] = mcpToolScopeRule{} }\n"
		r := Detect(reproSurface(t, "mcp-tool-scopes"), side(base), side(head))
		if r.Unevaluable == ReasonShapeUnrecognized {
			t.Logf("caught by the shape guard: %s", r.Detail)
			return
		}
		requireWidening(t, r, "mcp_tool.fishhawk_merge_run.any_of.*")
	})
}

// F7: OR-ing a non-stage term into a stage guard widens the grant to every
// stage, but collect() only treats != / ! as "any stage", so the grant stays
// keyed @implement on both sides.
func TestReviewRepro_F7_StageGuardOredWithNonStageTerm(t *testing.T) {
	base := reproRead(t, "../server/mcptoken.go")
	head := reproReplaceOnce(t, base,
		"\tif stageType == run.StageTypeImplement {\n",
		"\tif stageType == run.StageTypeImplement || r.Header.Get(\"X-Debug\") == \"1\" {\n")
	r := Detect(reproSurface(t, "run-token-scope-grants"), side(base), side(head))
	if r.Unevaluable == ReasonShapeUnrecognized {
		t.Logf("caught by the shape guard: %s", r.Detail)
		return
	}
	requireWidening(t, r, "run_token.write:scope-amendments@any_stage")
}
