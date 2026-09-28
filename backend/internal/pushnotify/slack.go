package pushnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EnvSlackWebhookURL configures the Slack sink: an incoming-webhook URL.
// The URL itself IS the bearer credential, so it is never echoed anywhere.
const EnvSlackWebhookURL = "FISHHAWKD_NOTIFY_SLACK_WEBHOOK_URL"

func init() {
	Register(SlackKind, slackFromEnv)
}

func slackFromEnv(getenv func(string) string) (Sink, error) {
	raw := strings.TrimSpace(getenv(EnvSlackWebhookURL))
	if raw == "" {
		return nil, nil
	}
	return NewSlackSink(SlackConfig{URL: raw}, EnvSlackWebhookURL)
}

// SlackConfig configures the Slack sink.
type SlackConfig struct {
	URL    string
	Client *http.Client
}

// SlackSink is a payload MODE of the webhook transport: it shares postJSON
// but renders a Slack message and sends no HMAC header (Slack incoming
// webhooks are unsigned; the URL is the credential).
type SlackSink struct {
	target *url.URL
	client *http.Client
}

// NewSlackSink validates cfg; errors name envVar, never the URL.
func NewSlackSink(cfg SlackConfig, envVar string) (*SlackSink, error) {
	target, err := validateSinkURL(envVar, cfg.URL)
	if err != nil {
		return nil, err
	}
	return &SlackSink{target: target, client: boundedClient(cfg.Client)}, nil
}

// Name implements Sink.
func (*SlackSink) Name() string { return SlackKind }

// DestinationHost implements Destination.
func (s *SlackSink) DestinationHost() string { return s.target.Hostname() }

// Deliver implements Sink.
func (s *SlackSink) Deliver(ctx context.Context, e Event) error {
	return postJSON(ctx, s.client, SlackKind, s.target, RenderSlackMessage(e), nil)
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Elements []slackText `json:"elements,omitempty"`
}

type slackMessage struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

// RenderSlackMessage renders e as a Slack incoming-webhook message: a `text`
// fallback line (`<decision> on <repo>#<issue> - waiting 8m - <link>`) plus
// blocks carrying the decision, identifiers, verdicts, accumulated wait and
// links. It is pure and deterministic.
func RenderSlackMessage(e Event) []byte {
	wait := ""
	if e.GateLatency.TotalWaitOnHumanSeconds > 0 {
		wait = "waiting " + FormatWait(e.GateLatency.TotalWaitOnHumanSeconds)
	}
	msg := slackMessage{
		Text: joinNonEmpty(" - ", decisionLine(e), wait, primaryLink(e)),
	}

	head := "*" + slackEscape(e.Decision) + "*\n" + slackEscape(Subject(e))
	msg.Blocks = append(msg.Blocks, slackBlock{Type: "section", Text: &slackText{Type: "mrkdwn", Text: head}})

	var ctxParts []string
	if e.Stage != nil && e.Stage.Type != "" {
		ctxParts = append(ctxParts, "stage "+slackEscape(joinNonEmpty(" ", e.Stage.Type, parens(e.Stage.State))))
	}
	if e.WorkflowID != "" {
		ctxParts = append(ctxParts, "workflow "+slackEscape(e.WorkflowID))
	}
	ctxParts = append(ctxParts, "event "+slackEscape(e.Event))
	msg.Blocks = append(msg.Blocks, slackBlock{Type: "context", Elements: []slackText{{Type: "mrkdwn", Text: strings.Join(ctxParts, " · ")}}})

	if len(e.Verdicts) > 0 {
		lines := make([]string, 0, len(e.Verdicts))
		for _, v := range e.Verdicts {
			lines = append(lines, "• "+slackEscape(v.ReviewerModel)+": "+slackEscape(v.Verdict))
		}
		msg.Blocks = append(msg.Blocks, slackBlock{Type: "section", Text: &slackText{Type: "mrkdwn", Text: "*Reviews*\n" + strings.Join(lines, "\n")}})
	}

	if e.GateLatency.TotalWaitOnHumanSeconds > 0 {
		txt := "Waiting on a human for " + FormatWait(e.GateLatency.TotalWaitOnHumanSeconds)
		if len(e.GateLatency.Gates) > 0 {
			gates := make([]string, 0, len(e.GateLatency.Gates))
			for _, g := range e.GateLatency.Gates {
				gates = append(gates, slackEscape(g.Gate)+" "+FormatWait(g.WaitSeconds))
			}
			txt += " (" + strings.Join(gates, ", ") + ")"
		}
		msg.Blocks = append(msg.Blocks, slackBlock{Type: "context", Elements: []slackText{{Type: "mrkdwn", Text: txt}}})
	}

	var links []string
	if e.Links.Run != "" {
		links = append(links, slackLink(e.Links.Run, "Open run"))
	}
	if e.Links.Issue != "" {
		links = append(links, slackLink(e.Links.Issue, "Issue"))
	}
	if e.Links.PullRequest != "" {
		links = append(links, slackLink(e.Links.PullRequest, "Pull request"))
	}
	if len(links) > 0 {
		msg.Blocks = append(msg.Blocks, slackBlock{Type: "section", Text: &slackText{Type: "mrkdwn", Text: strings.Join(links, " · ")}})
	}

	// Encoding a struct of strings cannot fail. HTML escaping is off so the
	// mrkdwn link markup is sent literally rather than as \u003c escapes.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(msg)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Subject names where the decision is: `<repo>#<issue>` for an
// issue-anchored run, `<repo> run <short>` otherwise.
func Subject(e Event) string {
	if e.Issue != nil && e.Issue.Number > 0 {
		return e.Repo + "#" + strconv.Itoa(e.Issue.Number)
	}
	return joinNonEmpty(" ", e.Repo, "run "+e.RunShortID)
}

// decisionLine is `<decision> on <subject>`.
func decisionLine(e Event) string {
	return joinNonEmpty(" on ", e.Decision, Subject(e))
}

func primaryLink(e Event) string {
	if e.Links.Run != "" {
		return e.Links.Run
	}
	return e.Links.Issue
}

// FormatWait renders seconds compactly: 45s, 8m, 1h5m, 2d3h.
func FormatWait(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm", seconds/60)
	case d < 24*time.Hour:
		h, m := seconds/3600, (seconds%3600)/60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days, h := seconds/86400, (seconds%86400)/3600
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// slackEscape escapes the three characters Slack mrkdwn treats as control
// characters, so payload text cannot forge a link or mention.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func slackLink(u, label string) string {
	return "<" + slackEscape(u) + "|" + label + ">"
}
