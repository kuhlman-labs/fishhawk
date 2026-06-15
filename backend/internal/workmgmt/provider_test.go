package workmgmt

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeProvider is a test double registered to exercise the registry.
type fakeProvider struct {
	name    string
	got     ProviderRequest
	gotTran TransitionRequest
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) File(_ context.Context, req ProviderRequest) (*CreatedItem, error) {
	f.got = req
	return &CreatedItem{Provider: f.name, Number: 42, URL: "https://example/42"}, nil
}

func (f *fakeProvider) Transition(_ context.Context, req TransitionRequest) (*TransitionResult, error) {
	f.gotTran = req
	return &TransitionResult{Moved: true, From: "Backlog", To: "In Progress"}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_registered"}
	Register(fp)

	got, err := Get("test_provider_registered")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name() != "test_provider_registered" {
		t.Errorf("got provider %q", got.Name())
	}
}

func TestRegistry_UnknownProviderFailsClosed(t *testing.T) {
	// jira is interface-only in v0 — never registered. An attempt to file
	// against it must fail closed with a typed error naming the id.
	_, err := Get("jira")
	var upe *UnknownProviderError
	if !errors.As(err, &upe) {
		t.Fatalf("want *UnknownProviderError, got %v", err)
	}
	if upe.ID != "jira" {
		t.Errorf("error ID = %q, want jira", upe.ID)
	}
	if !strings.Contains(upe.Error(), "jira") {
		t.Errorf("error message must name the missing provider: %q", upe.Error())
	}
}

func TestUnknownProviderError_MessageForms(t *testing.T) {
	empty := (&UnknownProviderError{ID: "x"}).Error()
	if !strings.Contains(empty, "no providers registered") {
		t.Errorf("empty-registry message = %q", empty)
	}
	withKnown := (&UnknownProviderError{ID: "x", Known: []string{"github_projects"}}).Error()
	if !strings.Contains(withKnown, "github_projects") {
		t.Errorf("known-set message = %q", withKnown)
	}
}

func TestRegistry_DispatchPassesRequest(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_dispatch"}
	Register(fp)
	p, err := Get("test_provider_dispatch")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := ProviderRequest{
		Item:   WorkItem{Type: "bug", Title: "boom"},
		Number: 0,
		Target: Target{InstallationID: 7, Repo: Repo{Owner: "o", Name: "r"}},
	}
	created, err := p.File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Number != 42 {
		t.Errorf("created.Number = %d, want 42", created.Number)
	}
	if fp.got.Item.Title != "boom" {
		t.Errorf("provider did not receive request: %+v", fp.got)
	}
}

func TestRegistry_DispatchTransition(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_transition"}
	Register(fp)
	p, err := Get("test_provider_transition")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	tr, ok := p.(Transitioner)
	if !ok {
		t.Fatalf("provider does not implement Transitioner")
	}
	req := TransitionRequest{
		IssueNumber:          1012,
		Trigger:              "run_started",
		Target:               Target{InstallationID: 7, Repo: Repo{Owner: "o", Name: "r"}},
		CanonicalState:       CanonicalStateInProgress,
		ExpectedSourceStates: []string{CanonicalStateBacklog},
		States:               map[string]string{CanonicalStateBacklog: "Backlog", CanonicalStateInProgress: "In Progress"},
	}
	res, err := tr.Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.To != "In Progress" {
		t.Errorf("result = %+v", res)
	}
	if fp.gotTran.IssueNumber != 1012 || fp.gotTran.CanonicalState != CanonicalStateInProgress {
		t.Errorf("provider did not receive transition request: %+v", fp.gotTran)
	}
}
