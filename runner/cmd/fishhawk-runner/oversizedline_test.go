package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// TestRun_ImplementStage_OversizedEventLine_PacksIntactAndOpensPR (#5535):
// an agent event whose bundle line is 5 MiB, over the old 4 MiB reader
// limit, ships intact in the raw bundle, which the bounded bundle.Open
// still reads, and the stage pushes, opens its PR and ships /pull-request.
func TestRun_ImplementStage_OversizedEventLine_PacksIntactAndOpensPR(t *testing.T) {
	const bigBytes = 5 << 20
	big := agent.Event{
		Kind:      "assistant",
		Timestamp: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
		Payload:   agent.MakePayload(map[string]string{"text": strings.Repeat("q", bigBytes)}),
	}
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{big}}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%.4000s", got, stderr.String())
	}

	var raw *upload.ShipArgs
	for i := range fu.gotShipCalls {
		if fu.gotShipCalls[i].Variant == "raw" {
			raw = &fu.gotShipCalls[i]
		}
	}
	if raw == nil {
		t.Fatalf("no raw ShipTrace call among %d", len(fu.gotShipCalls))
	}
	_, lines, _, err := bundle.Open(bytes.NewReader(raw.Bundle))
	if err != nil {
		t.Fatalf("bounded bundle.Open of the raw bundle: %v", err)
	}
	found := false
	for _, ln := range lines {
		if ln.Kind == bundle.KindEventElided {
			t.Fatalf("5 MiB event elided: %s", ln.Data)
		}
		if ln.Kind != "assistant" {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ln.Data, &p); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(ln)
		if len(p.Text) == bigBytes && len(body) > 4<<20 {
			found = true
		}
	}
	if !found {
		t.Fatal("raw bundle does not carry the 5 MiB event line intact")
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest not called")
	}
}

// TestRun_OversizedOrdinaryEvent_ElidedExcerptRedacted (Y5): an ordinary
// event over MaxLineBytes is elided in both variants. The redacted variant is
// packed from the REDACTED events, so its placeholder's head excerpt lacks the
// token the raw excerpt carries, and run() logs each elision to its own log
// sink with the variant, original_bytes and that variant's sha256.
func TestRun_OversizedOrdinaryEvent_ElidedExcerptRedacted(t *testing.T) {
	big := agent.Event{
		Kind:      "assistant",
		Timestamp: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
		Payload:   agent.MakePayload(map[string]string{"text": heldReportToken + " " + strings.Repeat("q", bundle.MaxLineBytes)}),
	}
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{big}}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var stderr strings.Builder
	if got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%.4000s", got, stderr.String())
	}

	placeholders := map[string]bundle.ElidedEventData{}
	for _, c := range fu.gotShipCalls {
		_, lines, _, err := bundle.Open(bytes.NewReader(c.Bundle))
		if err != nil {
			t.Fatalf("open %s bundle: %v", c.Variant, err)
		}
		for _, ln := range lines {
			if ln.Kind != bundle.KindEventElided {
				continue
			}
			var d bundle.ElidedEventData
			if err := json.Unmarshal(ln.Data, &d); err != nil {
				t.Fatal(err)
			}
			placeholders[c.Variant] = d
		}
	}
	raw, ok := placeholders["raw"]
	if !ok || !strings.Contains(raw.ExcerptHead, heldReportToken) {
		t.Fatalf("fixture: the raw placeholder's head excerpt must carry the token (%v): %.200q", ok, raw.ExcerptHead)
	}
	red, ok := placeholders["redacted"]
	if !ok {
		t.Fatal("redacted bundle has no event_elided placeholder")
	}
	if strings.Contains(red.ExcerptHead+red.ExcerptTail, heldReportToken) || !strings.Contains(red.ExcerptHead, "[REDACTED:") {
		t.Errorf("redacted excerpt leaks the token or lacks the marker: %.200q", red.ExcerptHead)
	}
	if raw.SHA256 == red.SHA256 {
		t.Errorf("raw and redacted placeholders share sha256 %s", raw.SHA256)
	}

	logged := map[string]map[string]any{}
	for _, l := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["event"] != "bundle_event_elided" {
			continue
		}
		v, _ := m["variant"].(string)
		logged[v] = m
	}
	for variant, d := range map[string]bundle.ElidedEventData{"raw": raw, "redacted": red} {
		m, ok := logged[variant]
		if !ok {
			t.Errorf("run() log sink lacks bundle_event_elided for variant %q:\n%.2000s", variant, stderr.String())
			continue
		}
		if m["kind"] != "assistant" || m["sha256"] != d.SHA256 || int(m["original_bytes"].(float64)) != d.OriginalBytes {
			t.Errorf("%s elision log = %v, want kind assistant, sha256 %s, original_bytes %d", variant, m, d.SHA256, d.OriginalBytes)
		}
	}
}
