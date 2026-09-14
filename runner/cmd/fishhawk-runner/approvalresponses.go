package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// approvalConditionsHeading is the stable, TrimSpace-exact heading line the
// implement prompt asks the agent to end its commit-message body with when
// operator approval conditions are attached (#3400). The backend's implement
// prompt names the SAME literal (backend/internal/prompt: the
// `Approval conditions:` instruction); the two modules cannot import each
// other, so each pins the literal in its own tests. Matching is exact on the
// TrimSpace'd line — `## Approval conditions` or a lower-case variant does NOT
// match — so the section can only be opened by the grammar the prompt taught.
const approvalConditionsHeading = "Approval conditions:"

// approvalConditionResponsesMaxBytes bounds the section text carried into
// gate_evidence (#3400). The text is agent-authored free text that reaches the
// reviewer prompt inside an UNTRUSTED envelope; 4 KiB is generous for one
// entry per condition and keeps the prompt cost bounded. Over the bound the
// text is cut on a rune boundary and the event carries truncated=true.
const approvalConditionResponsesMaxBytes = 4096

// parseCommitMessageSidecar splits a raw commit-message sidecar into
// (subject, body): CRLF-normalized, TrimSpace'd (so leading blank lines and an
// empty/whitespace-only file are handled), first line is the subject and the
// TrimSpace'd remainder is the body. ok=false when the sidecar is
// empty/whitespace-only. It is the ONE parse both the consuming loaders
// (loadImplementCommitMessage / loadFixupCommitMessage) and the pre-pack peek
// (peekApprovalConditionResponses) use, so the section the peek reports is
// extracted from exactly the body the commit will carry.
func parseCommitMessageSidecar(raw []byte) (subject, body string, ok bool) {
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if text == "" {
		return "", "", false
	}
	lines := strings.SplitN(text, "\n", 2)
	subject = strings.TrimSpace(lines[0])
	if len(lines) == 2 {
		body = strings.TrimSpace(lines[1])
	}
	return subject, body, true
}

// extractApprovalConditionResponses returns the text AFTER the first line whose
// TrimSpace equals approvalConditionsHeading, trimmed, and found=true. found is
// false when no such line exists or when the remainder is whitespace-only (a
// bare heading with no entries is NOT a recorded response). The FIRST matching
// heading wins so a later duplicate cannot hide entries from the reviewer.
func extractApprovalConditionResponses(body string) (text string, found bool) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != approvalConditionsHeading {
			continue
		}
		rest := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if rest == "" {
			return "", false
		}
		return rest, true
	}
	return "", false
}

// boundApprovalConditionResponses cuts text to approvalConditionResponsesMaxBytes
// on a rune boundary, reporting whether it cut anything.
func boundApprovalConditionResponses(text string) (string, bool) {
	if len(text) <= approvalConditionResponsesMaxBytes {
		return text, false
	}
	cut := approvalConditionResponsesMaxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// approvalConditionResponsesSource names which commit-message sidecar a
// peeked section came from, by pass kind.
func approvalConditionResponsesSource(cfg config) (path, source string) {
	if cfg.fixup {
		return fixupCommitMessagePath(cfg.runID, cfg.stageID), "fixup_commitmsg"
	}
	return implementCommitMessagePath(cfg.runID, cfg.stageID), "implement_commitmsg"
}

// peekApprovalConditionResponses reads the pass's commit-message sidecar
// (implement or fix-up, by cfg.fixup) WITHOUT deleting it, extracts the
// `Approval conditions:` section from its body, and returns an
// approval_condition_responses event for composeGateEvidence to fold into
// gate_evidence (#3400). nil when the sidecar is absent, unreadable, over the
// maxSidecarBytes ceiling, empty, or carries no section — every case the
// consuming loader ALSO treats as absent (same readSidecarBounded, same
// parseCommitMessageSidecar), so the section the reviewer sees can only be one
// the loader would carry into the commit message. The oversize path logs
// nothing here: the loader already emits *_commitmsg_oversize when it consumes.
//
// The peek runs at bundle-pack time, BEFORE the commit exists (#742 forward-
// gated upload): what it captures is the agent's PROPOSED commit message. The
// event carries nothing that asserts the commit was made; the runner logs
// approval_condition_responses_committed only after CommitAndPush returns a
// head (logApprovalConditionResponsesCommitted), so a `captured` line with no
// later `committed` line is the unpersisted state.
//
// The text is pre-redacted with redactString: the implement review dispatches
// on the RAW bundle (#793), so a credential the agent pasted into its commit
// body would otherwise reach the reviewer prompt unredacted. Redaction runs
// BEFORE the byte bound: a credential straddling the cut would otherwise be
// truncated to a prefix the patterns no longer recognize (ghp_ needs exactly
// 36 trailing chars), leaking the leading bytes of a token; redacting the
// full text first means the cut can only fall inside a marker, never a
// secret, and the bound still holds because it is applied last.
func peekApprovalConditionResponses(cfg config, logSink io.Writer) *agent.Event {
	path, source := approvalConditionResponsesSource(cfg)
	// readSidecarBounded is the ceiling control: over maxSidecarBytes it
	// returns errSidecarTooLarge with NIL bytes, so an oversize sidecar is
	// absent here exactly as it is to the consuming loader (which owns the
	// *_commitmsg_oversize diagnostic). Not-exist and any other read error are
	// absent too — only an existing, readable file can carry a section.
	raw, err := readSidecarBounded(path)
	if err != nil {
		return nil
	}
	_, body, ok := parseCommitMessageSidecar(raw)
	if !ok {
		return nil
	}
	text, found := extractApprovalConditionResponses(body)
	if !found {
		return nil
	}
	text, _ = redactString(text)
	text, truncated := boundApprovalConditionResponses(text)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"approval_condition_responses_captured","run_id":%q,"stage_id":%q,"source":%q,"bytes":%d,"truncated":%t}`+"\n",
		cfg.runID, cfg.stageID, source, len(text), truncated)
	return &agent.Event{
		Kind: "approval_condition_responses",
		Payload: agent.MakePayload(map[string]any{
			"run_id":    cfg.runID,
			"stage_id":  cfg.stageID,
			"source":    source,
			"text":      text,
			"truncated": truncated,
		}),
	}
}

// logApprovalConditionResponsesCommitted is the persistence half of the peek
// (#3400): called after CommitAndPush returns a head, it logs
// approval_condition_responses_committed {head_sha, source} when the commit
// message the runner actually committed carries an `Approval conditions:`
// section. A peek-time `approval_condition_responses_captured` line with NO
// later `committed` line is therefore the distinguishable unpersisted state —
// the commit failed, parked, or the message fell back to a synthetic one —
// rather than reading identically to a recorded response. No head (NoChanges)
// or no section logs nothing.
func logApprovalConditionResponsesCommitted(cfg config, commitMessage, headSHA string, logSink io.Writer) {
	if headSHA == "" {
		return
	}
	_, body, ok := parseCommitMessageSidecar([]byte(commitMessage))
	if !ok {
		return
	}
	if _, found := extractApprovalConditionResponses(body); !found {
		return
	}
	_, source := approvalConditionResponsesSource(cfg)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"approval_condition_responses_committed","run_id":%q,"stage_id":%q,"source":%q,"head_sha":%q}`+"\n",
		cfg.runID, cfg.stageID, source, headSHA)
}
