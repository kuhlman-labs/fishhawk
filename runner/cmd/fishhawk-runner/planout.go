package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// planArtifactDir is the directory the run/stage-keyed plan handoff lives in
// (#4067). var (not const) so tests can redirect it to a t.TempDir, the same
// seam as pullRequestDescriptionDir.
var planArtifactDir = "/tmp"

// legacyPlanArtifactPath is the fixed shared path every plan-typed stage was
// told to write before #4067 keyed it (backend prompt.LegacyPlanArtifactPath).
// Two concurrent plan stages on one host shared it, so a run could upload
// ANOTHER run's plan, or have a foreign sibling (clarification_request,
// grooming_report, ...) win the adoption precedence. It survives only as (1)
// the --plan-out value resolvePlanOut rewrites to the keyed path, because the
// operator-owned GHA workflow and the GitLab template still pass it, and (2)
// the bounded deprecation fallback claimLegacyPlanArtifact consumes under
// backend/runner version skew. var (not const) so tests redirect it.
var legacyPlanArtifactPath = "/tmp/fishhawk-plan.json"

// planArtifactPath mirrors prompt.PlanArtifactPath in the backend: the
// run/stage-keyed path a plan-typed stage writes its standard_v1 artifact (or
// sibling) to and the runner reads, adopts structured output into and uploads
// from (#4067). The format string is hardcoded in all three independent modules
// (backend prompt, runner, CLI) by design — the same coordination as
// pullRequestDescriptionPath (#1777); TestPlanArtifactPath_KeyedFormat pins the
// literal the backend and CLI tests also pin.
func planArtifactPath(runID, stageID string) string {
	return filepath.Join(planArtifactDir, fmt.Sprintf("fishhawk-plan-%s-%s.json", runID, stageID))
}

// resolvePlanOut keys cfg.planOut per run/stage (#4067). It is a no-op unless
// --run-id and --stage-id are both set (a local replay without ids keeps
// today's behavior; an empty --plan-out matches no case below). Given the
// legacy fixed path it rewrites planOut to
// the keyed path and logs one plan_out_keyed line; given the keyed path it
// leaves it. In both cases it arms the deprecation fallback by recording the
// legacy path in cfg.planOutLegacy. Any other (custom) path is left unchanged
// and unarmed. Called once right after parseFlags, so every later reader
// (detectPlanSibling, the clarification strip, adoptStructuredOutput,
// validatePlan, uploadPlan, retainPlanArtifact) sees the keyed path.
func resolvePlanOut(cfg *config, logSink io.Writer) {
	if cfg.runID == "" || cfg.stageID == "" {
		return
	}
	keyed := planArtifactPath(cfg.runID, cfg.stageID)
	switch cfg.planOut {
	case legacyPlanArtifactPath:
		logEvent(logSink, "plan_out_keyed", map[string]string{
			"run_id": cfg.runID, "stage_id": cfg.stageID,
			"from": cfg.planOut, "to": keyed,
		})
		cfg.planOut = keyed
		cfg.planOutLegacy = legacyPlanArtifactPath
	case keyed:
		cfg.planOutLegacy = legacyPlanArtifactPath
	}
}

// claimLegacyPlanArtifact is the BOUNDED deprecation fallback (#4067) for an
// older backend whose prompt still names the legacy fixed path while this
// runner reads the keyed one. It moves the legacy file onto cfg.planOut only
// when ALL of these hold:
//
//   - the fallback is armed (cfg.planOutLegacy set by resolvePlanOut) AND the
//     prompt text does NOT name cfg.planOut — an up-to-date prompt names the
//     keyed path, and then the fixed path is never read, which is what closes
//     the cross-run contamination;
//   - the keyed file is positively absent (the agent did not write it);
//   - the legacy path is a REGULAR file (a symlink or directory is refused)
//     modified at or after invokedAt truncated to the second (1s-granularity
//     filesystems), so a stale leftover from an earlier run is never adopted.
//
// The claim is os.Rename, atomic within one filesystem, so two runners racing
// for one legacy file cannot both consume it: the loser sees ENOENT and stays
// silent. Any other rename error (EXDEV, permissions) is logged as
// plan_artifact_legacy_unclaimable and the plan is then simply missing — the
// existing category-B path — never a silent copy. Best-effort: it returns
// nothing and never fails the stage itself.
func claimLegacyPlanArtifact(cfg config, promptText string, invokedAt time.Time, logSink io.Writer) {
	if cfg.planOutLegacy == "" || strings.Contains(promptText, cfg.planOut) {
		return
	}
	if _, err := os.Lstat(cfg.planOut); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	info, err := os.Lstat(cfg.planOutLegacy)
	if err != nil {
		return
	}
	fields := map[string]string{
		"run_id": cfg.runID, "stage_id": cfg.stageID,
		"legacy_path": cfg.planOutLegacy, "path": cfg.planOut,
	}
	if !info.Mode().IsRegular() {
		fields["reason"] = "not_regular_file"
		logEvent(logSink, "plan_artifact_legacy_unclaimable", fields)
		return
	}
	if info.ModTime().Before(invokedAt.Truncate(time.Second)) {
		fields["mtime"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		fields["invoked_at"] = invokedAt.UTC().Format(time.RFC3339Nano)
		logEvent(logSink, "plan_artifact_legacy_stale", fields)
		return
	}
	if err := os.Rename(cfg.planOutLegacy, cfg.planOut); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		fields["error"] = err.Error()
		logEvent(logSink, "plan_artifact_legacy_unclaimable", fields)
		return
	}
	logEvent(logSink, "plan_artifact_legacy_path", fields)
}
