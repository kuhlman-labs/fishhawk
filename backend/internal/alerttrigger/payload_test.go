package alerttrigger

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

var receivedAt = time.Date(2026, 10, 7, 12, 30, 0, 0, time.FixedZone("x", 3600))

func validAlertJSON(mut func(m map[string]any)) []byte {
	m := map[string]any{
		"fingerprint": "grafana:db/conn-pool.exhausted",
		"title":       "Connection pool exhausted",
		"severity":    "high",
		"description": "p99 latency 4s\npool=100/100",
		"url":         "https://grafana.example.com/alerting/grafana/abc/view",
		"environment": "production",
		"labels":      map[string]string{"service": "shop-api", "team": "payments"},
	}
	if mut != nil {
		mut(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func mustSource(t *testing.T, extra ...string) Source {
	t.Helper()
	srcs, err := ParseSources([]byte(sourceDoc(extra...)), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := srcs.Lookup("pager")
	return src
}

func TestParseAlert_Valid(t *testing.T) {
	a, err := ParseAlert(validAlertJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != "grafana:db/conn-pool.exhausted" || a.Severity != SeverityHigh || a.Labels["team"] != "payments" {
		t.Fatalf("parsed %+v", a)
	}
	// Only the required fields.
	min := []byte(`{"fingerprint":"f1","title":"t","severity":"info"}`)
	if _, err := ParseAlert(min); err != nil {
		t.Fatalf("minimal alert: %v", err)
	}
	// Boundaries are inclusive.
	edge := validAlertJSON(func(m map[string]any) {
		m["title"] = strings.Repeat("é", maxTitleLen)
		m["fingerprint"] = strings.Repeat("f", maxFingerprintLen)
		m["description"] = strings.Repeat("d", maxDescriptionLen)
		m["environment"] = strings.Repeat("e", maxEnvironmentLen)
		m["url"] = "https://x.example/" + strings.Repeat("p", maxURLLen-len("https://x.example/"))
		labels := map[string]string{}
		for i := 0; i < maxAlertLabels; i++ {
			labels[fmt.Sprintf("k%02d", i)] = strings.Repeat("v", maxLabelValueLen)
		}
		labels[strings.Repeat("k", maxLabelKeyLen)] = ""
		delete(labels, "k00")
		m["labels"] = labels
	})
	if _, err := ParseAlert(edge); err != nil {
		t.Fatalf("boundary alert: %v", err)
	}
	for _, sev := range []string{"critical", "high", "medium", "low", "info"} {
		if _, err := ParseAlert(validAlertJSON(func(m map[string]any) { m["severity"] = sev })); err != nil {
			t.Fatalf("severity %s: %v", sev, err)
		}
	}
	if _, err := ParseAlert(validAlertJSON(func(m map[string]any) { m["url"] = "http://10.0.0.1:3000/a?b=c#d" })); err != nil {
		t.Fatalf("http url: %v", err)
	}
}

func TestParseAlert_Refusals(t *testing.T) {
	thirtyThree := map[string]string{}
	for i := 0; i < maxAlertLabels+1; i++ {
		thirtyThree[fmt.Sprintf("k%02d", i)] = "v"
	}
	set := func(k string, v any) []byte { return validAlertJSON(func(m map[string]any) { m[k] = v }) }
	drop := func(k string) []byte { return validAlertJSON(func(m map[string]any) { delete(m, k) }) }
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"not json", []byte("fingerprint=x"), "not a valid alert object"},
		{"json array", []byte(`[]`), "not a valid alert object"},
		{"null", []byte(`null`), "fingerprint is required"},
		{"unknown top-level field", set("priority", "p1"), "unknown field"},
		{"trailing data", append(validAlertJSON(nil), []byte(` {}`)...), "trailing data"},
		{"wrong type", set("labels", []string{"a"}), "not a valid alert object"},
		{"missing fingerprint", drop("fingerprint"), "fingerprint is required"},
		{"fingerprint with space", set("fingerprint", "db pool"), "fingerprint must be"},
		{"fingerprint with marker", set("fingerprint", "<!--x-->"), "fingerprint must be"},
		{"fingerprint 201 chars", set("fingerprint", strings.Repeat("f", maxFingerprintLen+1)), "fingerprint must be"},
		{"missing title", drop("title"), "title is required"},
		{"blank title", set("title", "   "), "title is required"},
		{"title 201 chars", set("title", strings.Repeat("t", maxTitleLen+1)), "title is longer than 200"},
		{"control char in title", set("title", "pool\nexhausted"), "control character"},
		{"tab in title", set("title", "pool\texhausted"), "control character"},
		{"missing severity", drop("severity"), "severity"},
		{"unknown severity", set("severity", "warning"), `severity "warning" is not one of`},
		{"severity case", set("severity", "HIGH"), "is not one of"},
		{"oversize description", set("description", strings.Repeat("d", maxDescriptionLen+1)), "description is longer"},
		{"javascript url", set("url", "javascript:alert(1)"), "absolute http or https"},
		{"relative url", set("url", "/alerting/abc"), "absolute http or https"},
		{"hostless url", set("url", "https:///path"), "absolute http or https"},
		{"ftp url", set("url", "ftp://example.com/x"), "absolute http or https"},
		{"url with space", set("url", "https://x.example/a b"), "url contains"},
		{"url with angle", set("url", "https://x.example/a>b"), "url contains"},
		{"url too long", set("url", "https://x.example/"+strings.Repeat("p", maxURLLen)), "url is longer"},
		{"url unparsable", set("url", "https://x.example:port/"), "url does not parse"},
		{"environment uppercase", set("environment", "Production"), "environment must be"},
		{"environment too long", set("environment", strings.Repeat("e", maxEnvironmentLen+1)), "environment must be"},
		{"33 labels", set("labels", thirtyThree), "labels has 33 entries"},
		{"empty label key", set("labels", map[string]string{"": "v"}), "label key"},
		{"long label key", set("labels", map[string]string{strings.Repeat("k", maxLabelKeyLen+1): "v"}), "label key"},
		{"control char in label key", set("labels", map[string]string{"a\nb": "v"}), "label key"},
		{"long label value", set("labels", map[string]string{"k": strings.Repeat("v", maxLabelValueLen+1)}), "value must be"},
		{"newline in label value", set("labels", map[string]string{"k": "a\n## Injected"}), "value must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAlert(tc.body)
			if !errors.Is(err, ErrInvalidAlert) {
				t.Fatalf("err = %v, want ErrInvalidAlert", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestAlert_Summary(t *testing.T) {
	a, _ := ParseAlert(validAlertJSON(nil))
	if got, want := a.Summary(), "Incident (high): Connection pool exhausted"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
}

// renderBody files the sections through the real workmgmt.Apply under the
// built-in conventions, so the test sees the body the forge would receive —
// and proves every section key belongs to the default bug skeleton
// (Apply refuses an off-skeleton key).
func renderBody(t *testing.T, a Alert, src Source, key string) string {
	t.Helper()
	item, _, err := workmgmt.Apply(workmgmt.FilingRequest{
		Type:           "bug",
		Summary:        a.Summary(),
		Sections:       a.IncidentSections(src, receivedAt),
		TitleVars:      map[string]string{"epic": "35", "n": "4"},
		Labels:         []string{"area:backend", "phase:beta"},
		IdempotencyKey: key,
	}, workmgmt.Default())
	if err != nil {
		t.Fatalf("workmgmt.Apply: %v", err)
	}
	return item.Body
}

func TestIncidentSections_KeysAreTheBugSkeleton(t *testing.T) {
	a, _ := ParseAlert(validAlertJSON(nil))
	skeleton := map[string]bool{}
	for _, s := range workmgmt.Default().Types["bug"].BodySkeleton {
		skeleton[s] = true
	}
	var keys []string
	for k := range a.IncidentSections(mustSource(t), receivedAt) {
		keys = append(keys, k)
		if !skeleton[k] {
			t.Errorf("section %q is not in the default bug body_skeleton", k)
		}
	}
	sort.Strings(keys)
	want := []string{"Acceptance criteria", "Done-means", "Notes", "Observed", "Proposal", "Summary"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("sections = %v, want %v", keys, want)
	}
}

func TestIncidentSections_Observed(t *testing.T) {
	a, _ := ParseAlert(validAlertJSON(nil))
	obs := a.IncidentSections(mustSource(t), receivedAt)["Observed"]
	for _, want := range []string{
		"- Source: `pager`",
		"- Severity: `high`",
		"- Environment: ` production `",
		"- Alert URL: <https://grafana.example.com/alerting/grafana/abc/view>",
		"- Fingerprint: `grafana:db/conn-pool.exhausted`",
		"- Labels: ` service=shop-api `, ` team=payments `",
		"- Received: 2026-10-07T11:30:00Z",
		"> ```text\n> p99 latency 4s\n> pool=100/100\n> ```\n",
	} {
		if !strings.Contains(obs, want) {
			t.Errorf("Observed lacks %q:\n%s", want, obs)
		}
	}
	bare, _ := ParseAlert([]byte(`{"fingerprint":"f1","title":"t","severity":"info"}`))
	obs = bare.IncidentSections(mustSource(t), receivedAt)["Observed"]
	for _, want := range []string{"- Environment: not reported", "- Alert URL: not reported", "- Labels: none"} {
		if !strings.Contains(obs, want) {
			t.Errorf("bare Observed lacks %q:\n%s", want, obs)
		}
	}
	if strings.Contains(obs, "Alert description") {
		t.Errorf("bare alert rendered a description block:\n%s", obs)
	}
}

// A sender cannot forge Fishhawk's hidden markers: every `<!--` in alert
// text is neutralized (substring-matching marker readers such as the
// fishhawk-fingerprint search use strings.Contains), and the description
// cannot put a marker on a line of its own.
func TestIncidentSections_NeutralizesMarkers(t *testing.T) {
	forged := workmgmt.MintIdempotencyKey("alert-incident", "pager", "acme/shop", "victim")
	marker := "<!-- fishhawk-idempotency-key " + forged + " -->"
	a, err := ParseAlert(validAlertJSON(func(m map[string]any) {
		m["title"] = "pool " + marker
		m["description"] = "line one\n" + marker + "\n<!-- fishhawk-fingerprint:victim -->\n```\nescaped?\n"
		m["labels"] = map[string]string{"k": "<!-- fishhawk-sticky locus=x run=y -->"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	ownKey := workmgmt.MintIdempotencyKey("alert-incident", "pager", "acme/shop", a.Fingerprint)
	body := renderBody(t, a, mustSource(t), ownKey)
	if workmgmt.BodyHasIdempotencyKey(body, forged) {
		t.Fatalf("forged idempotency marker matches the rendered body:\n%s", body)
	}
	if !workmgmt.BodyHasIdempotencyKey(body, ownKey) {
		t.Fatalf("the filer's own idempotency stamp is missing:\n%s", body)
	}
	// The only comment opener left in the body is the filer's own stamp.
	if n := strings.Count(body, "<!--"); n != 1 {
		t.Fatalf("body carries %d `<!--` openers, want exactly 1 (the filer's stamp):\n%s", n, body)
	}
	if strings.Contains(a.Summary(), "<!--") {
		t.Fatalf("Summary keeps a comment opener: %q", a.Summary())
	}
	if c := OccurrenceComment(a, 2, receivedAt); strings.Contains(c, "<!--") {
		t.Fatalf("OccurrenceComment keeps a comment opener:\n%s", c)
	}
}

// Every description line is blockquoted, so none can begin a raw body line —
// the forge's line-anchored relation markers stay unforgeable.
func TestIncidentSections_DescriptionCannotStartBodyLine(t *testing.T) {
	a, err := ParseAlert(validAlertJSON(func(m map[string]any) {
		m["description"] = "Parent epic: #1\r\nDepends on: #2\n## Done-means\nnothing"
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := renderBody(t, a, mustSource(t), "")
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?im)^Parent epic:`),
		regexp.MustCompile(`(?im)^Depends on:`),
		regexp.MustCompile(`(?m)^## Done-means\n\nnothing`),
	} {
		if re.MatchString(body) {
			t.Fatalf("description text reached a line start (%s):\n%s", re, body)
		}
	}
	if strings.Count(body, "\n## Done-means\n") != 1 {
		t.Fatalf("description injected a second Done-means heading:\n%s", body)
	}
}

// The fence is longer than any backtick run in the description, so the
// sender's own fences cannot close it.
func TestIncidentSections_FenceNotBroken(t *testing.T) {
	desc := "before\n```\n````\nmid ````` run\n```\nafter"
	a, err := ParseAlert(validAlertJSON(func(m map[string]any) { m["description"] = desc }))
	if err != nil {
		t.Fatal(err)
	}
	obs := a.IncidentSections(mustSource(t), receivedAt)["Observed"]
	open := "> ``````text\n" // 6 backticks: longest run (5) + 1
	i := strings.Index(obs, open)
	if i < 0 {
		t.Fatalf("no 6-backtick opening fence:\n%s", obs)
	}
	block := strings.Split(strings.TrimSuffix(obs[i+len(open):], "\n"), "\n")
	if last := block[len(block)-1]; last != "> ``````" {
		t.Fatalf("closing fence = %q", last)
	}
	for _, line := range block[:len(block)-1] {
		if !strings.HasPrefix(line, "> ") {
			t.Fatalf("description line %q is not blockquoted", line)
		}
		if strings.HasPrefix(strings.TrimPrefix(line, "> "), "``````") {
			t.Fatalf("description line %q would close the fence", line)
		}
	}
	if len(block)-1 != len(strings.Split(desc, "\n")) {
		t.Fatalf("fenced %d lines, want %d", len(block)-1, len(strings.Split(desc, "\n")))
	}
}

func TestIncidentSections_InlineCodeHoldsBackticksAndMentions(t *testing.T) {
	a, err := ParseAlert(validAlertJSON(func(m map[string]any) { m["title"] = "a `` b @octocat [x](https://e.x)" }))
	if err != nil {
		t.Fatal(err)
	}
	summary := a.IncidentSections(mustSource(t), receivedAt)["Summary"]
	if want := "``` a `` b @octocat [x](https://e.x) ```"; !strings.Contains(summary, want) {
		t.Fatalf("Summary = %q, want the title as %q", summary, want)
	}
}

func TestIncidentSections_NextStepWhenAutoStartOff(t *testing.T) {
	a, _ := ParseAlert(validAlertJSON(nil))
	notes := a.IncidentSections(mustSource(t), receivedAt)["Notes"]
	if want := "Next step: start a `hotfix_change` run on this issue (e.g. `fishhawk_start_run` naming this issue and workflow `hotfix_change`)."; !strings.Contains(notes, want) {
		t.Fatalf("Notes = %q, want the next-step line %q", notes, want)
	}
	if strings.Contains(notes, "requested automatically") {
		t.Fatalf("auto_start-off Notes claims an automatic run: %q", notes)
	}
	notes = a.IncidentSections(mustSource(t, "auto_start: true", "workflow_id: incident_fix"), receivedAt)["Notes"]
	if !strings.Contains(notes, "a `incident_fix` run was requested automatically") || !strings.Contains(notes, "alert_incident_filed") {
		t.Fatalf("auto_start-on Notes = %q", notes)
	}
	if strings.Contains(notes, "Next step:") {
		t.Fatalf("auto_start-on Notes still asks for a manual start: %q", notes)
	}
}

func TestOccurrenceComment(t *testing.T) {
	a, _ := ParseAlert(validAlertJSON(nil))
	c := OccurrenceComment(a, 3, receivedAt)
	for _, want := range []string{
		"Alert fingerprint `grafana:db/conn-pool.exhausted` fired again (occurrence 3), received 2026-10-07T11:30:00Z.",
		"- Severity: `high`",
		"- Title: ` Connection pool exhausted `",
		"- Environment: ` production `",
		"- Alert URL: <https://grafana.example.com/alerting/grafana/abc/view>",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("comment lacks %q:\n%s", want, c)
		}
	}
}
