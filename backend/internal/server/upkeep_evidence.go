package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// Upkeep scan evidence gather (#3922). At prompt-serve time, a plan stage that
// declares `produces: upkeep_report` gets the deterministic evidence the
// upkeep detectors compute: toolchain pin drift over the pin-bearing files at
// the run's recorded admission commit, and flakes over recent recorded runs of
// the same repository and account. The gather never fails the prompt: every
// partial read is a named degrade the prompt renders, so a partial scan is
// never presented as a complete one. Contract and degrade table:
// backend/internal/upkeep/README.md § "Evidence gather".

// Bounds. Package vars, not consts, so a NON-PARALLEL test can shrink them
// (the groomingApplyBudget precedent).
var (
	// upkeepEvidenceBudget is the wall budget for the whole gather (pin gather
	// then flake gather), well inside the runner's 120s prompt-fetch timeout.
	upkeepEvidenceBudget = 20 * time.Second
	// upkeepFlakeWindow is how far back the flake gather looks.
	upkeepFlakeWindow = 14 * 24 * time.Hour
	// upkeepFlakeRunScanCap caps the runs the flake gather scans.
	upkeepFlakeRunScanCap = 300
	// upkeepFlakeBundleCap caps the redacted bundles the flake gather reads.
	upkeepFlakeBundleCap = 40
	// upkeepPinFileMaxBytes caps one pin file; a larger file is skipped.
	upkeepPinFileMaxBytes = 1 << 20
	// upkeepBundleMaxBytes caps one redacted bundle read.
	upkeepBundleMaxBytes int64 = 64 << 20
	// upkeepPinMaxWorkspaceDirs caps the go.work `use` dirs whose go.mod is read.
	upkeepPinMaxWorkspaceDirs = 32
	// upkeepPinMaxWorkflowFiles caps the .github/workflows files read.
	upkeepPinMaxWorkflowFiles = 64
)

// upkeepFlakeRunPageSize is the ListRuns page size of the flake gather.
const upkeepFlakeRunPageSize = 100

// upkeepWorkflowDir is the directory the pin gather lists for workflow files.
const upkeepWorkflowDir = ".github/workflows"

// upkeepPinFixedFiles are the non-workspace pin files read on every scan. An
// absent one is not a degrade.
var upkeepPinFixedFiles = []string{
	"go.mod",
	".golangci.yml",
	".golangci.yaml",
	"AGENTS.md",
	"docs/api/README.md",
	"docs/api/v0.md",
}

// Pin-gather degrade reasons (source toolchain_drift).
const (
	upkeepDegradePinReaderUnwired           = "pin_reader_unwired"
	upkeepDegradeBaseCommitUnrecorded       = "base_commit_unrecorded"
	upkeepDegradeRepoMalformed              = "repo_malformed"
	upkeepDegradeScopeUnavailable           = "scope_unavailable"
	upkeepDegradeWorkflowListingUnavailable = "workflow_listing_unavailable"
	upkeepDegradeWorkflowListingFailed      = "workflow_listing_failed"
	upkeepDegradePinFetchFailed             = "pin_fetch_failed"
	upkeepDegradePinFileTooLarge            = "pin_file_too_large"
	upkeepDegradeWorkspaceDirsCapped        = "workspace_dirs_capped"
	upkeepDegradeWorkflowFilesCapped        = "workflow_files_capped"
)

// Flake-gather degrade reasons (source flake).
const (
	upkeepDegradeTraceStoreUnwired       = "trace_store_unwired"
	upkeepDegradeRunListFailed           = "run_list_failed"
	upkeepDegradeRunScanCapped           = "run_scan_capped"
	upkeepDegradeStageListFailed         = "stage_list_failed"
	upkeepDegradeTraceListFailed         = "trace_list_failed"
	upkeepDegradeTraceFetchFailed        = "trace_fetch_failed"
	upkeepDegradeBundleTooLarge          = "bundle_too_large"
	upkeepDegradeGateEvidenceParseFailed = "gate_evidence_parse_failed"
	upkeepDegradeBundleCapReached        = "bundle_cap_reached"
)

// upkeepDegradeBudgetExceeded is shared by both sources.
const upkeepDegradeBudgetExceeded = "budget_exceeded"

// errUpkeepWorkflowListingUnavailable is what an upkeepPinSource returns from
// ListWorkflowFiles when it cannot list workflows at all (a non-GitHub forge,
// no GitHub client): the workflow_listing_unavailable degrade.
var errUpkeepWorkflowListingUnavailable = errors.New("upkeep: workflow listing unavailable for this run's forge")

// upkeepPinSource reads pin files at one fixed commit. FetchFile returns
// forge.ErrNotFound for an absent file; ListWorkflowFiles returns the
// repo-relative paths of the entries under .github/workflows (forge.ErrNotFound
// when the directory is absent, errUpkeepWorkflowListingUnavailable when the
// forge cannot list).
type upkeepPinSource interface {
	FetchFile(ctx context.Context, path string) ([]byte, error)
	ListWorkflowFiles(ctx context.Context) ([]string, error)
}

// newUpkeepPinSource builds the run's pin source, or returns a degrade reason
// when none can be built. A package var so a test substitutes a fake source
// (#3922 approval condition 9: no new Server/Config field).
var newUpkeepPinSource = func(ctx context.Context, s *Server, runRow *run.Run) (upkeepPinSource, string) {
	return s.upkeepForgePinSource(ctx, runRow)
}

// upkeepListWorkflowDir lists the workflow directory through the GitHub
// Contents API. ok=false means the listing is unavailable (no GitHub client).
// A package var so a test substitutes the listing without a GitHub server.
var upkeepListWorkflowDir = func(ctx context.Context, s *Server, scope forge.CredentialScope, repo forge.RepoRef, ref string) ([]string, bool, error) {
	if s.cfg.GitHub == nil {
		return nil, false, nil
	}
	entries, err := s.cfg.GitHub.ListDirectory(ctx, scope, repo, upkeepWorkflowDir, ref)
	if err != nil {
		return nil, true, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			out = append(out, e.Path)
		}
	}
	return out, true, nil
}

// upkeepForgePinSource is the production binding: the document-injection
// fetcher, under the document credential scope, at the run's RECORDED
// admission commit exactly. A run with no recorded commit degrades to
// base_commit_unrecorded rather than reading a mutable ref (the
// run.Run.DocumentBaseCommit contract).
func (s *Server) upkeepForgePinSource(ctx context.Context, runRow *run.Run) (upkeepPinSource, string) {
	if s.cfg.DocumentResolver == nil || s.cfg.DocumentResolver.Fetcher == nil {
		return nil, upkeepDegradePinReaderUnwired
	}
	if runRow.DocumentBaseCommit == nil || *runRow.DocumentBaseCommit == "" {
		return nil, upkeepDegradeBaseCommitUnrecorded
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return nil, upkeepDegradeRepoMalformed
	}
	var scope forge.CredentialScope
	if s.cfg.DocumentScope != nil {
		if scope, err = s.cfg.DocumentScope(ctx, repo); err != nil {
			return nil, upkeepDegradeScopeUnavailable
		}
	}
	return &forgeUpkeepPinSource{
		s:      s,
		fetch:  s.cfg.DocumentResolver.Fetcher.FetchFile,
		scope:  scope,
		repo:   repo,
		ref:    *runRow.DocumentBaseCommit,
		github: runForge(runRow) == "github",
	}, ""
}

type forgeUpkeepPinSource struct {
	s      *Server
	fetch  func(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, path, ref string) (*forge.FileContent, error)
	scope  forge.CredentialScope
	repo   forge.RepoRef
	ref    string
	github bool
}

func (f *forgeUpkeepPinSource) FetchFile(ctx context.Context, p string) ([]byte, error) {
	fc, err := f.fetch(ctx, f.scope, f.repo, p, f.ref)
	if err != nil {
		return nil, err
	}
	return fc.Content, nil
}

func (f *forgeUpkeepPinSource) ListWorkflowFiles(ctx context.Context) ([]string, error) {
	if !f.github {
		return nil, errUpkeepWorkflowListingUnavailable
	}
	paths, ok, err := upkeepListWorkflowDir(ctx, f.s, f.scope, f.repo, f.ref)
	if !ok {
		return nil, errUpkeepWorkflowListingUnavailable
	}
	return paths, err
}

// upkeepDegrades accumulates named degrades per (source, reason).
type upkeepDegrades struct {
	counts map[[2]string]int
}

func (d *upkeepDegrades) add(source, reason string, n int) {
	if d.counts == nil {
		d.counts = map[[2]string]int{}
	}
	d.counts[[2]string{source, reason}] += n
}

// sorted returns the degrades sorted by (source, reason); nil when none.
func (d *upkeepDegrades) sorted() []prompt.UpkeepEvidenceDegrade {
	if len(d.counts) == 0 {
		return nil
	}
	out := make([]prompt.UpkeepEvidenceDegrade, 0, len(d.counts))
	for k, n := range d.counts {
		out = append(out, prompt.UpkeepEvidenceDegrade{Source: k[0], Reason: k[1], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// resolveUpkeepScanContext returns the upkeep scan evidence for a plan stage
// that declares `produces: upkeep_report`, and nil for every other stage — a
// non-plan stage with no read at all, a plan stage with one GetRun and spec
// parse (resolveUpkeepStageBinding). The returned error is the binding's
// transport error only; the gather itself never errors (each partial read is
// a degrade).
//
// The preview (/prompt-render) calls this too and re-gathers, so the two
// endpoints can differ when a run lands or the budget expires between calls.
func (s *Server) resolveUpkeepScanContext(ctx context.Context, runRow *run.Run, stage *run.Stage) (*prompt.UpkeepScanContext, error) {
	if stage.Type != run.StageTypePlan {
		return nil, nil
	}
	b, err := s.resolveUpkeepStageBinding(ctx, runRow.ID, stage)
	if err != nil {
		return nil, err
	}
	if !b.StageDeclaresUpkeep {
		return nil, nil
	}
	gctx, cancel := context.WithTimeout(ctx, upkeepEvidenceBudget)
	defer cancel()

	var deg upkeepDegrades
	files, scanned := s.gatherUpkeepPins(gctx, runRow, &deg)
	flakeStages, runsScanned, stagesScanned := s.gatherUpkeepFlakes(gctx, runRow, &deg)

	out := &prompt.UpkeepScanContext{
		PinFilesScanned:    scanned,
		FlakeWindowDays:    int(upkeepFlakeWindow / (24 * time.Hour)),
		FlakeRunsScanned:   runsScanned,
		FlakeStagesScanned: stagesScanned,
		Degrades:           deg.sorted(),
	}
	if runRow.DocumentBaseCommit != nil {
		out.BaseCommit = *runRow.DocumentBaseCommit
	}
	for _, d := range upkeep.DetectPinDrift(files) {
		fact := prompt.UpkeepPinDriftFact{Family: d.Family, OmittedOccurrences: d.OmittedOccurrences}
		for _, o := range d.Occurrences {
			fact.Occurrences = append(fact.Occurrences, prompt.UpkeepPinOccurrence{Path: o.Path, Line: o.Line, Value: o.Value})
		}
		out.PinDrift = append(out.PinDrift, fact)
	}
	flakes, omitted := upkeep.CapFlakeSubjects(upkeep.AggregateFlakes(flakeStages))
	out.OmittedFlakes = omitted
	for _, f := range flakes {
		fact := prompt.UpkeepFlakeFact{Subject: f.Subject, Occurrences: f.Occurrences, OmittedRefs: f.OmittedRefs}
		for _, r := range f.Refs {
			fact.Refs = append(fact.Refs, prompt.UpkeepRunRef{RunID: r.RunID, StageID: r.StageID})
		}
		out.Flakes = append(out.Flakes, fact)
	}
	return out, nil
}

// upkeepDegrade records one degrade and WARN-logs it with the run id. No
// audit row is written: the prompt itself carries the degrade.
func (s *Server) upkeepDegrade(ctx context.Context, deg *upkeepDegrades, runID uuid.UUID, source, reason string, n int, attrs ...slog.Attr) {
	deg.add(source, reason, n)
	base := []slog.Attr{
		slog.String("run_id", runID.String()),
		slog.String("source", source),
		slog.String("reason", reason),
		slog.Int("count", n),
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep evidence: partial scan", append(base, attrs...)...)
}

// gatherUpkeepPins builds the run's pin source and reads the pin files. It
// returns path → content for every file read, and how many were read.
func (s *Server) gatherUpkeepPins(ctx context.Context, runRow *run.Run, deg *upkeepDegrades) (map[string]string, int) {
	src, reason := newUpkeepPinSource(ctx, s, runRow)
	if reason != "" {
		s.upkeepDegrade(ctx, deg, runRow.ID, plan.UpkeepSourceToolchainDrift, reason, 1)
		return map[string]string{}, 0
	}
	files, reasons := gatherUpkeepPinFiles(ctx, src)
	for _, r := range reasons.sortedReasons() {
		s.upkeepDegrade(ctx, deg, runRow.ID, plan.UpkeepSourceToolchainDrift, r, reasons[r])
	}
	return files, len(files)
}

// upkeepReasonCounts is reason → count for one source.
type upkeepReasonCounts map[string]int

func (c upkeepReasonCounts) sortedReasons() []string {
	out := make([]string, 0, len(c))
	for r := range c {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// gatherUpkeepPinFiles reads the fixed pin-file set through src: go.work, the
// go.mod of each go.work `use` dir (capped at upkeepPinMaxWorkspaceDirs), the
// root go.mod, both root golangci configs, the listed .github/workflows YAML
// files (capped at upkeepPinMaxWorkflowFiles), AGENTS.md, docs/api/README.md
// and docs/api/v0.md. An absent file (forge.ErrNotFound) is not a degrade.
// Every other partial read is counted by reason.
func gatherUpkeepPinFiles(ctx context.Context, src upkeepPinSource) (map[string]string, upkeepReasonCounts) {
	files := map[string]string{}
	reasons := upkeepReasonCounts{}
	budgetHit := false

	read := func(p string) {
		if budgetHit {
			reasons[upkeepDegradeBudgetExceeded]++
			return
		}
		if ctx.Err() != nil {
			budgetHit = true
			reasons[upkeepDegradeBudgetExceeded]++
			return
		}
		b, err := src.FetchFile(ctx, p)
		switch {
		case errors.Is(err, forge.ErrNotFound):
			return
		case err != nil && ctx.Err() != nil:
			budgetHit = true
			reasons[upkeepDegradeBudgetExceeded]++
			return
		case err != nil:
			reasons[upkeepDegradePinFetchFailed]++
			return
		case len(b) > upkeepPinFileMaxBytes:
			reasons[upkeepDegradePinFileTooLarge]++
			return
		}
		files[p] = string(b)
	}

	read("go.work")
	if work, ok := files["go.work"]; ok {
		dirs := goWorkUseDirs(work)
		if len(dirs) > upkeepPinMaxWorkspaceDirs {
			reasons[upkeepDegradeWorkspaceDirsCapped] += len(dirs) - upkeepPinMaxWorkspaceDirs
			dirs = dirs[:upkeepPinMaxWorkspaceDirs]
		}
		for _, d := range dirs {
			read(d + "/go.mod")
		}
	}
	for _, p := range upkeepPinFixedFiles {
		read(p)
	}

	if !budgetHit && ctx.Err() == nil {
		listed, err := src.ListWorkflowFiles(ctx)
		switch {
		case errors.Is(err, errUpkeepWorkflowListingUnavailable):
			reasons[upkeepDegradeWorkflowListingUnavailable]++
		case errors.Is(err, forge.ErrNotFound):
			// No workflow directory: nothing to read, not a degrade.
		case err != nil && ctx.Err() != nil:
			budgetHit = true
			reasons[upkeepDegradeBudgetExceeded]++
		case err != nil:
			reasons[upkeepDegradeWorkflowListingFailed]++
		default:
			wf := upkeepWorkflowYAML(listed)
			if len(wf) > upkeepPinMaxWorkflowFiles {
				reasons[upkeepDegradeWorkflowFilesCapped] += len(wf) - upkeepPinMaxWorkflowFiles
				wf = wf[:upkeepPinMaxWorkflowFiles]
			}
			for _, p := range wf {
				read(p)
			}
		}
	} else {
		budgetHit = true
		reasons[upkeepDegradeBudgetExceeded]++
	}
	return files, reasons
}

// upkeepWorkflowYAML keeps the listed paths that are .yml / .yaml files
// directly under .github/workflows, sorted and deduplicated.
func upkeepWorkflowYAML(listed []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range listed {
		c := path.Clean(p)
		ext := path.Ext(c)
		if path.Dir(c) != upkeepWorkflowDir || (ext != ".yml" && ext != ".yaml") || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// goWorkUseDirs returns the repo-relative directories a go.work `use`s, in
// both the single-line (`use ./backend`) and block (`use (` … `)`) forms,
// sorted and deduplicated. The root dir (".") is dropped — the root go.mod is
// read anyway — and so is any dir that is absolute or escapes the repo.
func goWorkUseDirs(work string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		tok, _, _ = strings.Cut(tok, "//")
		tok = strings.Trim(strings.TrimSpace(tok), "\"`")
		if tok == "" {
			return
		}
		c := path.Clean(tok)
		if c == "." || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") || seen[c] {
			return
		}
		seen[c] = true
		out = append(out, c)
	}
	inBlock := false
	for _, raw := range strings.Split(work, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if inBlock {
			if strings.HasPrefix(line, ")") {
				inBlock = false
				continue
			}
			add(line)
			continue
		}
		rest, ok := strings.CutPrefix(line, "use")
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '(') {
			continue
		}
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, "(") {
			inBlock = true
			if inner := strings.TrimSpace(strings.TrimPrefix(rest, "(")); inner != "" {
				if before, closed := strings.CutSuffix(inner, ")"); closed {
					inBlock = false
					inner = before
				}
				add(inner)
			}
			continue
		}
		add(rest)
	}
	sort.Strings(out)
	return out
}

// gatherUpkeepFlakes reads the verify history of recent same-tenancy runs'
// implement stages. It pages ListRuns newest-first over the run's repository
// and account, and per run:
//   - skips the scanning run itself;
//   - stops at the first run older than upkeepFlakeWindow (ListRuns orders
//     created_at DESC, id DESC);
//   - drops a run upkeepRunOwnershipRefusal rejects, so only runs the
//     upkeep_report ingest would accept as citations are ever read;
//   - counts toward upkeepFlakeRunScanCap.
//
// Per implement stage it reads the newest redacted bundle (at most
// upkeepFlakeBundleCap bundles in total) and maps its gate evidence to a
// FlakeStage. A stage with no redacted trace or no gate evidence contributes
// nothing and is not a degrade.
func (s *Server) gatherUpkeepFlakes(ctx context.Context, runRow *run.Run, deg *upkeepDegrades) (stages []upkeep.FlakeStage, runsScanned, stagesScanned int) {
	src := plan.UpkeepSourceFlake
	degrade := func(reason string, attrs ...slog.Attr) {
		s.upkeepDegrade(ctx, deg, runRow.ID, src, reason, 1, attrs...)
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.TraceStore == nil {
		degrade(upkeepDegradeTraceStoreUnwired)
		return nil, 0, 0
	}
	cutoff := time.Now().Add(-upkeepFlakeWindow)
	bundlesRead, noTrace := 0, 0

	for offset := 0; ; offset += upkeepFlakeRunPageSize {
		if ctx.Err() != nil {
			degrade(upkeepDegradeBudgetExceeded)
			break
		}
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			Repo: runRow.Repo, AccountID: runRow.AccountID, Limit: upkeepFlakeRunPageSize, Offset: offset,
		})
		if err != nil {
			if ctx.Err() != nil {
				degrade(upkeepDegradeBudgetExceeded)
			} else {
				degrade(upkeepDegradeRunListFailed, slog.String("error", err.Error()))
			}
			break
		}
		stop := false
		for _, rn := range page {
			if ctx.Err() != nil {
				degrade(upkeepDegradeBudgetExceeded)
				stop = true
				break
			}
			if rn.ID == runRow.ID {
				continue
			}
			if rn.CreatedAt.Before(cutoff) {
				stop = true
				break
			}
			if upkeepRunOwnershipRefusal(rn, runRow) != "" {
				continue
			}
			if runsScanned >= upkeepFlakeRunScanCap {
				degrade(upkeepDegradeRunScanCapped)
				stop = true
				break
			}
			runsScanned++
			got, n, halt := s.gatherUpkeepRunFlakes(ctx, rn, &bundlesRead, &noTrace, degrade)
			stages = append(stages, got...)
			stagesScanned += n
			if halt {
				stop = true
				break
			}
		}
		if stop || len(page) < upkeepFlakeRunPageSize {
			break
		}
	}
	if noTrace > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "upkeep evidence: implement stages without a redacted trace",
			slog.String("run_id", runRow.ID.String()), slog.Int("stages", noTrace))
	}
	return stages, runsScanned, stagesScanned
}

// gatherUpkeepRunFlakes reads one run's implement stages. halt reports that
// the whole gather must stop (bundle cap reached or budget exceeded).
func (s *Server) gatherUpkeepRunFlakes(ctx context.Context, rn *run.Run, bundlesRead, noTrace *int, degrade func(string, ...slog.Attr)) (stages []upkeep.FlakeStage, scanned int, halt bool) {
	cited := slog.String("cited_run_id", rn.ID.String())
	rows, err := s.cfg.RunRepo.ListStagesForRun(ctx, rn.ID)
	if err != nil {
		if ctx.Err() != nil {
			degrade(upkeepDegradeBudgetExceeded, cited)
			return nil, 0, true
		}
		degrade(upkeepDegradeStageListFailed, cited, slog.String("error", err.Error()))
		return nil, 0, false
	}
	var impl []*run.Stage
	for _, st := range rows {
		if st.Type == run.StageTypeImplement {
			impl = append(impl, st)
		}
	}
	if len(impl) == 0 {
		return nil, 0, false
	}
	sort.SliceStable(impl, func(i, j int) bool { return impl[i].Sequence < impl[j].Sequence })
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, rn.ID, "trace_uploaded")
	if err != nil {
		if ctx.Err() != nil {
			degrade(upkeepDegradeBudgetExceeded, cited)
			return nil, 0, true
		}
		degrade(upkeepDegradeTraceListFailed, cited, slog.String("error", err.Error()))
		return nil, 0, false
	}
	for _, st := range impl {
		hash, ok := pickRedactedTraceHash(entries, st.ID)
		if !ok {
			*noTrace++
			continue
		}
		if *bundlesRead >= upkeepFlakeBundleCap {
			degrade(upkeepDegradeBundleCapReached, cited)
			return stages, scanned, true
		}
		if ctx.Err() != nil {
			degrade(upkeepDegradeBudgetExceeded, cited)
			return stages, scanned, true
		}
		*bundlesRead++
		ge, reason := s.readUpkeepGateEvidence(ctx, rn.ID, hash)
		switch {
		case reason == upkeepDegradeBudgetExceeded:
			degrade(reason, cited)
			return stages, scanned, true
		case reason != "":
			degrade(reason, cited, slog.String("stage_id", st.ID.String()))
			continue
		case ge == nil:
			continue // no gate evidence in the bundle
		}
		scanned++
		fs := upkeep.FlakeStage{RunID: rn.ID.String(), StageID: st.ID.String(), InfraRetries: ge.FlakeRetries}
		for _, v := range ge.VerifyRuns {
			fs.Attempts = append(fs.Attempts, upkeep.VerifyAttempt{TreeSHA: v.TreeSHA, Outcome: v.Outcome, OutputTail: v.OutputTail})
		}
		stages = append(stages, fs)
	}
	return stages, scanned, false
}

// readUpkeepGateEvidence reads one redacted bundle (bounded at
// upkeepBundleMaxBytes) and extracts its gate evidence. It returns (nil, "")
// when the bundle carries no gate evidence, and a degrade reason on failure.
func (s *Server) readUpkeepGateEvidence(ctx context.Context, runID uuid.UUID, hash string) (*bundle.GateEvidence, string) {
	body, err := s.cfg.TraceStore.Get(ctx, tracestore.BundleRef{RunID: runID, Variant: tracestore.VariantRedacted, ContentHash: hash})
	if err != nil {
		if ctx.Err() != nil {
			return nil, upkeepDegradeBudgetExceeded
		}
		return nil, upkeepDegradeTraceFetchFailed
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, upkeepBundleMaxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, upkeepDegradeBudgetExceeded
		}
		return nil, upkeepDegradeTraceFetchFailed
	}
	if int64(len(data)) > upkeepBundleMaxBytes {
		return nil, upkeepDegradeBundleTooLarge
	}
	ge, err := bundle.ExtractGateEvidence(data)
	if errors.Is(err, bundle.ErrNoGateEvidence) {
		return nil, ""
	}
	if err != nil {
		return nil, upkeepDegradeGateEvidenceParseFailed
	}
	return &ge, ""
}
