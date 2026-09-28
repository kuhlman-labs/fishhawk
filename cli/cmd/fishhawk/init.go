package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/cli/internal/scaffold"
	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// appInstallURL is the GitHub App installation entrypoint. Kept
// byte-identical to the remediation doctor_onboarding.go emits so the
// init checklist and the doctor rung point at the same place.
const appInstallURL = "https://github.com/apps/fishhawk/installations/new"

// initScaffoldFiles is the scaffold render seam: init renders (and so
// self-validates) the scaffold BEFORE writing anything. Tests swap it to
// observe that a render failure fails init with nothing written.
var initScaffoldFiles = scaffold.Files

// runInit implements `fishhawk init` — the primary onboarding surface.
//
// It picks an autonomy preset (low|medium|high) plus a few structured
// deltas and writes a schema-valid .fishhawk/workflows.yaml; writes the
// charter SKELETON, operator overlay and work-management config via the
// scaffold package (E74.3 / #3718); ensures the AGENTS.md managed block +
// CLAUDE.md bridge via the E29.2 bridge package; registers the Fishhawk
// MCP server with a detected agent CLI (or prints the exact command);
// prints the local-path-first checklist; then runs the doctor preflight
// (soft — a doctor failure does not fail init, because the scaffold itself
// succeeded).
//
// Every write is idempotent and reported per file (created /
// skipped-existing), so re-running init over a scaffolded repository
// changes no bytes and exits 0. --force regenerates ONLY the workflow spec;
// the scaffold writer has no force parameter, so a charter a human wrote is
// never touched.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	presetFlag := fs.String("preset", "medium", "autonomy preset: low | medium | high")
	shapeFlag := fs.String("shape", "app", "repository shape: app | config-only")
	workingDir := fs.String("working-dir", ".", "directory to scaffold (walks up to the .git boundary for the repo root)")
	budgetUSD := fs.Int("budget-usd", 0, "override the feature_change weekly advisory cost ceiling (budgets[0].limit_usd)")
	singleReviewer := fs.Bool("single-reviewer", false, "drop the second agent reviewer, leaving one agent reviewer on every stage")
	humanGates := fs.String("human-gates", "", "comma-separated stage ids that keep their human gate; any stage with a gate whose id is not listed has it removed")
	force := fs.Bool("force", false, "regenerate an existing .fishhawk/workflows.yaml from --preset/--shape (never touches the charter, operator overlay or work-management config)")
	repo := fs.String("repo", "", "target repo owner/name for the checklist and doctor preflight; auto-detected from git origin when empty")
	projectNumber := fs.Int("project-number", 0, "GitHub Projects number for .fishhawk/work-management.yaml (the integer in the Project URL); without it the project block is written commented, to fill in")
	projectOwner := fs.String("project-owner", "", "GitHub Projects owner login; overrides the owner detected from git origin")
	gitlabProject := fs.String("gitlab-project", "", "target GitLab: namespaced project path (group/sub/project) for .fishhawk/work-management.yaml; selects the gitlab provider")
	skipMCP := fs.Bool("skip-mcp-register", false, "do not register the Fishhawk MCP server with a detected agent CLI; print the command instead")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk init [--preset low|medium|high] [--shape app|config-only] [--working-dir D] [flags]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Scaffold a repo for Fishhawk: write .fishhawk/workflows.yaml from an")
		_, _ = fmt.Fprintln(stderr, "autonomy preset, a charter skeleton, the operator overlay and the")
		_, _ = fmt.Fprintln(stderr, "work-management config; ensure the AGENTS.md + CLAUDE.md bridge;")
		_, _ = fmt.Fprintln(stderr, "register the MCP server; print the next steps; run the doctor preflight.")
		_, _ = fmt.Fprintln(stderr, "Existing files are never rewritten (only --force regenerates the spec).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	preset, ok := parsePreset(*presetFlag)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: unknown --preset %q (want one of low, medium, high)\n", *presetFlag)
		return exitUsage
	}

	shape, ok := parseShape(*shapeFlag)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: unknown --shape %q (want one of app, config-only)\n", *shapeFlag)
		return exitUsage
	}

	// Which optional deltas were actually provided — a delta is applied
	// only when its flag was set, not merely defaulted.
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if setFlags["project-number"] && *projectNumber < 1 {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: --project-number must be >= 1 (got %d)\n", *projectNumber)
		return exitUsage
	}

	root, err := resolveRepoRoot(*workingDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: %v\n", err)
		return exitFailure
	}

	target := resolveForgeTarget(root, *projectOwner, *projectNumber, *gitlabProject)
	for _, note := range target.notes {
		_, _ = fmt.Fprintf(stdout, "forge: %s\n", note)
	}
	opts := target.opts
	opts.Autonomy = string(preset)

	// Render the scaffold BEFORE touching disk, so an invalid render fails
	// init with nothing written.
	if _, err := initScaffoldFiles(opts); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: %v\n", err)
		return exitFailure
	}

	if code := writeInitSpec(root, preset, shape, *force, setFlags, *budgetUSD, *singleReviewer, *humanGates, stdout, stderr); code != exitOK {
		return code
	}

	res, err := scaffold.EnsureFiles(root, opts)
	for _, fr := range res.Files {
		_, _ = fmt.Fprintf(stdout, "%s %s\n", fr.Path, fr.Status)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: %v\n", err)
		return exitFailure
	}

	// Instruction files: AGENTS.md managed block + CLAUDE.md @AGENTS.md import.
	bres, err := bridge.EnsureAgentDocs(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: write agent docs: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "AGENTS.md %s\n", bres.AgentsMD)
	_, _ = fmt.Fprintf(stdout, "CLAUDE.md %s\n", bres.ClaudeMD)

	// Name the fields the written work-management config leaves to fill —
	// only when init wrote it; an existing config is the operator's.
	var incomplete []string
	for _, fr := range res.Files {
		if fr.Path == scaffold.WorkManagementPath && fr.Status == scaffold.StatusCreated {
			incomplete = opts.Missing()
		}
	}
	if len(incomplete) > 0 {
		_, _ = fmt.Fprintf(stdout, "%s is incomplete: fill %s in its commented project block (or re-create it with --project-owner/--project-number)\n",
			scaffold.WorkManagementPath, strings.Join(incomplete, " and "))
	}

	registerMCP(stdout, root, *cf.backendURL, *skipMCP)

	checklistRepo := *repo
	if checklistRepo == "" {
		checklistRepo = target.repo
	}
	printOnboardingChecklist(stdout, checklistRepo, opts.Provider, incomplete)

	// Closing doctor preflight — surfaces the same readiness rungs. Soft:
	// the scaffold succeeded, so a doctor failure is reported but does not
	// fail init.
	doctorArgs := []string{
		"--working-dir", root,
		"--backend-url", *cf.backendURL,
		"--token", *cf.token,
	}
	if *repo != "" {
		doctorArgs = append(doctorArgs, "--repo", *repo)
	}
	_, _ = fmt.Fprintln(stdout, "")
	_, _ = fmt.Fprintln(stdout, "Preflight (fishhawk doctor):")
	if runDoctor(doctorArgs, stdout, stderr) != exitOK {
		_, _ = fmt.Fprintln(stdout, "doctor reported issues above; address them and re-run `fishhawk doctor`.")
	}
	return exitOK
}

// writeInitSpec writes .fishhawk/workflows.yaml from the preset. An existing
// spec is reported skipped-existing (exit 0) unless force, which regenerates
// it; that relaxation is what makes a re-run of init uniformly idempotent.
func writeInitSpec(root string, preset spec.Preset, shape spec.Shape, force bool, setFlags map[string]bool,
	budgetUSD int, singleReviewer bool, humanGates string, stdout, stderr io.Writer) int {
	specPath := filepath.Join(root, specFileName)
	existed := false
	if _, statErr := os.Stat(specPath); statErr == nil {
		existed = true
		if !force {
			_, _ = fmt.Fprintf(stdout, "%s %s (pass --force to regenerate from --preset/--shape)\n", specFileName, scaffold.StatusSkippedExisting)
			return exitOK
		}
	}

	var deltas spec.Deltas
	deltas.Shape = shape
	if setFlags["budget-usd"] {
		v := budgetUSD
		deltas.BudgetLimitUSD = &v
	}
	deltas.SingleReviewer = singleReviewer
	if setFlags["human-gates"] {
		deltas.HumanGates = parseCommaList(humanGates)
	}

	specBytes, err := spec.Generate(preset, deltas)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: generate spec: %v\n", err)
		return exitFailure
	}
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: create .fishhawk dir: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(specPath, specBytes, 0o644); err != nil { //nolint:gosec // 0644 is the intended spec-file mode
		_, _ = fmt.Fprintf(stderr, "fishhawk init: write spec: %v\n", err)
		return exitFailure
	}
	status := string(scaffold.StatusCreated)
	if existed {
		status = "regenerated (--force)"
	}
	_, _ = fmt.Fprintf(stdout, "%s %s (preset: %s, shape: %s)\n", specFileName, status, preset, shape)
	return exitOK
}

// forgeTarget is init's resolved work-management target.
type forgeTarget struct {
	opts  scaffold.Options
	repo  string   // owner/name (or GitLab path) for the checklist; "" when unknown
	notes []string // one "forge:" line each
}

// resolveForgeTarget derives the work-management provider and connection
// from `git remote get-url origin` plus the explicit flags. --gitlab-project
// selects gitlab outright; a gitlab-host origin selects gitlab; a github.com
// origin selects github_projects with its owner. An absent or unrecognised
// origin DEGRADES to github_projects with the owner left as a fill-me-in
// marker, and says so.
func resolveForgeTarget(root, projectOwner string, projectNumber int, gitlabProject string) forgeTarget {
	var t forgeTarget
	raw, originErr := gitRemoteOriginURL(root)
	ghOwner, ghName, ghErr := "", "", fmt.Errorf("no origin")
	glPath, glOK := "", false
	if originErr == nil {
		ghOwner, ghName, ghErr = parseGitHubRemote(raw)
		glPath, glOK = parseGitLabRemote(raw)
	}

	switch {
	case gitlabProject != "" || (ghErr != nil && glOK):
		t.opts = scaffold.Options{Provider: scaffold.ProviderGitLab, GitLabProject: gitlabProject}
		t.repo = glPath
		if gitlabProject != "" {
			t.repo = gitlabProject
			t.notes = append(t.notes, fmt.Sprintf("%s (project %s, from --gitlab-project)", scaffold.ProviderGitLab, gitlabProject))
		} else {
			t.notes = append(t.notes, fmt.Sprintf("%s (project %s, from git origin)", scaffold.ProviderGitLab, glPath))
		}
		if projectOwner != "" || projectNumber != 0 {
			t.notes = append(t.notes, "--project-owner/--project-number ignored: they configure github_projects, not gitlab")
		}
		return t
	case ghErr == nil:
		t.repo = ghOwner + "/" + ghName
		t.notes = append(t.notes, fmt.Sprintf("%s (repo %s, from git origin)", scaffold.ProviderGitHubProjects, t.repo))
	case originErr != nil:
		t.notes = append(t.notes, fmt.Sprintf("no git origin remote; defaulting to %s (pass --gitlab-project for GitLab)", scaffold.ProviderGitHubProjects))
	default:
		t.notes = append(t.notes, fmt.Sprintf("origin %q is not a github.com or GitLab remote; defaulting to %s (pass --gitlab-project for GitLab)", raw, scaffold.ProviderGitHubProjects))
	}
	owner := ghOwner
	if projectOwner != "" {
		owner = projectOwner
	}
	t.opts = scaffold.Options{Provider: scaffold.ProviderGitHubProjects, ProjectOwner: owner, ProjectNumber: projectNumber}
	return t
}

// parseGitLabRemote recognises a remote on a host whose name contains
// "gitlab" (gitlab.com or a self-managed gitlab.<corp> host) and returns its
// namespaced project path. Forms: https://host/g/p(.git),
// git@host:g/p(.git), ssh://git@host[:port]/g/p(.git). A self-managed host
// without "gitlab" in its name is not recognised — pass --gitlab-project.
func parseGitLabRemote(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "ssh://"), strings.HasPrefix(s, "http://"):
		rest := s[strings.Index(s, "://")+3:]
		var ok bool
		host, path, ok = strings.Cut(rest, "/")
		if !ok {
			return "", false
		}
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
	case strings.Contains(s, "@") && strings.Contains(s, ":"):
		at := strings.Index(s, "@")
		var ok bool
		host, path, ok = strings.Cut(s[at+1:], ":")
		if !ok {
			return "", false
		}
	default:
		return "", false
	}
	if !strings.Contains(strings.ToLower(host), "gitlab") {
		return "", false
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if !strings.Contains(path, "/") || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return "", false
	}
	return path, true
}

// parsePreset maps a preset flag value to a spec.Preset, reporting
// whether it names one of the three known tiers.
func parsePreset(s string) (spec.Preset, bool) {
	switch spec.Preset(s) {
	case spec.PresetLow, spec.PresetMedium, spec.PresetHigh:
		return spec.Preset(s), true
	}
	return "", false
}

// parseShape maps a shape flag value to a spec.Shape, reporting whether
// it names one of the two known repository shapes.
func parseShape(s string) (spec.Shape, bool) {
	switch spec.Shape(s) {
	case spec.ShapeApp, spec.ShapeConfigOnly:
		return spec.Shape(s), true
	}
	return "", false
}

// parseCommaList splits a comma-separated flag value into a non-nil
// slice of trimmed, non-empty entries. An empty (or whitespace-only)
// input yields a non-nil empty slice — for --human-gates that means
// "remove every human gate", distinct from the nil "leave gates as
// authored" of an unset flag.
func parseCommaList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// resolveRepoRoot walks up from workingDir to the directory containing
// .git and returns it. When no .git is found, the (absolute) working
// dir is treated as the root. Mirrors the .git boundary logic in
// spec_discover.go.
func resolveRepoRoot(workingDir string) (string, error) {
	start, err := filepath.Abs(workingDir)
	if err != nil {
		return "", fmt.Errorf("resolve working dir: %w", err)
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Filesystem root reached with no .git — fall back to the
			// working dir as the scaffold root.
			return start, nil
		}
		dir = parent
	}
}

// printOnboardingChecklist writes the steps init deliberately does NOT
// perform, LOCAL execution path first: fill the charter, run the backend,
// issue an operator token, (GitHub) install the App, then drive a run with
// the local runner — with the GitHub Actions path as the labelled
// alternative. Every `fishhawk` verb named here exists in the cmdinfo
// inventory (TestInit_ChecklistNamesLocalPathFirst). Restrained voice per
// BRAND_FOUNDATIONS §5.
func printOnboardingChecklist(w io.Writer, repo, provider string, incomplete []string) {
	target := repo
	if target == "" {
		target = "<owner/name>"
	}
	charterStep := "Fill in " + scaffold.CharterPath + " — it is human-authored: the north star, current phase, non-goals and every rubric line"
	if len(incomplete) > 0 {
		charterStep += "; and fill " + strings.Join(incomplete, " and ") + " in " + scaffold.WorkManagementPath
	}
	lines := []string{
		"",
		"Scaffold written. Next steps init does not perform — complete them before the first run:",
		"",
		"1. " + charterStep + ".",
		"2. Run the backend locally: fishhawkd, with Postgres and object storage (see docs/deploy/).",
		"3. Issue an operator token:",
		"     fishhawkd token issue --subject <login> --scopes read:runs,write:runs,write:approvals,write:stages",
	}
	n := 4
	if provider != scaffold.ProviderGitLab {
		lines = append(lines,
			fmt.Sprintf("%d. Install the Fishhawk GitHub App on %s:", n, target),
			"     "+appInstallURL)
		n++
	}
	lines = append(lines,
		fmt.Sprintf("%d. Drive a first run on the local execution path:", n),
		"     fishhawk run start --runner-kind local --repo "+target+" --workflow feature_change --issue <n>",
		"     fishhawk runner start --run-id <run-id> --stage-id <stage-id>",
		"   Alternative — the GitHub Actions execution path:",
		"     - commit .github/workflows/fishhawk.yml",
		"     - set vars.FISHHAWK_BACKEND_URL",
		"     - set secrets.ANTHROPIC_API_KEY and secrets.OPENAI_API_KEY",
	)
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}
