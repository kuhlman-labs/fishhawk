package main

import (
	"fmt"
	"strings"
)

// remoteURLFor derives the git remote URL the runner pushes to (and reads
// back with ls-remote) for owner/repoName on the run's configured forge. It
// is the SINGLE derivation seam for every remote-touching git op the runner
// constructs: the implement/fix-up push, the fix-up push-landed probe, the
// conflict-resolution push and the acceptance scenario-corpus push all route
// through it.
//
// Before E45.80 (#3613) each of those four sites carried its own
// fmt.Sprintf("https://github.com/%s/%s", …) literal, so a --forge=gitlab run
// pushed at github.com and could not land its branch: the E45.5 forge
// abstraction reached the REST layer (openImplementChangeRequest, the MR
// opener) and never the git layer.
//
// Modes, one branch each:
//
//   - forgeGitHub, or the EMPTY zero-value cfg.forge that a hand-built config
//     (tests, crStageCfg) carries: https://github.com/<owner>/<repoName>,
//     byte-identical to the literal it replaced. parseFlags installs
//     forgeGitHub as the --forge default, so the empty case is never a
//     parsed config.
//   - forgeGitLab with a non-empty cfg.gitlabBaseURL:
//     <gitlabBaseURL>/<owner>/<repoName>, with a trailing slash trimmed so a
//     self-managed root carrying a path prefix (https://gl.example/gitlab/)
//     composes correctly.
//   - forgeGitLab with an EMPTY gitlabBaseURL: an error naming
//     --gitlab-base-url. Fail closed — never a silent gitlab.com fallback,
//     mirroring parseFlags' own rejection.
//   - any other non-empty forge: an error naming the value. parseFlags
//     rejects it too, so this defends a hand-built config.
//
// Rejoining owner + "/" + repoName is lossless for a nested GitLab group
// because every caller derives the pair with strings.Cut(slug, "/"), which
// splits at the FIRST separator and leaves the remainder (e.g. "sub/project")
// in repoName. A future caller splitting on the LAST separator would break
// that silently.
func remoteURLFor(cfg config, owner, repoName string) (string, error) {
	switch cfg.forge {
	case forgeGitHub, "":
		return "https://github.com/" + owner + "/" + repoName, nil
	case forgeGitLab:
		base := strings.TrimSuffix(cfg.gitlabBaseURL, "/")
		if base == "" {
			return "", fmt.Errorf("--forge=gitlab requires --gitlab-base-url (no gitlab.com default)")
		}
		return base + "/" + owner + "/" + repoName, nil
	default:
		return "", fmt.Errorf("invalid --forge %q: must be %q or %q", cfg.forge, forgeGitHub, forgeGitLab)
	}
}
