package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pushnotify"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Push-notification wiring (#2292): server.New must build the Router's
// channel list so the push channel is reachable with AND without a GitHub
// client, while no channel at all still leaves s.issueNotifier a nil
// interface.

type pushWiringRuns struct {
	run.Repository
	r *run.Run
}

func (f *pushWiringRuns) GetRun(context.Context, uuid.UUID) (*run.Run, error) { return f.r, nil }
func (f *pushWiringRuns) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, nil
}

type pushWiringAudit struct {
	audit.Repository
	mu      sync.Mutex
	entries []*audit.Entry
}

func (f *pushWiringAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := p.RunID
	e := &audit.Entry{Sequence: int64(len(f.entries) + 1), RunID: &r, Category: p.Category, Payload: p.Payload, Timestamp: p.Timestamp}
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *pushWiringAudit) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*audit.Entry(nil), f.entries...), nil
}

func (f *pushWiringAudit) ListForRunByCategory(ctx context.Context, id uuid.UUID, c string) ([]*audit.Entry, error) {
	all, _ := f.ListForRun(ctx, id)
	var out []*audit.Entry
	for _, e := range all {
		if e.Category == c {
			out = append(out, e)
		}
	}
	return out, nil
}

type pushWiringSink struct {
	mu sync.Mutex
	n  int
}

func (*pushWiringSink) Name() string { return "webhook" }
func (s *pushWiringSink) Deliver(context.Context, pushnotify.Event) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return nil
}
func (s *pushWiringSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

// pushWiringFixture: a CLI-triggered run (so the GitHub channel is a no-op
// on it) carrying one scope-amendment page-class event.
func pushWiringFixture(t *testing.T) (*pushWiringRuns, *pushWiringAudit, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	au := &pushWiringAudit{}
	_, _ = au.AppendChained(context.Background(), audit.ChainAppendParams{RunID: id, Category: "scope_amendment_requested", Timestamp: time.Now()})
	return &pushWiringRuns{r: &run.Run{ID: id, Repo: "acme/widgets", TriggerSource: run.TriggerCLI}}, au, id
}

func pushWiringDispatcher(t *testing.T, sinks ...pushnotify.Sink) *pushnotify.Dispatcher {
	t.Helper()
	d := pushnotify.NewDispatcher(sinks, pushnotify.Options{})
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

func drainPush(t *testing.T, d *pushnotify.Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestNew_PushChannelWiring(t *testing.T) {
	t.Run("github_and_sinks_fan_to_both", func(t *testing.T) {
		runs, au, id := pushWiringFixture(t)
		sink := &pushWiringSink{}
		d := pushWiringDispatcher(t, sink)
		s := New(Config{RunRepo: runs, AuditRepo: au, ArtifactRepo: newFakeArtifactRepo(),
			GitHub: &githubclient.Client{}, PushDispatcher: d})
		if s.issueNotifier == nil {
			t.Fatal("issueNotifier nil with GitHub and sinks configured")
		}
		if !s.issueNotifier.ArtifactListerWired() {
			t.Error("GitHub-comment channel missing from the Router (ArtifactListerWired false)")
		}
		if err := s.issueNotifier.NotifyPageClassForRun(context.Background(), id); err != nil {
			t.Fatalf("notify: %v", err)
		}
		drainPush(t, d)
		if sink.count() != 1 {
			t.Errorf("push deliveries = %d, want 1 (push channel missing from the Router)", sink.count())
		}
	})
	t.Run("sinks_without_github_still_push", func(t *testing.T) {
		runs, au, id := pushWiringFixture(t)
		sink := &pushWiringSink{}
		d := pushWiringDispatcher(t, sink)
		s := New(Config{RunRepo: runs, AuditRepo: au, PushDispatcher: d})
		if s.issueNotifier == nil {
			t.Fatal("issueNotifier nil without GitHub: push must be reachable without a forge client")
		}
		if s.issueNotifier.ArtifactListerWired() {
			t.Error("ArtifactListerWired true without a GitHub channel")
		}
		_ = s.issueNotifier.NotifyPageClassForRun(context.Background(), id)
		drainPush(t, d)
		if sink.count() != 1 {
			t.Errorf("push deliveries = %d, want 1", sink.count())
		}
	})
	t.Run("no_sinks_keeps_todays_notifier", func(t *testing.T) {
		runs, au, _ := pushWiringFixture(t)
		if s := New(Config{RunRepo: runs, AuditRepo: au}); s.issueNotifier != nil {
			t.Errorf("no GitHub and no sinks: issueNotifier = %T, want a nil interface", s.issueNotifier)
		}
		s := New(Config{RunRepo: runs, AuditRepo: au, ArtifactRepo: newFakeArtifactRepo(), GitHub: &githubclient.Client{}})
		if s.issueNotifier == nil || !s.issueNotifier.ArtifactListerWired() {
			t.Error("GitHub-only: want today's single-channel Router")
		}
		rec := httptest.NewRecorder()
		s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if !strings.Contains(rec.Body.String(), `"push_sinks":[]`) {
			t.Errorf("/healthz without sinks must carry push_sinks: []: %s", rec.Body.String())
		}
	})
}

// TestHealthz_PushSinksNamesOnly (H1): /healthz lists the sink KINDS and the
// full body carries none of the configured URL / address / recipient /
// secret tokens.
func TestHealthz_PushSinksNamesOnly(t *testing.T) {
	const (
		urlTok    = "HEALTHZURLTOKEN77"
		secretTok = "HEALTHZSECRETTOKEN78"
		smtpTok   = "healthzsmtptoken79"
		rcptTok   = "healthzrcpttoken80"
		passTok   = "HEALTHZPASSTOKEN81"
	)
	env := map[string]string{
		pushnotify.EnvWebhookURL:      "https://hooks.example.com/" + urlTok,
		pushnotify.EnvWebhookSecret:   secretTok,
		pushnotify.EnvSlackWebhookURL: "https://hooks.slack.example.com/services/" + urlTok,
		pushnotify.EnvEmailSMTPAddr:   smtpTok + ".example.com:587",
		pushnotify.EnvEmailFrom:       "fishhawk@example.com",
		pushnotify.EnvEmailTo:         rcptTok + "@example.com",
		pushnotify.EnvEmailUsername:   "ops",
		pushnotify.EnvEmailPassword:   passTok,
	}
	sinks, err := pushnotify.SinksFromEnv(func(k string) string { return env[k] })
	if err != nil || len(sinks) != 3 {
		t.Fatalf("SinksFromEnv = %d, %v", len(sinks), err)
	}
	s := New(Config{PushDispatcher: pushWiringDispatcher(t, sinks...)})
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(body.PushSinks, ",") != "email,slack,webhook" {
		t.Errorf("push_sinks = %v, want [email slack webhook]", body.PushSinks)
	}
	for _, tok := range []string{urlTok, secretTok, smtpTok, rcptTok, passTok} {
		if strings.Contains(rec.Body.String(), tok) {
			t.Errorf("/healthz leaked configuration token %q: %s", tok, rec.Body.String())
		}
	}
}
