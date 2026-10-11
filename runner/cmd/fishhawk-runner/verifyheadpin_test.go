package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// #4190: the verify-head pin.

// verifyRunAt is a verify_run event carrying head (and a derived tree id), the
// shape runVerifyCommittedTree emits.
func verifyRunAt(head string) agent.Event {
	tree := ""
	if head != "" {
		tree = "tree-of-" + head
	}
	return verifyRunEvent("scripts/test verify", head, tree, 1, "FAIL", "failed")
}

// TestLastVerifyRunHead: the LAST verify_run with a non-empty head wins, an
// empty-head run never blanks out an earlier real one, and other event kinds
// are ignored.
func TestLastVerifyRunHead(t *testing.T) {
	other := agent.Event{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{"head_sha": "not-a-verify-run"})}
	cases := []struct {
		name     string
		events   []agent.Event
		wantHead string
	}{
		{name: "last_wins", events: []agent.Event{verifyRunAt("A"), verifyRunAt(""), verifyRunAt("B")}, wantHead: "B"},
		{name: "empty_head_skipped", events: []agent.Event{verifyRunAt("A"), verifyRunAt("")}, wantHead: "A"},
		{name: "other_kinds_ignored", events: []agent.Event{verifyRunAt("A"), other}, wantHead: "A"},
		{name: "undecodable_payload_skipped", events: []agent.Event{verifyRunAt("A"), {Kind: "verify_run", Payload: []byte("{")}}, wantHead: "A"},
		{name: "none", events: nil, wantHead: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, tree := lastVerifyRunHead(tc.events)
			if head != tc.wantHead {
				t.Fatalf("head = %q, want %q", head, tc.wantHead)
			}
			wantTree := ""
			if tc.wantHead != "" {
				wantTree = "tree-of-" + tc.wantHead
			}
			if tree != wantTree {
				t.Errorf("tree = %q, want %q", tree, wantTree)
			}
		})
	}
}

// TestPinVerifyHead_PinsHeadAndBase: the -verify ref is created at the last
// verify_run head, resolves from the repository AND from a second linked
// worktree (refs/fishhawk/* is shared, git-worktree(1) "REFS"), and the base is
// the head's parent.
func TestPinVerifyHead_PinsHeadAndBase(t *testing.T) {
	repo, _, _, headSHA, treeSHA := pushResumeRepo(t)
	cfg := checkpointResumeCfg(repo)
	var logSink strings.Builder
	pin, ok := pinVerifyHead(context.Background(), cfg, repo, []agent.Event{verifyRunAt("ignored-earlier"), verifyRunEvent("v", headSHA, treeSHA, 1, "FAIL", "failed")}, &logSink)
	if !ok {
		t.Fatalf("pin not ok:\n%s", logSink.String())
	}
	ref := checkpointVerifyRef(cfg.runID, cfg.stageID)
	if pin.ref != ref || pin.headSHA != headSHA || pin.treeSHA != treeSHA {
		t.Errorf("pin = %+v, want ref %s head %s tree %s", pin, ref, headSHA, treeSHA)
	}
	if want := cprGit(t, repo, "rev-parse", headSHA+"^"); pin.baseSHA != want {
		t.Errorf("base = %q, want head^ %q", pin.baseSHA, want)
	}
	if got, ok := refTarget(repo, ref); !ok || got != headSHA {
		t.Fatalf("ref %s = %q (exists %v), want %s", ref, got, ok, headSHA)
	}
	wt := filepath.Join(t.TempDir(), "linked")
	cprGit(t, repo, "worktree", "add", "--detach", wt, "main")
	if got, ok := refTarget(wt, ref); !ok || got != headSHA {
		t.Errorf("ref %s from a linked worktree = %q (exists %v), want %s", ref, got, ok, headSHA)
	}
	if !strings.Contains(logSink.String(), `"event":"verify_head_pinned"`) {
		t.Errorf("missing verify_head_pinned:\n%s", logSink.String())
	}
	if !strings.HasSuffix(ref, "/"+cfg.stageID+checkpointVerifySuffix) {
		t.Errorf("ref %q must be the checkpoint ref with the -verify suffix", ref)
	}
}

// TestPinVerifyHead_FailOpen: every refusal returns ok=false with nothing
// pinned and a named log line — never a panic or an error the caller must
// handle.
func TestPinVerifyHead_FailOpen(t *testing.T) {
	ctx := context.Background()
	t.Run("no_verify_run_head", func(t *testing.T) {
		repo, _, _, _, _ := pushResumeRepo(t)
		cfg := checkpointResumeCfg(repo)
		var logSink strings.Builder
		if _, ok := pinVerifyHead(ctx, cfg, repo, []agent.Event{verifyRunAt("")}, &logSink); ok {
			t.Fatal("an empty head must not pin")
		}
		if _, exists := refTarget(repo, checkpointVerifyRef(cfg.runID, cfg.stageID)); exists {
			t.Error("nothing may be pinned without a head")
		}
		if !strings.Contains(logSink.String(), `"event":"verify_head_pin_skipped"`) {
			t.Errorf("missing verify_head_pin_skipped:\n%s", logSink.String())
		}
	})
	t.Run("invalid_refname", func(t *testing.T) {
		repo, _, _, headSHA, treeSHA := pushResumeRepo(t)
		cfg := checkpointResumeCfg(repo)
		// git check-ref-format refuses ".." in a refname BY CONSTRUCTION.
		cfg.runID = "bad..run"
		var logSink strings.Builder
		if _, ok := pinVerifyHead(ctx, cfg, repo, []agent.Event{verifyRunEvent("v", headSHA, treeSHA, 1, "x", "failed")}, &logSink); ok {
			t.Fatal("git must refuse a '..' refname")
		}
		if !strings.Contains(logSink.String(), `"event":"verify_head_pin_failed"`) {
			t.Errorf("missing verify_head_pin_failed:\n%s", logSink.String())
		}
	})
	t.Run("parentless_head", func(t *testing.T) {
		repo, _, _, _, treeSHA := pushResumeRepo(t)
		cfg := checkpointResumeCfg(repo)
		root := cprGit(t, repo, "commit-tree", treeSHA, "-m", "a root commit has no parent")
		var logSink strings.Builder
		if _, ok := pinVerifyHead(ctx, cfg, repo, []agent.Event{verifyRunEvent("v", root, treeSHA, 1, "x", "failed")}, &logSink); ok {
			t.Fatal("a head with no parent must not pin (no base to record)")
		}
		if _, exists := refTarget(repo, checkpointVerifyRef(cfg.runID, cfg.stageID)); exists {
			t.Error("the base is resolved BEFORE the pin: nothing may be pinned")
		}
		if !strings.Contains(logSink.String(), `"event":"verify_head_pin_failed"`) {
			t.Errorf("missing verify_head_pin_failed:\n%s", logSink.String())
		}
	})
}

// TestVerifyHeadFailureSuffix names the sha and the ref, and renders nothing
// when either is absent so an unpinned reason is byte-identical.
func TestVerifyHeadFailureSuffix(t *testing.T) {
	ref := checkpointVerifyRef("r", "s")
	if ref != "refs/fishhawk/checkpoints/r/s-verify" {
		t.Errorf("verify ref = %q", ref)
	}
	got := verifyHeadFailureSuffix("abc123", ref)
	if got != "; verified head abc123 pinned at refs/fishhawk/checkpoints/r/s-verify" {
		t.Errorf("suffix = %q", got)
	}
	if verifyHeadFailureSuffix("", ref) != "" || verifyHeadFailureSuffix("abc123", "") != "" {
		t.Error("an absent sha or ref must render nothing")
	}
}

// TestReleaseVerifyHeadRef releases ONLY the -verify pin.
func TestReleaseVerifyHeadRef(t *testing.T) {
	repo, _, _, headSHA, _ := pushResumeRepo(t)
	cfg := checkpointResumeCfg(repo)
	pinBothCheckpointRefs(t, repo, cfg, headSHA)
	verifyRef := checkpointVerifyRef(cfg.runID, cfg.stageID)
	cprGit(t, repo, "update-ref", verifyRef, headSHA)
	var logSink strings.Builder
	releaseVerifyHeadRef(context.Background(), repo, cfg.runID, cfg.stageID, &logSink)
	if _, ok := refTarget(repo, verifyRef); ok {
		t.Errorf("%s survived its release", verifyRef)
	}
	for _, ref := range []string{checkpointRef(cfg.runID, cfg.stageID), checkpointStashRef(cfg.runID, cfg.stageID)} {
		if _, ok := refTarget(repo, ref); !ok {
			t.Errorf("%s must survive a verify-head-only release", ref)
		}
	}
	if !strings.Contains(logSink.String(), `"event":"checkpoint_refs_released"`) {
		t.Errorf("missing checkpoint_refs_released:\n%s", logSink.String())
	}
}
