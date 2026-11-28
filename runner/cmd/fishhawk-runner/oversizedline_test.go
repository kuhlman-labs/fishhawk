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
