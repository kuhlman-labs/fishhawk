package pushnotify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

// Webhook sink environment variables.
const (
	EnvWebhookURL    = "FISHHAWKD_NOTIFY_WEBHOOK_URL"
	EnvWebhookSecret = "FISHHAWKD_NOTIFY_WEBHOOK_SECRET"
)

// Outbound webhook headers.
const (
	HeaderEvent     = "X-Fishhawk-Event"
	HeaderDelivery  = "X-Fishhawk-Delivery"
	HeaderSignature = "X-Fishhawk-Signature-256"
	// SignaturePrefix mirrors the inbound verifier in internal/webhook.
	SignaturePrefix = "sha256="
)

// DefaultHTTPTimeout bounds every HTTP sink request independently of the
// dispatcher's per-sink context timeout.
const DefaultHTTPTimeout = 10 * time.Second

// WebhookKind and SlackKind are the registered sink kinds.
const (
	WebhookKind = "webhook"
	SlackKind   = "slack"
)

func init() {
	Register(WebhookKind, webhookFromEnv)
}

func webhookFromEnv(getenv func(string) string) (Sink, error) {
	rawURL := strings.TrimSpace(getenv(EnvWebhookURL))
	secret := strings.TrimSpace(getenv(EnvWebhookSecret))
	switch {
	case rawURL == "" && secret == "":
		return nil, nil
	case rawURL == "":
		return nil, fmt.Errorf("%s: required when %s is set", EnvWebhookURL, EnvWebhookSecret)
	case secret == "":
		return nil, fmt.Errorf("%s: required when %s is set (the webhook sink sends only HMAC-signed payloads; there is no unsigned mode)",
			EnvWebhookSecret, EnvWebhookURL)
	}
	return NewWebhookSink(WebhookConfig{URL: rawURL, Secret: []byte(secret)}, EnvWebhookURL)
}

// WebhookConfig configures the generic outbound webhook sink.
type WebhookConfig struct {
	URL    string
	Secret []byte
	// Client overrides the HTTP client (tests). Its Timeout and redirect
	// policy are enforced by NewWebhookSink regardless.
	Client *http.Client
}

// WebhookSink POSTs the Event JSON, HMAC-SHA256-signed over the exact body.
type WebhookSink struct {
	target *url.URL
	secret []byte
	client *http.Client
}

// NewWebhookSink validates cfg. envVar is the variable NAME used in a
// configuration error; the URL value is never echoed.
func NewWebhookSink(cfg WebhookConfig, envVar string) (*WebhookSink, error) {
	target, err := validateSinkURL(envVar, cfg.URL)
	if err != nil {
		return nil, err
	}
	if len(cfg.Secret) == 0 {
		return nil, fmt.Errorf("%s: required (the webhook sink sends only HMAC-signed payloads)", EnvWebhookSecret)
	}
	return &WebhookSink{target: target, secret: append([]byte(nil), cfg.Secret...), client: boundedClient(cfg.Client)}, nil
}

// Name implements Sink.
func (*WebhookSink) Name() string { return WebhookKind }

// DestinationHost implements Destination.
func (s *WebhookSink) DestinationHost() string { return s.target.Hostname() }

// Deliver implements Sink.
func (s *WebhookSink) Deliver(ctx context.Context, e Event) error {
	body, err := MarshalEvent(e)
	if err != nil {
		return SanitizeTransportError(WebhookKind, AtHost(s.target.Hostname(), err))
	}
	h := http.Header{}
	h.Set(HeaderEvent, e.Event)
	h.Set(HeaderDelivery, DeliveryID(e))
	h.Set(HeaderSignature, Sign(s.secret, body))
	return postJSON(ctx, s.client, WebhookKind, s.target, body, h)
}

// DeliveryID is the X-Fishhawk-Delivery value: `<run_id>:<source_sequence>`.
func DeliveryID(e Event) string {
	return e.RunID + ":" + strconv.FormatInt(e.SourceSequence, 10)
}

// Sign returns `sha256=<lowercase hex HMAC-SHA256(secret, body)>` — the same
// construction internal/webhook verifies for inbound deliveries.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// validateSinkURL parses raw and requires an absolute https URL, or plain
// http to a loopback host. Every error names envVar and the problem, NEVER
// the value: a Slack incoming-webhook URL is itself the credential.
func validateSinkURL(envVar, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid URL", envVar)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%s: must be an absolute https URL", envVar)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return u, nil
		}
		return nil, fmt.Errorf("%s: must be https (plain http is accepted only for a loopback host)", envVar)
	default:
		return nil, fmt.Errorf("%s: must be an absolute https URL", envVar)
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// boundedClient returns an http.Client with a Timeout (so a sink bounds its
// own I/O even if the dispatcher abandons it) that never follows redirects
// (a redirect must not carry a signed body to an unconfigured host).
func boundedClient(c *http.Client) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	if out.Timeout <= 0 {
		out.Timeout = DefaultHTTPTimeout
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return out
}

// postJSON is the shared HTTP transport for the webhook and Slack sinks.
// Every error it returns has been through SanitizeTransportError; a non-2xx
// response is reported by status code only and its body is never read into
// an error.
func postJSON(ctx context.Context, client *http.Client, sinkName string, target *url.URL, body []byte, extra http.Header) error {
	host := target.Hostname()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return SanitizeTransportError(sinkName, AtHost(host, errors.New("request construction failed")))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fishhawk/"+version.Version)
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return SanitizeTransportError(sinkName, AtHost(host, err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return SanitizeTransportError(sinkName, AtHost(host, &HTTPStatusError{StatusCode: resp.StatusCode}))
	}
	return nil
}
