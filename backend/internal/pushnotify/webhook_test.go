package pushnotify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

type capturedRequest struct {
	header http.Header
	body   []byte
}

type captureServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
}

func newCaptureServer(t *testing.T, status int, respBody string) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.reqs = append(cs.reqs, capturedRequest{header: r.Header.Clone(), body: b})
		cs.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *captureServer) only(t *testing.T) capturedRequest {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(cs.reqs))
	}
	return cs.reqs[0]
}

func newWebhook(t *testing.T, rawURL string, secret string) *WebhookSink {
	t.Helper()
	s, err := NewWebhookSink(WebhookConfig{URL: rawURL, Secret: []byte(secret)}, EnvWebhookURL)
	if err != nil {
		t.Fatalf("NewWebhookSink: %v", err)
	}
	return s
}

// fixtureEvent's source_sequence (90417) is numerically distinct from every
// other number in the Event, so a wrong field cannot coincidentally match.
func fixtureEvent() Event {
	return Event{
		SchemaVersion:  SchemaVersion,
		Event:          "plan_awaiting_approval",
		SourceSequence: 90417,
		RunID:          "4d50291a-8e55-48a7-a5c6-c39a060f7dac",
		RunShortID:     "4d50291a",
		Repo:           "kuhlman-labs/fishhawk",
		Issue:          &Issue{Number: 2292, URL: "https://github.com/kuhlman-labs/fishhawk/issues/2292"},
		WorkflowID:     "feature_change",
		Stage:          &Stage{Type: "plan", State: "awaiting_approval"},
		Decision:       "Plan ready for approval",
		Verdicts:       []Verdict{{ReviewerModel: "claude-opus-5-5", Verdict: "approve"}, {ReviewerModel: "gpt-6", Verdict: "reject"}},
		GateLatency:    GateLatency{TotalWaitOnHumanSeconds: 480, Gates: []GateWait{{Gate: "plan_approval", WaitSeconds: 480}}},
		Links:          Links{Run: "https://fishhawk.example.com/runs/4d50291a", Issue: "https://github.com/kuhlman-labs/fishhawk/issues/2292"},
		OccurredAt:     time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
	}
}

func mac(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// W1: the received signature equals HMAC-SHA256 over the EXACT received body.
func TestWebhookSink_SignsBodyWithConfiguredSecret(t *testing.T) {
	srv := newCaptureServer(t, http.StatusNoContent, "")
	s := newWebhook(t, srv.URL+"/hook", "fixture-secret")
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	req := srv.only(t)
	if got, want := req.header.Get(HeaderSignature), mac("fixture-secret", req.body); got != want {
		t.Fatalf("%s = %q; want %q (HMAC over the received body)", HeaderSignature, got, want)
	}
	want, _ := MarshalEvent(fixtureEvent())
	if string(req.body) != string(want) {
		t.Fatalf("body = %s; want MarshalEvent output %s", req.body, want)
	}
	if ct := req.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if ua := req.header.Get("User-Agent"); ua != "fishhawk/"+version.Version {
		t.Fatalf("User-Agent = %q", ua)
	}
	if e := req.header.Get(HeaderEvent); e != "plan_awaiting_approval" {
		t.Fatalf("%s = %q", HeaderEvent, e)
	}
}

// W1b: ONE byte-identical body signed under secret A does not verify under
// secret B — the rejection is attributable to the key alone.
func TestWebhookSink_SignatureIsKeyBound(t *testing.T) {
	body, _ := MarshalEvent(fixtureEvent())
	sigA := Sign([]byte("secret-A"), body)
	if sigA != mac("secret-A", body) {
		t.Fatalf("Sign(A) = %q; not HMAC-SHA256 under A", sigA)
	}
	if hmac.Equal([]byte(sigA), []byte(mac("secret-B", body))) {
		t.Fatal("a signature under secret A verified under secret B")
	}
	if !strings.HasPrefix(sigA, "sha256=") || strings.ToLower(sigA) != sigA {
		t.Fatalf("signature %q is not sha256=<lowercase hex>", sigA)
	}
}

// W2: X-Fishhawk-Delivery is `<run_id>:<source_sequence>`.
func TestWebhookSink_DeliveryHeaderCarriesSourceSequence(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, "")
	s := newWebhook(t, srv.URL, "k")
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatal(err)
	}
	want := "4d50291a-8e55-48a7-a5c6-c39a060f7dac:90417"
	if got := srv.only(t).header.Get(HeaderDelivery); got != want {
		t.Fatalf("%s = %q; want %q", HeaderDelivery, got, want)
	}
}

// W3: a URL without a secret is half-configured: a NAMED error, no sink. The
// two arms differ ONLY in whether the secret var is set.
func TestSinksFromEnv_WebhookURLWithoutSecretFailsClosed(t *testing.T) {
	env := map[string]string{EnvWebhookURL: "https://hooks.example.com/" + canary}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	// The "required when <URL var> is set" phrase is produced ONLY by the
	// env-level half-configuration guard, isolating it from NewWebhookSink's
	// own empty-secret check.
	if err == nil || !strings.Contains(err.Error(), EnvWebhookSecret+": required when "+EnvWebhookURL+" is set") {
		t.Fatalf("err = %v; want a named %s half-configuration error", err, EnvWebhookSecret)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("config error echoes the URL value: %v", err)
	}
	if len(sinks) != 0 {
		t.Fatalf("sinks = %v; want none", sinks)
	}

	env[EnvWebhookSecret] = "s3cret"
	sinks, err = SinksFromEnv(func(k string) string { return env[k] })
	if err != nil || len(sinks) != 1 || sinks[0].Name() != WebhookKind {
		t.Fatalf("with secret: sinks=%v err=%v; want one webhook sink", sinks, err)
	}
}

func TestSinksFromEnv_WebhookSecretWithoutURLFailsClosed(t *testing.T) {
	env := map[string]string{EnvWebhookSecret: canary}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), EnvWebhookURL+": required when "+EnvWebhookSecret+" is set") || strings.Contains(err.Error(), canary) {
		t.Fatalf("err = %v; want a named %s error that does not echo the secret", err, EnvWebhookURL)
	}
	if len(sinks) != 0 {
		t.Fatalf("sinks = %v; want none", sinks)
	}
}

// W4: a non-https REMOTE URL is refused; plain http to a REACHABLE loopback
// server is accepted and delivers. The arms differ only in host, so the
// refusal cannot be attributed to unreachability.
func TestWebhookConfig_RefusesNonHTTPSRemoteURL(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, "")
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	_, err := NewWebhookSink(WebhookConfig{URL: "http://example.invalid:" + port + "/hook/" + canary, Secret: []byte("k")}, EnvWebhookURL)
	if err == nil || !strings.Contains(err.Error(), EnvWebhookURL) || !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("remote http: err = %v; want a named must-be-https error", err)
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("config error echoes the URL value: %v", err)
	}

	s, err := NewWebhookSink(WebhookConfig{URL: "http://127.0.0.1:" + port + "/hook", Secret: []byte("k")}, EnvWebhookURL)
	if err != nil {
		t.Fatalf("loopback http refused: %v", err)
	}
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("loopback deliver: %v", err)
	}
	srv.only(t)
}

func TestValidateSinkURL_ConfigErrorsNeverEchoTheValue(t *testing.T) {
	for _, raw := range []string{
		"http://user:" + canary + "@example.com/" + canary + "?t=" + canary, // remote http
		"ftp://" + canary + ".example.com/x",                                // bad scheme
		"://" + canary,                                                      // unparseable
		"/relative/" + canary,                                               // no host
		"https://:" + canary + "@/x",                                        // empty host
	} {
		_, err := validateSinkURL(EnvSlackWebhookURL, raw)
		if err == nil {
			t.Fatalf("%q accepted", raw)
		}
		if strings.Contains(err.Error(), canary) || !strings.HasPrefix(err.Error(), EnvSlackWebhookURL+": ") {
			t.Fatalf("config error for %q = %q; want the env var name and no value", raw, err)
		}
	}
	for _, ok := range []string{"https://hooks.example.com/x", "http://localhost:9/x", "http://[::1]:9/x"} {
		if _, err := validateSinkURL(EnvWebhookURL, ok); err != nil {
			t.Fatalf("%q refused: %v", ok, err)
		}
	}
}

// A non-2xx response is an error naming the status and host only; the
// response body is never echoed.
func TestWebhookSink_Non2xxReportsStatusNotBody(t *testing.T) {
	srv := newCaptureServer(t, http.StatusBadGateway, "upstream said "+canary)
	s := newWebhook(t, srv.URL, "k")
	err := s.Deliver(context.Background(), fixtureEvent())
	var de *DeliveryError
	if !errors.As(err, &de) || de.Reason != "http status 502" || de.Host != "127.0.0.1" || de.Sink != WebhookKind {
		t.Fatalf("err = %#v; want webhook/127.0.0.1/http status 502", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("response body echoed: %v", err)
	}
}

// A redirect is not followed: a signed body must not reach an unconfigured
// host.
func TestWebhookSink_DoesNotFollowRedirects(t *testing.T) {
	target := newCaptureServer(t, http.StatusOK, "")
	redir := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	t.Cleanup(redir.Close)
	s := newWebhook(t, redir.URL, "k")
	err := s.Deliver(context.Background(), fixtureEvent())
	var de *DeliveryError
	if !errors.As(err, &de) || de.Reason != "http status 307" {
		t.Fatalf("err = %v; want http status 307", err)
	}
	target.mu.Lock()
	n := len(target.reqs)
	target.mu.Unlock()
	if n != 0 {
		t.Fatalf("redirect followed: target received %d requests", n)
	}
}

func TestWebhookSink_ClientTimeoutAlwaysSet(t *testing.T) {
	s, err := NewWebhookSink(WebhookConfig{URL: "https://h.example.com", Secret: []byte("k"), Client: &http.Client{}}, EnvWebhookURL)
	if err != nil {
		t.Fatal(err)
	}
	if s.client.Timeout != DefaultHTTPTimeout {
		t.Fatalf("client timeout = %v; want %v", s.client.Timeout, DefaultHTTPTimeout)
	}
	if _, err := NewWebhookSink(WebhookConfig{URL: "https://h.example.com"}, EnvWebhookURL); err == nil || !strings.Contains(err.Error(), EnvWebhookSecret) {
		t.Fatalf("empty secret: err = %v; want a named %s error", err, EnvWebhookSecret)
	}
}

// A TLS failure against a real server classifies as tls, host only.
func TestWebhookSink_TLSFailureClassified(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	s := newWebhook(t, srv.URL+"/"+canary, "k")
	err := s.Deliver(context.Background(), fixtureEvent())
	var de *DeliveryError
	if !errors.As(err, &de) || de.Reason != ReasonTLS {
		t.Fatalf("err = %v; want a tls DeliveryError", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("tls error leaks the path: %v", err)
	}
}

// L2: the generic webhook sink's transport error leaks neither the URL
// (canary in userinfo, path and query) nor the secret, via the returned
// error, the dispatcher's WARN log, or the outcome that feeds the
// push_notification_failed row.
func TestWebhookSink_TransportErrorLeaksNoURL(t *testing.T) {
	secret := "SECRETCANARY0002"
	leaky := "http://user:" + canary + "@127.0.0.1:1/hook/" + canary + "?token=" + canary
	s := newWebhook(t, leaky, secret)

	err := s.Deliver(context.Background(), fixtureEvent())
	if err == nil {
		t.Fatal("Deliver to a refused port succeeded")
	}
	var de *DeliveryError
	if !errors.As(err, &de) || de.Reason != ReasonConnectionRefused || de.Host != "127.0.0.1" {
		t.Fatalf("err = %#v; want connection refused at 127.0.0.1", err)
	}

	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{s}, Options{OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)
	d.Enqueue(fixtureEvent())
	drain(t, d)
	outs := rec.all()
	if len(outs) != 1 || outs[0].Err == nil {
		t.Fatalf("outcomes = %+v; want one failure", outs)
	}
	for where, text := range map[string]string{"error": err.Error(), "log": logs.String(), "outcome": outs[0].Err.Error()} {
		if strings.Contains(text, canary) || strings.Contains(text, secret) {
			t.Fatalf("%s leaks the URL or secret: %s", where, text)
		}
		if !strings.Contains(text, WebhookKind) {
			t.Fatalf("%s does not name the sink (vacuous assertion): %s", where, text)
		}
	}
}
