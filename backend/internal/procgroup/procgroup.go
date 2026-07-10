// Package procgroup hardens a review-adapter subprocess so a context deadline
// (the #747 size-aware review budget applied at the server call site) actually
// takes effect instead of being defeated by a wedged reviewer that spawned a
// grandchild holding the inherited stdout pipe open.
//
// The plan/implement review adapters (backend/internal/codex,
// backend/internal/claudecode) capture the CLI's whole response with
// cmd.Output(). cmd.Output() does not return until BOTH the process has exited
// AND every writer of the stdout pipe has closed it. A reviewer CLI that forks
// a grandchild (a shell exec, an MCP server) which inherits that write-end can
// therefore keep cmd.Output() blocked long after the direct child is killed —
// the latent hang behind #1805, where a hung codex reviewer ran 60+ minutes
// until the operator killed it by hand even though the budget deadline had long
// since fired.
//
// Harden applies two os/exec mechanisms that close both gaps. This is the
// backend-reviewer analog of the technique the runner's codex EXECUTOR adapter
// already uses (runner/internal/agent/codex).
package procgroup

import (
	"os/exec"
	"time"
)

// Harden configures cmd so that when its context is cancelled (a deadline or an
// explicit cancel) the ENTIRE process group is killed — not just the direct
// child — and so cmd.Wait/cmd.Output cannot hang indefinitely on a stdout pipe
// held open by a group member that escaped the kill.
//
// It sets three things:
//
//   - SysProcAttr.Setpgid so the child becomes its own process-group leader
//     (pgid == pid) and every grandchild it forks joins that group by default.
//   - cmd.Cancel to a whole-group SIGKILL (kill(-pgid)), replacing os/exec's
//     default single-process kill, so an inherited-stdout grandchild is reaped
//     and the pipe writer closes.
//   - cmd.WaitDelay to grace, so even if a group member escaped the kill (it
//     called setpgid itself), cmd.Output() force-closes the parent-side pipe fd
//     after grace and returns rather than blocking forever.
//
// Harden MUST be called AFTER the *exec.Cmd is built and BEFORE
// cmd.Start()/cmd.Output()/cmd.Run(). cmd.Cancel is only invoked by os/exec for
// commands built with a context (exec.CommandContext), which every review
// adapter call site uses; on a context-less cmd the Cancel override is inert but
// Setpgid and WaitDelay still apply.
func Harden(cmd *exec.Cmd, grace time.Duration) {
	setpgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = grace
}
