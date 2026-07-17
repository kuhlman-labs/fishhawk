package runnerbackend

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- fakes ---

type dispatchCall struct {
	Scope        forge.CredentialScope
	Repo         githubclient.RepoRef
	WorkflowFile string
	Ref          string
	Inputs       githubclient.DispatchInputs
}

type fakeDispatchClient struct {
	calls []dispatchCall
	err   error
}

func (f *fakeDispatchClient) DispatchWorkflow(_ context.Context, scope forge.CredentialScope,
	repo githubclient.RepoRef, file, ref string, inputs githubclient.DispatchInputs) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, dispatchCall{Scope: scope, Repo: repo, WorkflowFile: file, Ref: ref, Inputs: inputs})
	return nil
}

// fakeRuns resolves parent runs for the decomposed-child resolver branches.
type fakeRuns struct {
	byID map[uuid.UUID]*run.Run
	err  error
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.byID[id]
	if !ok {
		return nil, errors.New("run not found")
	}
	return r, nil
}

func newResolver(runs RunGetter, client DispatchClient) *Resolver {
	return &Resolver{
		Runs: runs,
		Registry: Registry{
			run.RunnerKindGitHubActions: &GitHubActions{Client: client},
			run.RunnerKindLocal:         &Local{},
		},
	}
}

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }
func intPtr(i int) *int              { return &i }

// --- KindHostDispatched matrix ---

func TestKindHostDispatched(t *testing.T) {
	cases := []struct {
		kind           string
		hostDispatched bool
		known          bool
	}{
		{run.RunnerKindLocal, true, true},
		{run.RunnerKindGitHubActions, false, true},
		{run.RunnerKindGitLabCI, false, false},
		{"unknown_kind", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		hd, known := KindHostDispatched(c.kind)
		if hd != c.hostDispatched || known != c.known {
			t.Errorf("KindHostDispatched(%q) = (%v, %v), want (%v, %v)",
				c.kind, hd, known, c.hostDispatched, c.known)
		}
	}
}

// --- Resolver branch matrix ---

func TestResolver_Resolve(t *testing.T) {
	trigger := run.RunnerKindGitHubActions
	local := run.RunnerKindLocal

	parentResolvedLocal := uuid.New()
	parentResolvedActions := uuid.New()
	parentUnresolved := uuid.New()
	parentMissing := uuid.New()

	runs := &fakeRuns{byID: map[uuid.UUID]*run.Run{
		parentResolvedLocal:   {ID: parentResolvedLocal, RunnerKind: local, RunnerKindResolved: true},
		parentResolvedActions: {ID: parentResolvedActions, RunnerKind: trigger, RunnerKindResolved: true},
		parentUnresolved:      {ID: parentUnresolved, RunnerKind: local, RunnerKindResolved: false},
	}}

	cases := []struct {
		name     string
		run      *run.Run
		wantKind string
	}{
		{ // (a)
			"resolved-local -> local",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: true},
			local,
		},
		{ // (b)
			"resolved-github_actions -> trigger",
			&run.Run{ID: uuid.New(), RunnerKind: trigger, RunnerKindResolved: true},
			trigger,
		},
		{ // (c)
			"resolved-unknown-kind -> trigger (fire-through)",
			&run.Run{ID: uuid.New(), RunnerKind: "circleci", RunnerKindResolved: true},
			trigger,
		},
		{ // (d)
			"unresolved top-level -> trigger (legacy auto-resolve)",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false},
			trigger,
		},
		{ // (e)
			"unresolved child, inherited non-local -> trigger",
			&run.Run{ID: uuid.New(), RunnerKind: trigger, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentResolvedLocal)},
			trigger,
		},
		{ // (f)
			"unresolved child inherited-local + parent resolved-local -> local",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentResolvedLocal)},
			local,
		},
		{ // (g)
			"unresolved child inherited-local + parent resolved non-local -> trigger",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentResolvedActions)},
			trigger,
		},
		{ // (h)
			"unresolved child inherited-local + parent read error -> local (park recoverable)",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentMissing)},
			local,
		},
		{ // (i)
			"unresolved child inherited-local + parent unresolved -> local (park recoverable)",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentUnresolved)},
			local,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := newResolver(runs, &fakeDispatchClient{})
			got := rr.Resolve(context.Background(), c.run)
			if got.Kind() != c.wantKind {
				t.Errorf("Resolve() backend kind = %q, want %q", got.Kind(), c.wantKind)
			}
		})
	}
}

// --- Resolver parking-warn-log pinning (branches (h)/(i)) ---

// logRecord captures the level + message of one slog record.
type logRecord struct {
	Level slog.Level
	Msg   string
}

// recordingHandler is a minimal slog.Handler that appends each record's level
// and message. It pins the two resolver parking WARN logs (branches (h)/(i))
// against drift: the seam's zero-behavior-change claim leans on these two
// structured warn strings staying byte-identical, so a rename here fails the
// test rather than silently changing operator-visible output.
type recordingHandler struct{ records *[]logRecord }

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, logRecord{Level: r.Level, Msg: r.Message})
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestResolver_ParkingWarnLogs(t *testing.T) {
	local := run.RunnerKindLocal

	parentUnresolved := uuid.New()
	parentMissing := uuid.New() // absent from the fake -> GetRun errors

	runs := &fakeRuns{byID: map[uuid.UUID]*run.Run{
		parentUnresolved: {ID: parentUnresolved, RunnerKind: local, RunnerKindResolved: false},
	}}

	cases := []struct {
		name    string
		child   *run.Run
		wantMsg string
	}{
		{ // (h) parent read error
			"parent-lock read failed",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentMissing)},
			"orchestrator: decomposed child parent-lock read failed; parking child toward the recoverable state (awaiting_host_dispatch)",
		},
		{ // (i) parent un-resolved
			"parent runner_kind un-resolved",
			&run.Run{ID: uuid.New(), RunnerKind: local, RunnerKindResolved: false, DecomposedFrom: uuidPtr(parentUnresolved)},
			"orchestrator: decomposed child parent runner_kind un-resolved; parking child toward the recoverable state (awaiting_host_dispatch)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var recs []logRecord
			rr := newResolver(runs, &fakeDispatchClient{})
			rr.Logger = slog.New(recordingHandler{records: &recs})

			if got := rr.Resolve(context.Background(), c.child); got.Kind() != local {
				t.Errorf("Resolve() backend kind = %q, want %q (parks local)", got.Kind(), local)
			}

			var found bool
			for _, r := range recs {
				if r.Msg == c.wantMsg {
					found = true
					if r.Level != slog.LevelWarn {
						t.Errorf("parking log level = %v, want WARN", r.Level)
					}
				}
			}
			if !found {
				t.Errorf("expected parking WARN log %q; got %+v", c.wantMsg, recs)
			}
		})
	}
}

// --- GitHubActions.TriggerStage ---

func TestGitHubActions_TriggerStage_NilClientSkips(t *testing.T) {
	g := &GitHubActions{Client: nil}
	if err := g.TriggerStage(context.Background(), TriggerParams{RunID: uuid.New(), Scope: forge.FromGitHubInstallationID(42)}); err != nil {
		t.Fatalf("nil-client skip must return nil, got %v", err)
	}
}

func TestGitHubActions_TriggerStage_ZeroInstallationSkips(t *testing.T) {
	fc := &fakeDispatchClient{}
	g := &GitHubActions{Client: fc}
	if err := g.TriggerStage(context.Background(), TriggerParams{RunID: uuid.New(), Repo: "x/y"}); err != nil {
		t.Fatalf("zero-installation skip must return nil, got %v", err)
	}
	if len(fc.calls) != 0 {
		t.Errorf("zero installation must fire no dispatch, got %d", len(fc.calls))
	}
}

func TestGitHubActions_TriggerStage_MalformedRepoErrors(t *testing.T) {
	fc := &fakeDispatchClient{}
	g := &GitHubActions{Client: fc}
	err := g.TriggerStage(context.Background(), TriggerParams{RunID: uuid.New(), Repo: "no-slash", Scope: forge.FromGitHubInstallationID(42)})
	if err == nil {
		t.Fatal("malformed repo must error")
	}
	if len(fc.calls) != 0 {
		t.Errorf("malformed repo must fire no dispatch, got %d", len(fc.calls))
	}
}

func TestGitHubActions_TriggerStage_HappyPath(t *testing.T) {
	fc := &fakeDispatchClient{}
	g := &GitHubActions{Client: fc}
	runID, stageID := uuid.New(), uuid.New()
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: runID, StageID: stageID, WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Repo: "kuhlman-labs/fishhawk", Scope: forge.FromGitHubInstallationID(42),
	})
	if err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if len(fc.calls) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(fc.calls))
	}
	call := fc.calls[0]
	if call.Scope != forge.FromGitHubInstallationID(42) {
		t.Errorf("scope = %v, want scope for 42", call.Scope)
	}
	if call.Repo.Owner != "kuhlman-labs" || call.Repo.Name != "fishhawk" {
		t.Errorf("repo = %+v", call.Repo)
	}
	if call.Ref != "main" {
		t.Errorf("ref = %q, want main (default)", call.Ref)
	}
	if call.WorkflowFile != "fishhawk.yml" {
		t.Errorf("file = %q, want fishhawk.yml (default)", call.WorkflowFile)
	}
	want := githubclient.DispatchInputs{
		"run_id":      runID.String(),
		"stage_id":    stageID.String(),
		"workflow_id": "feature_change",
		"stage":       "claude-code",
	}
	if len(call.Inputs) != len(want) {
		t.Fatalf("inputs = %+v, want %+v", call.Inputs, want)
	}
	for k, v := range want {
		if call.Inputs[k] != v {
			t.Errorf("inputs[%q] = %q, want %q", k, call.Inputs[k], v)
		}
	}
	if _, ok := call.Inputs["parent_run_id"]; ok {
		t.Errorf("non-decomposed run must omit parent_run_id, got %q", call.Inputs["parent_run_id"])
	}
}

func TestGitHubActions_TriggerStage_CustomRefAndFile(t *testing.T) {
	fc := &fakeDispatchClient{}
	g := &GitHubActions{Client: fc, DefaultRef: "release", ActionsWorkflowFile: "custom.yml"}
	if err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), StageID: uuid.New(), Repo: "x/y", Scope: forge.FromGitHubInstallationID(7),
	}); err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if fc.calls[0].Ref != "release" || fc.calls[0].WorkflowFile != "custom.yml" {
		t.Errorf("ref/file = %q/%q, want release/custom.yml", fc.calls[0].Ref, fc.calls[0].WorkflowFile)
	}
}

func TestGitHubActions_TriggerStage_DecomposedChild_AddsParentRunID(t *testing.T) {
	fc := &fakeDispatchClient{}
	g := &GitHubActions{Client: fc}
	parent := uuid.New()
	err := g.TriggerStage(context.Background(), TriggerParams{
		RunID: uuid.New(), StageID: uuid.New(), WorkflowID: "feature_change",
		StageExecutorRef: "claude-code", Repo: "x/y", Scope: forge.FromGitHubInstallationID(42),
		DecomposedFrom: uuidPtr(parent), SliceIndex: intPtr(2),
	})
	if err != nil {
		t.Fatalf("TriggerStage: %v", err)
	}
	if got := fc.calls[0].Inputs["parent_run_id"]; got != parent.String() {
		t.Errorf("parent_run_id = %q, want %q", got, parent.String())
	}
}

func TestGitHubActions_TriggerStage_PropagatesDispatchError(t *testing.T) {
	sentinel := errors.New("boom")
	fc := &fakeDispatchClient{err: sentinel}
	g := &GitHubActions{Client: fc}
	err := g.TriggerStage(context.Background(), TriggerParams{RunID: uuid.New(), Repo: "x/y", Scope: forge.FromGitHubInstallationID(42)})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel propagated", err)
	}
}

// --- Local ---

func TestLocal_HostDispatchedAndNoOpTrigger(t *testing.T) {
	l := &Local{}
	if !l.HostDispatched() {
		t.Error("Local.HostDispatched() = false, want true")
	}
	if l.Kind() != run.RunnerKindLocal {
		t.Errorf("Local.Kind() = %q, want local", l.Kind())
	}
	if err := l.TriggerStage(context.Background(), TriggerParams{RunID: uuid.New()}); err != nil {
		t.Errorf("Local.TriggerStage must be a warn+nil no-op, got %v", err)
	}
}
