package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

func TestRedactEvents_ReplacesPayloadSecrets(t *testing.T) {
	secret := "ghp_" + strings.Repeat("z", 36)
	events := []agent.Event{
		{Kind: "system.init", Payload: json.RawMessage(`{}`)},
		{Kind: "raw", Payload: json.RawMessage(`{"text":"saw ` + secret + ` here"}`)},
	}
	got, hits := redactEvents(events)

	if len(got) != len(events) {
		t.Fatalf("len = %d, want %d", len(got), len(events))
	}
	if string(got[0].Payload) != string(events[0].Payload) {
		t.Errorf("system.init payload changed unnecessarily")
	}
	if strings.Contains(string(got[1].Payload), secret) {
		t.Errorf("secret survived in event payload: %s", got[1].Payload)
	}
	if !strings.Contains(string(got[1].Payload), "[REDACTED:github-pat-classic]") {
		t.Errorf("redaction marker missing in event payload: %s", got[1].Payload)
	}
	if findHitCount(hits, "github-pat-classic") != 1 {
		t.Errorf("expected 1 github-pat-classic hit, got %+v", hits)
	}
}

func TestRedactEvents_DoesNotMutateInput(t *testing.T) {
	secret := "sk-" + strings.Repeat("a", 48)
	original := json.RawMessage(`{"k":"` + secret + `"}`)
	events := []agent.Event{{Kind: "raw", Payload: original}}

	_, _ = redactEvents(events)
	if !strings.Contains(string(events[0].Payload), secret) {
		t.Errorf("input slice mutated; redactEvents must return a fresh slice")
	}
}

func TestRedactEvents_PassesThroughEmptyPayloads(t *testing.T) {
	events := []agent.Event{{Kind: "system.init"}}
	got, hits := redactEvents(events)
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Payload != nil {
		t.Errorf("nil payload should pass through")
	}
	if hits != nil {
		t.Errorf("expected nil hits when nothing matched, got %+v", hits)
	}
}

func TestRedactEvents_AggregatesAcrossPayloads(t *testing.T) {
	a := "ghp_" + strings.Repeat("a", 36)
	b := "ghp_" + strings.Repeat("b", 36)
	events := []agent.Event{
		{Kind: "raw", Payload: json.RawMessage(`{"x":"` + a + `"}`)},
		{Kind: "raw", Payload: json.RawMessage(`{"x":"` + b + `"}`)},
	}
	_, hits := redactEvents(events)
	if findHitCount(hits, "github-pat-classic") != 2 {
		t.Errorf("hits aggregation wrong: %+v", hits)
	}
}

func TestRedactString_RedactsManifestReason(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("x", 60)
	got, hits := redactString("crashed with " + secret + " in argv")
	if strings.Contains(got, secret) {
		t.Errorf("secret survived in reason: %q", got)
	}
	if findHitCount(hits, "anthropic-api-key") != 1 {
		t.Errorf("hits = %+v", hits)
	}
}

func TestRedactString_EmptyPassesThrough(t *testing.T) {
	got, hits := redactString("")
	if got != "" || hits != nil {
		t.Errorf("empty input: got=%q hits=%+v", got, hits)
	}
}

func TestMergeHits_SumsByPattern(t *testing.T) {
	a := []redaction.Hit{{Pattern: "p1", Count: 2}, {Pattern: "p2", Count: 1}}
	b := []redaction.Hit{{Pattern: "p1", Count: 1}, {Pattern: "p3", Count: 5}}
	out := mergeHits(a, b)
	if findHitCount(out, "p1") != 3 ||
		findHitCount(out, "p2") != 1 ||
		findHitCount(out, "p3") != 5 {
		t.Errorf("merge wrong: %+v", out)
	}
	// Sort order: alphabetical.
	if out[0].Pattern != "p1" || out[1].Pattern != "p2" || out[2].Pattern != "p3" {
		t.Errorf("expected alpha sort, got %+v", out)
	}
}

func TestMergeHits_HandlesEmptySides(t *testing.T) {
	a := []redaction.Hit{{Pattern: "x", Count: 1}}
	if got := mergeHits(nil, a); len(got) != 1 || got[0].Count != 1 {
		t.Errorf("nil-left merge dropped data: %+v", got)
	}
	if got := mergeHits(a, nil); len(got) != 1 || got[0].Count != 1 {
		t.Errorf("nil-right merge dropped data: %+v", got)
	}
	if got := mergeHits(nil, nil); got != nil {
		t.Errorf("nil-both merge should be nil, got %+v", got)
	}
}

func findHitCount(hits []redaction.Hit, name string) int {
	for _, h := range hits {
		if h.Pattern == name {
			return h.Count
		}
	}
	return 0
}

// knownTestValue is a runtime-built bound value shaped to match NO
// DefaultPatterns regex, so only the known-value pass can remove it.
func knownTestValue() string {
	return "acc/Zq+" + strings.Repeat("x9", 6) + ":k"
}

func knownTestSet(t *testing.T, v string) *redaction.KnownValues {
	t.Helper()
	kv, below := redaction.NewKnownValues(redaction.KnownValue{Name: "ACC_TOKEN", Value: v})
	if len(below) != 0 || kv.Len() != 1 {
		t.Fatalf("fixture value must clear the floor: below=%v len=%d", below, kv.Len())
	}
	return kv
}

func TestScrubKnownEvents_NilSetReturnsInput(t *testing.T) {
	v := knownTestValue()
	events := []agent.Event{{Kind: "raw", Payload: json.RawMessage(`{"text":"` + v + `"}`)}}
	got, hits := scrubKnownEvents(events, nil)
	if &got[0] != &events[0] {
		t.Error("a nil known set must return the input slice itself")
	}
	if hits != nil {
		t.Errorf("hits = %+v, want nil", hits)
	}
	empty, _ := redaction.NewKnownValues()
	got, hits = scrubKnownEvents(events, empty)
	if &got[0] != &events[0] || hits != nil {
		t.Errorf("an empty known set must return the input slice and nil hits: %+v", hits)
	}
}

func TestScrubKnownEvents_ReplacesValueWithoutMutatingInput(t *testing.T) {
	v := knownTestValue()
	kv := knownTestSet(t, v)
	original := `{"text":"saw ` + v + ` twice ` + v + `"}`
	events := []agent.Event{
		{Kind: "system.init"},
		{Kind: "raw", Payload: json.RawMessage(original)},
	}
	got, hits := scrubKnownEvents(events, kv)
	if len(got) != 2 || &got[0] == &events[0] {
		t.Fatal("scrubKnownEvents must return a fresh slice of the same length")
	}
	if got[0].Payload != nil {
		t.Errorf("a nil payload must pass through: %s", got[0].Payload)
	}
	if strings.Contains(string(got[1].Payload), v) {
		t.Errorf("known value survived: %s", got[1].Payload)
	}
	if !strings.Contains(string(got[1].Payload), "[REDACTED:credential:ACC_TOKEN]") {
		t.Errorf("marker missing: %s", got[1].Payload)
	}
	if string(events[1].Payload) != original {
		t.Errorf("input payload mutated: %s", events[1].Payload)
	}
	if findHitCount(hits, "credential:ACC_TOKEN") != 2 {
		t.Errorf("hits = %+v, want 2 credential:ACC_TOKEN", hits)
	}
	for _, h := range hits {
		if strings.Contains(h.Pattern, v) {
			t.Errorf("hit carries the value: %+v", h)
		}
	}
}

func TestScrubKnownString(t *testing.T) {
	v := knownTestValue()
	kv := knownTestSet(t, v)
	if got, hits := scrubKnownString("agent printed "+v, nil); got != "agent printed "+v || hits != nil {
		t.Errorf("nil set must be identity: %q %+v", got, hits)
	}
	if got, hits := scrubKnownString("", kv); got != "" || hits != nil {
		t.Errorf("empty string must pass through: %q %+v", got, hits)
	}
	got, hits := scrubKnownString("agent printed "+v, kv)
	if got != "agent printed [REDACTED:credential:ACC_TOKEN]" {
		t.Errorf("scrubbed = %q", got)
	}
	if findHitCount(hits, "credential:ACC_TOKEN") != 1 {
		t.Errorf("hits = %+v", hits)
	}
}
