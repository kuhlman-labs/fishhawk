package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// implementFailureReasonClauses is the shape of the shared wire fixture
// testdata/wire/implement_failure_reason_clauses.json (#5215, R3).
type implementFailureReasonClauses struct {
	ExampleSHA        string `json:"example_sha"`
	CheckpointClauses []struct {
		Kind           string `json:"kind"`
		RunID          string `json:"run_id"`
		StageID        string `json:"stage_id"`
		Ref            string `json:"ref"`
		ClauseTemplate string `json:"clause_template"`
	} `json:"checkpoint_clauses"`
	AssertionCompoundFragment string `json:"assertion_compound_fragment"`
	AssertionSoleSentinel     string `json:"assertion_sole_sentinel"`
}

func readImplementFailureReasonClauses(t *testing.T) implementFailureReasonClauses {
	t.Helper()
	root := requireWorkspaceRoot(t, pkgSrcDir)
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "wire", "implement_failure_reason_clauses.json")) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("reading the shared implement failure-reason fixture: %v", err)
	}
	var fx implementFailureReasonClauses
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return fx
}

// renderClause substitutes the pinned SHA into a fixture template.
func renderClause(tmpl, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return strings.ReplaceAll(strings.ReplaceAll(tmpl, "<sha12>", short), "<sha>", sha)
}

// TestCheckpointRefClauses_MatchWireFixture pins every entry of the shared
// fixture against its REAL runner producer. The runner and the backend are
// separate modules, so this fixture is the only link between the text the
// runner emits and the backend's checkpointRefIn extractor and
// binding-assertion compound detector (backend/internal/mcpserver
// TestImplementFailureReasonClauses_WireFixture). Update both sides together.
func TestCheckpointRefClauses_MatchWireFixture(t *testing.T) {
	fx := readImplementFailureReasonClauses(t)
	seen := map[string]bool{}
	for _, c := range fx.CheckpointClauses {
		seen[c.Kind] = true
		t.Run(c.Kind, func(t *testing.T) {
			if !strings.HasPrefix(c.Ref, checkpointRefPrefix) {
				t.Fatalf("fixture ref %q does not start with checkpointRefPrefix %q", c.Ref, checkpointRefPrefix)
			}
			switch c.Kind {
			case "held", "stash":
				suffix := checkpointFailureSuffix(checkpointRef(c.RunID, c.StageID), checkpointStashRef(c.RunID, c.StageID))
				if !strings.Contains(suffix, c.ClauseTemplate) {
					t.Errorf("checkpointFailureSuffix = %q\ndoes not contain the fixture clause %q", suffix, c.ClauseTemplate)
				}
				want := checkpointRef(c.RunID, c.StageID)
				if c.Kind == "stash" {
					want = checkpointStashRef(c.RunID, c.StageID)
				}
				if c.Ref != want {
					t.Errorf("fixture ref = %q, producer ref = %q", c.Ref, want)
				}
			case "verify":
				ref := checkpointVerifyRef(c.RunID, c.StageID)
				if ref != c.Ref {
					t.Errorf("fixture ref = %q, checkpointVerifyRef = %q", c.Ref, ref)
				}
				if got, want := verifyHeadFailureSuffix(fx.ExampleSHA, ref), renderClause(c.ClauseTemplate, fx.ExampleSHA); got != want {
					t.Errorf("verifyHeadFailureSuffix = %q\nwant fixture %q", got, want)
				}
			case "agent_kill_timeout", "agent_kill_killed":
				// A timeout pins under -timeout-<n>; every other kill kind (here a
				// loop kill, a non-timeout invoke error) pins under -killed-<n>.
				invokeErr, failure := agent.ErrTimeout, "agent timeout after 1h0m0s"
				if c.Kind == "agent_kill_killed" {
					invokeErr, failure = agent.ErrLoopDetected, "agent loop detected"
				}
				repo, _ := agentKillRepo(t)
				cfg := config{runID: c.RunID, stageID: c.StageID, workingDir: repo}
				res := agent.Result{OK: false, FailureCategory: "A", FailureReason: failure}
				checkpointKilledAgentTree(context.Background(), cfg, "implement", &res, invokeErr, io.Discard)
				sha, ok := refTarget(repo, c.Ref)
				if !ok {
					t.Fatalf("fixture ref %q was not pinned by checkpointKilledAgentTree (reason %q)", c.Ref, res.FailureReason)
				}
				if want := renderClause(c.ClauseTemplate, sha); !strings.HasSuffix(res.FailureReason, want) {
					t.Errorf("reason = %q\nwant it to end with the fixture clause %q", res.FailureReason, want)
				}
			case "fixup":
				repo, base := strandedRepo(t)
				head := strandedCommit(t, repo, "fixup.txt")
				cfg := config{runID: c.RunID, stageID: c.StageID}
				clause := pinStrandedFixupCommit(context.Background(), cfg, io.Discard, repo, base)
				if want := renderClause(c.ClauseTemplate, head); clause != want {
					t.Errorf("pinStrandedFixupCommit clause = %q\nwant fixture %q", clause, want)
				}
				if got, want := checkpointFixupRef(c.RunID, c.StageID, head), renderClause(c.Ref, head); got != want {
					t.Errorf("checkpointFixupRef = %q, fixture ref = %q", got, want)
				}
				if !strings.HasPrefix(checkpointFixupRef(c.RunID, c.StageID, head), checkpointRef(c.RunID, c.StageID)+checkpointFixupSuffix) {
					t.Errorf("fix-up ref does not extend checkpointRef with checkpointFixupSuffix")
				}
			default:
				t.Fatalf("fixture kind %q has no runner producer pin; add one", c.Kind)
			}
		})
	}
	for _, k := range []string{"held", "stash", "verify", "agent_kill_timeout", "agent_kill_killed", "fixup"} {
		if !seen[k] {
			t.Errorf("fixture is missing the %s checkpoint clause", k)
		}
	}

	t.Run("assertion_sole_sentinel", func(t *testing.T) {
		msg := gitops.ErrBindingAssertionUnsatisfied.Error()
		if !strings.Contains(msg, fx.AssertionSoleSentinel) {
			t.Errorf("ErrBindingAssertionUnsatisfied = %q, want it to contain the fixture sentinel %q", msg, fx.AssertionSoleSentinel)
		}
		if strings.Contains(msg, fx.AssertionCompoundFragment) {
			t.Errorf("ErrBindingAssertionUnsatisfied = %q must NOT contain the compound fragment %q (the backend would misread a sole park as compound)", msg, fx.AssertionCompoundFragment)
		}
	})

	t.Run("assertion_compound_fragment", func(t *testing.T) {
		cfg, _, _, _, fu, issued := assertionCompoundHarness(t, "true")
		cfg.scopeFiles = append(cfg.scopeFiles, upload.ScopeFile{Path: "b.txt", Operation: "modify"})
		_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
		if gerr != nil || verifiedTree == "" {
			t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
		}
		err := openPRAndShipArtifact(context.Background(), cfg, io.Discard, fu, issued, "", false, false, nil, false, verifiedTree, "", unsatisfiedBinding, nil, nil, nil)
		if err == nil {
			t.Fatal("compound shortfall returned nil; want the category-B compound abort")
		}
		if !strings.Contains(err.Error(), fx.AssertionCompoundFragment) {
			t.Errorf("compound abort = %q\nwant it to contain the fixture fragment %q", err.Error(), fx.AssertionCompoundFragment)
		}
	})
}
