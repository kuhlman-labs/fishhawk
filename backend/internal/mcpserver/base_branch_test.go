package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// baseBranchRemedy is the remedy every base_branch_unresolved refusal names
// (approval condition C1(a), #4758).
const baseBranchRemedy = "Pass base_branch explicitly"

// allDefaultBranchReasons is defaultbranch's closed reason set (#4660) plus the
// MCP-local not_reported (both fields absent, a pre-#4660 fishhawkd).
var allDefaultBranchReasons = []string{
	string(defaultbranch.ReasonResolverUnconfigured),
	string(defaultbranch.ReasonForgeUnsupported),
	string(defaultbranch.ReasonNoInstallation),
	string(defaultbranch.ReasonRepositoryInvalid),
	string(defaultbranch.ReasonRepositoryNotVisible),
	string(defaultbranch.ReasonForbidden),
	string(defaultbranch.ReasonDefaultBranchMissing),
	string(defaultbranch.ReasonLookupFailed),
	baseBranchNotReported,
}

// TestResolveSpawnBaseBranch pins the ONE base-branch derivation every spawn
// verb calls (#4758), one assertion per branch: an explicit base_branch wins
// over a resolved default and over EVERY unresolved reason (including
// not_reported, C1(d)); a resolved default is returned; every reason —
// resolver_unconfigured and no_installation included (C1(c)), forge_unsupported
// on a gitlab target, and not_reported when both fields are absent — is named
// in the refusal together with the remedy.
func TestResolveSpawnBaseBranch(t *testing.T) {
	runID := uuid.New()
	// targetFor builds the target a single-run read carrying reason yields;
	// not_reported is the both-fields-empty read.
	targetFor := func(reason string) runForgeTarget {
		tgt := runForgeTarget{Forge: startRunForgeGitHub, Repo: "x/y"}
		if reason == string(defaultbranch.ReasonForgeUnsupported) {
			tgt = runForgeTarget{Forge: startRunForgeGitLab, GitLabBaseURL: "https://gitlab.example.com", Repo: "acme/widgets"}
		}
		if reason != baseBranchNotReported {
			tgt.DefaultBranchUnresolvedReason = reason
		}
		return tgt
	}

	t.Run("explicit_wins_over_resolved_default", func(t *testing.T) {
		got, err := runForgeTarget{Forge: startRunForgeGitHub, DefaultBranch: "trunk"}.resolveSpawnBaseBranch(runID, "release/x")
		if err != nil || got != "release/x" {
			t.Fatalf("got (%q, %v), want release/x", got, err)
		}
	})
	t.Run("resolved_default_returned", func(t *testing.T) {
		got, err := runForgeTarget{Forge: startRunForgeGitHub, DefaultBranch: "trunk"}.resolveSpawnBaseBranch(runID, "")
		if err != nil || got != "trunk" {
			t.Fatalf("got (%q, %v), want trunk", got, err)
		}
	})
	for _, reason := range allDefaultBranchReasons {
		t.Run("explicit_wins_over_"+reason, func(t *testing.T) {
			got, err := targetFor(reason).resolveSpawnBaseBranch(runID, "release/x")
			if err != nil || got != "release/x" {
				t.Fatalf("got (%q, %v), want the explicit release/x", got, err)
			}
		})
		t.Run("refuses_naming_"+reason, func(t *testing.T) {
			got, err := targetFor(reason).resolveSpawnBaseBranch(runID, "")
			if err == nil {
				t.Fatalf("got %q, want a refusal naming %s", got, reason)
			}
			if got != "" {
				t.Errorf("a refusal must return no branch, got %q", got)
			}
			for _, want := range []string{
				"base_branch_unresolved",
				"default_branch_unresolved_reason: " + reason,
				runID.String(),
				baseBranchRemedy,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal should contain %q: %v", want, err)
				}
			}
		})
	}
}

// TestSpawnVerbSchemas_BaseBranchDescriptions (approval condition C5) reads
// the REGISTERED tool schemas over ListTools: none of the four spawn verbs'
// base_branch input descriptions may still say "defaults to main", each names
// default_branch, and drive_run's stopped_reason vocabulary carries
// base_branch_unresolved.
func TestSpawnVerbSchemas_BaseBranchDescriptions(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()
	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		tools[tl.Name] = tl
	}
	propDesc := func(t *testing.T, schema any, prop string) string {
		t.Helper()
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		var s struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode schema: %v", err)
		}
		p, ok := s.Properties[prop]
		if !ok {
			t.Fatalf("schema has no %q property: %s", prop, raw)
		}
		return p.Description
	}
	for _, name := range []string{"fishhawk_run_stage", "fishhawk_dispatch_stage", "fishhawk_drive_run", "fishhawk_run_children"} {
		tool := tools[name]
		if tool == nil {
			t.Errorf("%s not registered/visible over ListTools", name)
			continue
		}
		desc := propDesc(t, tool.InputSchema, "base_branch")
		if strings.Contains(strings.ToLower(desc), "defaults to main") {
			t.Errorf("%s base_branch description still claims it defaults to main (#4758):\n%s", name, desc)
		}
		if !strings.Contains(desc, "default_branch") {
			t.Errorf("%s base_branch description should name the run's default_branch (#4758):\n%s", name, desc)
		}
	}
	if drive := tools["fishhawk_drive_run"]; drive != nil {
		if desc := propDesc(t, drive.OutputSchema, "stopped_reason"); !strings.Contains(desc, stoppedBaseBranchUnresolved) {
			t.Errorf("fishhawk_drive_run stopped_reason vocabulary is missing %s (#4758):\n%s", stoppedBaseBranchUnresolved, desc)
		}
	}
}

// --- cross-boundary: the REAL server's GET /v0/runs/{run_id} -> argv -------

// e2eDispatchNoBaseBranch dispatches st through the real dispatchStage with
// base_branch OMITTED.
func e2eDispatchNoBaseBranch(t *testing.T, f *e2eSlotFixture, st *runpkg.Stage) error {
	t.Helper()
	_, _, err := f.r.dispatchStage(f.ctx, nil, DispatchStageInput{
		RunID: st.RunID.String(), StageID: st.ID.String(), Workflow: "feature_change", Stage: "implement",
		WorkingDir: t.TempDir(), GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
	})
	return err
}

// TestSpawnBaseBranch_E2E_RealServerDefaultBranch drives the REAL server.New
// (pgtest Postgres) with a defaultbranch.Static("trunk") source through the
// real apiClient and dispatchStage: handleGetRun -> JSON -> client Run mirror
// -> resolveRunForgeTarget -> resolveSpawnBaseBranch -> the recorded detached
// spawn argv carries --base-branch trunk.
func TestSpawnBaseBranch_E2E_RealServerDefaultBranch(t *testing.T) {
	f := newE2ESlotFixtureWithDefaultBranch(t, nil, defaultbranch.Static("trunk"))
	st := f.park(t, nil)

	if err := e2eDispatchNoBaseBranch(t, f, st); err != nil {
		t.Fatalf("dispatchStage: %v", err)
	}
	argvs := f.rec.spawnedArgvs()
	if len(argvs) != 1 {
		t.Fatalf("recorded %d spawns, want 1", len(argvs))
	}
	if !argvHasPair(argvs[0], "--base-branch", "trunk") || !argvHasPair(argvs[0], "--check-base-ref", "trunk") {
		t.Fatalf("spawn argv should carry the server-resolved default branch trunk: %v", argvs[0])
	}
}

// TestSpawnBaseBranch_E2E_RealServerUnresolvedRefuses: the same seam with an
// UNCONFIGURED source (nil) → the real server reports resolver_unconfigured,
// the dispatch refuses naming it, and the COMMITTED state shows nothing
// happened: the stage read back from the run repository is still
// awaiting_host_dispatch and no spawn was recorded.
func TestSpawnBaseBranch_E2E_RealServerUnresolvedRefuses(t *testing.T) {
	f := newE2ESlotFixtureWithDefaultBranch(t, nil, nil)
	st := f.park(t, nil)

	err := e2eDispatchNoBaseBranch(t, f, st)
	if err == nil {
		t.Fatal("expected a base_branch_unresolved refusal from an unconfigured resolver")
	}
	for _, want := range []string{"base_branch_unresolved", string(defaultbranch.ReasonResolverUnconfigured), baseBranchRemedy} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should contain %q: %v", want, err)
		}
	}
	if got := f.stageState(t, st); got != string(runpkg.StageStateAwaitingHostDispatch) {
		t.Fatalf("stage state read back = %q, want %s (the refusal must commit nothing)", got, runpkg.StageStateAwaitingHostDispatch)
	}
	if n := len(f.rec.spawnedArgvs()); n != 0 {
		t.Fatalf("recorded %d spawns despite the refusal", n)
	}
}
