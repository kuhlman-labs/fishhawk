package server

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// errConventionsDestinationUnauthorized is the sentinel every
// destination-authorization refusal wraps, so a caller can classify a
// tenancy-boundary refusal apart from a fetch/parse failure.
var errConventionsDestinationUnauthorized = errors.New("work-management destination not authorized for the filing repo's account")

// workMgmtDestinationProviders is the closed set of conventions providers the
// allow-list may name. Validating the segment at parse time turns a typo into
// a boot failure instead of a silently inert allow-list entry that only
// surfaces as a refused filing much later.
var workMgmtDestinationProviders = map[string]struct{}{
	"github_projects": {},
	"gitlab":          {},
	"jira":            {},
}

// DestinationAllowList is the administrator-controlled set of
// <account-key>:<provider>:<destination-key> exceptions to the strict
// destination binding, parsed from FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS.
// Keys are pre-lowercased so lookup is case-insensitive. A nil or empty map
// means strict binding with no exceptions.
type DestinationAllowList map[string]struct{}

// ParseWorkMgmtDestinationAllowList parses the comma-separated
// FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS value into a DestinationAllowList.
// Each entry is <account-key>:<provider>:<destination-key>; surrounding
// whitespace is trimmed, empty entries are skipped, and an empty raw value
// yields an empty allow-list with a nil error. A malformed entry (wrong
// arity, an empty segment, a provider outside the closed set, or a gitlab
// destination key carrying a full project path) returns an actionable error
// naming the offending entry — the caller MUST fail boot on it rather than
// degrade to an empty (strict) allow-list: a typo silently reverting to
// strict would masquerade as the security posture working while breaking a
// legitimate cross-namespace deployment.
func ParseWorkMgmtDestinationAllowList(raw string) (DestinationAllowList, error) {
	allow := make(DestinationAllowList)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("work-management destination allow-list entry %q: want exactly 3 colon-separated segments <account-key>:<provider>:<destination-key>, got %d", entry, len(parts))
		}
		accountKey := strings.TrimSpace(parts[0])
		provider := strings.TrimSpace(parts[1])
		destKey := strings.TrimSpace(parts[2])
		if accountKey == "" || provider == "" || destKey == "" {
			return nil, fmt.Errorf("work-management destination allow-list entry %q: every segment of <account-key>:<provider>:<destination-key> must be non-empty", entry)
		}
		if _, ok := workMgmtDestinationProviders[provider]; !ok {
			return nil, fmt.Errorf("work-management destination allow-list entry %q: provider %q is not one of github_projects, gitlab, jira", entry, provider)
		}
		// conventionsDestination derives a gitlab destination key as the
		// NAMESPACE ROOT of the configured project path, so a full-path entry
		// ("group/team") could never match at authorization time. Reject it at
		// parse time for the same reason the closed provider set is validated
		// here: a boot failure naming the fix beats a silently inert allow-list
		// entry that only surfaces much later as a refused filing.
		if provider == "gitlab" && strings.Contains(destKey, "/") {
			root, _, _ := strings.Cut(destKey, "/")
			return nil, fmt.Errorf("work-management destination allow-list entry %q: a gitlab destination key is the namespace ROOT, not a project path; use %q", entry, destinationAllowKey(accountKey, provider, root))
		}
		allow[destinationAllowKey(accountKey, provider, destKey)] = struct{}{}
	}
	return allow, nil
}

// destinationAllowKey renders the canonical (lowercased) allow-list lookup
// key. The provider segment is already a closed-set lowercase token; the
// account and destination keys are lowercased because forge logins and
// namespace paths are case-preserving but case-insensitive for identity.
func destinationAllowKey(accountKey, provider, destKey string) string {
	return strings.ToLower(accountKey) + ":" + provider + ":" + strings.ToLower(destKey)
}

// conventionsDestination derives the (provider, owning key) tuple a parsed
// conventions file selects as its filing destination, for the filing repo
// ("owner/name"):
//
//   - github_projects -> the Projects owner login;
//   - gitlab          -> the namespace root of the configured project path,
//     or the filing repo's own owner when the block omits project (the
//     provider then files into the repo's own path);
//   - jira            -> the Jira project key (which has no forge account to
//     bind to; authorization refuses it unless allow-listed).
//
// A provider outside that set — including the empty string — is an error, so
// the policy fails closed on anything it has no binding rule for. A declared
// provider with a nil connection block is likewise an error rather than an
// implicit pass; workmgmt.Parse's semantic checks already reject it, and this
// is the defensive second line.
func conventionsDestination(conv workmgmt.Conventions, repo string) (string, string, error) {
	switch conv.Provider {
	case "github_projects":
		if conv.Project == nil {
			return "", "", fmt.Errorf("provider github_projects with no project connection block")
		}
		return "github_projects", conv.Project.Owner, nil
	case "gitlab":
		if conv.GitLab == nil {
			return "", "", fmt.Errorf("provider gitlab with no gitlab connection block")
		}
		if conv.GitLab.Project != "" {
			root, _, _ := strings.Cut(conv.GitLab.Project, "/")
			return "gitlab", root, nil
		}
		owner, _, _ := strings.Cut(repo, "/")
		return "gitlab", owner, nil
	case "jira":
		if conv.Jira == nil {
			return "", "", fmt.Errorf("provider jira with no jira connection block")
		}
		return "jira", conv.Jira.ProjectKey, nil
	default:
		return "", "", fmt.Errorf("unrecognized work-management provider %q", conv.Provider)
	}
}

// authorizeConventionsDestination binds the filing DESTINATION a
// repo-committed conventions file selects to the filing repo's own tenancy
// account (E44.14 / #2090). accountProvider is the ADR-057/ADR-058
// discriminator ("github"/"gitlab") resolved for repo, whose owner segment IS
// the account_key that discriminator lookup used.
//
// The destination is authorized when it appears in the administrator-
// controlled allow-list, or when BOTH the forge family matches (a github
// account admits only a github_projects destination; a gitlab account only a
// gitlab one) AND the destination's owning key equals the account key,
// case-insensitively. Everything else is refused: without this, a malicious
// file committed to any repo the deployment can read could name any project
// reachable by deployment credentials and redirect filed work items out of
// the repo's tenancy boundary.
func authorizeConventionsDestination(accountProvider, repo string, conv workmgmt.Conventions, allow DestinationAllowList) error {
	accountKey, _, _ := strings.Cut(repo, "/")
	destProvider, destKey, err := conventionsDestination(conv, repo)
	if err != nil {
		return fmt.Errorf("%w: %s in %s (account %s:%s): %w",
			errConventionsDestinationUnauthorized, conventionsFilePath, repo, accountProvider, accountKey, err)
	}
	if _, ok := allow[destinationAllowKey(accountKey, destProvider, destKey)]; ok {
		return nil
	}
	if destinationForgeFamily(destProvider) == accountProvider && strings.EqualFold(destKey, accountKey) {
		return nil
	}
	return fmt.Errorf(
		"%w: %s in %s selects destination %s:%s but the repo's account is %s:%s; "+
			"add %q to FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS to permit it",
		errConventionsDestinationUnauthorized, conventionsFilePath, repo,
		destProvider, destKey, accountProvider, accountKey,
		destinationAllowKey(accountKey, destProvider, destKey))
}

// destinationForgeFamily maps a conventions provider to the accounts.provider
// forge family it belongs to. jira maps to no forge family, so a jira
// destination can never satisfy the family match and is authorized only by
// the allow-list.
func destinationForgeFamily(destProvider string) string {
	switch destProvider {
	case "github_projects":
		return "github"
	case "gitlab":
		return "gitlab"
	default:
		return ""
	}
}
