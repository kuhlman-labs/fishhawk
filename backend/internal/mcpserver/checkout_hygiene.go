package mcpserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Checkout-hygiene probe (#5214). fishhawk_start_run (and, through the same
// helpers, the campaign-start verbs) reports a working_dir that is not a clean
// checkout of the remote default branch: a dirty tree, uncommitted .fishhawk/
// edits, and a HEAD that is detached, on another branch, or ahead of or behind
// refs/remotes/origin/HEAD as last fetched. The probe is BEST-EFFORT and
// OFFLINE: it never fetches, never authenticates, and every git failure
// degrades that one check to "no finding" rather than a tool error. It reports;
// it never refuses a create.

// checkoutProbeTimeout bounds the whole probe (every git call shares it).
const checkoutProbeTimeout = 5 * time.Second

// maxCheckoutValueBytes bounds each repository-controlled string (a dirty path,
// a branch name, the resolved base ref) interpolated into a warning.
const maxCheckoutValueBytes = 80

// maxCheckoutDirtyPathsNamed bounds how many dirty paths a warning names before
// summarising the rest as a count.
const maxCheckoutDirtyPathsNamed = 5

// checkoutGitHardeningArgs are passed on EVERY probe git call, before -C. They
// neutralise the repository-config hooks a status/rev-parse could otherwise
// execute (an fsmonitor command, a hooks directory, an ssh command) and keep
// status from taking .git/index.lock, so the probe never contends with a
// concurrent runner's git operations.
var checkoutGitHardeningArgs = []string{
	"--no-optional-locks",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.sshCommand=false",
}

// checkoutGitArgv composes the full argv (after the "git" program name) for one
// probe call: the hardening flags, then -C dir, then the subcommand.
func checkoutGitArgv(dir string, args ...string) []string {
	argv := make([]string, 0, len(checkoutGitHardeningArgs)+2+len(args))
	argv = append(argv, checkoutGitHardeningArgs...)
	argv = append(argv, "-C", dir)
	return append(argv, args...)
}

// checkoutGitEnv is the probe's MINIMAL environment: PATH and HOME only, plus
// switches that ignore the system and global git config, disable optional
// locks and refuse a terminal prompt. Nothing else is inherited, so no forge
// token or credential helper setting in the serving process's environment
// reaches the subprocess.
func checkoutGitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
}

// checkoutGitCommand builds the hardened *exec.Cmd for one probe call. It is the
// one place the argv and environment are composed; runCheckoutGit runs it.
func checkoutGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", checkoutGitArgv(dir, args...)...)
	cmd.Env = checkoutGitEnv()
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	return cmd
}

// runCheckoutGit is the production checkoutGit: it runs the hardened command and
// returns stdout.
func runCheckoutGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := checkoutGitCommand(ctx, dir, args...).Output()
	return string(out), err
}

// checkoutGit is the package-level git seam every probe call goes through.
// Tests replace it with canned output; production uses runCheckoutGit.
var checkoutGit = runCheckoutGit

// checkoutState is what probeCheckout read from a checkout. A zero field means
// "not observed" as much as "clean": every check degrades independently.
type checkoutState struct {
	// DirtyPaths are the porcelain paths (repository-root relative), nil when
	// the tree is clean or git status failed.
	DirtyPaths []string
	// Branch is the current branch short name; empty when detached or unread.
	Branch string
	// Detached is true when HEAD is not on a branch.
	Detached bool
	// BaseRef is refs/remotes/origin/HEAD's short target (e.g. "origin/main");
	// empty when it does not resolve, which skips the off-base checks only.
	BaseRef string
	// Ahead / Behind count commits relative to BaseRef as last fetched;
	// meaningful only when CountsKnown.
	Ahead, Behind int
	CountsKnown   bool
}

// probeCheckout reads dir's hygiene offline under checkoutProbeTimeout. ok is
// false — and the caller reports nothing — when dir is not inside a git work
// tree (or git is absent). Each later git error degrades that one check.
func probeCheckout(ctx context.Context, dir string) (checkoutState, bool) {
	ctx, cancel := context.WithTimeout(ctx, checkoutProbeTimeout)
	defer cancel()

	out, err := checkoutGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(out) != "true" {
		return checkoutState{}, false
	}

	var st checkoutState
	if out, err := checkoutGit(ctx, dir, "status", "--porcelain", "-z"); err == nil {
		st.DirtyPaths = parsePorcelainZ(out)
	}
	if out, err := checkoutGit(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		switch b := strings.TrimSpace(out); b {
		case "":
		case "HEAD":
			st.Detached = true
		default:
			st.Branch = b
		}
	}
	if out, err := checkoutGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if base := strings.TrimSpace(out); usableBaseRef(base) {
			st.BaseRef = base
			if out, err := checkoutGit(ctx, dir, "rev-list", "--left-right", "--count", base+"...HEAD"); err == nil {
				if f := strings.Fields(out); len(f) == 2 {
					behind, berr := strconv.Atoi(f[0])
					ahead, aerr := strconv.Atoi(f[1])
					if berr == nil && aerr == nil {
						st.Behind, st.Ahead, st.CountsKnown = behind, ahead, true
					}
				}
			}
		}
	}
	return st, true
}

// usableBaseRef admits a resolved base ref only when it cannot be read as a git
// option or a range by the rev-list call it is interpolated into.
func usableBaseRef(base string) bool {
	if base == "" || strings.HasPrefix(base, "-") || strings.Contains(base, "..") {
		return false
	}
	for _, r := range base {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// parsePorcelainZ parses `git status --porcelain -z` output into its paths.
// Each entry is "XY path\x00"; a rename or copy entry is followed by its
// original path, which is skipped.
func parsePorcelainZ(out string) []string {
	var paths []string
	tokens := strings.Split(out, "\x00")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) < 4 {
			continue
		}
		paths = append(paths, tok[3:])
		if tok[0] == 'R' || tok[0] == 'C' {
			i++
		}
	}
	return paths
}

// checkoutSurface selects the warning wording for where the spec comes from.
type checkoutSurface int

const (
	// checkoutSurfaceStartRunDiscovered: fishhawk_start_run auto-discovered the
	// spec from working_dir, so the run pinned the WORKING-TREE spec bytes.
	checkoutSurfaceStartRunDiscovered checkoutSurface = iota
	// checkoutSurfaceStartRunExplicitSpec: fishhawk_start_run was given an
	// inline workflow_spec or a spec_file, so the checkout's .fishhawk/ is not
	// what the run pinned and W2 does not fire.
	checkoutSurfaceStartRunExplicitSpec
	// checkoutSurfaceCampaign: fishhawk_start_campaign /
	// fishhawk_start_campaign_item_run, whose runs fetch the spec from the forge
	// at workflow_ref, never from the checkout.
	checkoutSurfaceCampaign
)

// quoteCheckoutValue renders one repository-controlled string as quoted DATA:
// invalid UTF-8, control, format (bidi, zero-width) and line/paragraph
// separator characters are stripped, the result is cut to
// maxCheckoutValueBytes (rune-safe, "..." marking the cut), then Go-quoted.
func quoteCheckoutValue(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		b.WriteRune(r)
	}
	clean := b.String()
	if len(clean) > maxCheckoutValueBytes {
		clean = strings.ToValidUTF8(clean[:maxCheckoutValueBytes-3], "") + "..."
	}
	return strconv.Quote(clean)
}

// isFishhawkSpecPath reports whether a porcelain path sits under a .fishhawk/
// directory (the repository root's, or a nested one spec discovery could walk
// up to).
func isFishhawkSpecPath(p string) bool {
	return strings.HasPrefix(p, ".fishhawk/") || strings.Contains(p, "/.fishhawk/")
}

// quotedPathList names up to maxCheckoutDirtyPathsNamed paths, each quoted as
// data, and summarises the rest as a count.
func quotedPathList(paths []string) string {
	shown := paths
	if len(shown) > maxCheckoutDirtyPathsNamed {
		shown = shown[:maxCheckoutDirtyPathsNamed]
	}
	quoted := make([]string, len(shown))
	for i, p := range shown {
		quoted[i] = quoteCheckoutValue(p)
	}
	list := strings.Join(quoted, ", ")
	if rest := len(paths) - len(shown); rest > 0 {
		list += fmt.Sprintf(" and %d more", rest)
	}
	return list
}

// checkoutHygieneWarnings is the pure warning composer over a probed state. It
// returns nil for a clean checkout on the remote default branch. surface picks
// the spec-source wording (see checkoutSurface).
func checkoutHygieneWarnings(surface checkoutSurface, dir string, st checkoutState) []string {
	var out []string
	qdir := strconv.Quote(dir)

	// W1: dirty tree.
	if len(st.DirtyPaths) > 0 {
		out = append(out, fmt.Sprintf(
			"checkout hygiene: working_dir %s has %d uncommitted or untracked path(s) (git status): %s. Clean the tree (check git status; commit, stash or remove the leftovers) before dispatching a stage: a runner sweep can carry leftover files into a stage commit.",
			qdir, len(st.DirtyPaths), quotedPathList(st.DirtyPaths)))
	}

	// W2: uncommitted .fishhawk/ edits, worded for where the spec comes from.
	var specPaths []string
	for _, p := range st.DirtyPaths {
		if isFishhawkSpecPath(p) {
			specPaths = append(specPaths, p)
		}
	}
	if len(specPaths) > 0 {
		switch surface {
		case checkoutSurfaceStartRunDiscovered:
			out = append(out, fmt.Sprintf(
				"checkout hygiene: working_dir %s has uncommitted change(s) under .fishhawk/ (%s). This run pinned the WORKING-TREE workflow spec bytes, which no base commit carries, and the run branch will not include them. The spec is fixed for the run's life: commit the change to the base branch first, then start a new run (cancel this one if the uncommitted spec is not what it should run).",
				qdir, quotedPathList(specPaths)))
		case checkoutSurfaceCampaign:
			out = append(out, fmt.Sprintf(
				"checkout hygiene: working_dir %s has uncommitted change(s) under .fishhawk/ (%s). A campaign run fetches its workflow spec from the forge at workflow_ref, not from this checkout, so the edit is INVISIBLE to the run until it is committed and pushed to the base branch.",
				qdir, quotedPathList(specPaths)))
		}
	}

	// W3: HEAD detached, on another branch, or ahead of / behind the remote
	// default branch as last fetched.
	var findings []string
	if st.Detached {
		findings = append(findings, "HEAD is detached")
	}
	if st.BaseRef != "" {
		baseBranch := strings.TrimPrefix(st.BaseRef, "origin/")
		if st.Branch != "" && st.Branch != baseBranch {
			findings = append(findings, fmt.Sprintf("HEAD is on branch %s, not %s",
				quoteCheckoutValue(st.Branch), quoteCheckoutValue(baseBranch)))
		}
		if st.CountsKnown && st.Ahead > 0 {
			findings = append(findings, fmt.Sprintf("HEAD is %d commit(s) ahead (unmerged local commits)", st.Ahead))
		}
		if st.CountsKnown && st.Behind > 0 {
			findings = append(findings, fmt.Sprintf("HEAD is %d commit(s) behind", st.Behind))
		}
	}
	if len(findings) > 0 {
		base := "the remote default branch as last fetched (refs/remotes/origin/HEAD), which did not resolve, so ahead/behind was not checked"
		if st.BaseRef != "" {
			base = fmt.Sprintf("the remote default branch as last fetched (refs/remotes/origin/HEAD), which resolves to %s", quoteCheckoutValue(st.BaseRef))
		}
		specClause := "A"
		if surface == checkoutSurfaceStartRunDiscovered {
			specClause = "This run's workflow spec was read from this checkout, and a"
		}
		out = append(out, fmt.Sprintf(
			"checkout hygiene: working_dir %s is not a clean checkout of %s: %s. %s lineage worktree falls back to seeding from this checkout's HEAD when the declared base cannot be fetched. Bind a clean checkout of that default branch, not a session-scoped worktree carrying unmerged commits; when behind, fetch and fast-forward.",
			qdir, base, strings.Join(findings, "; "), specClause))
	}
	return out
}
