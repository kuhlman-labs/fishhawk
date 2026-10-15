package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// incident4337Changed is the incident fix-up commit 6ccdf30e's changed-file
// list (`git show --stat`), the diff its message is checked against.
var incident4337Changed = []string{
	"AGENTS.md",
	"backend/internal/prompt/README.md",
	"backend/internal/prompt/mergecandidatefailure.go",
	"backend/internal/server/merge_candidate_verify_test.go",
	"docs/api/v0.openapi.yaml",
	"runner/cmd/fishhawk-runner/README.md",
	"runner/cmd/fishhawk-runner/mergecandidateverify_test.go",
	"runner/internal/upload/upload.go",
	"runner/internal/upload/upload_test.go",
}

// incident4337Body reads the committed incident fixture, strips its `#` header
// and returns the commit-message body exactly as the loader would parse it.
func incident4337Body(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "testdata", "fixupclaims", "incident-4337-commitmsg.txt"))
	if err != nil {
		t.Fatalf("read incident fixture: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], "#") {
		i++
	}
	if i == 0 {
		t.Fatal("incident fixture lost its # header (the C1 mapping)")
	}
	subject, body, ok := parseCommitMessageSidecar([]byte(strings.Join(lines[i:], "\n")))
	if !ok || subject != "fix: shrink merge-candidate report to the body cap" {
		t.Fatalf("fixture subject = %q (ok=%v), want the incident commit's", subject, ok)
	}
	return body
}

// TestFixupCommitClaims_Incident4337 pins the cross-check on the incident's
// REAL commit message against its real changed files, with the item mapping
// observed from `git show -s 6ccdf30e` (approval condition C1; the fixture
// header states it): the issue's routed item 4 is the message's `Item 3`, which
// names the untouched injection_test.go and is CAUGHT; routed item 5 is the
// message's `Item 4`, identifier-only and NOT caught.
//
// MECHANISMS. (A) injection_test.go is absent from the changed list by
// construction, so only fileRefBacked makes Item 3 unbacked: mutating its
// `if p == ref || strings.HasSuffix(p, "/"+ref) {` to `if true {` yields zero
// claims. (B) the `Left alone` and `Verification` paragraphs name untouched
// paths but carry no Item/Finding/Concern lead: accepting every paragraph as a
// claim adds them and breaks the exactly-one assertion.
func TestFixupCommitClaims_Incident4337(t *testing.T) {
	body := incident4337Body(t)

	// The observed mapping, paragraph by paragraph.
	byMarker := map[string][]string{}
	for _, para := range fixupClaimParagraphs(body) {
		if marker, ok := fixupClaimMarker(para[0]); ok {
			byMarker[marker] = para
		}
	}
	for _, m := range []string{"Item 1", "Item 2", "Item 3", "Item 4", "Item 5"} {
		if byMarker[m] == nil {
			t.Fatalf("incident message has no %q paragraph; markers seen: %v", m, claimMarkersOf(byMarker))
		}
	}
	if len(byMarker) != 5 {
		t.Errorf("claim paragraphs = %v, want exactly Item 1-5", claimMarkersOf(byMarker))
	}
	if !strings.Contains(byMarker["Item 3"][0], "SIXTH byte-exact") {
		t.Errorf("Item 3 is not the sixth-drift-copy paragraph (issue item 4): %q", byMarker["Item 3"][0])
	}
	if !strings.HasPrefix(byMarker["Item 4"][0], "Item 4: fixupRenders") {
		t.Errorf("Item 4 is not the fixupRenders paragraph (issue item 5): %q", byMarker["Item 4"][0])
	}
	item4 := strings.Join(byMarker["Item 4"], "\n")
	if refs := fixupClaimFileRefRe.FindAllString(item4, -1); len(refs) != 0 {
		t.Errorf("Item 4 names file refs %v, want none (identifier-only)", refs)
	}
	if tests := fixupClaimTestNameRe.FindAllString(item4, -1); len(tests) != 0 {
		t.Errorf("Item 4 names tests %v, want none (identifier-only)", tests)
	}

	claims, truncated := checkFixupCommitClaims(body, incident4337Changed, "", false)
	want := []fixupCommitClaim{{Marker: "Item 3", FileRefs: []string{"injection_test.go"}}}
	if truncated || !reflect.DeepEqual(claims, want) {
		t.Fatalf("claims = %+v (truncated=%v), want exactly %+v", claims, truncated, want)
	}
}

func claimMarkersOf(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestFixupCommitClaims_TestNameBacking: a test name is evidence only when the
// patch is known; then it is backed iff the patch carries it.
func TestFixupCommitClaims_TestNameBacking(t *testing.T) {
	body := "Item 1: TestFoo now pins the ladder."
	if claims, _ := checkFixupCommitClaims(body, nil, "+func TestFoo(t *testing.T) {", true); len(claims) != 0 {
		t.Errorf("patch carries TestFoo: claims = %+v, want none", claims)
	}
	claims, _ := checkFixupCommitClaims(body, nil, "+func TestBar(t *testing.T) {", true)
	want := []fixupCommitClaim{{Marker: "Item 1", TestNames: []string{"TestFoo"}}}
	if !reflect.DeepEqual(claims, want) {
		t.Errorf("patch lacks TestFoo: claims = %+v, want %+v", claims, want)
	}
	if claims, _ := checkFixupCommitClaims(body, nil, "", false); len(claims) != 0 {
		t.Errorf("patch unknown: claims = %+v, want none (undecidable)", claims)
	}
	// A paragraph with an unbacked file and an undecidable test name carries
	// only the decidable token.
	claims, _ = checkFixupCommitClaims("Finding 2: TestFoo in gone_test.go", []string{"x.go"}, "", false)
	want = []fixupCommitClaim{{Marker: "Finding 2", FileRefs: []string{"gone_test.go"}}}
	if !reflect.DeepEqual(claims, want) {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
}

// TestFixupCommitClaims_IdentifierOnlyUndecidable: a claim naming only
// identifiers is silent — the documented residual.
func TestFixupCommitClaims_IdentifierOnlyUndecidable(t *testing.T) {
	body := "Item 4: fixupRenders is counted inside assertMergeCandidateFixupRender."
	if claims, _ := checkFixupCommitClaims(body, nil, "+x", true); len(claims) != 0 {
		t.Errorf("claims = %+v, want none", claims)
	}
}

// TestFixupCommitClaims_StopsAtApprovalConditions: a claim-led paragraph AFTER
// the `Approval conditions:` heading is the #3400 channel, never a claim.
// MECHANISM: the section's paragraph names an untouched path under an Item
// lead by construction, so deleting the stop reports it.
func TestFixupCommitClaims_StopsAtApprovalConditions(t *testing.T) {
	body := "Item 1: edited a.go.\n\nApproval conditions:\n\nItem 2: pullrequest_test.go is not edited."
	if claims, _ := checkFixupCommitClaims(body, []string{"a.go"}, "", false); len(claims) != 0 {
		t.Errorf("claims = %+v, want none", claims)
	}
}

// TestFixupCommitClaims_LeadsAndMarkers: the lead grammar and the marker
// normalization.
func TestFixupCommitClaims_LeadsAndMarkers(t *testing.T) {
	for _, tc := range []struct {
		first  string
		marker string
		ok     bool
	}{
		{"Item 3: x", "Item 3", true},
		{"- Finding #2. y", "Finding #2", true},
		{"*   concerns   C1, z", "concerns C1", true},
		{"Items 1-5 done", "Items 1-5", true},
		{"Low concern (lossy passthrough): q", "", false},
		{"Left alone: r", "", false},
		{"Item #", "", false},
		{"Item: no id", "", false},
	} {
		marker, ok := fixupClaimMarker(tc.first)
		if marker != tc.marker || ok != tc.ok {
			t.Errorf("fixupClaimMarker(%q) = (%q, %v), want (%q, %v)", tc.first, marker, ok, tc.marker, tc.ok)
		}
	}
}

// TestFixupCommitClaims_FileRefForms: suffix backing, a `./` prefix, a
// trailing `:<line>` and punctuation all resolve to the diff's path.
func TestFixupCommitClaims_FileRefForms(t *testing.T) {
	changed := []string{"runner/internal/upload/upload.go"}
	for _, ref := range []string{"upload.go", "./upload/upload.go", "internal/upload/upload.go:42", "runner/internal/upload/upload.go."} {
		body := "Item 1: changed " + ref
		if claims, _ := checkFixupCommitClaims(body, changed, "", false); len(claims) != 0 {
			t.Errorf("%q: claims = %+v, want backed", ref, claims)
		}
	}
	// A suffix match must be on a path boundary.
	if claims, _ := checkFixupCommitClaims("Item 1: load.go", changed, "", false); len(claims) != 1 {
		t.Errorf("load.go is not upload.go: claims = %+v, want one", claims)
	}
}

// TestFixupCommitClaims_Bounds: the claim cap with its truncated flag, the
// per-kind token cap, and the charset/length re-validation that keeps every
// emitted token inside [A-Za-z0-9_./-].
func TestFixupCommitClaims_Bounds(t *testing.T) {
	var paras []string
	for i := 0; i < 20; i++ {
		paras = append(paras, fmt.Sprintf("Item %d: gone%d.go", i, i))
	}
	claims, truncated := checkFixupCommitClaims(strings.Join(paras, "\n\n"), nil, "", false)
	if len(claims) != fixupClaimsMaxClaims || !truncated {
		t.Errorf("20 unbacked: got %d claims truncated=%v, want %d truncated=true", len(claims), truncated, fixupClaimsMaxClaims)
	}
	if claims, truncated := checkFixupCommitClaims(strings.Join(paras[:fixupClaimsMaxClaims], "\n\n"), nil, "", false); len(claims) != fixupClaimsMaxClaims || truncated {
		t.Errorf("exactly at the cap: got %d truncated=%v, want %d not truncated", len(claims), truncated, fixupClaimsMaxClaims)
	}

	var refs []string
	for i := 0; i < 12; i++ {
		refs = append(refs, fmt.Sprintf("f%d.go", i))
	}
	claims, _ = checkFixupCommitClaims("Item 1: "+strings.Join(refs, " ")+" f0.go", nil, "", false)
	if len(claims) != 1 || !reflect.DeepEqual(claims[0].FileRefs, refs[:fixupClaimsMaxTokensPerKind]) {
		t.Errorf("12 refs: claims = %+v, want the first %d, deduped", claims, fixupClaimsMaxTokensPerKind)
	}

	body := "Item 1: `evil.go`\nnew\nline.go and " + strings.Repeat("a", fixupClaimTokenMaxBytes) + ".go"
	claims, _ = checkFixupCommitClaims(body, nil, "", false)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want one", claims)
	}
	if want := []string{"evil.go", "line.go"}; !reflect.DeepEqual(claims[0].FileRefs, want) {
		t.Errorf("file refs = %q, want %q (the over-long ref dropped)", claims[0].FileRefs, want)
	}
	for _, c := range claims {
		for _, tok := range append(append([]string{}, c.FileRefs...), c.TestNames...) {
			if !fixupClaimTokenCharsetRe.MatchString(tok) {
				t.Errorf("emitted token %q escapes the charset", tok)
			}
		}
		if !fixupClaimMarkerCharsetRe.MatchString(c.Marker) {
			t.Errorf("emitted marker %q escapes the charset", c.Marker)
		}
	}
}

// TestFixupClaimTokens_CredentialShapedDropped: a token the default redaction
// patterns match is dropped whole. MECHANISM: the token is charset-clean and
// short by construction, so only the redaction check removes it. Built at run
// time so no credential-shaped literal sits in the source.
func TestFixupClaimTokens_CredentialShapedDropped(t *testing.T) {
	cred := "ghp_" + strings.Repeat("A1", 18) + ".txt"
	if got := fixupClaimTokens([]string{cred, "ok.go"}, true); !reflect.DeepEqual(got, []string{"ok.go"}) {
		t.Errorf("tokens = %q, want the credential-shaped ref dropped", got)
	}
	cases := []string{"Test_" + "ghp_" + strings.Repeat("B2", 18)}
	if got := fixupClaimTokens(cases, false); len(got) != 0 {
		t.Errorf("tokens = %q, want the credential-shaped test name dropped", got)
	}
}

// TestFixupClaimTokens_RevalidationDrops pins the re-validation directly: an
// input that is not a charset-clean token never survives, whatever produced it.
// MECHANISM: the inputs are handed in raw, so the regex extraction cannot
// pre-filter them; deleting the charset/length check emits them.
func TestFixupClaimTokens_RevalidationDrops(t *testing.T) {
	got := fixupClaimTokens([]string{"ok.go", "bad`.go", "nl\n.go", strings.Repeat("b", fixupClaimTokenMaxBytes+1), "./", "ok.go"}, true)
	if want := []string{"ok.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tokens = %q, want %q", got, want)
	}
}

// claimsGitDiffEvent builds a git_diff event over paths with an optional patch.
func claimsGitDiffEvent(patch string, truncated bool, files ...gitDiffFile) agent.Event {
	return agent.Event{Kind: "git_diff", Payload: agent.MakePayload(gitDiffPayload{
		Kind: "name_status", BaseRef: "main", Files: files, NumFiles: len(files),
		Patch: patch, PatchTruncated: truncated,
	})}
}

func claimsCfg() config {
	return config{runID: strandedRunID, stageID: strandedStageID, fixup: true}
}

func writeFixupSidecar(t *testing.T, cfg config, text string) string {
	t.Helper()
	path := fixupCommitMessagePath(cfg.runID, cfg.stageID)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEmitFixupCommitClaims_PeekDoesNotConsume: an unbacked claim is emitted as
// one fixup_commit_claims_unbacked event, the sidecar SURVIVES (the commit-time
// loader must still find it), and the LAST git_diff is the one consulted.
func TestEmitFixupCommitClaims_PeekDoesNotConsume(t *testing.T) {
	redirectCommitMessageDirs(t)
	cfg := claimsCfg()
	path := writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: edited a.go.\n\nItem 2: edited gone.go.\n")
	res := agent.Result{Events: []agent.Event{
		claimsGitDiffEvent("", false, gitDiffFile{Path: "gone.go", Status: "M"}),
		claimsGitDiffEvent("", false, gitDiffFile{Path: "a.go", Status: "M"}),
	}}
	var log strings.Builder
	emitFixupCommitClaims(cfg, &res, &log)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the peek consumed the sidecar: %v", err)
	}
	if len(res.Events) != 3 || res.Events[2].Kind != fixupCommitClaimsUnbackedEvent {
		t.Fatalf("events = %+v, want one appended %s", res.Events, fixupCommitClaimsUnbackedEvent)
	}
	var got fixupCommitClaimsEvidence
	if err := json.Unmarshal(res.Events[2].Payload, &got); err != nil {
		t.Fatal(err)
	}
	want := fixupCommitClaimsEvidence{Claims: []fixupCommitClaimEvidence{{Marker: "Item 2", FileRefs: []string{"gone.go"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %+v, want %+v (judged against the LAST git_diff)", got, want)
	}
	if !strings.Contains(log.String(), `"event":"fixup_commit_claims_unbacked"`) || !strings.Contains(log.String(), `"claims":1`) {
		t.Errorf("missing the count line:\n%s", log.String())
	}
}

// TestEmitFixupCommitClaims_RenameOldPathBacks: a renamed file backs a claim
// naming its old path.
func TestEmitFixupCommitClaims_RenameOldPathBacks(t *testing.T) {
	redirectCommitMessageDirs(t)
	cfg := claimsCfg()
	writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: moved old.go.\n")
	res := agent.Result{Events: []agent.Event{claimsGitDiffEvent("", false, gitDiffFile{Path: "new.go", Status: "R", OldPath: "old.go"})}}
	var log strings.Builder
	emitFixupCommitClaims(cfg, &res, &log)
	if len(res.Events) != 1 {
		t.Errorf("a rename's old path must back the claim: events = %+v", res.Events)
	}
}

// TestEmitFixupCommitClaims_PatchKnownness: test names are decided only on a
// non-empty, untruncated patch.
func TestEmitFixupCommitClaims_PatchKnownness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		patch     string
		truncated bool
		wantEvent bool
	}{
		{"known patch lacking the test", "+func TestOther(t *testing.T) {}", false, true},
		{"truncated patch", "+func TestOther(t *testing.T) {}", true, false},
		{"empty patch", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectCommitMessageDirs(t)
			cfg := claimsCfg()
			writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: TestNamed pins it.\n")
			res := agent.Result{Events: []agent.Event{claimsGitDiffEvent(tc.patch, tc.truncated, gitDiffFile{Path: "a.go", Status: "M"})}}
			var log strings.Builder
			emitFixupCommitClaims(cfg, &res, &log)
			if got := len(res.Events) == 2; got != tc.wantEvent {
				t.Errorf("event emitted = %v, want %v", got, tc.wantEvent)
			}
		})
	}
}

// TestEmitFixupCommitClaims_Unchecked: each precondition miss emits nothing and
// names its own reason; a non-fix-up pass is a silent no-op.
func TestEmitFixupCommitClaims_Unchecked(t *testing.T) {
	diff := claimsGitDiffEvent("", false, gitDiffFile{Path: "a.go", Status: "M"})
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, cfg config)
		events []agent.Event
		reason string
	}{
		{"sidecar absent", func(*testing.T, config) {}, []agent.Event{diff}, claimsUncheckedSidecarAbsent},
		{"sidecar unreadable", func(t *testing.T, cfg config) {
			if err := os.Mkdir(fixupCommitMessagePath(cfg.runID, cfg.stageID), 0o700); err != nil {
				t.Fatal(err)
			}
		}, []agent.Event{diff}, claimsUncheckedSidecarUnreadable},
		{"sidecar empty", func(t *testing.T, cfg config) { writeFixupSidecar(t, cfg, " \n\n") }, []agent.Event{diff}, claimsUncheckedSidecarEmpty},
		{"no git_diff", func(t *testing.T, cfg config) { writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: gone.go\n") }, nil, claimsUncheckedNoGitDiff},
		{"git_diff undecodable", func(t *testing.T, cfg config) { writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: gone.go\n") },
			[]agent.Event{{Kind: "git_diff", Payload: json.RawMessage(`{"files":"not-a-list"}`)}}, claimsUncheckedGitDiffUndecodable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectCommitMessageDirs(t)
			cfg := claimsCfg()
			tc.setup(t, cfg)
			res := agent.Result{Events: append([]agent.Event(nil), tc.events...)}
			var log strings.Builder
			emitFixupCommitClaims(cfg, &res, &log)
			if len(res.Events) != len(tc.events) {
				t.Errorf("events = %+v, want nothing appended", res.Events)
			}
			if !strings.Contains(log.String(), `"event":"fixup_commit_claims_unchecked"`) || !strings.Contains(log.String(), `"reason":"`+tc.reason+`"`) {
				t.Errorf("want fixup_commit_claims_unchecked reason %s:\n%s", tc.reason, log.String())
			}
		})
	}

	t.Run("non-fix-up pass", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		cfg := claimsCfg()
		cfg.fixup = false
		writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: gone.go\n")
		res := agent.Result{Events: []agent.Event{diff}}
		var log strings.Builder
		emitFixupCommitClaims(cfg, &res, &log)
		if len(res.Events) != 1 || log.Len() != 0 {
			t.Errorf("a non-fix-up pass must be a silent no-op: events=%d log=%q", len(res.Events), log.String())
		}
	})
}

// TestRun_Fixup_CommitClaimsReachGateEvidence is the run()-level wiring test: a
// fix-up whose commit-message sidecar (written by the agent, as in production)
// carries an Item paragraph naming a path the git_diff does not touch ships a
// bundle whose gate_evidence carries fixup_commit_claims_unbacked. MECHANISM:
// the sidecar names gone.go, absent from the fake git_diff by construction, so
// only main.go's `emitFixupCommitClaims(cfg, &res, logSink)` call produces the
// member; deleting it leaves gate_evidence without it.
func TestRun_Fixup_CommitClaimsReachGateEvidence(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	redirectCommitMessageDirs(t)
	cfg := claimsCfg()
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true, Events: []agent.Event{
			claimsGitDiffEvent("", false, gitDiffFile{Path: "a.go", Status: "M"}),
		}},
		onInvoke: func(int, agent.Invocation) {
			writeFixupSidecar(t, cfg, "fix: x\n\nItem 1: edited gone.go.\n")
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = fixupLandingPrompt()
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")

	var stderr strings.Builder
	got := run([]string{
		"--run-id", strandedRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", strandedStageID,
		"--fetch-prompt", "--upload-trace",
		"--bundle-out", bundlePath,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	var member json.RawMessage
	for _, ev := range bundleEventsForTest(t, bundlePath) {
		if ev.Kind != "gate_evidence" {
			continue
		}
		var all map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &all); err != nil {
			t.Fatal(err)
		}
		member = all["fixup_commit_claims_unbacked"]
	}
	if want := `{"claims":[{"marker":"Item 1","file_refs":["gone.go"]}]}`; string(member) != want {
		t.Fatalf("gate_evidence.fixup_commit_claims_unbacked = %s, want %s\n%s", member, want, stderr.String())
	}
}
