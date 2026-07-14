# backend/internal/procgroup

Reviewer subprocess hardening (#1805): whole-process-group termination for wedged review agents.

## Harden

`Harden(cmd, grace)` sets `SysProcAttr.Setpgid`, overrides `cmd.Cancel` to a whole-group SIGKILL (`kill(-pgid)`), and sets `cmd.WaitDelay` so a review-budget deadline actually terminates a wedged reviewer holding an inherited stdout pipe instead of hanging `cmd.Output()` (`procgroup.go` + `procgroup_unix.go` / `procgroup_other.go` platform split).

Applied by both `backend/internal/claudecode` and `backend/internal/codex` before `cmd.Output()`; both hoist the `ctx.Err()==DeadlineExceeded` timeout classification above the `*exec.ExitError` gate so the `implement_review_failed{Timeout:true}` label survives a `WaitDelay`-forced non-ExitError return.

Backend analog of the runner codex executor's group-kill (`runner/internal/agent/codex`). See `docs/ARCHITECTURE.md` §4.2 "Subprocess hardening".
