package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// appInstallURL is the GitHub App installation entrypoint. Kept
// byte-identical to the remediation doctor_onboarding.go emits so the
// init checklist and the doctor rung point at the same place.
const appInstallURL = "https://github.com/apps/fishhawk/installations/new"

// runInit implements `fishhawk init` — the primary onboarding surface.
//
// It picks an autonomy preset (low|medium|high) plus a few structured
// deltas, writes a schema-valid .fishhawk/workflows.yaml (refusing to
// clobber an existing spec unless --force), ensures the AGENTS.md
// managed block + CLAUDE.md bridge via the E29.2 bridge package, prints
// the out-of-band checklist for the three prerequisites init cannot
// perform, then runs the doctor preflight (soft — a doctor failure does
// not fail init, because the scaffold itself succeeded).
//
// Reuses the E29.1 spec.Generate preset generator (which validates its
// own output and fails closed on an invalid delta) and the E29.2
// bridge.EnsureAgentDocs (idempotent, preserves user content), so this
// is largely CLI wiring plus a printed checklist.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	presetFlag := fs.String("preset", "medium", "autonomy preset: low | medium | high")
	workingDir := fs.String("working-dir", ".", "directory to scaffold (walks up to the .git boundary for the repo root)")
	budgetUSD := fs.Int("budget-usd", 0, "override the feature_change weekly advisory cost ceiling (budgets[0].limit_usd)")
	singleReviewer := fs.Bool("single-reviewer", false, "drop the Codex agent reviewer, leaving Claude alone on every stage")
	humanGates := fs.String("human-gates", "", "comma-separated stage ids that keep their human gate; any stage with a gate whose id is not listed has it removed")
	force := fs.Bool("force", false, "overwrite an existing .fishhawk/workflows.yaml")
	repo := fs.String("repo", "", "target repo owner/name for the checklist and doctor preflight; auto-detected from git origin when empty")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: fishhawk init [--preset low|medium|high] [--working-dir D] [flags]")
		_, _ = fmt.Fprintln(stderr, "")
		_, _ = fmt.Fprintln(stderr, "Scaffold a repo for Fishhawk: write .fishhawk/workflows.yaml from an")
		_, _ = fmt.Fprintln(stderr, "autonomy preset, ensure the AGENTS.md + CLAUDE.md bridge, print the")
		_, _ = fmt.Fprintln(stderr, "out-of-band prerequisites, and run the doctor preflight.")
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

	// Which optional deltas were actually provided — a delta is applied
	// only when its flag was set, not merely defaulted.
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	root, err := resolveRepoRoot(*workingDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: %v\n", err)
		return exitFailure
	}
	specPath := filepath.Join(root, specFileName)

	// Non-destructive: refuse to clobber an existing spec unless --force.
	if !*force {
		if _, statErr := os.Stat(specPath); statErr == nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk init: %s already exists; pass --force to overwrite it\n", specPath)
			return exitFailure
		}
	}

	var deltas spec.Deltas
	if setFlags["budget-usd"] {
		v := *budgetUSD
		deltas.BudgetLimitUSD = &v
	}
	deltas.SingleReviewer = *singleReviewer
	if setFlags["human-gates"] {
		deltas.HumanGates = parseCommaList(*humanGates)
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
	_, _ = fmt.Fprintf(stdout, "wrote %s (preset: %s)\n", specPath, preset)

	// Instruction files: AGENTS.md managed block + CLAUDE.md @AGENTS.md import.
	res, err := bridge.EnsureAgentDocs(root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk init: write agent docs: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "AGENTS.md %s\n", res.AgentsMD)
	_, _ = fmt.Fprintf(stdout, "CLAUDE.md %s\n", res.ClaudeMD)

	printOnboardingChecklist(stdout, *repo)

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

// parsePreset maps a preset flag value to a spec.Preset, reporting
// whether it names one of the three known tiers.
func parsePreset(s string) (spec.Preset, bool) {
	switch spec.Preset(s) {
	case spec.PresetLow, spec.PresetMedium, spec.PresetHigh:
		return spec.Preset(s), true
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

// printOnboardingChecklist writes the three out-of-band prerequisites
// init deliberately does NOT perform: App install, operator-token
// issue, and execution-path setup. Restrained voice per
// BRAND_FOUNDATIONS §5.
func printOnboardingChecklist(w io.Writer, repo string) {
	target := repo
	if target == "" {
		target = "<owner/name>"
	}
	for _, line := range []string{
		"",
		"Scaffold written. Three prerequisites init does not perform — complete them before the first run:",
		"",
		"1. Install the Fishhawk GitHub App on " + target + ":",
		"     " + appInstallURL,
		"2. Issue an operator token:",
		"     fishhawkd token issue --subject <login> --scopes read:runs,write:runs,write:approvals,write:stages",
		"3. Configure the execution path:",
		"     - commit .github/workflows/fishhawk.yml",
		"     - set vars.FISHHAWK_BACKEND_URL",
		"     - set secrets.ANTHROPIC_API_KEY and secrets.OPENAI_API_KEY",
	} {
		_, _ = fmt.Fprintln(w, line)
	}
}
