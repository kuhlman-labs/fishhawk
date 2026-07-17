package runnerbackend

import (
	"context"
	"log/slog"
)

// Local is the backend behind runner_kind=local. The local runner is
// host-spawned (ADR-024): fishhawkd cannot start it, so HostDispatched() is
// true and its agent stages PARK at awaiting_host_dispatch (#1912) rather than
// being fired. The host-dispatch marker endpoint (or an MCP spawn verb calling
// it) flips awaiting_host_dispatch -> dispatched at the moment of the spawn.
type Local struct {
	Logger *slog.Logger
}

// Kind reports runner_kind=local.
func (*Local) Kind() string { return "local" }

// HostDispatched is true: the local runner is spawned host-side, so its stages
// park rather than fire.
func (*Local) HostDispatched() bool { return true }

// TriggerStage is a defensive warn+no-op preserving fireDispatch's locked-local
// backstop: a run locked to runner_kind=local must never fire a github_actions
// workflow_dispatch (the #1355 channel-mismatch guardrail). In normal flow the
// park branch keys on HostDispatched() and returns before this is reached, so
// this warn is effectively unreachable — the backstop it always was.
func (l *Local) TriggerStage(ctx context.Context, p TriggerParams) error {
	l.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: run locked to runner_kind=local; skipping github_actions workflow_dispatch",
		slog.String("run_id", p.RunID.String()),
		slog.String("runner_kind", "local"),
	)
	return nil
}

func (l *Local) logger() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}
