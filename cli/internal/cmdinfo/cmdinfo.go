// Package cmdinfo is the importable inventory of the fishhawk CLI
// surface: every leaf command, its one-line synopsis, its positional
// argument summary, and the exact set of flag NAMES it registers.
//
// It exists so two consumers render from ONE source of truth:
//
//   - cli/cmd/fishhawk's printUsage renders the top-level command list
//     from Commands(), and
//   - cli/internal/docgen renders the CLI reference page from Commands().
//
// cmdinfo imports nothing from package main, so both the binary and the
// docs generator can consume it without an import cycle.
//
// The inventory is hand-maintained, but it is NOT free-floating: the
// package-main test TestCLIFlagsMatchExecutableSurface drives every
// command in-process with `-h` and asserts SET EQUALITY, in both
// directions, between each command's live registered flag set (as the
// flag package renders it via its own flag.VisitAll walk) and the Flags
// slice here. A flag added to a command without updating cmdinfo fails;
// a cmdinfo flag that no longer exists fails. That binding is what makes
// the CLI reference non-transcribed in the sense that matters — it
// cannot silently drift from the binary.
package cmdinfo

// Command is one leaf command of the CLI.
type Command struct {
	// Key is the full command path as typed, space-joined:
	// "run start", "audit tail", "migrate-spec".
	Key string
	// Synopsis is a one-line description.
	Synopsis string
	// Args summarizes the positional arguments, e.g. "<run-id>" or
	// "[path]"; empty when the command takes flags only.
	Args string
	// Flags is the set of flag names registered on the command's
	// flag.FlagSet, WITHOUT the leading dash, including short-form
	// aliases (e.g. both "output" and "o") and the common flags
	// (backend-url, token, timeout) when the command binds them.
	Flags []string
}

// commonFlags are the three flags bindCommonFlags registers in
// cli/cmd/fishhawk. Commands that call it carry all three.
var commonFlags = []string{"backend-url", "token", "timeout"}

// withCommon returns the common flag set followed by extra, so an
// entry reads as "the common flags plus these".
func withCommon(extra ...string) []string {
	out := make([]string, 0, len(commonFlags)+len(extra))
	out = append(out, commonFlags...)
	out = append(out, extra...)
	return out
}

// Group is a top-level command group and its subcommand keys, used to
// cross-check the dispatch tables in package main.
type Group struct {
	Name        string
	Subcommands []string
}

// Groups returns the command groups and their subcommands. The
// subcommand list mirrors each dispatcher's `subcommand required (...)`
// message in package main; TestInventoryCoversDispatch pins the two
// together.
func Groups() []Group {
	return []Group{
		{Name: "run", Subcommands: []string{"start", "status", "list", "cancel", "open", "retry", "watch"}},
		{Name: "plan", Subcommands: []string{"approve", "reject", "revise"}},
		{Name: "token", Subcommands: []string{"login", "list"}},
		{Name: "deploy", Subcommands: []string{"status", "approve", "reject", "rollback"}},
		{Name: "release", Subcommands: []string{"preview", "prepare", "cut", "publish"}},
		{Name: "campaign", Subcommands: []string{"start", "status", "list", "resume"}},
		{Name: "audit", Subcommands: []string{"list", "tail"}},
		{Name: "runner", Subcommands: []string{"start"}},
	}
}

// Commands returns the full, ordered inventory. Order is the order the
// commands appear in the top-level usage listing.
func Commands() []Command {
	return []Command{
		// run
		{Key: "run start", Synopsis: "Trigger a workflow run.", Args: "",
			Flags: withCommon("repo", "workflow", "workflow-sha", "trigger-ref", "runner-kind",
				"working-dir", "spec-file", "issue", "override-budget", "upstream-run-id",
				"applies-to-override", "applies-to-override-reason")},
		{Key: "run status", Synopsis: "Show a run's current state.", Args: "<run-id>",
			Flags: withCommon("output", "o")},
		{Key: "run list", Synopsis: "List runs with optional filters.", Args: "",
			Flags: withCommon("repo", "workflow", "state", "limit", "cursor")},
		{Key: "run cancel", Synopsis: "Cancel an in-flight run.", Args: "<run-id>",
			Flags: withCommon()},
		{Key: "run open", Synopsis: "Open a run's detail page in the browser.", Args: "<run-id>",
			Flags: withCommon("print-url")},
		{Key: "run retry", Synopsis: "Retry a failed stage (takes a stage id, not a run id).", Args: "<stage-id>",
			Flags: withCommon("output", "o")},
		{Key: "run watch", Synopsis: "Block until a stage settles.", Args: "<run-id>",
			Flags: withCommon("stage", "until", "poll", "max-duration")},

		// plan
		{Key: "plan approve", Synopsis: "Approve the plan stage on a run.", Args: "<run-id>",
			Flags: withCommon("reason", "output", "o")},
		{Key: "plan reject", Synopsis: "Reject the plan stage on a run (category-D failure).", Args: "<run-id>",
			Flags: withCommon("reason", "output", "o")},
		{Key: "plan revise", Synopsis: "Force a constrained replan pass.", Args: "<run-id>",
			Flags: withCommon("constraint", "force", "output", "o")},

		// token
		{Key: "token login", Synopsis: "Log in via the OAuth device flow; mint + store a user-bound token.", Args: "",
			Flags: withCommon("provider", "client-id")},
		{Key: "token list", Synopsis: "List locally stored credentials (per backend URL).", Args: "",
			Flags: []string{}},

		// deploy
		{Key: "deploy status", Synopsis: "Show the deploy stage state and the deployment artifact.", Args: "<run-id>",
			Flags: withCommon("output", "o")},
		{Key: "deploy approve", Synopsis: "Approve the deploy stage's pre-execution gate (needs write:deploy).", Args: "<run-id>",
			Flags: withCommon("reason", "environment", "override-freeze", "output", "o")},
		{Key: "deploy reject", Synopsis: "Reject the deploy stage's pre-execution gate (category-D failure).", Args: "<run-id>",
			Flags: withCommon("reason", "output", "o")},
		{Key: "deploy rollback", Synopsis: "Roll back a settled deploy (re-dispatches the rollback path).", Args: "<run-id>",
			Flags: withCommon("output", "o")},

		// release
		{Key: "release preview", Synopsis: "Render release notes for a ref range without persisting.", Args: "",
			Flags: withCommon("repo", "from", "to", "output", "o")},
		{Key: "release prepare", Synopsis: "Persist rendered release notes as a release_notes artifact.", Args: "",
			Flags: withCommon("repo", "from", "to", "stage-id", "output", "o")},
		{Key: "release cut", Synopsis: "Record the operator's ratified release version (no git tag push).", Args: "",
			Flags: withCommon("repo", "run-id", "artifact-id", "version", "stage-id", "bump-level", "output", "o")},
		{Key: "release publish", Synopsis: "Write the notes to the GitHub Release body + asset.", Args: "",
			Flags: withCommon("repo", "tag", "run-id", "artifact-id", "stage-id", "output", "o")},

		// campaign
		{Key: "campaign start", Synopsis: "Create a campaign from an epic ref.", Args: "",
			Flags: withCommon("repo", "epic", "pause-policy", "operator-agent", "output", "o")},
		{Key: "campaign status", Synopsis: "Show a campaign's rollup status and next action.", Args: "<campaign-id>",
			Flags: withCommon("output", "o")},
		{Key: "campaign list", Synopsis: "List campaigns with optional filters.", Args: "",
			Flags: withCommon("repo", "state", "limit", "cursor")},
		{Key: "campaign resume", Synopsis: "Resume a paused campaign (hand back to the auto-driver).", Args: "<campaign-id>",
			Flags: withCommon("output", "o")},

		// audit
		{Key: "audit list", Synopsis: "List audit entries for a run.", Args: "<run-id>",
			Flags: withCommon("category", "stage", "limit", "cursor", "output", "o")},
		{Key: "audit tail", Synopsis: "Follow the audit log of a run in real time.", Args: "<run-id>",
			Flags: withCommon("interval", "output", "o", "max-polls")},

		// standalone
		{Key: "init", Synopsis: "Scaffold a repo for Fishhawk (workflow spec + agent docs + preflight).", Args: "",
			Flags: withCommon("preset", "working-dir", "budget-usd", "single-reviewer", "human-gates", "force", "repo")},
		{Key: "validate", Synopsis: "Validate a workflow spec file locally.", Args: "[path]",
			Flags: []string{"emit-resolved"}},
		{Key: "migrate-spec", Synopsis: "Migrate a workflow-v1 spec to workflow-v2 with an approval-eligibility report.", Args: "[path]",
			Flags: []string{"out", "in-place", "report-only"}},
		{Key: "runner start", Synopsis: "Spawn the fishhawk-runner locally against an already-minted run.", Args: "",
			Flags: withCommon("run-id", "stage-id", "workflow", "stage", "working-dir",
				"github-repo", "base-branch", "no-pr", "runner-binary")},
		{Key: "doctor", Synopsis: "Run local-loop install checks.", Args: "",
			Flags: withCommon("runner-binary", "working-dir", "repo", "spec-only",
				"run-verify-command", "skip-verify-command", "verify-timeout")},
		{Key: "file-issue", Synopsis: "File a work item (issue/bug/chore/adr) via repo conventions.", Args: "",
			Flags: withCommon("repo", "type", "summary", "body", "complexity", "status",
				"parent-epic", "run-id", "label", "supersedes", "companion-to", "evidence-run", "output", "o")},
		{Key: "diagnose", Synopsis: "Show a run's product-facts diagnostic bundle.", Args: "<run-id>",
			Flags: withCommon("output", "o")},
		{Key: "report-issue", Synopsis: "File an upstream Fishhawk product bug/feature with a redacted, deduped bundle.", Args: "<run-id>",
			Flags: withCommon("kind", "description", "include-free-text", "output", "o")},
		{Key: "export", Synopsis: "Assemble a complete compliance export (JSON or --csv) for external verification.", Args: "",
			Flags: withCommon("from", "to", "repo", "run", "limit", "csv", "out")},
	}
}
