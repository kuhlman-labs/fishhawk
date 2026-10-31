package policy

import (
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

func workflowLaneSpec(t *testing.T) *spec.Spec {
	t.Helper()
	raw, err := os.ReadFile("../../../.fishhawk/workflows.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func laneStage(t *testing.T, s *spec.Spec, workflow, id string) spec.Stage {
	t.Helper()
	for _, st := range s.Workflows[workflow].Stages {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("missing %s.%s", workflow, id)
	return spec.Stage{}
}

func TestWorkflowLanesRoutineEnvelope(t *testing.T) {
	st := laneStage(t, workflowLaneSpec(t), "routine_change", "implement")
	c := st.Constraints[0]
	paths := Constraints{AllowedPaths: c.AllowedPaths, ForbiddenPaths: c.ForbiddenPaths}
	for _, p := range []string{"README.md", "site/src/content/docs/start/first-run.md", "site/src/content/docs/operating/reviews.md"} {
		if v := Evaluate(diff(p), paths); len(v) != 0 {
			t.Errorf("ordinary prose %s rejected: %v", p, v)
		}
	}
	modules, err := os.ReadDir("../../../")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, mod := range modules {
		if !mod.IsDir() {
			continue
		}
		if _, err := os.Stat("../../../" + mod.Name() + "/go.mod"); err != nil {
			continue
		}
		count++
		for _, file := range []string{"go.mod", "go.sum"} {
			p := mod.Name() + "/" + file
			if v := Evaluate(diff(p), paths); len(v) != 0 {
				t.Errorf("module dependency %s rejected: %v", p, v)
			}
		}
	}
	if count == 0 {
		t.Fatal("no module manifests checked")
	}
	for _, p := range []string{"go.mod", "newmodule/go.mod", "docs/spec/workflow-v2.schema.json", "docs/api/v0.openapi.yaml", "docs/adr/001-test.md", "docs/review/security-remit.md", "docs/compliance/planreview-catchrate-evidence.md", "backend/internal/auth/auth_test.go", "backend/internal/server/handlers.go", "backend/internal/prompt/README.md", "AGENTS.md", "a/.Agents/skills/x/SKILL.md", ".fishhawk/workflows.yaml", "frontend/package.json", "frontend/pnpm-lock.yaml", "site/package.json", "site/src/content/docs/reference/api.md"} {
		if v := Evaluate(diff(p), paths); len(v) == 0 {
			t.Errorf("sensitive/unsupported path %s admitted", p)
		}
	}
	if st.Reviewers == nil || st.Reviewers.Authority != "gating" || st.Reviewers.AgentCount() != 2 {
		t.Fatal("routine requires explicit gating dual review")
	}
}

func TestWorkflowLanesVerificationEvidence(t *testing.T) {
	s := workflowLaneSpec(t)
	for _, wf := range []string{"feature_change", "hotfix_change", "routine_change"} {
		st := laneStage(t, s, wf, "implement")
		if st.Executor.Verify == nil || st.Executor.Verify.Command != "scripts/verify-change" {
			t.Fatalf("%s lacks complete verification", wf)
		}
		c := st.Constraints[0]
		if !slices.Contains(c.RequiredOutcomes, "verification_reported") {
			t.Fatalf("%s accepts missing verification", wf)
		}
		if v := Evaluate(diff("README.md"), Constraints{RequiredOutcomes: []string{"verification_reported"}}); len(v) == 0 {
			t.Fatal("absent verification passed")
		}
	}
	if !slices.Contains(s.VerdictTransfer.GlobalSurface, "frontend/**") {
		t.Fatal("frontend verification inputs must invalidate verdict transfer")
	}
}

func TestWorkflowLanesHotfixInheritance(t *testing.T) {
	s := workflowLaneSpec(t)
	for _, id := range []string{"plan", "implement", "acceptance", "review"} {
		f, h := laneStage(t, s, "feature_change", id), laneStage(t, s, "hotfix_change", id)
		if !reflect.DeepEqual(f.Executor, h.Executor) || !reflect.DeepEqual(f.Reviewers, h.Reviewers) || !reflect.DeepEqual(f.Gates, h.Gates) || !reflect.DeepEqual(f.Inputs, h.Inputs) || !reflect.DeepEqual(f.Egress, h.Egress) || !reflect.DeepEqual(f.Concurrency, h.Concurrency) {
			t.Errorf("hotfix %s drifted from shared stage contract", id)
		}
	}
	for _, st := range s.Workflows["hotfix_change"].Stages {
		for _, gate := range st.Gates {
			if gate.Autonomy != "" || gate.Actions != nil {
				t.Fatal("an inherited gate must not override incident delegation")
			}
		}
	}
	st := laneStage(t, s, "hotfix_change", "implement")
	if st.Constraints[0].MaxFilesChanged != 15 {
		t.Fatal("hotfix diff ceiling lost")
	}
	if v := Evaluate(diff("AGENTS.md"), Constraints{ForbiddenPaths: st.Constraints[0].ForbiddenPaths}); len(v) == 0 {
		t.Fatal("inherited instruction exemption must not make root AGENTS writable")
	}
	if s.Workflows["hotfix_change"].Policy.MaxStageRuntime.Duration >= s.Workflows["feature_change"].Policy.MaxStageRuntime.Duration {
		t.Fatal("hotfix runtime policy lost")
	}
}

func TestWorkflowLanesSecurityCoverage(t *testing.T) {
	s := workflowLaneSpec(t)
	convention := s.ReviewConventions["security_conventions"].AppliesTo
	if convention == nil {
		t.Fatal("missing convention predicate")
	}
	for _, wf := range []string{"feature_change", "hotfix_change"} {
		var paths []string
		for _, e := range s.Workflows[wf].Escalations {
			if slices.Contains(e.Require.Reviewers, "security") {
				paths = append(paths, e.Match.Paths...)
			}
		}
		want, got := slices.Clone(convention.Paths), slices.Clone(paths)
		slices.Sort(want)
		slices.Sort(got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s security convention/escalation drift", wf)
		}
		for _, p := range []string{"backend/internal/server/approvals.go", "runner/internal/gateiso/container.go", "runner/cmd/fishhawk-runner/acceptanceenv.go", "scripts/dev", "scripts/test"} {
			if v := Evaluate(diff(p), Constraints{AllowedPaths: paths}); len(v) != 0 {
				t.Errorf("%s security review misses %s", wf, p)
			}
		}
	}
}
