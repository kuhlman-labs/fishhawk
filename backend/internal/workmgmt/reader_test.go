package workmgmt

// Tests for the optional work-item read capability (#2230 / ADR-064):
// ReaderFor's resolution ladder, the typed UnavailableError contract, and the
// canonical-state reverse mapping.
//
// This file is SELF-CONTAINED by design: it shares the package test namespace
// with provider_test.go but defines its OWN doubles (readerFileOnlyProvider /
// readerCapableProvider) rather than extending that file's fakeProvider, so
// nothing here requires editing a file outside this change's scope.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// readerFileOnlyProvider implements ONLY the base Provider — the v0 gitlab /
// jira shape. ReaderFor must resolve it to a typed unavailable, never a nil
// interface a caller could dispatch against.
type readerFileOnlyProvider struct{ name string }

func (f *readerFileOnlyProvider) Name() string { return f.name }

func (f *readerFileOnlyProvider) File(_ context.Context, _ ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: f.name, Number: 1, URL: "https://example/1"}, nil
}

// readerCapableProvider implements the base Provider PLUS WorkItemReader.
type readerCapableProvider struct {
	name    string
	gotRead ReadWorkItemRequest
	gotList ListWorkItemsRequest
}

func (f *readerCapableProvider) Name() string { return f.name }

func (f *readerCapableProvider) File(_ context.Context, _ ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: f.name, Number: 2, URL: "https://example/2"}, nil
}

func (f *readerCapableProvider) ReadWorkItem(_ context.Context, req ReadWorkItemRequest) (*WorkItemRecord, error) {
	f.gotRead = req
	return &WorkItemRecord{Number: 2230, Title: "grow the abstraction", BoardState: CanonicalStateInProgress}, nil
}

func (f *readerCapableProvider) ListWorkItems(_ context.Context, req ListWorkItemsRequest) (*WorkItemPage, error) {
	f.gotList = req
	return &WorkItemPage{Items: []WorkItemRecord{{Number: 2230}}, BoardStateResolved: true}, nil
}

// TestReaderFor_ResolvesImplementingProvider proves the happy path: a provider
// that implements the capability is returned as a usable WorkItemReader and
// the request reaches it verbatim.
func TestReaderFor_ResolvesImplementingProvider(t *testing.T) {
	fp := &readerCapableProvider{name: "reader-capable"}
	Register(fp)
	r, err := ReaderFor("reader-capable")
	if err != nil {
		t.Fatalf("ReaderFor: %v", err)
	}
	page, err := r.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Labels: []string{"type:feature"}, BoardStates: []string{CanonicalStateUpNext},
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Number != 2230 {
		t.Errorf("page = %+v, want the provider's single item", page)
	}
	if len(fp.gotList.Labels) != 1 || fp.gotList.Labels[0] != "type:feature" {
		t.Errorf("provider got labels %v, want the caller's filter forwarded", fp.gotList.Labels)
	}
	rec, err := r.ReadWorkItem(context.Background(), ReadWorkItemRequest{Ref: "#2230", ResolveBoardState: true})
	if err != nil {
		t.Fatalf("ReadWorkItem: %v", err)
	}
	if rec.Number != 2230 {
		t.Errorf("rec = %+v, want the provider's record", rec)
	}
	if fp.gotRead.Ref != "#2230" || !fp.gotRead.ResolveBoardState {
		t.Errorf("provider got read request %+v, want the caller's ref and board opt-in", fp.gotRead)
	}
}

// TestReaderFor_FileOnlyProviderUnavailable is failure mode 5: a File-only
// provider (the v0 gitlab / jira shape) resolves to a TYPED
// *UnavailableError{Reason: ReasonNotImplemented} with a NIL reader — never a
// nil interface a caller could dispatch against, and never an empty page it
// could misread as an empty backlog.
func TestReaderFor_FileOnlyProviderUnavailable(t *testing.T) {
	Register(&readerFileOnlyProvider{name: "reader-file-only"})
	r, err := ReaderFor("reader-file-only")
	if r != nil {
		t.Errorf("reader = %v, want nil alongside the unavailable error", r)
	}
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UnavailableError", err, err)
	}
	if ue.Reason != ReasonNotImplemented {
		t.Errorf("Reason = %q, want %q", ue.Reason, ReasonNotImplemented)
	}
	if ue.Provider != "reader-file-only" || ue.Capability != ReaderCapability {
		t.Errorf("error names provider %q capability %q, want the resolved provider and the read capability", ue.Provider, ue.Capability)
	}
	if !strings.Contains(ue.Error(), string(ReasonNotImplemented)) {
		t.Errorf("message %q should name the reason", ue.Error())
	}
}

// TestReaderFor_UnknownProviderKeepsRegistryError proves ReaderFor does NOT
// swallow the registry's existing fail-closed error: an unregistered id still
// yields *UnknownProviderError, distinct from the capability-unavailable case.
func TestReaderFor_UnknownProviderKeepsRegistryError(t *testing.T) {
	_, err := ReaderFor("reader-never-registered")
	var upe *UnknownProviderError
	if !errors.As(err, &upe) {
		t.Fatalf("err = %v (%T), want *UnknownProviderError", err, err)
	}
	var ue *UnavailableError
	if errors.As(err, &ue) {
		t.Errorf("an unregistered id must not masquerade as a capability-unavailable error: %v", err)
	}
}

// errSentinelForge stands in for a forge sentinel (githubclient.ErrForbidden)
// so this package's test does not import the GitHub client.
var errSentinelForge = errors.New("forge: forbidden")

// TestUnavailableError_UnwrapKeepsSentinel is condition C2: the SAME error
// value must satisfy BOTH errors.As (yielding the typed reason a caller
// switches on) AND errors.Is against the wrapped forge sentinel (which
// existing forge-level handling already matches). A struct with no cause field
// — or an Unwrap that drops it — fails the second assertion.
func TestUnavailableError_UnwrapKeepsSentinel(t *testing.T) {
	err := error(&UnavailableError{
		Provider:   "github_projects",
		Capability: ReaderCapability,
		Reason:     ReasonForbidden,
		Detail:     "the forge refused the read",
		Cause:      errSentinelForge,
	})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("errors.As failed on %v", err)
	}
	if ue.Reason != ReasonForbidden {
		t.Errorf("Reason = %q, want %q", ue.Reason, ReasonForbidden)
	}
	if !errors.Is(err, errSentinelForge) {
		t.Errorf("errors.Is lost the wrapped forge sentinel through Unwrap: %v", err)
	}
	if ue.Unwrap() != errSentinelForge {
		t.Errorf("Unwrap() = %v, want the retained cause", ue.Unwrap())
	}
	msg := err.Error()
	for _, want := range []string{"github_projects", ReaderCapability, string(ReasonForbidden), "the forge refused the read", errSentinelForge.Error()} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestUnavailableError_NoCauseUnwrapsNil proves a locally-decided degradation
// (no project, no token, no installation, not implemented) carries no cause
// and unwraps to nil rather than fabricating one.
func TestUnavailableError_NoCauseUnwrapsNil(t *testing.T) {
	e := &UnavailableError{Provider: "github_projects", Capability: ReaderCapability, Reason: ReasonNoProjectsToken}
	if e.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil for a locally-decided degradation", e.Unwrap())
	}
	if got := e.Error(); !strings.Contains(got, string(ReasonNoProjectsToken)) || strings.HasSuffix(got, ": ") {
		t.Errorf("message = %q, want the reason with no dangling detail separator", got)
	}
}

// TestCanonicalStateForOption pins the reverse mapping: an option bound to a
// canonical state resolves to it, an UNMAPPED option resolves to "" (reported
// as unmapped, never guessed), an empty option is "", and a tie resolves
// deterministically to the alphabetically-first canonical state.
func TestCanonicalStateForOption(t *testing.T) {
	states := map[string]string{
		CanonicalStateBacklog:    "Backlog",
		CanonicalStateInProgress: "In Progress",
		CanonicalStateDone:       "Done",
	}
	for _, tc := range []struct{ option, want string }{
		{"In Progress", CanonicalStateInProgress},
		{"Backlog", CanonicalStateBacklog},
		{"Icebox", ""},
		{"", ""},
	} {
		if got := CanonicalStateForOption(states, tc.option); got != tc.want {
			t.Errorf("CanonicalStateForOption(%q) = %q, want %q", tc.option, got, tc.want)
		}
	}
	// A tie (two canonical states bound to one option) is deterministic across
	// runs — map iteration order must not decide the answer.
	tied := map[string]string{CanonicalStateInReview: "Review", CanonicalStateBlocked: "Review"}
	first := CanonicalStateForOption(tied, "Review")
	if first != CanonicalStateBlocked {
		t.Errorf("tie resolved to %q, want the alphabetically-first canonical state %q", first, CanonicalStateBlocked)
	}
	for i := 0; i < 20; i++ {
		if got := CanonicalStateForOption(tied, "Review"); got != first {
			t.Fatalf("tie resolution is not deterministic: %q then %q", first, got)
		}
	}
}

// readerBoundedProvider is a reader whose ListWorkItems HONOURS the bounded
// window: it truncates its corpus at MaxScanned and reports Truncated when it
// did. It is a second double rather than a flag on readerCapableProvider so
// the bounded-window contract reads on its own.
type readerBoundedProvider struct {
	name   string
	corpus []WorkItemRecord
	gotReq ListWorkItemsRequest
}

func (f *readerBoundedProvider) Name() string { return f.name }

func (f *readerBoundedProvider) File(_ context.Context, _ ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: f.name, Number: 3, URL: "https://example/3"}, nil
}

func (f *readerBoundedProvider) ReadWorkItem(_ context.Context, _ ReadWorkItemRequest) (*WorkItemRecord, error) {
	return &WorkItemRecord{Number: 2239}, nil
}

func (f *readerBoundedProvider) ListWorkItems(_ context.Context, req ListWorkItemsRequest) (*WorkItemPage, error) {
	f.gotReq = req
	items := f.corpus
	truncated := false
	if req.MaxScanned > 0 && len(items) > req.MaxScanned {
		items, truncated = items[:req.MaxScanned], true
	}
	return &WorkItemPage{Items: items, Truncated: truncated}, nil
}

// TestReaderFor_CarriesTheBoundedWindowVocabulary pins the E54.7 / #2239
// read-request vocabulary at the ABSTRACTION layer: the bounded-window fields
// reach the provider through the ReaderFor chokepoint verbatim, and a cut
// window comes back reported as such. Without Truncated a caller cannot tell
// "nothing in the backlog" from "nothing in the part I paid to look at".
func TestReaderFor_CarriesTheBoundedWindowVocabulary(t *testing.T) {
	fp := &readerBoundedProvider{name: "reader-bounded"}
	for i := 1; i <= 10; i++ {
		fp.corpus = append(fp.corpus, WorkItemRecord{Number: i})
	}
	Register(fp)
	r, err := ReaderFor("reader-bounded")
	if err != nil {
		t.Fatalf("ReaderFor: %v", err)
	}
	page, err := r.ListWorkItems(context.Background(), ListWorkItemsRequest{Newest: true, MaxScanned: 4})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if !fp.gotReq.Newest || fp.gotReq.MaxScanned != 4 {
		t.Errorf("provider got newest %v / maxScanned %d, want true / 4", fp.gotReq.Newest, fp.gotReq.MaxScanned)
	}
	if len(page.Items) != 4 || !page.Truncated {
		t.Errorf("page = %d items / truncated %v, want 4 items reported as a CUT window", len(page.Items), page.Truncated)
	}

	// The unbounded request is the pre-#2239 shape: no window, and a page that
	// never claims truncation.
	all, err := r.ListWorkItems(context.Background(), ListWorkItemsRequest{})
	if err != nil {
		t.Fatalf("ListWorkItems (unbounded): %v", err)
	}
	if fp.gotReq.Newest || fp.gotReq.MaxScanned != 0 {
		t.Errorf("unbounded request carried newest %v / maxScanned %d, want the zero-value opt-outs", fp.gotReq.Newest, fp.gotReq.MaxScanned)
	}
	if len(all.Items) != 10 || all.Truncated {
		t.Errorf("unbounded page = %d items / truncated %v, want the whole corpus and no truncation claim", len(all.Items), all.Truncated)
	}
}
