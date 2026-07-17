// Package runnerbackend is the forge-neutral dispatcher seam (E45.7,
// ADR-058 / #1851, ADR-022 growth path). It replaces the four scattered
// runner_kind comparisons that decided "park for a host spawn vs fire a
// github_actions workflow_dispatch" with a single Backend interface plus a
// Resolver that owns the run-lineage semantics those sites shared.
//
// A Backend abstracts one execution channel: Kind() is its runner_kind
// string, HostDispatched() reports whether the backend cannot be spawned by
// fishhawkd itself (the local runner is host-spawned per ADR-024, so its
// stages PARK at awaiting_host_dispatch instead of dispatched), and
// TriggerStage fires whatever wakes the runner (a github_actions
// workflow_dispatch; a warn+no-op for the host-spawned local channel).
//
// Three implementations ship: GitHubActions (githubactions.go), GitLabCI
// (gitlabci.go, #1861 — creates a GitLab pipeline; NOT host-dispatched, and
// dispatch-only: it writes no commit status), and Local (local.go). A Registry
// maps runner_kind -> Backend, KindHostDispatched is the package-level predicate
// over the KNOWN kinds for guard sites that hold only a kind string, and
// Resolver.Resolve ports the runLockedLocal lineage rules verbatim. See
// README.md for the full contract, including the companion status-publish /
// CI-ingest surfaces owned by the sibling #1861 slices.
package runnerbackend

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TriggerParams carries everything a Backend needs to trigger a stage,
// forge-neutrally. The github_actions backend maps these onto its
// workflow_dispatch inputs; the gitlab_ci backend maps them onto its pipeline
// trigger. Repo is the "owner/name" string as stored on the run row.
//
// Scope is the forge-neutral credential scope that authenticates the trigger
// (#1861: the InstallationID int64 seam flipped to a forge.CredentialScope so
// a "gitlab:<project_id>" ref rides the same field a GitHub installation id
// does). A zero Scope (Scope.IsZero()) is the unwired sentinel — the direct
// analogue of the pre-flip InstallationID == 0 — so a backend warn+skips on it
// exactly as it did on the zero id.
//
// Ref is the git ref the dispatch targets. It is resolved ONCE by the caller
// from the run-branch derivation (orchestrator.runBranchPrefix ->
// "fishhawk/run-<short>"; a decomposed child's per-slice branch is
// "fishhawk/run-<short>/slice-<n>") so BOTH backends consume one source: the
// github_actions backend maps it onto workflow_dispatch's ref (falling back to
// its DefaultRef when empty), and the gitlab_ci backend requires it as the
// pipeline's ref (which selects BOTH the .gitlab-ci.yml evaluated AND the
// commit the pipeline runs against). See README.md for the run-branch contract.
//
// DecomposedFrom / SliceIndex carry a decomposed child's provenance so the
// fan-out is observable and the parent_run_id input (#1227) can be attached.
type TriggerParams struct {
	RunID            uuid.UUID
	StageID          uuid.UUID
	WorkflowID       string
	StageExecutorRef string
	Repo             string                // "owner/name"
	Scope            forge.CredentialScope // zero = unwired
	Ref              string                // dispatch ref (run branch); empty falls back per-backend
	DecomposedFrom   *uuid.UUID
	SliceIndex       *int
}

// Backend abstracts one runner execution channel.
type Backend interface {
	// Kind is the runner_kind string this backend serves.
	Kind() string
	// HostDispatched reports whether the backend is spawned host-side rather
	// than by fishhawkd. A host-dispatched backend's agent stages PARK at
	// awaiting_host_dispatch (ADR-024 / #1912) rather than being fired here.
	HostDispatched() bool
	// TriggerStage wakes the runner for the stage described by p. For a
	// host-dispatched backend this is a defensive warn+no-op — the host spawn
	// is the real trigger.
	TriggerStage(ctx context.Context, p TriggerParams) error
}

// Registry maps a runner_kind string to its Backend.
type Registry map[string]Backend

// Backend returns the backend registered for kind, and whether one exists.
func (r Registry) Backend(kind string) (Backend, bool) {
	b, ok := r[kind]
	return b, ok
}

// KindHostDispatched is the package-level predicate over the KNOWN runner
// kinds, for guard sites that hold only a runner_kind string (no client
// wiring). It returns (hostDispatched, known): local -> (true, true),
// github_actions -> (false, true), any other kind -> (false, false). gitlab_ci
// is deliberately NOT recognized here yet: the two guard sites that consume this
// predicate (the host-dispatch endpoint and the MCP guard) each carry their own
// unknown-kind posture, and flipping gitlab_ci to a known kind is owned by the
// slice that updates those guards — this seam only supplies the gitlab_ci
// Backend + Registry entry. The two-value shape lets each guard site keep its
// unknown-kind posture explicit so a future registry addition cannot silently
// flip either.
func KindHostDispatched(kind string) (hostDispatched, known bool) {
	switch kind {
	case run.RunnerKindLocal:
		return true, true
	case run.RunnerKindGitHubActions:
		return false, true
	default:
		return false, false
	}
}

// RunGetter is the slice of run.Repository the Resolver needs to consult a
// decomposed child's parent lock.
type RunGetter interface {
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
}

// Resolver decides WHICH backend a run's stage dispatch flows through, porting
// the runLockedLocal lineage semantics verbatim.
//
// The RESOLVED lock (r.RunnerKindResolved) is authoritative: its kind maps to
// the registry backend (an unknown resolved kind falls to the trigger backend,
// matching today's fire-through). An un-resolved TOP-LEVEL run is NOT treated
// as locked — it auto-resolves on its first dispatch (#1346 decision-1), so it
// resolves to the github_actions trigger backend (its legacy channel).
//
// #1980: a decomposed child is the special case. Its row is minted
// runner_kind-UNRESOLVED with RunnerKind COPIED from the parent, so the
// resolved gate never holds for a freshly minted child even when the parent is
// locked-local. An un-resolved child that INHERITED a local kind consults the
// parent's lock via GetRun; a github_actions child (inherited kind not local)
// resolves to the trigger backend and fires unchanged. Fail toward the
// RECOVERABLE state: when the parent read errors or the parent is itself
// un-resolved, resolve to the LOCAL backend so the stage PARKS at
// awaiting_host_dispatch (CAS-recoverable with one host-dispatch verb) rather
// than firing an unrecoverable github_actions workflow_dispatch.
type Resolver struct {
	Runs     RunGetter
	Registry Registry
	Logger   *slog.Logger
}

// Resolve returns the backend that owns run r's stage dispatch. It never
// returns nil for a Registry holding the two default kinds.
func (rr *Resolver) Resolve(ctx context.Context, r *run.Run) Backend {
	if r.RunnerKindResolved {
		if b, ok := rr.Registry.Backend(r.RunnerKind); ok {
			return b
		}
		// Unknown resolved kind: fall to the trigger backend (today's
		// fire-through, matching runLockedLocal returning false for a resolved
		// non-local kind).
		return rr.triggerBackend()
	}
	// Un-resolved top-level run: leave the legacy github_actions auto-resolve
	// path unchanged.
	if r.DecomposedFrom == nil {
		return rr.triggerBackend()
	}
	// Un-resolved decomposed child. Only a child that inherited a local kind
	// can be locked-local; a github_actions child fires unchanged.
	if r.RunnerKind != run.RunnerKindLocal {
		return rr.triggerBackend()
	}
	parent, err := rr.Runs.GetRun(ctx, *r.DecomposedFrom)
	if err != nil {
		rr.logger().LogAttrs(ctx, slog.LevelWarn,
			"orchestrator: decomposed child parent-lock read failed; parking child toward the recoverable state (awaiting_host_dispatch)",
			slog.String("run_id", r.ID.String()),
			slog.String("decomposed_from", r.DecomposedFrom.String()),
			slog.String("error", err.Error()),
		)
		return rr.localBackend()
	}
	if parent.RunnerKindResolved {
		// Parent lock is authoritative: local -> park; non-local -> the child's
		// inherited local hint was superseded, fall through to fire.
		if parent.RunnerKind == run.RunnerKindLocal {
			return rr.localBackend()
		}
		return rr.triggerBackend()
	}
	// Parent itself un-resolved: the inherited local hint is the best signal we
	// have — park toward the recoverable state.
	rr.logger().LogAttrs(ctx, slog.LevelWarn,
		"orchestrator: decomposed child parent runner_kind un-resolved; parking child toward the recoverable state (awaiting_host_dispatch)",
		slog.String("run_id", r.ID.String()),
		slog.String("decomposed_from", r.DecomposedFrom.String()),
	)
	return rr.localBackend()
}

// triggerBackend is the github_actions channel: the legacy first-dispatch
// auto-resolve target and the fire-through for any resolved kind the registry
// does not recognize.
func (rr *Resolver) triggerBackend() Backend {
	return rr.Registry[run.RunnerKindGitHubActions]
}

// localBackend is the host-dispatched local channel (parks at
// awaiting_host_dispatch).
func (rr *Resolver) localBackend() Backend {
	return rr.Registry[run.RunnerKindLocal]
}

func (rr *Resolver) logger() *slog.Logger {
	if rr.Logger != nil {
		return rr.Logger
	}
	return slog.Default()
}
