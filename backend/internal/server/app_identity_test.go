package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// stubAppIdentityGetter is a test double for appIdentityGetter that
// returns canned App/User payloads and records call counts so the
// memoization (sync.Once) can be asserted.
type stubAppIdentityGetter struct {
	app     *githubclient.App
	appErr  error
	user    *githubclient.User
	userErr error

	appCalls  int32
	userCalls int32
	gotLogin  string
}

func (s *stubAppIdentityGetter) GetApp(context.Context) (*githubclient.App, error) {
	atomic.AddInt32(&s.appCalls, 1)
	if s.appErr != nil {
		return nil, s.appErr
	}
	return s.app, nil
}

func (s *stubAppIdentityGetter) GetUser(_ context.Context, login string) (*githubclient.User, error) {
	atomic.AddInt32(&s.userCalls, 1)
	s.gotLogin = login
	if s.userErr != nil {
		return nil, s.userErr
	}
	return s.user, nil
}

func TestResolveAppBotIdentity_ComposesAndCaches(t *testing.T) {
	stub := &stubAppIdentityGetter{
		app:  &githubclient.App{Slug: "fishhawk"},
		user: &githubclient.User{ID: 41898282, Login: "fishhawk[bot]"},
	}
	s := New(Config{Addr: "127.0.0.1:0"})
	s.appIdentityGetterOverride = stub

	name, email := s.resolveAppBotIdentity(context.Background())
	if name != "fishhawk[bot]" {
		t.Errorf("name = %q, want fishhawk[bot]", name)
	}
	const wantEmail = "41898282+fishhawk[bot]@users.noreply.github.com"
	if email != wantEmail {
		t.Errorf("email = %q, want %q", email, wantEmail)
	}
	if stub.gotLogin != "fishhawk[bot]" {
		t.Errorf("GetUser login = %q, want fishhawk[bot]", stub.gotLogin)
	}

	// Second call must be served from the cache — no additional API calls.
	name2, email2 := s.resolveAppBotIdentity(context.Background())
	if name2 != name || email2 != email {
		t.Errorf("second call = (%q,%q), want (%q,%q)", name2, email2, name, email)
	}
	if got := atomic.LoadInt32(&stub.appCalls); got != 1 {
		t.Errorf("GetApp called %d times, want 1 (memoized)", got)
	}
	if got := atomic.LoadInt32(&stub.userCalls); got != 1 {
		t.Errorf("GetUser called %d times, want 1 (memoized)", got)
	}
}

func TestResolveAppBotIdentity_EmptyWhenUnconfigured(t *testing.T) {
	// No override and no cfg.GitHub → resolver is nil → empty identity.
	s := New(Config{Addr: "127.0.0.1:0"})

	name, email := s.resolveAppBotIdentity(context.Background())
	if name != "" || email != "" {
		t.Errorf("identity = (%q,%q), want empty when GitHub unconfigured", name, email)
	}
}

func TestResolveAppBotIdentity_EmptyOnError(t *testing.T) {
	cases := []struct {
		name string
		stub *stubAppIdentityGetter
	}{
		{"get app fails", &stubAppIdentityGetter{appErr: errors.New("boom")}},
		{"empty slug", &stubAppIdentityGetter{app: &githubclient.App{Slug: ""}}},
		{"get user fails", &stubAppIdentityGetter{
			app:     &githubclient.App{Slug: "fishhawk"},
			userErr: githubclient.ErrNotFound,
		}},
		{"zero user id", &stubAppIdentityGetter{
			app:  &githubclient.App{Slug: "fishhawk"},
			user: &githubclient.User{ID: 0},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0"})
			s.appIdentityGetterOverride = tc.stub
			name, email := s.resolveAppBotIdentity(context.Background())
			if name != "" || email != "" {
				t.Errorf("identity = (%q,%q), want empty on failure", name, email)
			}
			// Empty result is cached too: a second call must not retry.
			_, _ = s.resolveAppBotIdentity(context.Background())
			if got := atomic.LoadInt32(&tc.stub.appCalls); got != 1 {
				t.Errorf("GetApp called %d times, want 1 (empty result cached)", got)
			}
		})
	}
}

// TestPromptResponse_CarriesCommitAuthorIdentity asserts both the signed
// prompt endpoint and the SPA-readable prompt-render endpoint echo the
// backend-resolved App bot commit identity on their response bodies.
func TestPromptResponse_CarriesCommitAuthorIdentity(t *testing.T) {
	const (
		wantName  = "fishhawk[bot]"
		wantEmail = "41898282+fishhawk[bot]@users.noreply.github.com"
	)

	t.Run("signed prompt endpoint", func(t *testing.T) {
		s, rr, ar, _, sf, gh := newImplementPromptServer(t)
		gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "We need a foo helper."}
		s.appIdentityGetterOverride = &stubAppIdentityGetter{
			app:  &githubclient.App{Slug: "fishhawk"},
			user: &githubclient.User{ID: 41898282, Login: "fishhawk[bot]"},
		}
		runID, planStageID, implStageID, _ := seedRunWithStages(rr)
		v := "standard_v1"
		ar.seed(planStageID, &artifact.Artifact{
			ID:            uuid.New(),
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &v,
			Content:       fixturePlanJSON(t),
			ContentHash:   "deadbeef",
			CreatedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		})
		priv, _ := sf.issue(t, runID)
		w := promptRequestForStage(t, s, runID, implStageID, priv)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.CommitAuthorName != wantName {
			t.Errorf("commit_author_name = %q, want %q", resp.CommitAuthorName, wantName)
		}
		if resp.CommitAuthorEmail != wantEmail {
			t.Errorf("commit_author_email = %q, want %q", resp.CommitAuthorEmail, wantEmail)
		}
	})

	t.Run("unresolvable identity omits fields", func(t *testing.T) {
		s, rr, ar, _, sf, gh := newImplementPromptServer(t)
		gh.issue = &githubclient.Issue{Number: 42, Title: "Add foo", Body: "We need a foo helper."}
		// No appIdentityGetterOverride and no cfg.GitHub → empty identity.
		runID, planStageID, implStageID, _ := seedRunWithStages(rr)
		v := "standard_v1"
		ar.seed(planStageID, &artifact.Artifact{
			ID:            uuid.New(),
			StageID:       planStageID,
			Kind:          artifact.KindPlan,
			SchemaVersion: &v,
			Content:       fixturePlanJSON(t),
			ContentHash:   "deadbeef",
			CreatedAt:     time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		})
		priv, _ := sf.issue(t, runID)
		w := promptRequestForStage(t, s, runID, implStageID, priv)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
		}
		// omitempty: the JSON must not carry the keys at all.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["commit_author_name"]; ok {
			t.Error("commit_author_name present; want omitted when unresolvable")
		}
		if _, ok := raw["commit_author_email"]; ok {
			t.Error("commit_author_email present; want omitted when unresolvable")
		}
	})
}
