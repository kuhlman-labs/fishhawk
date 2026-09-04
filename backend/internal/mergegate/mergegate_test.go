package mergegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

const wantCheck = "fishhawk_audit_complete"

// fakeAPI is an in-process ProtectionAPI. Every field is set BY
// CONSTRUCTION — no test drives a real client and none calls a control
// inside its own setup, so a counterfactual deletion reddens the
// behavioral assertion rather than a fixture.
type fakeAPI struct {
	classic    *forge.BranchProtection
	classicErr error

	rulesets     []forge.RulesetRequiredCheck
	rulesetsAuth bool
	rulesetsErr  error

	gotBranch        string
	gotDefaultBranch string
}

func (f *fakeAPI) GetBranchProtection(_ context.Context, _ forge.CredentialScope,
	_ forge.RepoRef, branch string) (*forge.BranchProtection, error) {
	f.gotBranch = branch
	return f.classic, f.classicErr
}

func (f *fakeAPI) ListRulesetRequiredChecksForDefault(_ context.Context, _ forge.CredentialScope,
	_ forge.RepoRef, branch, defaultBranch string) ([]forge.RulesetRequiredCheck, bool, error) {
	f.gotBranch, f.gotDefaultBranch = branch, defaultBranch
	return f.rulesets, f.rulesetsAuth, f.rulesetsErr
}

func reconcile(t *testing.T, f *fakeAPI) Reconciliation {
	t.Helper()
	got, err := Reconcile(context.Background(), f, forge.FromGitHubInstallationID(42),
		forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, "main", "main", wantCheck)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error %v", err)
	}
	return got
}

// --- the ONLY path that may say not_required -----------------------

func TestReconcile_AuthoritativeAndAbsent_IsNotRequired(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic:      &forge.BranchProtection{RequiredStatusCheckContexts: []string{"ci/build"}, EnforceAdmins: true},
		rulesets:     nil,
		rulesetsAuth: true,
	})
	if got.Status != StatusNotRequired {
		t.Fatalf("Status = %q, want %q (reason=%q detail=%q)", got.Status, StatusNotRequired, got.Reason, got.Detail)
	}
	if !got.Authoritative {
		t.Errorf("Authoritative = false, want true")
	}
	if got.Reason != "" || got.Detail != "" {
		t.Errorf("Reason/Detail = %q/%q, want empty on an authoritative absence", got.Reason, got.Detail)
	}
	if len(got.Sources) != 0 {
		t.Errorf("Sources = %+v, want none", got.Sources)
	}
	if got.Bypassable {
		t.Errorf("Bypassable = true with no requiring source")
	}
	// The union of what IS required is still reported, so an operator
	// can see the branch is protected by something else.
	if len(got.RequiredContexts) != 1 || got.RequiredContexts[0] != "ci/build" {
		t.Errorf("RequiredContexts = %v, want [ci/build]", got.RequiredContexts)
	}
	if !strings.Contains(got.Remediation, wantCheck) {
		t.Errorf("Remediation = %q, want it to name the check", got.Remediation)
	}
}

// --- fail-closed: every degrade lands on unknown, never not_required -

// TestReconcile_NonAuthoritative_IsUnknownNotNotRequired is the
// keystone of the fail-closed contract and the counterfactual vehicle
// for the `if !authoritative` branch in Reconcile: the fake returns
// authoritative=false DIRECTLY, so nothing in the fixture depends on
// the control being present.
func TestReconcile_NonAuthoritative_IsUnknownNotNotRequired(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic:      &forge.BranchProtection{EnforceAdmins: true},
		rulesets:     nil,
		rulesetsAuth: false,
	})
	if got.Status != StatusNotRequired && got.Status != StatusUnknown {
		t.Fatalf("Status = %q, not one of the two candidates", got.Status)
	}
	if got.Status == StatusNotRequired {
		t.Fatalf("Status = not_required on a NON-authoritative evaluation: an unread surface was reported as a positive absence")
	}
	if got.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
	}
	if got.Authoritative {
		t.Errorf("Authoritative = true")
	}
	if got.Reason != ReasonNonAuthoritative {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonNonAuthoritative)
	}
	if got.Detail == "" {
		t.Errorf("Detail is empty; unknown must name why")
	}
}

func TestReconcile_UnevaluatableRefNameToken_IsUnknown(t *testing.T) {
	// An fnmatch include (`refs/heads/release/*`) makes the client
	// report authoritative=false while still returning the rulesets it
	// COULD read — none of which require our check.
	got := reconcile(t, &fakeAPI{
		classic:      &forge.BranchProtection{EnforceAdmins: true},
		rulesets:     []forge.RulesetRequiredCheck{{RulesetID: 7, Contexts: []string{"e2e"}, BypassEntries: 0}},
		rulesetsAuth: false,
	})
	if got.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
	}
	if got.Reason != ReasonNonAuthoritative {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonNonAuthoritative)
	}
}

func TestReconcile_RulesetsNotFound_IsUnknown(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic:     &forge.BranchProtection{EnforceAdmins: true},
		rulesetsErr: fmt.Errorf("list rulesets: %w", forge.ErrNotFound),
	})
	if got.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
	}
	if got.Reason != ReasonRulesetsUnqueryable {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonRulesetsUnqueryable)
	}
	if got.Detail == "" {
		t.Errorf("Detail is empty")
	}
}

func TestReconcile_ForbiddenAdministrationRead_IsUnknown(t *testing.T) {
	cases := []struct {
		name string
		api  *fakeAPI
	}{
		{"classic 403", &fakeAPI{classicErr: fmt.Errorf("get branch protection: %w", forge.ErrForbidden)}},
		{"rulesets 403", &fakeAPI{
			classic:     &forge.BranchProtection{EnforceAdmins: true},
			rulesetsErr: fmt.Errorf("list rulesets: %w", forge.ErrForbidden),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcile(t, tc.api)
			if got.Status != StatusUnknown {
				t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
			}
			if got.Reason != ReasonScopeMissing {
				t.Errorf("Reason = %q, want %q", got.Reason, ReasonScopeMissing)
			}
			if !strings.Contains(got.Detail, "administration: read") {
				t.Errorf("Detail = %q, want it to name `administration: read`", got.Detail)
			}
			if !strings.Contains(got.Remediation, "Re-install") {
				t.Errorf("Remediation = %q, want the re-install step", got.Remediation)
			}
		})
	}
}

func TestReconcile_TransportError_IsUnknown(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	cases := []struct {
		name string
		api  *fakeAPI
	}{
		{"classic transport", &fakeAPI{classicErr: boom}},
		{"rulesets transport", &fakeAPI{
			classic:     &forge.BranchProtection{EnforceAdmins: true},
			rulesetsErr: boom,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcile(t, tc.api)
			if got.Status != StatusUnknown {
				t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
			}
			if got.Reason != ReasonTransportError {
				t.Errorf("Reason = %q, want %q", got.Reason, ReasonTransportError)
			}
			if !strings.Contains(got.Detail, "connection refused") {
				t.Errorf("Detail = %q, want it to carry the underlying failure", got.Detail)
			}
		})
	}
}

// TestReconcile_UnknownCarriesNoBypassClaim pins the unknown() clearing
// behavior: a report that could not settle the question must not also
// carry a bypass verdict derived from the half of the read that landed.
func TestReconcile_UnknownCarriesNoBypassClaim(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		// Classic requires the check AND exempts admins — read alone,
		// that is a "required, bypassable" finding.
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{wantCheck},
			EnforceAdmins:               false,
		},
		// ...but the rulesets surface then 403s.
		rulesetsErr: fmt.Errorf("list rulesets: %w", forge.ErrForbidden),
	})
	if got.Status != StatusUnknown {
		t.Fatalf("Status = %q, want %q", got.Status, StatusUnknown)
	}
	if got.Bypassable {
		t.Errorf("Bypassable = true on an unknown verdict")
	}
	if len(got.Sources) != 0 {
		t.Errorf("Sources = %+v, want cleared on unknown", got.Sources)
	}
}

// --- positive findings ---------------------------------------------

func TestReconcile_RequiredViaRuleset_IsRequired(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic:      &forge.BranchProtection{EnforceAdmins: true},
		rulesets:     []forge.RulesetRequiredCheck{{RulesetID: 12, Contexts: []string{"lint", wantCheck}}},
		rulesetsAuth: true,
	})
	if got.Status != StatusRequired {
		t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
	}
	if len(got.Sources) != 1 || got.Sources[0].Identity != "ruleset:12" {
		t.Fatalf("Sources = %+v, want one ruleset:12", got.Sources)
	}
	if got.Sources[0].Classic {
		t.Errorf("ruleset source marked Classic")
	}
	if got.Bypassable {
		t.Errorf("Bypassable = true with zero bypass entries")
	}
	if got.Remediation != "" {
		t.Errorf("Remediation = %q, want empty when the gate holds", got.Remediation)
	}
}

func TestReconcile_RequiredViaClassicProtection_IsRequired(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{wantCheck},
			EnforceAdmins:               true,
		},
		rulesetsAuth: true,
	})
	if got.Status != StatusRequired {
		t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
	}
	if len(got.Sources) != 1 || got.Sources[0].Identity != SourceBranchProtection || !got.Sources[0].Classic {
		t.Fatalf("Sources = %+v, want the classic source", got.Sources)
	}
	if got.Sources[0].BypassEntries != 0 {
		t.Errorf("classic BypassEntries = %d; classic exemption is a named condition, never a count",
			got.Sources[0].BypassEntries)
	}
	if got.Bypassable {
		t.Errorf("Bypassable = true with enforce_admins on")
	}
}

// TestReconcile_RequiredButBypassable_ReportsPerSourceBypass covers
// each source's own bypass condition, and asserts classic exemption is
// NEVER coerced into a count of 1.
func TestReconcile_RequiredButBypassable_ReportsPerSourceBypass(t *testing.T) {
	t.Run("ruleset bypass entries", func(t *testing.T) {
		got := reconcile(t, &fakeAPI{
			classic:      &forge.BranchProtection{EnforceAdmins: true},
			rulesets:     []forge.RulesetRequiredCheck{{RulesetID: 3, Contexts: []string{wantCheck}, BypassEntries: 2}},
			rulesetsAuth: true,
		})
		if got.Status != StatusRequired {
			t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
		}
		if !got.Bypassable {
			t.Fatalf("Bypassable = false; the sole requiring source has 2 bypass entries")
		}
		if got.Sources[0].BypassEntries != 2 {
			t.Errorf("BypassEntries = %d, want 2", got.Sources[0].BypassEntries)
		}
		if !strings.Contains(got.Remediation, "bypass entries (roles, teams or apps)") {
			t.Errorf("Remediation = %q, want the entries-not-people wording", got.Remediation)
		}
		if strings.Contains(got.Remediation, "actors)") {
			t.Errorf("Remediation renders a headcount: %q", got.Remediation)
		}
	})

	t.Run("classic admins exempt", func(t *testing.T) {
		got := reconcile(t, &fakeAPI{
			classic: &forge.BranchProtection{
				RequiredStatusCheckContexts: []string{wantCheck},
				EnforceAdmins:               false,
			},
			rulesetsAuth: true,
		})
		if got.Status != StatusRequired {
			t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
		}
		if !got.Bypassable {
			t.Fatalf("Bypassable = false; enforce_admins is off so admins are exempt")
		}
		if got.Sources[0].BypassEntries != 0 {
			t.Errorf("BypassEntries = %d, want 0: enforce_admins:false is a named condition, not a count of 1",
				got.Sources[0].BypassEntries)
		}
		if got.Sources[0].EnforceAdmins {
			t.Errorf("EnforceAdmins = true, want the decoded false")
		}
		if !strings.Contains(got.Remediation, "repository admins are exempt") {
			t.Errorf("Remediation = %q, want the admin-exemption condition named", got.Remediation)
		}
	})
}

// TestReconcile_BypassRequiresEveryRequiringSource is the discrimination
// case for the conjunction model (approval CONDITION 1). Two sources
// require the check; only ONE is bypassable. A merger must bypass BOTH,
// so the gate is NOT bypassable — the summing model this replaces would
// report it bypassable off the one source that allows it.
func TestReconcile_BypassRequiresEveryRequiringSource(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		// Classic requires it and does NOT exempt admins: not bypassable.
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{wantCheck},
			EnforceAdmins:               true,
		},
		// A ruleset requires it and DOES have bypass entries.
		rulesets:     []forge.RulesetRequiredCheck{{RulesetID: 9, Contexts: []string{wantCheck}, BypassEntries: 3}},
		rulesetsAuth: true,
	})
	if got.Status != StatusRequired {
		t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("Sources = %+v, want both requiring sources", got.Sources)
	}
	if got.Bypassable {
		t.Fatalf("Bypassable = true with one NON-bypassable requiring source: the gate was reported bypassable because ANY source allowed it, not because EVERY one did")
	}
	// The per-source detail survives the conjunction.
	if got.Sources[0].Identity != SourceBranchProtection || got.Sources[0].Bypassable {
		t.Errorf("classic source = %+v, want non-bypassable", got.Sources[0])
	}
	if got.Sources[1].Identity != "ruleset:9" || !got.Sources[1].Bypassable || got.Sources[1].BypassEntries != 3 {
		t.Errorf("ruleset source = %+v, want bypassable with 3 entries", got.Sources[1])
	}
	if got.Remediation != "" {
		t.Errorf("Remediation = %q, want empty: one source still enforces", got.Remediation)
	}
}

// TestReconcile_AllRequiringSourcesBypassable is the other half of the
// conjunction: make BOTH sources bypassable and the verdict flips. Run
// alongside the case above, it proves the AND is a real conjunction and
// not a constant false.
func TestReconcile_AllRequiringSourcesBypassable(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{wantCheck},
			EnforceAdmins:               false,
		},
		rulesets:     []forge.RulesetRequiredCheck{{RulesetID: 9, Contexts: []string{wantCheck}, BypassEntries: 3}},
		rulesetsAuth: true,
	})
	if !got.Bypassable {
		t.Fatalf("Bypassable = false with every requiring source bypassable: Sources = %+v", got.Sources)
	}
	if !strings.Contains(got.Remediation, "repository admins are exempt") ||
		!strings.Contains(got.Remediation, "3 bypass entries") {
		t.Errorf("Remediation = %q, want BOTH sources' own conditions named", got.Remediation)
	}
}

// TestReconcile_RequiredButNonAuthoritative_KeepsPositiveFinding pins
// that a positive finding stands on its own — an incomplete sweep does
// not erase an observed requirement — while still carrying the caveat.
func TestReconcile_RequiredButNonAuthoritative_KeepsPositiveFinding(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{wantCheck},
			EnforceAdmins:               true,
		},
		rulesetsAuth: false,
	})
	if got.Status != StatusRequired {
		t.Fatalf("Status = %q, want %q", got.Status, StatusRequired)
	}
	if got.Authoritative {
		t.Errorf("Authoritative = true")
	}
	if got.Reason != ReasonNonAuthoritative {
		t.Errorf("Reason = %q, want the caveat retained alongside the finding", got.Reason)
	}
}

// --- argument validation --------------------------------------------

func TestReconcile_ArgumentErrors(t *testing.T) {
	cases := []struct {
		name       string
		api        ProtectionAPI
		branch     string
		check      string
		wantSubstr string
	}{
		{"nil api", nil, "main", wantCheck, "nil ProtectionAPI"},
		{"empty branch", &fakeAPI{}, "", wantCheck, "branch is required"},
		{"empty check", &fakeAPI{}, "main", "", "check context is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reconcile(context.Background(), tc.api, forge.FromGitHubInstallationID(1),
				forge.RepoRef{Owner: "x", Name: "y"}, tc.branch, "main", tc.check)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSubstr)
			}
			if got.Status != "" {
				t.Errorf("Status = %q on an argument error, want the zero value", got.Status)
			}
		})
	}
}

// TestReconcile_PassesRealDefaultBranchThrough proves Reconcile does not
// substitute its own guess for the caller's resolved default branch —
// the `~DEFAULT_BRANCH` matcher downstream depends on it.
func TestReconcile_PassesRealDefaultBranchThrough(t *testing.T) {
	f := &fakeAPI{classic: &forge.BranchProtection{EnforceAdmins: true}, rulesetsAuth: true}
	if _, err := Reconcile(context.Background(), f, forge.FromGitHubInstallationID(42),
		forge.RepoRef{Owner: "x", Name: "y"}, "trunk", "trunk", wantCheck); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f.gotBranch != "trunk" || f.gotDefaultBranch != "trunk" {
		t.Errorf("branch/defaultBranch = %q/%q, want trunk/trunk", f.gotBranch, f.gotDefaultBranch)
	}
}

// TestReconcile_RequiredContextsDedupes pins the union: a context
// required by two sources is listed once.
func TestReconcile_RequiredContextsDedupes(t *testing.T) {
	got := reconcile(t, &fakeAPI{
		classic: &forge.BranchProtection{
			RequiredStatusCheckContexts: []string{"ci/build", wantCheck},
			EnforceAdmins:               true,
		},
		rulesets: []forge.RulesetRequiredCheck{
			{RulesetID: 1, Contexts: []string{"ci/build", "lint"}},
		},
		rulesetsAuth: true,
	})
	want := []string{"ci/build", wantCheck, "lint"}
	if len(got.RequiredContexts) != len(want) {
		t.Fatalf("RequiredContexts = %v, want %v", got.RequiredContexts, want)
	}
	for i := range want {
		if got.RequiredContexts[i] != want[i] {
			t.Fatalf("RequiredContexts = %v, want %v", got.RequiredContexts, want)
		}
	}
}
