package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

func ghDest(owner string) workmgmt.Conventions {
	return workmgmt.Conventions{Provider: "github_projects", Project: &workmgmt.Project{Owner: owner, Number: 3}}
}

func glDest(project string) workmgmt.Conventions {
	return workmgmt.Conventions{Provider: "gitlab", GitLab: &workmgmt.GitLabConnection{Project: project}}
}

func jiraDest(key string) workmgmt.Conventions {
	return workmgmt.Conventions{Provider: "jira", Jira: &workmgmt.JiraConnection{ProjectKey: key}}
}

// TestParseWorkMgmtDestinationAllowList covers every parse branch: the
// well-formed multi-entry value (whitespace-padded), the empty value, and
// each malformed shape that MUST error rather than silently yield a strict
// (empty) allow-list.
func TestParseWorkMgmtDestinationAllowList(t *testing.T) {
	t.Run("well-formed multi-entry with whitespace", func(t *testing.T) {
		allow, err := ParseWorkMgmtDestinationAllowList("  Acme:github_projects:Enterprise , , group:gitlab:Other ,acme:jira:FISH")
		if err != nil {
			t.Fatalf("parse err = %v, want nil", err)
		}
		for _, want := range []string{
			"acme:github_projects:enterprise",
			"group:gitlab:other",
			"acme:jira:fish",
		} {
			if _, ok := allow[want]; !ok {
				t.Errorf("allow-list missing %q; got %v", want, allow)
			}
		}
		if len(allow) != 3 {
			t.Errorf("allow-list size = %d, want 3 (the empty entry is skipped)", len(allow))
		}
	})

	t.Run("empty value is an empty allow-list, not an error", func(t *testing.T) {
		allow, err := ParseWorkMgmtDestinationAllowList("")
		if err != nil {
			t.Fatalf("parse(\"\") err = %v, want nil", err)
		}
		if len(allow) != 0 {
			t.Errorf("allow-list = %v, want empty (strict binding)", allow)
		}
	})

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"too few segments", "acme:github_projects"},
		{"too many segments", "acme:github_projects:owner:extra"},
		{"empty account-key segment", ":github_projects:acme"},
		{"empty provider segment", "acme::acme"},
		{"empty destination-key segment", "acme:github_projects:"},
		{"unknown provider segment", "acme:bitbucket:acme"},
		{"gitlab destination key carrying a full project path", "group:gitlab:other/team"},
		{"gitlab destination key carrying a nested-group path", "group:gitlab:other/sub/team"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			allow, err := ParseWorkMgmtDestinationAllowList(tc.raw)
			if err == nil {
				t.Fatalf("parse(%q) err = nil, want a malformed-entry error (a typo must never degrade to strict)", tc.raw)
			}
			if allow != nil {
				t.Errorf("parse(%q) allow = %v, want nil on error", tc.raw, allow)
			}
			if !strings.Contains(err.Error(), tc.raw) && !strings.Contains(err.Error(), strings.TrimSpace(tc.raw)) {
				t.Errorf("err = %v, want the offending entry named", err)
			}
		})
	}

	// A gitlab destination key is derived as the namespace ROOT
	// (conventionsDestination cuts at the first "/"), so a full-path entry
	// would be silently inert. The refusal must name the root-granularity
	// entry to use instead, so remediation is a copy-paste.
	t.Run("gitlab full-path rejection names the namespace-root entry", func(t *testing.T) {
		_, err := ParseWorkMgmtDestinationAllowList("Group:gitlab:Other/Team")
		if err == nil {
			t.Fatal("parse err = nil, want a rejection of the full-path gitlab destination key")
		}
		if !strings.Contains(err.Error(), "group:gitlab:other") {
			t.Errorf("err = %v, want the namespace-root entry %q named", err, "group:gitlab:other")
		}
	})
}

// TestConventionsDestination pins the per-provider destination derivation,
// including the gitlab nested-group namespace root and the project-absent
// default (the filing repo's own path).
func TestConventionsDestination(t *testing.T) {
	for _, tc := range []struct {
		name         string
		conv         workmgmt.Conventions
		repo         string
		wantProvider string
		wantKey      string
	}{
		{"github_projects owner", ghDest("acme"), "acme/widgets", "github_projects", "acme"},
		{"gitlab explicit project", glDest("group/app"), "group/lib", "gitlab", "group"},
		{"gitlab nested group takes the namespace root", glDest("group/subgroup/project"), "group/lib", "gitlab", "group"},
		{"gitlab absent project defaults to the filing repo's own owner", glDest(""), "group/lib", "gitlab", "group"},
		{"jira project key", jiraDest("FISH"), "acme/widgets", "jira", "FISH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, key, err := conventionsDestination(tc.conv, tc.repo)
			if err != nil {
				t.Fatalf("conventionsDestination err = %v, want nil", err)
			}
			if provider != tc.wantProvider || key != tc.wantKey {
				t.Errorf("destination = %s:%s, want %s:%s", provider, key, tc.wantProvider, tc.wantKey)
			}
		})
	}
}

// TestAuthorizeConventionsDestination is one case per named allow/refuse
// branch of the destination-binding policy.
func TestAuthorizeConventionsDestination(t *testing.T) {
	for _, tc := range []struct {
		name            string
		accountProvider string
		repo            string
		conv            workmgmt.Conventions
		allow           string
		wantErr         bool
	}{
		{
			name: "github account, github_projects destination owned by another login",
			// The redirect attempt: a repo in the victim's account naming the
			// attacker's project board.
			accountProvider: "github", repo: "victim/widgets", conv: ghDest("attacker"), wantErr: true,
		},
		{
			name:            "github account, own owner in differing case",
			accountProvider: "github", repo: "Acme/widgets", conv: ghDest("acme"), wantErr: false,
		},
		{
			name:            "gitlab account, project path in another namespace",
			accountProvider: "gitlab", repo: "victim/lib", conv: glDest("attacker/app"), wantErr: true,
		},
		{
			name:            "gitlab account, no project override files into the repo's own path",
			accountProvider: "gitlab", repo: "group/lib", conv: glDest(""), wantErr: false,
		},
		{
			name:            "cross-forge: github account, gitlab destination",
			accountProvider: "github", repo: "acme/widgets", conv: glDest("acme/app"), wantErr: true,
		},
		{
			name:            "cross-forge: gitlab account, github_projects destination",
			accountProvider: "gitlab", repo: "acme/lib", conv: ghDest("acme"), wantErr: true,
		},
		{
			name:            "jira destination with an empty allow-list",
			accountProvider: "github", repo: "acme/widgets", conv: jiraDest("FISH"), wantErr: true,
		},
		{
			name:            "jira destination present in the allow-list",
			accountProvider: "github", repo: "acme/widgets", conv: jiraDest("FISH"),
			allow: "acme:jira:FISH", wantErr: false,
		},
		{
			name:            "allow-listed cross-namespace github destination",
			accountProvider: "github", repo: "acme/widgets", conv: ghDest("enterprise"),
			allow: "acme:github_projects:enterprise", wantErr: false,
		},
		{
			name:            "unknown conventions provider fails closed",
			accountProvider: "github", repo: "acme/widgets",
			conv:    workmgmt.Conventions{Provider: "bitbucket_boards"},
			wantErr: true,
		},
		{
			name:            "empty conventions provider fails closed",
			accountProvider: "github", repo: "acme/widgets",
			conv:    workmgmt.Conventions{},
			wantErr: true,
		},
		{
			name:            "github_projects with a nil connection block fails closed",
			accountProvider: "github", repo: "acme/widgets",
			conv:    workmgmt.Conventions{Provider: "github_projects"},
			wantErr: true,
		},
		{
			name:            "gitlab with a nil connection block fails closed",
			accountProvider: "gitlab", repo: "group/lib",
			conv:    workmgmt.Conventions{Provider: "gitlab"},
			wantErr: true,
		},
		{
			// The empty-family guard: without it, jira's ""-family sentinel would
			// equal an empty accountProvider and an empty destination key would
			// EqualFold an empty account key, authorizing a jira destination with
			// no allow-list entry.
			name:            "jira destination with an empty account provider stays refused",
			accountProvider: "", repo: "/widgets", conv: jiraDest(""),
			wantErr: true,
		},
		{
			name:            "jira with a nil connection block fails closed",
			accountProvider: "github", repo: "acme/widgets",
			conv:    workmgmt.Conventions{Provider: "jira"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allow, err := ParseWorkMgmtDestinationAllowList(tc.allow)
			if err != nil {
				t.Fatalf("allow-list fixture %q failed to parse: %v", tc.allow, err)
			}
			err = authorizeConventionsDestination(tc.accountProvider, tc.repo, tc.conv, allow)
			if tc.wantErr {
				if err == nil {
					t.Fatal("authorize err = nil, want a refusal")
				}
				if !errors.Is(err, errConventionsDestinationUnauthorized) {
					t.Fatalf("authorize err = %v, want it to wrap errConventionsDestinationUnauthorized so callers can classify it", err)
				}
				if !strings.Contains(err.Error(), tc.repo) || !strings.Contains(err.Error(), tc.accountProvider) {
					t.Errorf("err = %v, want the repo and its resolved account named", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorize err = %v, want nil (the destination is bound to the repo's own account)", err)
			}
		})
	}
}

// TestAuthorizeConventionsDestination_RefusalNamesTheRemedy: the refusal must
// be precise about what failed AND how to fix it — the exact allow-list entry
// an administrator would add.
func TestAuthorizeConventionsDestination_RefusalNamesTheRemedy(t *testing.T) {
	err := authorizeConventionsDestination("github", "victim/widgets", ghDest("Attacker"), nil)
	if err == nil {
		t.Fatal("authorize err = nil, want a refusal")
	}
	for _, want := range []string{
		"victim:github_projects:attacker",
		"FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS",
		"github_projects:Attacker",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}
