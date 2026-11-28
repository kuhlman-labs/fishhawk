package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// #5410: a non-composed first-pass PR body is rebuilt from the run-branch commit
// message when the agent wrote its initial-implement commit-message sidecar.

const (
	rebuildSidecarSubject = "feat(runner): rebuild widget summary"
	rebuildSidecarBody    = "Adds the rebuild widget so a first-pass PR carries a summary."

	// notComposedMarkerFirstLine is the #3012 marker's first line, which the
	// rebuilt marker keeps byte-identical.
	notComposedMarkerFirstLine = "> **This pull-request body was not composed by the implement agent.**"
	// rebuiltMarkerLine is the line only the rebuilt marker carries.
	rebuiltMarkerLine = "Rebuilt by the runner from the run-branch commit message"
	// rebuiltMarkerCost is the CONDITIONAL cost line (approval condition U1):
	// the marker must not assert facts about the copied commit text.
	rebuiltMarkerCost = "unless the summary below carries a closing reference, merging will not auto-close the trigger issue"
	// unconditionalCostClaim is the #3012 placeholder marker's unconditional
	// claim, which a rebuilt marker must never carry.
	unconditionalCostClaim = "so merging will not auto-close the trigger issue"
	// placeholderBodyLine is the runner placeholder body a rebuild replaces.
	placeholderBodyLine = "Opened by Fishhawk for run"
)

// rebuildShip drives the REAL openPRAndShipArtifact on the verifiedTreeRepo
// fixture with the PR handoff arranged by arrange and, when sidecar is non-empty,
// an initial-implement commit-message sidecar written. With sidecar empty the
// sidecar dir is redirected to an EMPTY temp dir so no stray /tmp file can feed
// the run. It returns the opened PR title/body, the decoded artifact, the runner
// log and the commit message pushed to the bare remote.
type rebuildShipResult struct {
	title, body string
	artifact    map[string]any
	logs        string
	pushedMsg   string
	keyed       string
	branch      string
}

func rebuildShip(t *testing.T, arrange func(t *testing.T, keyedPath string), sidecar string) rebuildShipResult {
	t.Helper()
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keyed := withPRDescriptionPath(t, verifiedTreeRunID, verifiedTreeStageID)
	arrange(t, keyed)
	cfg := verifiedTreeCfg(repo, "true")
	if sidecar != "" {
		writeImplementCommitMsgSidecar(t, cfg, sidecar)
	} else {
		orig := implementCommitMessageDir
		implementCommitMessageDir = t.TempDir()
		t.Cleanup(func() { implementCommitMessageDir = orig })
	}

	var logSink strings.Builder
	if err := openPRAndShipArtifact(context.Background(), cfg,
		&logSink, fu, issued, "", false, false, nil, false, "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("openPRAndShipArtifact: %v\n%s", err, logSink.String())
	}
	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest not called")
	}
	var artifact map[string]any
	if err := json.Unmarshal(fu.gotPRArgs.Body, &artifact); err != nil {
		t.Fatalf("decode shipped artifact: %v", err)
	}
	out, err := exec.Command("git", "-C", bare, "log", "-1", "--format=%B", branch).Output()
	if err != nil {
		t.Fatalf("read pushed commit message: %v", err)
	}
	return rebuildShipResult{
		title:     fpr.gotArgs.Title,
		body:      fpr.gotArgs.Body,
		artifact:  artifact,
		logs:      logSink.String(),
		pushedMsg: string(out),
		keyed:     keyed,
		branch:    branch,
	}
}

// pickEvent decodes the single log line carrying event, or fails.
func pickEvent(t *testing.T, logs, event string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"event":"`+event+`"`) {
			var got map[string]any
			if err := json.Unmarshal([]byte(line), &got); err != nil {
				t.Fatalf("decode %s %q: %v", event, line, err)
			}
			return got
		}
	}
	t.Fatalf("expected %s in the log:\n%s", event, logs)
	return nil
}

// agentHandoffTitle is the title the body_absent cases' handoff carries — it
// DIFFERS from the sidecar subject, so a title rewrite on an agent-authored
// handoff is observable (counterfactual 2).
func agentHandoffTitle(name string) string {
	if name == "body_absent via a separator with nothing after it" {
		return "fix(runner): a title"
	}
	return "fix(runner): a title and nothing else"
}

// TestOpenPRAndShipArtifact_RebuildsBodyFromCommitMessage is the #5410
// done-means test: every #3012 composition-failure branch, with the commit-
// message sidecar written, ships a PR body rebuilt from that commit message
// through the REAL call site — and the composed branch is untouched.
func TestOpenPRAndShipArtifact_RebuildsBodyFromCommitMessage(t *testing.T) {
	sidecar := rebuildSidecarSubject + "\n\n" + rebuildSidecarBody + "\n"
	for _, tc := range prBodyHandoffCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := rebuildShip(t, tc.arrange, sidecar)

			// Ordering control: the commit message is the sidecar, byte for
			// byte (plus only the pusher's DCO sign-off trailer), and neither
			// the marker nor the rebuilt summary reached it.
			msg := strings.TrimRight(got.pushedMsg, "\n")
			rest, ok := strings.CutPrefix(msg, rebuildSidecarSubject+"\n\n"+rebuildSidecarBody)
			if rest = strings.TrimSpace(rest); !ok || (rest != "" && !strings.HasPrefix(rest, "Signed-off-by: ")) || strings.Count(rest, "\n") > 0 {
				t.Errorf("pushed commit message = %q, want the sidecar (+ sign-off trailer)", msg)
			}
			if strings.Contains(got.pushedMsg, "was not composed") || strings.Contains(got.pushedMsg, "## Summary") {
				t.Errorf("the rebuild must not reach the commit message:\n%s", got.pushedMsg)
			}
			if got.artifact["body"] != got.body {
				t.Errorf("artifact body != opened PR body:\nartifact: %v\nopened:   %q", got.artifact["body"], got.body)
			}
			if got.artifact["title"] != got.title {
				t.Errorf("artifact title %v != opened title %q", got.artifact["title"], got.title)
			}
			footer := prAttributionFooter(verifiedTreeCfg("", ""), got.branch)

			if tc.want == prBodyReasonNone {
				if strings.Contains(got.body, "was not composed") || strings.Contains(got.body, rebuiltMarkerLine) {
					t.Errorf("a composed body must carry no marker:\n%s", got.body)
				}
				if !strings.HasPrefix(got.body, "## Summary\n\n- real work\n\nCloses #3012") || !strings.HasSuffix(got.body, footer) {
					t.Errorf("a composed body must be the agent's body + footer:\n%s", got.body)
				}
				if got.title != "fix(runner): a real subject" {
					t.Errorf("composed title = %q, want the agent's", got.title)
				}
				if strings.Contains(got.logs, "pr_body_rebuilt") {
					t.Errorf("a composed ship must emit no pr_body_rebuilt:\n%s", got.logs)
				}
				return
			}

			if !strings.HasPrefix(got.body, notComposedMarkerFirstLine+"\n") {
				t.Errorf("rebuilt body must START with the unchanged marker first line:\n%s", got.body)
			}
			for _, want := range []string{
				rebuiltMarkerLine,
				rebuiltMarkerCost,
				"## Summary\n\n" + rebuildSidecarSubject + "\n\n" + rebuildSidecarBody + footer,
				string(tc.want),
				got.keyed,
			} {
				if !strings.Contains(got.body, want) {
					t.Errorf("rebuilt body missing %q:\n%s", want, got.body)
				}
			}
			if strings.Contains(got.body, unconditionalCostClaim) {
				t.Errorf("rebuilt marker must not carry the unconditional cost claim:\n%s", got.body)
			}
			if strings.Contains(got.body, placeholderBodyLine) {
				t.Errorf("rebuilt body must not carry the placeholder:\n%s", got.body)
			}
			if strings.Count(got.body, "\n---\n_Opened by [Fishhawk]") != 1 {
				t.Errorf("rebuilt body must carry exactly one footer:\n%s", got.body)
			}
			if r := got.artifact["pr_body_fallback_reason"]; r != string(tc.want) {
				t.Errorf("artifact pr_body_fallback_reason = %v, want %q", r, tc.want)
			}
			nc := pickEvent(t, got.logs, "pr_body_not_composed")
			if nc["reason"] != string(tc.want) {
				t.Errorf("pr_body_not_composed reason = %v, want %q", nc["reason"], tc.want)
			}
			rb := pickEvent(t, got.logs, "pr_body_rebuilt")
			wantTitle, wantTitleRebuilt := rebuildSidecarSubject, true
			if tc.agentAuthored {
				wantTitle, wantTitleRebuilt = agentHandoffTitle(tc.name), false
			}
			if rb["reason"] != string(tc.want) || rb["source"] != "commit_message" ||
				rb["title_rebuilt"] != wantTitleRebuilt ||
				rb["run_id"] != verifiedTreeRunID || rb["stage_id"] != verifiedTreeStageID {
				t.Errorf("pr_body_rebuilt = %v, want reason=%q source=commit_message title_rebuilt=%t", rb, tc.want, wantTitleRebuilt)
			}
			if got.title != wantTitle {
				t.Errorf("title = %q, want %q", got.title, wantTitle)
			}
		})
	}
}

// TestOpenPRAndShipArtifact_RebuildCarriesClosingReferenceVerbatim is approval
// condition U1: a sidecar body that DOES carry `Closes #12` and a `## Test plan`
// lands verbatim under `## Summary`, and the marker still uses the conditional
// wording rather than claiming the merge will not auto-close.
func TestOpenPRAndShipArtifact_RebuildCarriesClosingReferenceVerbatim(t *testing.T) {
	sidecarBody := "Adds the widget.\n\n## Test plan\n\n- [ ] run the widget test\n\nCloses #12"
	got := rebuildShip(t, func(*testing.T, string) {}, rebuildSidecarSubject+"\n\n"+sidecarBody+"\n")

	if !strings.Contains(got.body, "## Summary\n\n"+rebuildSidecarSubject+"\n\n"+sidecarBody) {
		t.Errorf("sidecar body (Test plan + Closes #12) must ride verbatim under ## Summary:\n%s", got.body)
	}
	if !strings.Contains(got.body, rebuiltMarkerCost) {
		t.Errorf("marker must use the conditional cost wording:\n%s", got.body)
	}
	if strings.Contains(got.body, unconditionalCostClaim) {
		t.Errorf("marker must not claim unconditionally that merging will not auto-close:\n%s", got.body)
	}
	if strings.Contains(got.body, "no summary, no test plan and no `Closes #N`") {
		t.Errorf("marker must not assert the copied text has no test plan / Closes:\n%s", got.body)
	}
}

// TestOpenPRAndShipArtifact_NoSidecarKeepsPlaceholder pins the no-sidecar branch:
// no PR handoff AND no commit-message sidecar ships today's placeholder plus the
// #3012 marker, and nothing claims a rebuild.
func TestOpenPRAndShipArtifact_NoSidecarKeepsPlaceholder(t *testing.T) {
	got := rebuildShip(t, func(*testing.T, string) {}, "")

	// The handoff dir is still redirected to the (empty) temp dir, so this
	// yields today's placeholder body for the same run/stage/branch.
	cfg := verifiedTreeCfg("", "")
	_, placeholder, _, _ := prTitleAndBodyParts(cfg, got.branch, &strings.Builder{})
	want := prBodyNotComposedMarker(cfg, prBodyReasonHandoffAbsent, got.keyed) + placeholder
	if got.body != want {
		t.Errorf("no-sidecar body changed:\n got: %q\nwant: %q", got.body, want)
	}
	if !strings.HasPrefix(got.title, "chore: fishhawk implement stage ") {
		t.Errorf("no-sidecar title = %q, want the placeholder", got.title)
	}
	if strings.Contains(got.body, rebuiltMarkerLine) || strings.Contains(got.body, "## Summary") {
		t.Errorf("no-sidecar body must not claim a rebuild:\n%s", got.body)
	}
	if strings.Contains(got.logs, "pr_body_rebuilt") {
		t.Errorf("no-sidecar ship must emit no pr_body_rebuilt:\n%s", got.logs)
	}
	pickEvent(t, got.logs, "pr_body_not_composed")
}

// TestRebuildPRTextFromCommitMessage drives the pure helper.
func TestRebuildPRTextFromCommitMessage(t *testing.T) {
	cfg := config{runID: "r", stageID: "s", backendURL: "https://x"}
	const branch = "fishhawk/run-r/stage-s"
	footer := prAttributionFooter(cfg, branch)
	cases := []struct {
		name          string
		title         string
		agentAuthored bool
		cm            commitMessageSource
		wantRebuilt   bool
		wantTitle     string
		wantSummary   string
	}{
		{
			name:        "subject and body",
			title:       "chore: fishhawk implement stage s",
			cm:          commitMessageSource{subject: "feat: x", body: "why x", fromSidecar: true},
			wantRebuilt: true,
			wantTitle:   "feat: x",
			wantSummary: "## Summary\n\nfeat: x\n\nwhy x" + footer,
		},
		{
			name:        "subject-only sidecar has no dangling separator",
			title:       "chore: fishhawk implement stage s",
			cm:          commitMessageSource{subject: "feat: x", fromSidecar: true},
			wantRebuilt: true,
			wantTitle:   "feat: x",
			wantSummary: "## Summary\n\nfeat: x" + footer,
		},
		{
			name:          "body_absent keeps the agent title",
			title:         "fix: the agent's title",
			agentAuthored: true,
			cm:            commitMessageSource{subject: "feat: x", body: "why x", fromSidecar: true},
			wantRebuilt:   true,
			wantTitle:     "fix: the agent's title",
			wantSummary:   "## Summary\n\nfeat: x\n\nwhy x" + footer,
		},
		{
			name:        "fromSidecar=false declines",
			title:       "chore: fishhawk implement stage s",
			cm:          commitMessageSource{subject: "feat: x", body: "why x"},
			wantRebuilt: false,
			wantTitle:   "chore: fishhawk implement stage s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body, rebuilt := rebuildPRTextFromCommitMessage(cfg, branch, tc.title, tc.agentAuthored, prBodyReasonHandoffAbsent, "/h", tc.cm)
			if rebuilt != tc.wantRebuilt {
				t.Fatalf("rebuilt = %t, want %t", rebuilt, tc.wantRebuilt)
			}
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if !tc.wantRebuilt {
				if body != "" {
					t.Errorf("a declined rebuild must return no body, got %q", body)
				}
				return
			}
			want := prBodyRebuiltMarker(cfg, prBodyReasonHandoffAbsent, "/h", implementCommitMessagePath("r", "s")) + tc.wantSummary
			if body != want {
				t.Errorf("body:\n got: %q\nwant: %q", body, want)
			}
			if strings.Count(body, "\n---\n_Opened by [Fishhawk]") != 1 || !strings.HasSuffix(body, footer) {
				t.Errorf("body must end with exactly one attribution footer:\n%s", body)
			}
		})
	}
}

// TestImplementCommitMessageParts_Source pins the source flag the rebuild keys
// on, and that the message is byte-identical to implementCommitMessage's.
func TestImplementCommitMessageParts_Source(t *testing.T) {
	cfg := implementCommitMsgCfg()
	t.Run("sidecar", func(t *testing.T) {
		writeImplementCommitMsgSidecar(t, cfg, "feat: a\n\nb\n")
		msg, src := implementCommitMessageParts(cfg, "T", "B", &strings.Builder{})
		if msg != "feat: a\n\nb" || src != (commitMessageSource{subject: "feat: a", body: "b", fromSidecar: true}) {
			t.Errorf("got %q %+v", msg, src)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		orig := implementCommitMessageDir
		implementCommitMessageDir = t.TempDir()
		t.Cleanup(func() { implementCommitMessageDir = orig })
		msg, src := implementCommitMessageParts(cfg, "T", "B", &strings.Builder{})
		if msg != "T\n\nB" || src != (commitMessageSource{}) {
			t.Errorf("got %q %+v", msg, src)
		}
	})
}
