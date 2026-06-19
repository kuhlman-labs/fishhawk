package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// INTERIM (#1233, retire when #1232 ships). `fishhawk run auto-decide`
// is an operator-side, SECOND decision channel for mid-stage scope
// amendments: launched detached alongside a blocking
// fishhawk_run_stage, it polls the run's pending amendments and
// AUTO-APPROVES only a conservative allowlist — amendments whose every
// path is a coupled test sibling of an in-scope production file. It
// changes NO product/runtime code; it reuses endpoints that already
// exist. Everything outside the allowlist is left undecided (it times
// out → today's fail-and-retry). This exists only because a single
// MCP session is blocked on fishhawk_run_stage while a known-safe
// amendment waits; #1232 (durable non-blocking dispatch) makes the
// in-band decision native and supersedes this subcommand.

// autoDecidePollDefault is the default per-iteration ?wait seconds.
// Clamped to the backend's long-poll cap so we never rely on
// server-side clamping for correctness.
const autoDecidePollDefault = 25

// autoDecidePollCap mirrors backend maxScopeAmendmentWaitSeconds: a
// single ?wait holds at most this long server-side. We clamp our own
// flag to it so --poll above the cap still works but never claims a
// longer hold than the server honors.
const autoDecidePollCap = 30

// autoDecideMaxDurationDefault bounds the overall loop. Comfortably
// under a typical implement-stage budget so the detached decider exits
// rather than lingering past the run.
const autoDecideMaxDurationDefault = 50 * time.Minute

// autoDecideReason is the decision reason POSTed on an auto-approve,
// naming this as the interim auto-decider so the audit trail is honest
// about provenance.
const autoDecideReason = "auto-approved by `fishhawk run auto-decide` (interim #1233, retire when #1232 ships): every amended path is a coupled test sibling of an in-scope production file (#1214-proven-safe)"

// isAutoApprovable reports whether an amendment is safe to auto-approve
// under the conservative tightened allowlist (#1233 binding condition).
// It returns true ONLY when the amendment has at least one path AND
// EVERY path:
//   - has operation create or modify (never delete), AND
//   - is a `<dir>/<stem>_test.go` test file (slash-normalized), AND
//   - has its coupled production sibling `<dir>/<stem>.go` present in
//     the run's plan scope.files (the in-scope set).
//
// This matches the issue's done-means allowlist exactly: a test-only
// amendment whose production sibling is already scoped cannot change
// shipped behavior and is the #1214-proven-safe case. ANY path that is
// not a coupled-sibling test of an in-scope production file makes the
// whole amendment ineligible (fail-closed → left undecided).
func isAutoApprovable(am httpclient.ScopeAmendment, inScope map[string]bool) bool {
	if len(am.Paths) == 0 {
		return false
	}
	for _, p := range am.Paths {
		switch p.Operation {
		case "create", "modify":
		default:
			return false
		}
		norm := filepath.ToSlash(p.Path)
		if !strings.HasSuffix(norm, "_test.go") {
			return false
		}
		sibling := strings.TrimSuffix(norm, "_test.go") + ".go"
		if !inScope[sibling] {
			return false
		}
	}
	return true
}

// runAutoDecide implements `fishhawk run auto-decide <run-id>`.
func runAutoDecide(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run auto-decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	poll := fs.Int("poll", autoDecidePollDefault,
		fmt.Sprintf("per-iteration scope-amendment long-poll seconds (clamped to the backend's %ds cap)", autoDecidePollCap))
	maxDuration := fs.Duration("max-duration", autoDecideMaxDurationDefault,
		"overall stop: exit after this long even if the run is still running")
	dryRun := fs.Bool("dry-run", false,
		"log auto-approve verdicts without POSTing the decision")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run auto-decide: <run-id> required")
		return exitUsage
	}
	runID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run auto-decide: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}
	pollSeconds := *poll
	if pollSeconds > autoDecidePollCap {
		pollSeconds = autoDecidePollCap
	}
	if pollSeconds <= 0 {
		pollSeconds = autoDecidePollDefault
	}

	client := newClient(cf)

	_, _ = fmt.Fprintf(stderr,
		"fishhawk run auto-decide: INTERIM workaround for #1189/#1232 — second-channel auto-decider for run %s.\n"+
			"  Auto-approves ONLY amendments whose every path is a coupled *_test.go sibling of an in-scope production file; everything else is left for manual decision.\n",
		runID)
	if *dryRun {
		_, _ = fmt.Fprintln(stderr, "  --dry-run: verdicts are logged but no decision is POSTed.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *maxDuration)
	defer cancel()

	// Fetch the run's plan scope.files once up front: the approved plan
	// is fixed for the run's lifetime, so the in-scope set doesn't change
	// across iterations. On a fetch failure the set is empty, which makes
	// the matcher decline every amendment (fail-closed → manual decision).
	inScope := fetchInScopeFiles(ctx, client, runID, stderr)

	return autoDecideLoop(ctx, client, runID, inScope, pollSeconds, *dryRun, stdout, stderr)
}

// autoDecideLoop is the poll→match→decide→terminal-stop loop, split out
// of runAutoDecide so tests can drive it against an httptest.Server.
func autoDecideLoop(ctx context.Context, client *httpclient.Client, runID uuid.UUID, inScope map[string]bool, pollSeconds int, dryRun bool, stdout, stderr io.Writer) int {
	for {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(stderr, "fishhawk run auto-decide: max-duration reached; exiting.")
			return exitOK
		}

		// Stop the moment the run is terminal — no point polling a run
		// that can no longer file amendments.
		r, err := client.GetRun(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(stderr, "fishhawk run auto-decide: max-duration reached; exiting.")
				return exitOK
			}
			_, _ = fmt.Fprintf(stderr, "fishhawk run auto-decide: get run failed: %v\n", err)
			return exitFailure
		}
		if isTerminalRunState(r.State) {
			_, _ = fmt.Fprintf(stdout, "fishhawk run auto-decide: run %s reached terminal state %q; exiting.\n", runID, r.State)
			return exitOK
		}

		amendments, err := client.ListScopeAmendments(ctx, runID, pollSeconds)
		if err != nil {
			if ctx.Err() != nil {
				_, _ = fmt.Fprintln(stderr, "fishhawk run auto-decide: max-duration reached; exiting.")
				return exitOK
			}
			// A transient list error shouldn't abort the detached loop;
			// log and re-poll on the next iteration.
			_, _ = fmt.Fprintf(stderr, "fishhawk run auto-decide: list amendments failed (will retry): %v\n", err)
			continue
		}

		for _, am := range amendments {
			if am.Status != "pending" {
				continue
			}
			if !isAutoApprovable(am, inScope) {
				_, _ = fmt.Fprintf(stdout,
					"fishhawk run auto-decide: amendment %s left for manual decision (not a coupled in-scope test sibling): %s\n",
					am.ID, pathsSummary(am.Paths))
				continue
			}
			if dryRun {
				_, _ = fmt.Fprintf(stdout,
					"fishhawk run auto-decide: [dry-run] would approve amendment %s: %s\n",
					am.ID, pathsSummary(am.Paths))
				continue
			}
			if _, derr := client.DecideScopeAmendment(ctx, runID, am.ID, "approve", autoDecideReason); derr != nil {
				// 409 already-decided (operator raced us) and transient
				// 5xx are best-effort: log and keep going, never abort the
				// loop on one amendment.
				_, _ = fmt.Fprintf(stderr,
					"fishhawk run auto-decide: approve of amendment %s failed (continuing): %v\n",
					am.ID, derr)
				continue
			}
			_, _ = fmt.Fprintf(stdout,
				"fishhawk run auto-decide: auto-approved amendment %s: %s\n",
				am.ID, pathsSummary(am.Paths))
		}
	}
}

// isTerminalRunState reports whether a run state is terminal (the run
// can no longer file scope amendments).
func isTerminalRunState(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// pathsSummary renders an amendment's paths compactly for log lines.
func pathsSummary(paths []httpclient.ScopeAmendmentPath) string {
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Path, p.Operation))
	}
	return strings.Join(parts, ", ")
}

// fetchInScopeFiles resolves the run's approved-plan scope.files into a
// slash-normalized set, mirroring the MCP get_plan path (list stages →
// find the plan stage → newest standard_v1 plan artifact → scope.files).
// When the run has no plan stage or no usable plan artifact (a
// decomposed child / implement-only run), it walks once to the parent
// run. A resolution failure returns an empty set so the matcher declines
// every amendment (fail-closed) rather than auto-approving blind.
func fetchInScopeFiles(ctx context.Context, client *httpclient.Client, runID uuid.UUID, stderr io.Writer) map[string]bool {
	inScope := map[string]bool{}
	current := runID
	for hop := 0; hop < 4; hop++ {
		files, found, err := planScopeFilesForRun(ctx, client, current)
		if err != nil {
			_, _ = fmt.Fprintf(stderr,
				"fishhawk run auto-decide: warning: resolve scope.files failed; no amendment will auto-approve: %v\n", err)
			return inScope
		}
		if found {
			for _, f := range files {
				inScope[filepath.ToSlash(f)] = true
			}
			return inScope
		}
		// No plan on this run; walk to the parent if there is one.
		r, err := client.GetRun(ctx, current)
		if err != nil {
			_, _ = fmt.Fprintf(stderr,
				"fishhawk run auto-decide: warning: resolve parent run failed; no amendment will auto-approve: %v\n", err)
			return inScope
		}
		switch {
		case r.ParentRunID != nil:
			current = *r.ParentRunID
		case r.DecomposedFrom != nil:
			current = *r.DecomposedFrom
		default:
			_, _ = fmt.Fprintln(stderr,
				"fishhawk run auto-decide: warning: no approved plan found for run; no amendment will auto-approve.")
			return inScope
		}
	}
	_, _ = fmt.Fprintln(stderr,
		"fishhawk run auto-decide: warning: parent walk exceeded depth without a plan; no amendment will auto-approve.")
	return inScope
}

// planScopeFile is the minimal projection of a standard_v1 plan needed
// to read scope.files paths.
type planScopeFile struct {
	Scope struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	} `json:"scope"`
}

// planScopeFilesForRun lists the run's stages, finds the plan stage's
// newest standard_v1 plan artifact, and decodes its scope.files paths.
// Returns (paths, true, nil) on a hit; (nil, false, nil) when the run
// has no plan stage or no usable plan artifact; (nil, false, err) on a
// transport/decode failure.
func planScopeFilesForRun(ctx context.Context, client *httpclient.Client, runID uuid.UUID) ([]string, bool, error) {
	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		return nil, false, fmt.Errorf("list stages: %w", err)
	}
	var planStageID uuid.UUID
	var havePlanStage bool
	for _, st := range stages.Items {
		if st.Type == "plan" {
			planStageID = st.ID
			havePlanStage = true
			break
		}
	}
	if !havePlanStage {
		return nil, false, nil
	}
	arts, err := client.ListStageArtifacts(ctx, planStageID)
	if err != nil {
		return nil, false, fmt.Errorf("list plan artifacts: %w", err)
	}
	var picked *httpclient.Artifact
	for i := range arts {
		a := &arts[i]
		if a.Kind != "plan" {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil || len(picked.Content) == 0 {
		return nil, false, nil
	}
	var pc planScopeFile
	if err := json.Unmarshal(picked.Content, &pc); err != nil {
		return nil, false, fmt.Errorf("decode plan artifact: %w", err)
	}
	paths := make([]string, 0, len(pc.Scope.Files))
	for _, f := range pc.Scope.Files {
		if f.Path != "" {
			paths = append(paths, f.Path)
		}
	}
	return paths, true, nil
}
