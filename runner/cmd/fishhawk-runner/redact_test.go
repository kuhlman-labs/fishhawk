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
