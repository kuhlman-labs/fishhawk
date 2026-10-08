package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// goCacheVanishedOutput3901 is the verbatim-corpus string for the #3901 class.
// It reconstructs run 26ede2f1's verify failure as the #3901 comment quotes it
// (`could not load export data: open …/Library/Caches/go-build/e5/e5470b5b…-d:
// no such file or directory (typecheck)`). The comment ELIDES the home prefix
// and the hash tail, and the matcher keys on the FULL 64-hex shape, so the
// reconstruction keeps the observed `e5/e5470b5b` prefix and `-d` suffix and
// fills the remaining 56 hex digits. backend/internal/delegation's
// delegation_test.go carries the BYTE-IDENTICAL string (the two modules cannot
// share a fixture); edit both together.
const goCacheVanishedOutput3901 = `could not load export data: open /Users/brettkuhlman/Library/Caches/go-build/e5/e5470b5bb9216d877b13b4a5c7e153e2f649dfd191fec16d8f608ea2f2951e36-d: no such file or directory (typecheck)`

// TestIsGoBuildCacheEntryVanished is the UNIT pin on the #3901 matcher. It is
// not the counterfactual vehicle for the isVerifyInfraFailure WIRING (it calls
// the helper directly); that is TestIsVerifyInfraFailure's cache-vanished row
// plus the two end-to-end tests below.
func TestIsGoBuildCacheEntryVanished(t *testing.T) {
	const hex64 = "e5470b5bb9216d877b13b4a5c7e153e2f649dfd191fec16d8f608ea2f2951e36"
	for _, tt := range []struct {
		name   string
		output string
		want   bool
	}{
		{"verbatim-corpus #3901 export-data failure", goCacheVanishedOutput3901, true},
		{"container gate cache path (/gatecache/gocache)", "open /gatecache/gocache/e5/" + hex64 + "-d: no such file or directory", true},
		{"action entry (-a)", "open /Users/x/Library/Caches/go-build/e5/" + hex64 + "-a: no such file or directory", true},
		{"compiler could-not-import rendering", "backend/internal/run/run.go:7:2: could not import github.com/kuhlman-labs/fishhawk/backend/internal/audit (open /root/.cache/go-build/e5/" + hex64 + "-d: no such file or directory)\nFAIL", true},
		{"testdata ENOENT is a test failure, not a cache entry", "--- FAIL: TestLoad (0.00s)\n    load_test.go:9: open testdata/fixture.json: no such file or directory\nFAIL", false},
		{"63-hex name", "open /x/go-build/e5/" + hex64[:63] + "-d: no such file or directory", false},
		{"65-hex name", "open /x/go-build/e5/" + hex64 + "0-d: no such file or directory", false},
		{"uppercase hex", "open /x/go-build/E5/" + strings.ToUpper(hex64) + "-d: no such file or directory", false},
		{"missing -a/-d suffix", "open /x/go-build/e5/" + hex64 + ": no such file or directory", false},
		{"permission denied on a cache path", "open /x/go-build/e5/" + hex64 + "-d: permission denied", false},
		{"empty output", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGoBuildCacheEntryVanished(tt.output); got != tt.want {
				t.Errorf("isGoBuildCacheEntryVanished(...) = %v, want %v\noutput:\n%s", got, tt.want, tt.output)
			}
		})
	}
}

// TestRun_VerifyGateCommitted_GoBuildCacheVanished_CategoryC is the #3901
// end-to-end pin on the single-shot committed gate: a verify that fails EVERY
// time with ONLY the cache-vanished signature (none of the other four
// diff-independent classes' markers, so no other disjunct can mask a deleted
// one) is re-run once in place and then classified category C — never A (the
// pre-#3901 agent-failure verdict) and never B. runTestMain stubs readHostLoad
// to non-overloaded package-wide, so the host-load reclassification cannot
// produce the C either.
func TestRun_VerifyGateCommitted_GoBuildCacheVanished_CategoryC(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       scriptedVerifyCmd(t, goCacheVanishedOutput3901), // fails every invocation
		VerifyMaxIterations: 0,                                               // single-shot committed gate (#802)
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("a persistent vanished-cache-entry signature must classify category C:\n%s", stderr.String())
	}
	for _, wrong := range []string{`"category":"A"`, `"category":"B"`} {
		if strings.Contains(stderr.String(), wrong) {
			t.Errorf("a persistent vanished-cache-entry signature must NOT classify %s:\n%s", wrong, stderr.String())
		}
	}
	if fp.gotArgs != nil {
		t.Error("CommitAndPush must not run after a committed-tree gate block")
	}

	var verifyRuns, retries int
	for _, ev := range readBundleEvents(t, bundlePath) {
		switch ev.Kind {
		case "verify_run":
			verifyRuns++
		case "verify_infra_flake_retry":
			retries++
		}
	}
	if verifyRuns != 2 {
		t.Errorf("verify_run events = %d, want 2: the absorb must re-run once before classifying", verifyRuns)
	}
	if retries != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1", retries)
	}
}
