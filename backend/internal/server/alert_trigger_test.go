package server

// Tests for POST /v0/triggers/alert (E35.4 / #1601). They override the
// package-level conventionsLoader and register a work-item provider in the
// process-global workmgmt registry, so NONE of them calls t.Parallel. The
// dedup ledger is the real alerttrigger.PostgresStore over pgtest.NewPool.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/alerttrigger"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

const (
	alertTestSource    = "grafana-prod"
	alertTestRepo      = "kuhlman-labs/fishhawk"
	alertTestSecretEnv = "ALERT_TEST_SECRET"
)

var alertTestSecret = []byte("alert-test-secret-0123456789abcdef-0123")

// alertTestConventionsYAML is a github_projects conventions document whose bug
// type's title is the bare summary, so a source needs no parent epic.
const alertTestConventionsYAML = `
spec_version: work-management-v0
provider: github_projects
project:
  owner: kuhlman-labs
  number: 7
required_fields: [Summary, Done-means, complexity]
types:
  bug:
    body_skeleton: [Summary, Observed, Proposal, Done-means, Acceptance criteria, Notes]
    default_fields: {complexity: medium}
`

const alertTestGitLabConventionsYAML = `
spec_version: work-management-v0
provider: gitlab
gitlab:
  project: kuhlman-labs/fishhawk
required_fields: [Summary, Done-means, complexity]
types: {bug: {body_skeleton: [Summary]}}
`

func parseAlertTestConventions(t *testing.T, doc string) workmgmt.Conventions {
	t.Helper()
	conv, err := workmgmt.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse test conventions: %v", err)
	}
	return conv
}

// setAlertConventionsLoader installs loader for the test and restores the
// previous one on cleanup.
func setAlertConventionsLoader(t *testing.T, loader func(context.Context, string) (workmgmt.Conventions, error)) {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = loader
	t.Cleanup(func() { conventionsLoader = prev })
}

// alertTestSources parses a one-source file; extra is appended to the source
// entry (e.g. "    auto_start: true\n").
func alertTestSources(t *testing.T, extra string) alerttrigger.Sources {
	t.Helper()
	doc := "version: 1\nsources:\n  - id: " + alertTestSource +
		"\n    secret_env: " + alertTestSecretEnv +
		"\n    repo: " + alertTestRepo +
		"\n    labels: [area:backend]\n" + extra
	src, err := alerttrigger.ParseSources([]byte(doc), func(k string) string {
		if k == alertTestSecretEnv {
			return string(alertTestSecret)
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parse test sources: %v", err)
	}
	return src
}

// alertWorkProvider is the github_projects provider double: it records every
// filing, numbers issues 901, 902, ... (or always fixedNumber when set, the
// shape of a forge-side idempotent replay) and fails the next failNext filings.
type alertWorkProvider struct {
	mu          sync.Mutex
	filings     []workmgmt.ProviderRequest
	failNext    int
	fixedNumber int
}

func (*alertWorkProvider) Name() string { return workmgmtgithub.ProviderName }

func (p *alertWorkProvider) File(_ context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext > 0 {
		p.failNext--
		return nil, errors.New("forge unavailable")
	}
	p.filings = append(p.filings, req)
	n := 900 + len(p.filings)
	if p.fixedNumber != 0 {
		n = p.fixedNumber
	}
	return &workmgmt.CreatedItem{
		Provider: workmgmtgithub.ProviderName,
		Number:   n,
		URL:      fmt.Sprintf("https://github.com/%s/issues/%d", alertTestRepo, n),
		Boarded:  true,
	}, nil
}

func (p *alertWorkProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.filings)
}

func (p *alertWorkProvider) last() workmgmt.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.filings[len(p.filings)-1]
}

// alertFakeGitHub is the GitHub REST stub: installation lookup, issue reads
// for the auto-start's issue_context hydration, and issue comments (recorded;
// commentFailNext answers 500).
type alertFakeGitHub struct {
	mu              sync.Mutex
	comments        map[int][]string
	commentFailNext int
	installStatus   int
}

func (f *alertFakeGitHub) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		status := f.installStatus
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":7}`))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"number":%s,"title":"Incident","body":"incident body"}`, r.PathValue("n"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{n}/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if f.commentFailNext > 0 {
			f.commentFailNext--
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n, _ := strconv.Atoi(r.PathValue("n"))
		if f.comments == nil {
			f.comments = map[int][]string{}
		}
		f.comments[n] = append(f.comments[n], body.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"body":"ok"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// set mutates the stub under its lock: the stub's handlers run on the
// httptest server's goroutines.
func (f *alertFakeGitHub) set(fn func(*alertFakeGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *alertFakeGitHub) commentsOn(issue int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.comments[issue]...)
}

func (f *alertFakeGitHub) commentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.comments {
		n += len(c)
	}
	return n
}

// alertRecordingDeliveries wraps the in-memory delivery store and records
// every Mark / Unmark, so a test can assert an unauthenticated request never
// wrote the store. markErr / unmarkErr inject a store failure.
type alertRecordingDeliveries struct {
	mu        sync.Mutex
	inner     *webhook.MemoryStore
	marks     []string
	unmarks   []string
	markErr   error
	unmarkErr error
}

func (d *alertRecordingDeliveries) Mark(id string) error {
	d.mu.Lock()
	d.marks = append(d.marks, id)
	err := d.markErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return d.inner.Mark(id)
}

func (d *alertRecordingDeliveries) Unmark(id string) error {
	d.mu.Lock()
	d.unmarks = append(d.unmarks, id)
	err := d.unmarkErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	return d.inner.Unmark(id)
}

func (d *alertRecordingDeliveries) unmarkCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.unmarks)
}

func (d *alertRecordingDeliveries) markCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.marks)
}

// alertFakeSpecSource serves a fixed spec (or error) and counts fetches.
type alertFakeSpecSource struct {
	mu    sync.Mutex
	calls int
	spec  []byte
	err   error
}

func (f *alertFakeSpecSource) FetchSpec(context.Context, string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.spec, "blobsha", f.err
}

// alertFlakyIncidents wraps the real store to inject one Claim error, a fixed
// claim kind, or a Complete error.
type alertFlakyIncidents struct {
	alerttrigger.Store
	claimErrNext int
	claimKind    alerttrigger.ClaimKind
	completeErr  error
}

func (s *alertFlakyIncidents) Claim(ctx context.Context, key alerttrigger.Key, staleAfter time.Duration) (alerttrigger.Claim, error) {
	if s.claimErrNext > 0 {
		s.claimErrNext--
		return alerttrigger.Claim{}, errors.New("db unavailable")
	}
	if s.claimKind != "" {
		return alerttrigger.Claim{Kind: s.claimKind}, nil
	}
	return s.Store.Claim(ctx, key, staleAfter)
}

func (s *alertFlakyIncidents) Complete(ctx context.Context, key alerttrigger.Key, token uuid.UUID, n int, url string) error {
	if s.completeErr != nil {
		return s.completeErr
	}
	return s.Store.Complete(ctx, key, token, n, url)
}

type alertFixture struct {
	s          *Server
	pool       *pgxpool.Pool
	incidents  *alerttrigger.PostgresStore
	provider   *alertWorkProvider
	gh         *alertFakeGitHub
	deliveries *alertRecordingDeliveries
	audit      *auditFake
	specs      *alertFakeSpecSource
	runs       *fakeRepo
}

// newAlertFixture wires a server with one configured source (sourceExtra is
// appended to its entry), the github conventions, a recording provider, a
// GitHub stub, a recording in-memory delivery store, a WORKING spec source
// (so an auto-start would mint a run if it fired), the fake RunRepo, and the
// real pgtest dedup ledger.
func newAlertFixture(t *testing.T, sourceExtra string) *alertFixture {
	t.Helper()
	conv := parseAlertTestConventions(t, alertTestConventionsYAML)
	setAlertConventionsLoader(t, func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil })
	provider := &alertWorkProvider{}
	workmgmt.Register(provider)

	pool := pgtest.NewPool(t)
	f := &alertFixture{
		pool:       pool,
		incidents:  alerttrigger.NewPostgresStore(pool),
		provider:   provider,
		gh:         &alertFakeGitHub{},
		deliveries: &alertRecordingDeliveries{inner: webhook.NewMemoryStore(24 * time.Hour)},
		audit:      newAuditFake(),
		specs:      &alertFakeSpecSource{spec: []byte(alertSpec("diff"))},
		runs:       newFakeRepo(),
	}
	f.s = New(Config{
		Addr:              "127.0.0.1:0",
		RunRepo:           f.runs,
		AuditRepo:         f.audit,
		GitHub:            f.gh.client(t),
		WebhookDeliveries: f.deliveries,
		AlertSources:      alertTestSources(t, sourceExtra),
		AlertIncidents:    f.incidents,
		AlertSpecSource:   f.specs,
	})
	return f
}

// alertSend is one request's exact wire values, so a resend is byte-identical.
type alertSend struct {
	source, ts, sig string
	body            []byte
}

func signAlert(body []byte, at time.Time) alertSend {
	ts := strconv.FormatInt(at.Unix(), 10)
	return alertSend{source: alertTestSource, ts: ts, sig: alerttrigger.Sign(alertTestSecret, ts, body), body: body}
}

func alertBody(t *testing.T, fingerprint string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"fingerprint": fingerprint,
		"title":       "Checkout 5xx rate above 5%",
		"severity":    "high",
		"description": "p99 latency spiked after the 14:02 deploy",
		"environment": "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *alertFixture) send(t *testing.T, a alertSend) *httptest.ResponseRecorder {
	t.Helper()
	return sendAlertTo(t, f.s, a)
}

func sendAlertTo(t *testing.T, s *Server, a alertSend) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/triggers/alert", bytes.NewReader(a.body))
	req.Header.Set("Content-Type", "application/json")
	if a.source != "" {
		req.Header.Set(alerttrigger.HeaderSource, a.source)
	}
	if a.ts != "" {
		req.Header.Set(alerttrigger.HeaderTimestamp, a.ts)
	}
	if a.sig != "" {
		req.Header.Set(alerttrigger.HeaderSignature, a.sig)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeAlertResult(t *testing.T, rec *httptest.ResponseRecorder) alertTriggerResponse {
	t.Helper()
	var out alertTriggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode alert result: %v\n%s", err, rec.Body.String())
	}
	return out
}

func wantAlertStatus(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d %s:\n%s", rec.Code, status, code, rec.Body.String())
	}
	if code != "" {
		if got := decodeErrorEnvelope(t, rec).Code; got != code {
			t.Fatalf("code = %q, want %q:\n%s", got, code, rec.Body.String())
		}
	}
}

// incidentRow reads the dedup ledger row for fingerprint back from Postgres.
func (f *alertFixture) incidentRow(t *testing.T, fingerprint string) (occurrences int, issue *int, runID *uuid.UUID) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT occurrences, issue_number, run_id FROM alert_incidents WHERE source_id = $1 AND repo = $2 AND fingerprint = $3`,
		alertTestSource, alertTestRepo, fingerprint).Scan(&occurrences, &issue, &runID)
	if err != nil {
		t.Fatalf("read alert_incidents row for %q: %v", fingerprint, err)
	}
	return occurrences, issue, runID
}

func (f *alertFixture) auditCount(category string) int {
	n := 0
	for _, p := range f.audit.globalAppended {
		if p.Category == category {
			n++
		}
	}
	return n
}

func (f *alertFixture) auditPayload(t *testing.T, category string) map[string]any {
	t.Helper()
	for i := len(f.audit.globalAppended) - 1; i >= 0; i-- {
		p := f.audit.globalAppended[i]
		if p.Category == category {
			var m map[string]any
			if err := json.Unmarshal(p.Payload, &m); err != nil {
				t.Fatalf("decode %s payload: %v", category, err)
			}
			if p.ActorSubject == nil || *p.ActorSubject != AlertRunSubject {
				t.Errorf("%s actor subject = %v, want %s", category, p.ActorSubject, AlertRunSubject)
			}
			return m
		}
	}
	t.Fatalf("no %s audit row (rows: %d)", category, len(f.audit.globalAppended))
	return nil
}

// TestAlertTrigger_Unconfigured503: no configured sources → the ingress is off.
func TestAlertTrigger_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	rec := sendAlertTo(t, s, signAlert(alertBody(t, "fp-1"), time.Now()))
	wantAlertStatus(t, rec, http.StatusServiceUnavailable, "alert_trigger_unconfigured")
}

// TestAlertTrigger_StoreUnconfigured503: sources configured but one of the
// three dependencies is missing → 503 alert_store_unconfigured, per dependency.
func TestAlertTrigger_StoreUnconfigured503(t *testing.T) {
	gh := (&alertFakeGitHub{}).client(t)
	store := alerttrigger.Store(&alertFlakyIncidents{})
	deliveries := webhook.NewMemoryStore(time.Hour)
	cases := map[string]Config{
		"no incident store": {GitHub: gh, WebhookDeliveries: deliveries},
		"no delivery store": {GitHub: gh, AlertIncidents: store},
		"no github":         {AlertIncidents: store, WebhookDeliveries: deliveries},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Addr = "127.0.0.1:0"
			cfg.AlertSources = alertTestSources(t, "")
			rec := sendAlertTo(t, New(cfg), signAlert(alertBody(t, "fp-1"), time.Now()))
			wantAlertStatus(t, rec, http.StatusServiceUnavailable, "alert_store_unconfigured")
		})
	}
}

// TestAlertTrigger_BodyTooLarge413: a validly signed body over 64 KiB is
// refused before verification; nothing is marked or filed.
func TestAlertTrigger_BodyTooLarge413(t *testing.T) {
	f := newAlertFixture(t, "")
	body := bytes.Repeat([]byte("x"), maxAlertTriggerBodyBytes+1)
	rec := f.send(t, signAlert(body, time.Now()))
	wantAlertStatus(t, rec, http.StatusRequestEntityTooLarge, "body_too_large")
	if f.deliveries.markCount() != 0 || f.provider.count() != 0 {
		t.Errorf("marks = %d, filings = %d; want 0 and 0", f.deliveries.markCount(), f.provider.count())
	}
}

// alertErrReader fails every Read, standing in for a broken request body.
type alertErrReader struct{}

func (alertErrReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// TestAlertTrigger_BodyReadError400: a body that cannot be read is refused
// 400 validation_failed before verification; nothing is marked.
func TestAlertTrigger_BodyReadError400(t *testing.T) {
	f := newAlertFixture(t, "")
	a := signAlert(alertBody(t, "fp-readerr"), time.Now())
	req := httptest.NewRequest(http.MethodPost, "/v0/triggers/alert", alertErrReader{})
	req.Header.Set(alerttrigger.HeaderSource, a.source)
	req.Header.Set(alerttrigger.HeaderTimestamp, a.ts)
	req.Header.Set(alerttrigger.HeaderSignature, a.sig)
	rec := httptest.NewRecorder()
	f.s.handleAlertTrigger(rec, req)
	wantAlertStatus(t, rec, http.StatusBadRequest, "validation_failed")
	if f.deliveries.markCount() != 0 {
		t.Errorf("marks = %d, want 0", f.deliveries.markCount())
	}
}

// TestAlertTrigger_RejectionBranches: one row per 401 branch, each asserting
// the status, the specific code, and that nothing was filed, commented,
// marked or audited.
func TestAlertTrigger_RejectionBranches(t *testing.T) {
	now := time.Now()
	unrelatedKey := []byte("an-unrelated-32-byte-key-for-tests!!")
	cases := []struct {
		name   string
		send   func(body []byte) alertSend
		code   string
		reason string
	}{
		{"no signature header", func(b []byte) alertSend {
			a := signAlert(b, now)
			a.sig = ""
			return a
		}, "alert_signature_missing", ""},
		{"bad timestamp", func(b []byte) alertSend {
			a := signAlert(b, now)
			a.ts = "17x"
			a.sig = alerttrigger.Sign(alertTestSecret, a.ts, b)
			return a
		}, "alert_timestamp_invalid", ""},
		{"unrelated-key signature", func(b []byte) alertSend {
			a := signAlert(b, now)
			a.sig = alerttrigger.Sign(unrelatedKey, a.ts, b)
			return a
		}, "alert_signature_invalid", ""},
		{"unknown source", func(b []byte) alertSend {
			a := signAlert(b, now)
			a.source = "not-a-source"
			return a
		}, "alert_signature_invalid", ""},
		{"stale but validly signed", func(b []byte) alertSend {
			return signAlert(b, now.Add(-10*time.Minute))
		}, "alert_replayed", "stale"},
		// The HTTP-level twin of verify.go's binding fix A: a validly signed
		// 12-digit far-future timestamp (~31,700 years ahead) overflows a
		// signed-Duration skew, so only an overflow-free window refuses it.
		{"far-future validly signed", func(b []byte) alertSend {
			const ts = "999999999999"
			return alertSend{source: alertTestSource, ts: ts, sig: alerttrigger.Sign(alertTestSecret, ts, b), body: b}
		}, "alert_replayed", "stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAlertFixture(t, "")
			rec := f.send(t, tc.send(alertBody(t, "fp-reject")))
			wantAlertStatus(t, rec, http.StatusUnauthorized, tc.code)
			if tc.reason != "" {
				if got := decodeErrorEnvelope(t, rec).Details["reason"]; got != tc.reason {
					t.Errorf("details.reason = %v, want %s", got, tc.reason)
				}
			}
			if f.provider.count() != 0 || f.gh.commentCount() != 0 {
				t.Errorf("filings = %d, comments = %d; want 0 and 0", f.provider.count(), f.gh.commentCount())
			}
			if f.deliveries.markCount() != 0 {
				t.Errorf("a rejected request marked the delivery store %d times, want 0", f.deliveries.markCount())
			}
			if n := len(f.audit.globalAppended); n != 0 {
				t.Errorf("a rejected request wrote %d audit rows, want 0", n)
			}
		})
	}
}

// TestAlertTrigger_ExactResendIsReplayed: an in-window exact resend is
// refused 401 alert_replayed{duplicate}. Asserted on STATUS and ZERO comments:
// with the Mark removed, the resend would reach fingerprint dedup and answer
// 200 occurrence with one comment, so "one issue" alone would not discriminate.
func TestAlertTrigger_ExactResendIsReplayed(t *testing.T) {
	f := newAlertFixture(t, "")
	a := signAlert(alertBody(t, "fp-resend"), time.Now())
	wantAlertStatus(t, f.send(t, a), http.StatusCreated, "")

	rec := f.send(t, a)
	wantAlertStatus(t, rec, http.StatusUnauthorized, "alert_replayed")
	if got := decodeErrorEnvelope(t, rec).Details["reason"]; got != "duplicate" {
		t.Errorf("details.reason = %v, want duplicate", got)
	}
	if f.gh.commentCount() != 0 || f.provider.count() != 1 {
		t.Errorf("comments = %d, filings = %d; want 0 and 1", f.gh.commentCount(), f.provider.count())
	}
	if n := len(f.audit.globalAppended); n != 1 {
		t.Errorf("audit rows = %d, want 1 (the filing only; the replay rejection is not audited)", n)
	}
}

// TestAlertTrigger_HexCaseFlippedResendIsReplayed: the nonce is keyed on the
// DECODED MAC, so upper-casing the signature hex does not evade it.
func TestAlertTrigger_HexCaseFlippedResendIsReplayed(t *testing.T) {
	f := newAlertFixture(t, "")
	a := signAlert(alertBody(t, "fp-case"), time.Now())
	wantAlertStatus(t, f.send(t, a), http.StatusCreated, "")

	flipped := a
	flipped.sig = alerttrigger.SignaturePrefix + strings.ToUpper(strings.TrimPrefix(a.sig, alerttrigger.SignaturePrefix))
	if flipped.sig == a.sig {
		t.Fatal("fixture: the upper-cased signature equals the original")
	}
	rec := f.send(t, flipped)
	wantAlertStatus(t, rec, http.StatusUnauthorized, "alert_replayed")
	if f.gh.commentCount() != 0 {
		t.Errorf("comments = %d, want 0", f.gh.commentCount())
	}
}

// TestAlertTrigger_UnsignedRequestsDoNotMarkDeliveries: an unauthenticated
// caller cannot write the delivery store.
func TestAlertTrigger_UnsignedRequestsDoNotMarkDeliveries(t *testing.T) {
	f := newAlertFixture(t, "")
	for i := 0; i < 5; i++ {
		a := signAlert(alertBody(t, fmt.Sprintf("fp-unsigned-%d", i)), time.Now())
		a.sig = ""
		wantAlertStatus(t, f.send(t, a), http.StatusUnauthorized, "alert_signature_missing")
	}
	if n := f.deliveries.markCount(); n != 0 {
		t.Fatalf("5 unsigned requests marked the delivery store %d times, want 0", n)
	}
}

// TestAlertTrigger_DedupOneIssuePerFingerprint is the done-means test: two
// independently signed alerts with one fingerprint file ONE issue and post
// ONE occurrence comment; a different fingerprint files a second issue.
func TestAlertTrigger_DedupOneIssuePerFingerprint(t *testing.T) {
	f := newAlertFixture(t, "")
	now := time.Now()
	first := f.send(t, signAlert(alertBody(t, "fp-dedup"), now))
	wantAlertStatus(t, first, http.StatusCreated, "")
	filed := decodeAlertResult(t, first)
	if filed.Action != alertActionFiled || filed.IssueNumber != 901 || filed.Occurrences != 1 {
		t.Fatalf("first = %+v, want filed #901 occurrences 1", filed)
	}

	second := f.send(t, signAlert(alertBody(t, "fp-dedup"), now.Add(-time.Second)))
	wantAlertStatus(t, second, http.StatusOK, "")
	occ := decodeAlertResult(t, second)
	if occ.Action != alertActionOccurrence || occ.IssueNumber != 901 || occ.Occurrences != 2 || occ.IssueURL != filed.IssueURL {
		t.Fatalf("second = %+v, want occurrence on #901 occurrences 2", occ)
	}
	if occ.AutoStart != nil {
		t.Errorf("occurrence carried auto_start %+v, want none", occ.AutoStart)
	}
	if f.provider.count() != 1 {
		t.Fatalf("filings = %d, want exactly 1", f.provider.count())
	}
	if got := f.gh.commentsOn(901); len(got) != 1 || !strings.Contains(got[0], "occurrence 2") {
		t.Fatalf("comments on #901 = %q, want one occurrence-2 comment", got)
	}
	if n, issue, _ := f.incidentRow(t, "fp-dedup"); n != 2 || issue == nil || *issue != 901 {
		t.Fatalf("ledger row occurrences=%d issue=%v, want 2 and 901", n, issue)
	}

	third := f.send(t, signAlert(alertBody(t, "fp-other"), now))
	wantAlertStatus(t, third, http.StatusCreated, "")
	if got := decodeAlertResult(t, third); got.IssueNumber != 902 {
		t.Fatalf("a different fingerprint filed #%d, want a second issue #902", got.IssueNumber)
	}

	// The global audit rows carry the documented keys.
	if f.auditCount(categoryAlertIncidentFiled) != 2 || f.auditCount(categoryAlertIncidentOccurrence) != 1 {
		t.Fatalf("audit filed=%d occurrence=%d, want 2 and 1",
			f.auditCount(categoryAlertIncidentFiled), f.auditCount(categoryAlertIncidentOccurrence))
	}
	fp := f.auditPayload(t, categoryAlertIncidentFiled)
	for _, k := range []string{"source", "repo", "fingerprint", "severity", "issue_number", "issue_url", "auto_start"} {
		if _, ok := fp[k]; !ok {
			t.Errorf("alert_incident_filed payload lacks %q: %v", k, fp)
		}
	}
	op := f.auditPayload(t, categoryAlertIncidentOccurrence)
	for _, k := range []string{"source", "repo", "fingerprint", "issue_number", "occurrences"} {
		if _, ok := op[k]; !ok {
			t.Errorf("alert_incident_occurrence payload lacks %q: %v", k, op)
		}
	}
	if op["occurrences"] != float64(2) {
		t.Errorf("occurrence payload occurrences = %v, want 2", op["occurrences"])
	}
}

// TestAlertTrigger_FiledIssueIsConventionsComplete: the filing carries the
// alert summary, the source's labels, the bug sections and the idempotency
// marker that names a duplicate.
func TestAlertTrigger_FiledIssueIsConventionsComplete(t *testing.T) {
	f := newAlertFixture(t, "")
	wantAlertStatus(t, f.send(t, signAlert(alertBody(t, "fp-shape"), time.Now())), http.StatusCreated, "")
	item := f.provider.last().Item
	if item.Title != "Incident (high): Checkout 5xx rate above 5%" {
		t.Errorf("title = %q", item.Title)
	}
	key := workmgmt.MintIdempotencyKey(alertIncidentKeyNamespace, alertTestSource, alertTestRepo, "fp-shape")
	if !workmgmt.BodyHasIdempotencyKey(item.Body, key) {
		t.Errorf("filed body lacks the %s idempotency marker:\n%s", key, item.Body)
	}
	if !containsString(item.Classification.Labels, "area:backend") {
		t.Errorf("labels %v lack the source's area:backend", item.Classification.Labels)
	}
	for _, want := range []string{"## Observed", "Fingerprint: `fp-shape`", "## Done-means"} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("filed body lacks %q:\n%s", want, item.Body)
		}
	}
}

// TestAlertTrigger_FilingFailureReleasesClaim: the provider fails once; the
// EXACT same signed request retried is filed 201 — the claim was Released
// (else 202 in_flight) and the nonce Unmarked (else 401 alert_replayed).
func TestAlertTrigger_FilingFailureReleasesClaim(t *testing.T) {
	f := newAlertFixture(t, "")
	f.provider.failNext = 1
	a := signAlert(alertBody(t, "fp-fail"), time.Now())

	wantAlertStatus(t, f.send(t, a), http.StatusBadGateway, "work_item_filing_failed")
	if f.auditCount(categoryAlertIncidentFiled) != 0 {
		t.Error("a failed filing was audited as filed")
	}

	retry := f.send(t, a)
	wantAlertStatus(t, retry, http.StatusCreated, "")
	if got := decodeAlertResult(t, retry); got.Action != alertActionFiled || got.Occurrences != 1 {
		t.Fatalf("retry = %+v, want filed with occurrences 1 (the released claim's row is gone)", got)
	}
}

// TestAlertTrigger_PostMarkFailureUnmarks pins approval condition 2 and the
// README's Mark/Unmark contract: every post-Mark branch that neither files the
// issue (201) nor posts the occurrence comment (200) releases the nonce, so the
// SAME signed request (same timestamp, same signature, inside the window)
// resent after the cause clears is PROCESSED — answered with its branch's own
// outcome or success — and never refused 401 alert_replayed{duplicate}.
//
// Counterfactual per subtest: setting keep = true on that branch (or deleting
// the deferred release) leaves the nonce recorded, so the resend answers 401
// alert_replayed and the subtest goes RED.
func TestAlertTrigger_PostMarkFailureUnmarks(t *testing.T) {
	type mode struct {
		name   string
		status int
		code   string
		// body overrides the alert body (default: a valid fp-transient alert).
		body func(t *testing.T) []byte
		// seed prepares state before the request under test (optional).
		seed func(t *testing.T, f *alertFixture)
		// arm breaks the dependency for the first request; heal restores it.
		arm, heal func(t *testing.T, f *alertFixture)
		// retryStatus / retryCode are the resend's expected answer.
		retryStatus int
		retryCode   string
	}
	ghConv := parseAlertTestConventions(t, alertTestConventionsYAML)
	githubLoader := func(context.Context, string) (workmgmt.Conventions, error) { return ghConv, nil }
	noop := func(*testing.T, *alertFixture) {}
	inFlightKey := alerttrigger.Key{SourceID: alertTestSource, Repo: alertTestRepo, Fingerprint: "fp-transient"}
	var inFlightToken uuid.UUID
	modes := []mode{
		{name: "payload refusal", status: http.StatusBadRequest, code: "validation_failed",
			body: func(*testing.T) []byte {
				return []byte(`{"fingerprint":"has space","title":"t","severity":"high"}`)
			},
			arm: noop, heal: noop,
			// The body is still invalid, so the resend gets the branch's own
			// answer again — not 401 alert_replayed.
			retryStatus: http.StatusBadRequest, retryCode: "validation_failed"},
		{name: "conventions load", status: http.StatusInternalServerError, code: "internal_error",
			arm: func(t *testing.T, _ *alertFixture) {
				setAlertConventionsLoader(t, func(context.Context, string) (workmgmt.Conventions, error) {
					return workmgmt.Conventions{}, errors.New("forge read failed")
				})
			},
			heal:        func(*testing.T, *alertFixture) { conventionsLoader = githubLoader },
			retryStatus: http.StatusCreated},
		{name: "repo scope", status: http.StatusBadGateway, code: "work_item_filing_failed",
			arm: func(_ *testing.T, f *alertFixture) {
				f.gh.set(func(g *alertFakeGitHub) { g.installStatus = http.StatusInternalServerError })
			},
			heal: func(_ *testing.T, f *alertFixture) {
				f.gh.set(func(g *alertFakeGitHub) { g.installStatus = http.StatusOK })
			},
			retryStatus: http.StatusCreated},
		{name: "provider unimplemented", status: http.StatusNotImplemented, code: "provider_unimplemented",
			arm: func(t *testing.T, _ *alertFixture) {
				gl := parseAlertTestConventions(t, alertTestGitLabConventionsYAML)
				setAlertConventionsLoader(t, func(context.Context, string) (workmgmt.Conventions, error) { return gl, nil })
			},
			heal:        func(*testing.T, *alertFixture) { conventionsLoader = githubLoader },
			retryStatus: http.StatusCreated},
		{name: "claim error", status: http.StatusInternalServerError, code: "internal_error",
			arm: func(_ *testing.T, f *alertFixture) {
				f.s.cfg.AlertIncidents = &alertFlakyIncidents{Store: f.incidents, claimErrNext: 1}
			},
			heal: noop, retryStatus: http.StatusCreated},
		{name: "unknown claim kind", status: http.StatusInternalServerError, code: "internal_error",
			arm: func(_ *testing.T, f *alertFixture) {
				f.s.cfg.AlertIncidents = &alertFlakyIncidents{Store: f.incidents, claimKind: "bogus"}
			},
			heal:        func(_ *testing.T, f *alertFixture) { f.s.cfg.AlertIncidents = f.incidents },
			retryStatus: http.StatusCreated},
		{name: "filing error", status: http.StatusBadGateway, code: "work_item_filing_failed",
			arm:  func(_ *testing.T, f *alertFixture) { f.provider.failNext = 1 },
			heal: noop, retryStatus: http.StatusCreated},
		{name: "occurrence comment failure", status: http.StatusBadGateway, code: "alert_occurrence_failed",
			// File the incident with an independently signed alert first, so
			// the request under test takes the existing-issue branch.
			seed: func(t *testing.T, f *alertFixture) {
				first := signAlert(alertBody(t, "fp-transient"), time.Now().Add(-2*time.Second))
				wantAlertStatus(t, f.send(t, first), http.StatusCreated, "")
			},
			arm:         func(_ *testing.T, f *alertFixture) { f.gh.set(func(g *alertFakeGitHub) { g.commentFailNext = 1 }) },
			heal:        noop,
			retryStatus: http.StatusOK},
		{name: "in_flight", status: http.StatusAccepted,
			// Another filer holds a live claim on the fingerprint.
			seed: func(t *testing.T, f *alertFixture) {
				c, err := f.incidents.Claim(context.Background(), inFlightKey, time.Hour)
				if err != nil || c.Kind != alerttrigger.ClaimNew {
					t.Fatalf("seed claim = %+v, %v; want new", c, err)
				}
				inFlightToken = c.Token
			},
			arm: noop,
			// The competing filer completes; the resend lands as an occurrence.
			heal: func(t *testing.T, f *alertFixture) {
				if err := f.incidents.Complete(context.Background(), inFlightKey, inFlightToken, 950,
					"https://github.com/"+alertTestRepo+"/issues/950"); err != nil {
					t.Fatalf("complete the competing claim: %v", err)
				}
			},
			retryStatus: http.StatusOK},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			f := newAlertFixture(t, "")
			if m.seed != nil {
				m.seed(t, f)
			}
			body := alertBody(t, "fp-transient")
			if m.body != nil {
				body = m.body(t)
			}
			a := signAlert(body, time.Now())
			filings, comments, audits := f.provider.count(), f.gh.commentCount(), len(f.audit.globalAppended)
			unmarks := f.deliveries.unmarkCount()

			m.arm(t, f)
			wantAlertStatus(t, f.send(t, a), m.status, m.code)
			if f.provider.count() != filings || f.gh.commentCount() != comments {
				t.Fatalf("the non-durable request filed %d / commented %d",
					f.provider.count()-filings, f.gh.commentCount()-comments)
			}
			if n := len(f.audit.globalAppended); n != audits {
				t.Errorf("the non-durable request wrote %d audit rows, want 0", n-audits)
			}
			if f.deliveries.unmarkCount() != unmarks+1 {
				t.Errorf("unmarks = %d, want %d (the branch releases the nonce)", f.deliveries.unmarkCount(), unmarks+1)
			}

			m.heal(t, f)
			retry := f.send(t, a)
			wantAlertStatus(t, retry, m.retryStatus, m.retryCode)
		})
	}
}

// gitlabNamedProvider records filings under the "gitlab" provider id, so a
// test can prove the ingress never dispatches to a non-github provider.
type gitlabNamedProvider struct{ alertWorkProvider }

func (*gitlabNamedProvider) Name() string { return "gitlab" }

// TestAlertTrigger_NonGitHubProviderNeverFiles isolates the provider check: a
// repository whose conventions resolve a non-github provider is refused 501
// provider_unimplemented and NOTHING is dispatched — even with a provider
// registered under that id, which the filing core would otherwise use.
func TestAlertTrigger_NonGitHubProviderNeverFiles(t *testing.T) {
	f := newAlertFixture(t, "")
	gl := &gitlabNamedProvider{}
	workmgmt.Register(gl)
	conv := parseAlertTestConventions(t, alertTestGitLabConventionsYAML)
	setAlertConventionsLoader(t, func(context.Context, string) (workmgmt.Conventions, error) { return conv, nil })

	rec := f.send(t, signAlert(alertBody(t, "fp-gitlab"), time.Now()))
	wantAlertStatus(t, rec, http.StatusNotImplemented, "provider_unimplemented")
	if gl.count() != 0 || f.provider.count() != 0 {
		t.Fatalf("filings gitlab=%d github=%d, want 0 and 0", gl.count(), f.provider.count())
	}
}

// TestAlertTrigger_InFlight202: a live claim held by another filer → 202
// in_flight, nothing filed or commented, the occurrence counted, and the
// nonce RELEASED: an exact resend is answered in_flight again (counted once
// more) rather than refused alert_replayed, so the sender's retry can land
// once the claim resolves (TestAlertTrigger_PostMarkFailureUnmarks/in_flight).
func TestAlertTrigger_InFlight202(t *testing.T) {
	f := newAlertFixture(t, "")
	key := alerttrigger.Key{SourceID: alertTestSource, Repo: alertTestRepo, Fingerprint: "fp-inflight"}
	if c, err := f.incidents.Claim(context.Background(), key, time.Hour); err != nil || c.Kind != alerttrigger.ClaimNew {
		t.Fatalf("seed claim = %+v, %v; want new", c, err)
	}

	a := signAlert(alertBody(t, "fp-inflight"), time.Now())
	rec := f.send(t, a)
	wantAlertStatus(t, rec, http.StatusAccepted, "")
	if got := decodeAlertResult(t, rec); got.Action != alertActionInFlight || got.Occurrences != 2 {
		t.Fatalf("result = %+v, want in_flight occurrences 2", got)
	}
	if f.provider.count() != 0 || f.gh.commentCount() != 0 {
		t.Errorf("filings = %d, comments = %d; want 0 and 0", f.provider.count(), f.gh.commentCount())
	}
	if n := len(f.audit.globalAppended); n != 0 {
		t.Errorf("an in_flight answer wrote %d audit rows, want 0", n)
	}
	resend := f.send(t, a)
	wantAlertStatus(t, resend, http.StatusAccepted, "")
	if got := decodeAlertResult(t, resend); got.Occurrences != 3 {
		t.Fatalf("resend = %+v, want in_flight occurrences 3", got)
	}
}

// TestAlertTrigger_MarkStoreError500: a delivery store that fails to record the
// nonce (not a duplicate) answers 500 internal_error, and nothing is filed or
// released (there is no nonce to release).
func TestAlertTrigger_MarkStoreError500(t *testing.T) {
	f := newAlertFixture(t, "")
	f.deliveries.markErr = errors.New("db unavailable")
	wantAlertStatus(t, f.send(t, signAlert(alertBody(t, "fp-markerr"), time.Now())), http.StatusInternalServerError, "internal_error")
	if f.provider.count() != 0 || f.deliveries.unmarkCount() != 0 {
		t.Fatalf("filings = %d, unmarks = %d; want 0 and 0", f.provider.count(), f.deliveries.unmarkCount())
	}
}

// TestAlertTrigger_UnmarkFailureKeepsOriginalAnswer: the release is
// best-effort — an Unmark failure is logged and never masks the branch's own
// answer.
func TestAlertTrigger_UnmarkFailureKeepsOriginalAnswer(t *testing.T) {
	f := newAlertFixture(t, "")
	f.deliveries.unmarkErr = errors.New("db unavailable")
	f.provider.failNext = 1
	wantAlertStatus(t, f.send(t, signAlert(alertBody(t, "fp-unmarkerr"), time.Now())), http.StatusBadGateway, "work_item_filing_failed")
	if f.deliveries.unmarkCount() != 1 {
		t.Fatalf("unmarks = %d, want 1 (attempted)", f.deliveries.unmarkCount())
	}
}

// TestAlertTrigger_CompleteClaimLostReported: Complete answering ErrClaimLost
// still files (201) and reports dedup_claim_lost in the response and the audit
// row; any other Complete error files without the flag.
func TestAlertTrigger_CompleteClaimLostReported(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantLost bool
	}{
		{"claim lost", alerttrigger.ErrClaimLost, true},
		{"db error", errors.New("db unavailable"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAlertFixture(t, "")
			f.s.cfg.AlertIncidents = &alertFlakyIncidents{Store: f.incidents, completeErr: tc.err}
			rec := f.send(t, signAlert(alertBody(t, "fp-lost"), time.Now()))
			wantAlertStatus(t, rec, http.StatusCreated, "")
			if got := decodeAlertResult(t, rec); got.DedupClaimLost != tc.wantLost {
				t.Errorf("dedup_claim_lost = %v, want %v", got.DedupClaimLost, tc.wantLost)
			}
			if _, ok := f.auditPayload(t, categoryAlertIncidentFiled)["dedup_claim_lost"]; ok != tc.wantLost {
				t.Errorf("audit dedup_claim_lost present = %v, want %v", ok, tc.wantLost)
			}
		})
	}
}

// TestAlertTrigger_AutoStartOffByDefault is the done-means test for the
// shipped default: the source OMITS auto_start while a WORKING spec source and
// RunRepo are wired (so an auto-start WOULD mint a run if it fired). No run is
// started, the spec is never fetched, the response reports disabled, and the
// filed body carries the next-step line.
func TestAlertTrigger_AutoStartOffByDefault(t *testing.T) {
	f := newAlertFixture(t, "")
	rec := f.send(t, signAlert(alertBody(t, "fp-default"), time.Now()))
	wantAlertStatus(t, rec, http.StatusCreated, "")
	got := decodeAlertResult(t, rec)
	if got.AutoStart == nil || got.AutoStart.Enabled || got.AutoStart.Outcome != alertAutoStartDisabled {
		t.Fatalf("auto_start = %+v, want {enabled:false outcome:disabled}", got.AutoStart)
	}
	if n := runRowCount(f.runs); n != 0 {
		t.Fatalf("auto_start omitted, yet %d runs were started", n)
	}
	if f.specs.calls != 0 {
		t.Errorf("spec fetched %d times with auto_start off, want 0", f.specs.calls)
	}
	if body := f.provider.last().Item.Body; !strings.Contains(body, "Next step: start a `hotfix_change` run") {
		t.Errorf("filed body lacks the next-step line:\n%s", body)
	}
}

// TestAlertTrigger_AutoStartOutcomes: with auto_start on, every failure is an
// outcome on a 201 — never a failed filing.
func TestAlertTrigger_AutoStartOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		arm     func(f *alertFixture)
		outcome string
		code    string
		runs    int
	}{
		{"started (ledger record fails on the fake run id, logged only)", func(*alertFixture) {}, alertAutoStartStarted, "", 1},
		{"no spec source", func(f *alertFixture) { f.s.cfg.AlertSpecSource = nil }, alertAutoStartError, "alert_spec_source_unconfigured", 0},
		{"spec fetch fails", func(f *alertFixture) { f.specs.err = errors.New("404") }, alertAutoStartError, "spec_fetch_failed", 0},
		{"workflow not in spec", func(f *alertFixture) { f.specs.spec = []byte(scheduledSpec("diff")) }, alertAutoStartRefused, "", 0},
		{"start 5xx", func(f *alertFixture) { f.runs.createErr = errors.New("disk full") }, alertAutoStartError, "start_failed", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAlertFixture(t, "    auto_start: true\n")
			tc.arm(f)
			rec := f.send(t, signAlert(alertBody(t, "fp-auto"), time.Now()))
			wantAlertStatus(t, rec, http.StatusCreated, "")
			as := decodeAlertResult(t, rec).AutoStart
			if as == nil || !as.Enabled || as.Outcome != tc.outcome {
				t.Fatalf("auto_start = %+v, want enabled outcome %s", as, tc.outcome)
			}
			if tc.code != "" && as.Code != tc.code {
				t.Errorf("auto_start.code = %q, want %q", as.Code, tc.code)
			}
			if tc.outcome == alertAutoStartRefused && as.Code == "" {
				t.Error("a refused auto-start carries no code")
			}
			if tc.outcome == alertAutoStartStarted && as.RunID == "" {
				t.Error("a started auto-start carries no run_id")
			}
			if n := runRowCount(f.runs); n != tc.runs {
				t.Errorf("runs = %d, want %d", n, tc.runs)
			}
			if f.provider.count() != 1 {
				t.Errorf("filings = %d, want 1 (auto-start never fails the filing)", f.provider.count())
			}
			if as2 := f.auditPayload(t, categoryAlertIncidentFiled)["auto_start"].(map[string]any); as2["outcome"] != tc.outcome {
				t.Errorf("audit auto_start = %v, want outcome %s", as2, tc.outcome)
			}
		})
	}
}

// TestAlertTrigger_AutoStartReplayIsAlreadyStarted: the auto-start's
// Idempotency-Key is derived from the issue and the incident digest, so a
// second filing that resolves to the SAME issue (the ledger row lost, the
// forge answering the idempotent replay with one issue number) replays the
// existing run as already_started instead of minting a second one.
func TestAlertTrigger_AutoStartReplayIsAlreadyStarted(t *testing.T) {
	f := newAlertFixture(t, "    auto_start: true\n")
	f.provider.fixedNumber = 950
	now := time.Now()
	first := f.send(t, signAlert(alertBody(t, "fp-replay"), now))
	wantAlertStatus(t, first, http.StatusCreated, "")
	started := decodeAlertResult(t, first).AutoStart
	if started == nil || started.Outcome != alertAutoStartStarted || started.RunID == "" {
		t.Fatalf("first auto_start = %+v, want started with a run id", started)
	}

	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM alert_incidents WHERE fingerprint = 'fp-replay'`); err != nil {
		t.Fatalf("drop the ledger row: %v", err)
	}
	second := f.send(t, signAlert(alertBody(t, "fp-replay"), now.Add(-time.Second)))
	wantAlertStatus(t, second, http.StatusCreated, "")
	replayed := decodeAlertResult(t, second).AutoStart
	if replayed == nil || replayed.Outcome != alertAutoStartAlreadyStarted || replayed.RunID != started.RunID {
		t.Fatalf("second auto_start = %+v, want already_started for run %s", replayed, started.RunID)
	}
	if n := runRowCount(f.runs); n != 1 {
		t.Fatalf("runs = %d, want 1 (the replay mints nothing)", n)
	}
}

// TestAlertTrigger_EndToEnd_AutoStartPersistsAlertRun is the CROSS-LAYER test:
// real pgtest RunRepo and dedup ledger on one database. A signed alert with
// auto_start on files the issue, StartAlertRun drives handleCreateRun, and the
// PERSISTED runs row reads back trigger_source 'alert' / trigger_ref
// 'issue:<filed number>' (through migration 0100's widened CHECK), with
// alert_incidents.run_id pointing at it.
func TestAlertTrigger_EndToEnd_AutoStartPersistsAlertRun(t *testing.T) {
	f := newAlertFixture(t, "    auto_start: true\n    runner_kind: local\n")
	runRepo := run.NewPostgresRepository(f.pool)
	f.s.cfg.RunRepo = runRepo

	rec := f.send(t, signAlert(alertBody(t, "fp-e2e"), time.Now()))
	wantAlertStatus(t, rec, http.StatusCreated, "")
	res := decodeAlertResult(t, rec)
	if res.AutoStart == nil || res.AutoStart.Outcome != alertAutoStartStarted {
		t.Fatalf("auto_start = %+v, want started", res.AutoStart)
	}

	var source, ref string
	err := f.pool.QueryRow(context.Background(),
		`SELECT trigger_source, trigger_ref FROM runs WHERE id = $1`, res.AutoStart.RunID).Scan(&source, &ref)
	if err != nil {
		t.Fatalf("read back run %s: %v", res.AutoStart.RunID, err)
	}
	if source != string(run.TriggerAlert) || ref != "issue:"+strconv.Itoa(res.IssueNumber) {
		t.Fatalf("persisted run trigger = (%q, %q), want (alert, issue:%d)", source, ref, res.IssueNumber)
	}
	_, issue, runID := f.incidentRow(t, "fp-e2e")
	if issue == nil || *issue != res.IssueNumber || runID == nil || runID.String() != res.AutoStart.RunID {
		t.Fatalf("ledger issue=%v run_id=%v, want %d and %s", issue, runID, res.IssueNumber, res.AutoStart.RunID)
	}
}
