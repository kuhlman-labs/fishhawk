package reviewresolver

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// recordingResolver is a minimal named Resolver that records its last
// ResolveReviewFromPollState call and returns a canned error.
type recordingResolver struct {
	name   string
	called bool
	gotRun uuid.UUID
	gotMrg bool
	gotURL string
	err    error
}

func (r *recordingResolver) Name() string { return r.name }

func (r *recordingResolver) ResolveReviewFromPollState(_ context.Context, runID uuid.UUID, merged bool, prURL string) error {
	r.called = true
	r.gotRun, r.gotMrg, r.gotURL = runID, merged, prURL
	return r.err
}

// withCleanRegistry resets the global registry around a test so registry
// mutations don't leak across cases. Returns a restore the caller defers.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = map[string]Resolver{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

// TestRegisterGetRoundTrip: Register then Get returns the same provider.
func TestRegisterGetRoundTrip(t *testing.T) {
	withCleanRegistry(t)
	r := &recordingResolver{name: "github_merge"}
	Register(r)

	got, err := Get("github_merge")
	if err != nil {
		t.Fatalf("Get(github_merge) errored: %v", err)
	}
	if got != r {
		t.Errorf("Get returned %p, want the registered %p", got, r)
	}
	if reg := Registered(); len(reg) != 1 || reg[0] != "github_merge" {
		t.Errorf("Registered() = %v, want [github_merge]", reg)
	}
}

// TestGetUnknown_FailsClosed: Get on an unknown id returns an
// *UnknownResolverError listing the registered ids — the fail-closed branch.
func TestGetUnknown_FailsClosed(t *testing.T) {
	withCleanRegistry(t)
	Register(&recordingResolver{name: "github_merge"})

	_, err := Get("nonexistent")
	var unknown *UnknownResolverError
	if !errors.As(err, &unknown) {
		t.Fatalf("Get(nonexistent) error = %v, want *UnknownResolverError", err)
	}
	if unknown.ID != "nonexistent" {
		t.Errorf("UnknownResolverError.ID = %q, want nonexistent", unknown.ID)
	}
	if len(unknown.Known) != 1 || unknown.Known[0] != "github_merge" {
		t.Errorf("UnknownResolverError.Known = %v, want [github_merge]", unknown.Known)
	}
}

// TestUnknownResolverError_Message covers both the empty-registry and
// populated-registry message branches.
func TestUnknownResolverError_Message(t *testing.T) {
	empty := (&UnknownResolverError{ID: "x"}).Error()
	if empty == "" {
		t.Error("empty-registry message is blank")
	}
	withKnown := (&UnknownResolverError{ID: "x", Known: []string{"github_merge"}}).Error()
	if withKnown == empty {
		t.Error("populated-registry message should differ from the empty one")
	}
}

// TestSelectEmpty_DefaultsToGithubMerge: Select("") resolves to the
// github_merge default (the default branch).
func TestSelectEmpty_DefaultsToGithubMerge(t *testing.T) {
	withCleanRegistry(t)
	r := &recordingResolver{name: DefaultResolution}
	Register(r)

	got, err := Select("")
	if err != nil {
		t.Fatalf("Select(\"\") errored: %v", err)
	}
	if got != r {
		t.Errorf("Select(\"\") returned %p, want the github_merge provider %p", got, r)
	}
}

// TestSelectGithubMerge_ReturnsGithubProvider: Select("github_merge") returns
// the registered github provider (the explicit happy path).
func TestSelectGithubMerge_ReturnsGithubProvider(t *testing.T) {
	withCleanRegistry(t)
	r := &recordingResolver{name: DefaultResolution}
	Register(r)

	got, err := Select(DefaultResolution)
	if err != nil {
		t.Fatalf("Select(github_merge) errored: %v", err)
	}
	if got != r {
		t.Errorf("Select(github_merge) returned %p, want %p", got, r)
	}
}

// TestSelectUnknown_FailsClosed: Select("nonexistent") returns an
// *UnknownResolverError — the fail-closed branch ADR-031 requires (a
// misconfigured resolver must not silently default to github_merge).
func TestSelectUnknown_FailsClosed(t *testing.T) {
	withCleanRegistry(t)
	Register(&recordingResolver{name: DefaultResolution})

	_, err := Select("nonexistent")
	var unknown *UnknownResolverError
	if !errors.As(err, &unknown) {
		t.Fatalf("Select(nonexistent) error = %v, want *UnknownResolverError", err)
	}
	if unknown.ID != "nonexistent" {
		t.Errorf("UnknownResolverError.ID = %q, want nonexistent", unknown.ID)
	}
}

// TestFuncAdapter_ForwardsAndPropagates: the Func adapter forwards
// ctx/runID/merged/prURL unchanged to the wrapped func and propagates its
// error and Name().
func TestFuncAdapter_ForwardsAndPropagates(t *testing.T) {
	wantRun := uuid.New()
	wantErr := errors.New("resolve failed")
	var gotRun uuid.UUID
	var gotMerged bool
	var gotURL string

	r := Func("github_merge", func(_ context.Context, runID uuid.UUID, merged bool, prURL string) error {
		gotRun, gotMerged, gotURL = runID, merged, prURL
		return wantErr
	})

	if r.Name() != "github_merge" {
		t.Errorf("Name() = %q, want github_merge", r.Name())
	}
	err := r.ResolveReviewFromPollState(context.Background(), wantRun, true, "https://github.com/x/y/pull/1")
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the wrapped error propagated", err)
	}
	if gotRun != wantRun || !gotMerged || gotURL != "https://github.com/x/y/pull/1" {
		t.Errorf("forwarded args = (%s, %v, %q), want (%s, true, .../pull/1)", gotRun, gotMerged, gotURL, wantRun)
	}
}
