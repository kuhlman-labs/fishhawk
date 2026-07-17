package role

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// stubLister is the in-memory TeamLister for tests. Returns canned
// results per (org, slug) and counts calls so cache-hit scenarios
// can assert no upstream traffic.
type stubLister struct {
	teams map[string][]TeamMember
	err   error
	calls atomic.Int64
}

func (s *stubLister) ListTeamMembers(_ context.Context, _ forge.CredentialScope, org, slug string) ([]TeamMember, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	if got, ok := s.teams[org+"/"+slug]; ok {
		return got, nil
	}
	return nil, nil
}

func newRoleStub(teams map[string][]TeamMember) *stubLister {
	return &stubLister{teams: teams}
}

func TestExpandRole_LiteralLoginPassthrough(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	roles := map[string]spec.Role{
		"single": {Members: []string{"octocat"}},
	}
	got, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "single", roles)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "octocat" {
		t.Errorf("got %v, want [octocat]", got)
	}
}

func TestExpandRole_TeamRefExpands(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice", ID: 1}, {Login: "bob", ID: 2}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng_team": {Members: []string{"@acme/eng"}},
	}
	got, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(99), "eng_team", roles)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("got %v, want [alice bob]", got)
	}
}

func TestExpandRole_MultipleRefsUnioned(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng":   {{Login: "alice"}, {Login: "bob"}},
		"acme/leads": {{Login: "bob"}, {Login: "carol"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"any": {Members: []string{"@acme/eng", "@acme/leads", "extern_doe"}},
	}
	got, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(99), "any", roles)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"alice": true, "bob": true, "carol": true, "extern_doe": true}
	if len(got) != len(want) {
		t.Errorf("got %v, want %d entries", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected member %q", g)
		}
	}
}

func TestExpandRole_UnknownRole(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	_, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "nonexistent", map[string]spec.Role{})
	if !errors.Is(err, ErrUnknownRole) {
		t.Errorf("err = %v, want ErrUnknownRole", err)
	}
}

func TestExpandRole_InvalidRef(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	roles := map[string]spec.Role{
		"bad": {Members: []string{"@no-slash"}},
	}
	_, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "bad", roles)
	if !errors.Is(err, ErrInvalidRef) {
		t.Errorf("err = %v, want ErrInvalidRef", err)
	}
}

func TestExpandRole_CaseInsensitive(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "Octocat"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	got, _ := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles)
	if len(got) != 1 || got[0] != "octocat" {
		t.Errorf("got %v, want [octocat] (case-folded)", got)
	}
}

func TestCanApprove_AnyOf_Allowed(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}, {Login: "bob"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	approvers := &spec.Approvers{AnyOf: []string{"eng"}}

	ok, err := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, roles, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("CanApprove = false, want true (alice in eng)")
	}
}

func TestCanApprove_AnyOf_Denied(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	approvers := &spec.Approvers{AnyOf: []string{"eng"}}
	ok, _ := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, roles, "mallory")
	if ok {
		t.Errorf("CanApprove = true, want false (mallory not in eng)")
	}
}

func TestCanApprove_AllOf_AllRequired(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng":   {{Login: "alice"}, {Login: "bob"}},
		"acme/leads": {{Login: "alice"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng":   {Members: []string{"@acme/eng"}},
		"leads": {Members: []string{"@acme/leads"}},
	}
	approvers := &spec.Approvers{AllOf: []string{"eng", "leads"}}

	// Alice is in both → allowed.
	ok, _ := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, roles, "alice")
	if !ok {
		t.Errorf("expected alice (in both) to be allowed")
	}
	// Bob is only in eng → denied.
	ok, _ = r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, roles, "bob")
	if ok {
		t.Errorf("expected bob (only in eng) to be denied")
	}
}

func TestCanApprove_NilApprovers(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	ok, err := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), nil, nil, "alice")
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("nil approvers should never approve")
	}
}

func TestCanApprove_EmptySubject(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	approvers := &spec.Approvers{AnyOf: []string{"eng"}}
	ok, _ := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, nil, "")
	if ok {
		t.Errorf("empty subject should never approve")
	}
}

func TestCanApprove_CaseInsensitiveSubject(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	approvers := &spec.Approvers{AnyOf: []string{"eng"}}
	ok, _ := r.CanApprove(context.Background(), forge.FromGitHubInstallationID(1), approvers, roles, "Alice")
	if !ok {
		t.Errorf("subject 'Alice' should match member 'alice' (case-fold)")
	}
}

func TestExpandRole_CacheHitsSkipUpstream(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}

	for i := 0; i < 5; i++ {
		if _, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles); err != nil {
			t.Fatal(err)
		}
	}
	if got := gh.calls.Load(); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (cache should serve repeats)", got)
	}
}

func TestExpandRole_CacheExpires(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	clock := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := NewResolver(gh, WithTTL(time.Minute), WithNow(func() time.Time { return clock }))
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	if _, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles); err != nil {
		t.Fatal(err)
	}
	// Move clock 2 minutes ahead — past TTL.
	clock = clock.Add(2 * time.Minute)
	if _, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles); err != nil {
		t.Fatal(err)
	}
	if got := gh.calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (cache expired)", got)
	}
}

func TestInvalidate_BypassesCache(t *testing.T) {
	gh := newRoleStub(map[string][]TeamMember{
		"acme/eng": {{Login: "alice"}},
	})
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}

	_, _ = r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles)
	r.Invalidate("acme", "eng")
	_, _ = r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles)
	if got := gh.calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (invalidate should force refetch)", got)
	}
}

func TestExpandRole_GitHubError(t *testing.T) {
	gh := &stubLister{err: errors.New("api down")}
	r := NewResolver(gh)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	_, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles)
	if err == nil {
		t.Fatal("expected error from upstream failure")
	}
}

func TestNewResolver_NilListerErrorsOnFetch(t *testing.T) {
	r := NewResolver(nil)
	roles := map[string]spec.Role{
		"eng": {Members: []string{"@acme/eng"}},
	}
	_, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "eng", roles)
	if err == nil || err.Error() == "" {
		t.Errorf("expected non-nil error when TeamLister missing")
	}
}

func TestExpandRole_EmptyRoleMembers(t *testing.T) {
	r := NewResolver(newRoleStub(nil))
	roles := map[string]spec.Role{
		"empty": {Members: []string{}},
	}
	got, err := r.ExpandRole(context.Background(), forge.FromGitHubInstallationID(1), "empty", roles)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty list", got)
	}
}
