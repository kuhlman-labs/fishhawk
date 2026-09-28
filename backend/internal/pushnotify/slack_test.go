package pushnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// slackGolden is the SHIPPED Slack rendering of fixtureEvent: the AC-1
// decision context (decision, repo#issue, wait, link) plus verdicts. A no-op
// touch of the renderer cannot satisfy it.
const slackGolden = `{
  "text": "Plan ready for approval on kuhlman-labs/fishhawk#2292 - waiting 8m - https://fishhawk.example.com/runs/4d50291a",
  "blocks": [
    {"type": "section", "text": {"type": "mrkdwn", "text": "*Plan ready for approval*\nkuhlman-labs/fishhawk#2292"}},
    {"type": "context", "elements": [{"type": "mrkdwn", "text": "stage plan (awaiting_approval) · workflow feature_change · event plan_awaiting_approval"}]},
    {"type": "section", "text": {"type": "mrkdwn", "text": "*Reviews*\n• claude-opus-5-5: approve\n• gpt-6: reject"}},
    {"type": "context", "elements": [{"type": "mrkdwn", "text": "Waiting on a human for 8m (plan_approval 8m)"}]},
    {"type": "section", "text": {"type": "mrkdwn", "text": "<https://fishhawk.example.com/runs/4d50291a|Open run> · <https://github.com/kuhlman-labs/fishhawk/issues/2292|Issue>"}}
  ]
}`

// slackGoldenNoIssue is the rendering for a CLI-triggered run: no issue, no
// verdicts, no wait, no links.
const slackGoldenNoIssue = `{
  "text": "Scope amendment requested on kuhlman-labs/fishhawk run 0badcafe",
  "blocks": [
    {"type": "section", "text": {"type": "mrkdwn", "text": "*Scope amendment requested*\nkuhlman-labs/fishhawk run 0badcafe"}},
    {"type": "context", "elements": [{"type": "mrkdwn", "text": "event scope_amendment"}]}
  ]
}`

func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("golden is not JSON: %v", err)
	}
	return b.String()
}

func TestSlackMessageGolden(t *testing.T) {
	if got, want := string(RenderSlackMessage(fixtureEvent())), compact(t, slackGolden); got != want {
		t.Fatalf("Slack rendering drifted from the golden.\n got: %s\nwant: %s", got, want)
	}
	cli := Event{Event: "scope_amendment", RunID: "0badcafe-0000", RunShortID: "0badcafe", Repo: "kuhlman-labs/fishhawk", Decision: "Scope amendment requested"}
	if got, want := string(RenderSlackMessage(cli)), compact(t, slackGoldenNoIssue); got != want {
		t.Fatalf("CLI-run Slack rendering drifted.\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderSlackMessage_EscapesMrkdwnControlCharacters(t *testing.T) {
	e := fixtureEvent()
	e.Decision = "Approve <!channel> & <https://evil.example|click>"
	out := string(RenderSlackMessage(e))
	var msg slackMessage
	if err := json.Unmarshal([]byte(out), &msg); err != nil {
		t.Fatal(err)
	}
	head := msg.Blocks[0].Text.Text
	if strings.Contains(head, "<!channel>") || strings.Contains(head, "<https://evil") {
		t.Fatalf("mrkdwn control characters not escaped: %s", head)
	}
	if !strings.Contains(head, "&lt;!channel&gt; &amp; ") {
		t.Fatalf("escaped form missing: %s", head)
	}
}

func TestFormatWait(t *testing.T) {
	for in, want := range map[int64]string{-5: "0s", 0: "0s", 45: "45s", 480: "8m", 3600: "1h", 3900: "1h5m", 86400: "1d", 97200: "1d3h"} {
		if got := FormatWait(in); got != want {
			t.Errorf("FormatWait(%d) = %q, want %q", in, got, want)
		}
	}
}

// Slack is a payload mode of the webhook transport: it POSTs the rendered
// message and sends NO signature header (incoming webhooks are unsigned).
func TestSlackSink_PostsRenderedMessageUnsigned(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, "ok")
	s, err := NewSlackSink(SlackConfig{URL: srv.URL + "/services/T/B/X"}, EnvSlackWebhookURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	req := srv.only(t)
	if string(req.body) != string(RenderSlackMessage(fixtureEvent())) {
		t.Fatalf("body = %s; want the rendered Slack message", req.body)
	}
	if sig := req.header.Get(HeaderSignature); sig != "" {
		t.Fatalf("Slack sink sent a signature header %q", sig)
	}
	if s.Name() != SlackKind || s.DestinationHost() != "127.0.0.1" {
		t.Fatalf("Name/DestinationHost = %q/%q", s.Name(), s.DestinationHost())
	}
}

func TestSinksFromEnv_SlackConfigured(t *testing.T) {
	env := map[string]string{EnvSlackWebhookURL: "https://hooks.slack.com/services/T/B/" + canary}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	if err != nil || len(sinks) != 1 || sinks[0].Name() != SlackKind {
		t.Fatalf("sinks=%v err=%v; want one slack sink", sinks, err)
	}
}

// L1: the Slack URL IS the credential. Neither a configuration error nor a
// transport error nor a non-2xx error may carry any part of it (canary in
// userinfo, path and query) into the returned error, the dispatcher's WARN
// log, or the outcome that feeds the push_notification_failed row.
func TestSlackSink_TransportErrorLeaksNoURL(t *testing.T) {
	// Configuration error.
	env := map[string]string{EnvSlackWebhookURL: "http://user:" + canary + "@hooks.slack.com/services/" + canary + "?t=" + canary}
	_, cfgErr := SinksFromEnv(func(k string) string { return env[k] })
	if cfgErr == nil || !strings.Contains(cfgErr.Error(), EnvSlackWebhookURL+": must be https") {
		t.Fatalf("config err = %v; want %s: must be https", cfgErr, EnvSlackWebhookURL)
	}
	if strings.Contains(cfgErr.Error(), canary) || strings.Contains(cfgErr.Error(), "hooks.slack.com") {
		t.Fatalf("config error echoes the URL: %v", cfgErr)
	}

	// Transport error: loopback port 1 refuses immediately.
	s, err := NewSlackSink(SlackConfig{URL: "http://user:" + canary + "@127.0.0.1:1/services/" + canary + "?token=" + canary}, EnvSlackWebhookURL)
	if err != nil {
		t.Fatal(err)
	}
	derr := s.Deliver(context.Background(), fixtureEvent())
	var de *DeliveryError
	if !errors.As(derr, &de) || de.Reason != ReasonConnectionRefused {
		t.Fatalf("Deliver err = %#v; want connection refused", derr)
	}

	// Non-2xx error with the canary in the URL path and response body.
	srv := newCaptureServer(t, http.StatusForbidden, "invalid_token "+canary)
	s403, err := NewSlackSink(SlackConfig{URL: srv.URL + "/services/" + canary + "?q=" + canary}, EnvSlackWebhookURL)
	if err != nil {
		t.Fatal(err)
	}
	statusErr := s403.Deliver(context.Background(), fixtureEvent())
	if !errors.As(statusErr, &de) || de.Reason != "http status 403" {
		t.Fatalf("status err = %v; want http status 403", statusErr)
	}

	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{s, s403}, Options{OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)
	d.Enqueue(fixtureEvent())
	drain(t, d)
	outs := rec.all()
	if len(outs) != 2 || outs[0].Err == nil || outs[1].Err == nil {
		t.Fatalf("outcomes = %+v; want two failures", outs)
	}
	texts := map[string]string{
		"transport error": derr.Error(), "status error": statusErr.Error(),
		"log": logs.String(), "outcome[0]": outs[0].Err.Error(), "outcome[1]": outs[1].Err.Error(),
	}
	for where, text := range texts {
		if strings.Contains(text, canary) {
			t.Fatalf("%s leaks the Slack URL: %s", where, text)
		}
		if !strings.Contains(text, SlackKind) {
			t.Fatalf("%s does not name the sink (vacuous assertion): %s", where, text)
		}
	}
}
