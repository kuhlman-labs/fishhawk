package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Commit-message claims cross-check (#4337).
//
// The incident (run af08d34e): a fix-up pass's commit message said routed items
// were done and counterfactual-checked, but the commit carried none of their
// work — the file those items needed was never touched. Nothing compared the
// message with the diff, so the claim reached the operator unchallenged.
//
// emitFixupCommitClaims PEEKS the fix-up commit-message sidecar (the same
// readSidecarBounded + parseCommitMessageSidecar pair the consuming loader and
// the #3400 approval-responses peek use, so the checked text is exactly what
// the commit will carry), splits its body into blank-line paragraphs, and
// treats a paragraph whose first line leads with `Item N` / `Finding X` /
// `Concern X` as a CLAIM. A claim paragraph that names evidence — file paths,
// and Test/Fuzz/Benchmark/Example names when the patch is known — of which this
// pass's own scope-bounded git_diff touches NONE is UNBACKED and is reported in
// a fixup_commit_claims_unbacked event, folded into gate_evidence for the
// implement review.
//
// ADVISORY and heuristic. It never touches res.OK / FailureCategory / budget.
// Two documented residuals: a paragraph naming only identifiers (no path, no
// test name) is undecidable and silent — the incident's own `Item 4` is one;
// and a claim paragraph that names only untouched CONTEXT files is a false
// positive, which is why the review render tells the reviewer to verify first.
// A paragraph holding several items is judged as a unit under its first marker.
//
// Every emitted token is re-matched against a fixed charset, so only
// `[A-Za-z0-9_./-]` (plus the marker's ` #,` ) ever leaves the runner, and a
// token the default redaction patterns match is dropped: no free agent text
// reaches the review prompt through this member. The tokens come from the
// commit message the pass is about to commit, so they carry nothing the PR
// itself would not.

// fixupClaimsMaxClaims bounds the claims one event carries; past it the event
// sets truncated=true.
const fixupClaimsMaxClaims = 16

// fixupClaimsMaxTokensPerKind bounds the file refs and the test names carried
// per claim. The backing decision is made over every token first.
const fixupClaimsMaxTokensPerKind = 8

// fixupClaimTokenMaxBytes bounds one emitted token; a longer token is dropped
// before the decision, never truncated into a different path.
const fixupClaimTokenMaxBytes = 256

// fixupCommitClaimsUnbackedEvent is the trace event kind the cross-check emits
// and composeGateEvidence folds into gate_evidence.fixup_commit_claims_unbacked.
const fixupCommitClaimsUnbackedEvent = "fixup_commit_claims_unbacked"

// Bounded fixup_commit_claims_unchecked reasons.
const (
	claimsUncheckedSidecarAbsent      = "sidecar_absent"
	claimsUncheckedSidecarUnreadable  = "sidecar_unreadable"
	claimsUncheckedSidecarEmpty       = "sidecar_empty"
	claimsUncheckedNoGitDiff          = "no_git_diff"
	claimsUncheckedGitDiffUndecodable = "git_diff_undecodable"
)

var (
	// fixupClaimLeadRe matches a claim paragraph's lead on its TrimSpace'd
	// first line; group 1 is the keyword, group 2 the id (with its optional #).
	fixupClaimLeadRe = regexp.MustCompile(`^(?:[-*]\s+)?((?i:items?|findings?|concerns?))\s+(#?[A-Za-z0-9][A-Za-z0-9.,-]{0,15})`)
	// fixupClaimFileRefRe matches a file reference. The leftmost-longest path
	// prefix is taken; a trailing `:<line>` or punctuation falls outside the
	// match. Longer extensions precede their prefixes in the alternation.
	fixupClaimFileRefRe = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.(?:go|md|tsx|ts|mjs|js|yaml|yml|json|sql|sh|py|toml|txt)\b`)
	// fixupClaimTestNameRe matches a Go test, fuzz, benchmark or example name.
	fixupClaimTestNameRe = regexp.MustCompile(`\b(?:Test|Fuzz|Benchmark|Example)[A-Z_][A-Za-z0-9_]*`)
	// fixupClaimTokenCharsetRe is the emit-time re-validation every file ref and
	// test name must pass in full.
	fixupClaimTokenCharsetRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	// fixupClaimMarkerCharsetRe is the emit-time re-validation of a marker.
	fixupClaimMarkerCharsetRe = regexp.MustCompile(`^[A-Za-z]{4,8} #?[A-Za-z0-9][A-Za-z0-9.,-]{0,15}$`)
)

// fixupCommitClaim is one unbacked claim paragraph: its normalized lead marker
// and the decidable evidence tokens it names, none of which the diff backs.
type fixupCommitClaim struct {
	Marker    string
	FileRefs  []string
	TestNames []string
}

// checkFixupCommitClaims is the pure core. body is the commit-message body
// (subject excluded); changed the paths the pass's diff changes; patch its
// unified diff, consulted for test names only when patchKnown (non-empty and not
// truncated). It returns the unbacked claims, at most fixupClaimsMaxClaims, and
// whether more were found.
func checkFixupCommitClaims(body string, changed []string, patch string, patchKnown bool) (claims []fixupCommitClaim, truncated bool) {
	for _, para := range fixupClaimParagraphs(body) {
		marker, ok := fixupClaimMarker(para[0])
		if !ok {
			continue
		}
		text := strings.Join(para, "\n")
		refs := fixupClaimTokens(fixupClaimFileRefRe.FindAllString(text, -1), true)
		var tests []string
		if patchKnown {
			tests = fixupClaimTokens(fixupClaimTestNameRe.FindAllString(text, -1), false)
		}
		if len(refs) == 0 && len(tests) == 0 {
			continue // identifier-only: undecidable, silent
		}
		backed := false
		for _, ref := range refs {
			if fileRefBacked(ref, changed) {
				backed = true
				break
			}
		}
		for _, name := range tests {
			if strings.Contains(patch, name) {
				backed = true
				break
			}
		}
		if backed {
			continue
		}
		if len(claims) == fixupClaimsMaxClaims {
			return claims, true
		}
		claims = append(claims, fixupCommitClaim{
			Marker:    marker,
			FileRefs:  capClaimTokens(refs),
			TestNames: capClaimTokens(tests),
		})
	}
	return claims, false
}

// fixupClaimParagraphs splits body into blank-line paragraphs of TrimSpace'd
// lines, STOPPING at the `Approval conditions:` heading: that section is the
// #3400 approval-responses channel and is not an item claim.
func fixupClaimParagraphs(body string) [][]string {
	var (
		paras [][]string
		cur   []string
	)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == approvalConditionsHeading {
			break
		}
		if line == "" {
			if len(cur) > 0 {
				paras = append(paras, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		paras = append(paras, cur)
	}
	return paras
}

// fixupClaimMarker returns the normalized lead of a claim paragraph's first
// line — `<keyword> <id>`, with the bullet dropped, the whitespace collapsed and
// trailing `.,-` trimmed off the id — and ok=false when the line is no claim.
func fixupClaimMarker(first string) (string, bool) {
	m := fixupClaimLeadRe.FindStringSubmatch(first)
	if m == nil {
		return "", false
	}
	id := strings.TrimRight(m[2], ".,-")
	if id == "" || id == "#" {
		return "", false
	}
	marker := m[1] + " " + id
	if !fixupClaimMarkerCharsetRe.MatchString(marker) {
		return "", false
	}
	return marker, true
}

// fixupClaimTokens dedupes raw matches in first-seen order, dropping any that
// fail the charset re-validation or the length bound, or that the default
// redaction patterns match (a credential-shaped token is dropped whole, never
// shipped as a marker). File refs lose a leading `./`, which no diff path
// carries.
func fixupClaimTokens(raw []string, fileRefs bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range raw {
		if fileRefs {
			for strings.HasPrefix(tok, "./") {
				tok = strings.TrimPrefix(tok, "./")
			}
		}
		if tok == "" || len(tok) > fixupClaimTokenMaxBytes || !fixupClaimTokenCharsetRe.MatchString(tok) || seen[tok] {
			continue
		}
		if _, hits := redactString(tok); len(hits) > 0 {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// capClaimTokens bounds one claim's tokens of a kind.
func capClaimTokens(tokens []string) []string {
	if len(tokens) > fixupClaimsMaxTokensPerKind {
		return tokens[:fixupClaimsMaxTokensPerKind]
	}
	return tokens
}

// fileRefBacked reports whether the diff changes the referenced file: an exact
// path, or a path ending in `/<ref>` (a message naming `upload.go` or
// `internal/upload/upload.go` for runner/internal/upload/upload.go).
func fileRefBacked(ref string, changed []string) bool {
	for _, p := range changed {
		if p == ref || strings.HasSuffix(p, "/"+ref) {
			return true
		}
	}
	return false
}

// lastGitDiff decodes the LAST git_diff event on events — the reconciled
// scope-bounded diff (the verify-fix loop's re-emit supersedes the first, the
// backend's ExtractDiff last-write-wins rule) — into the changed paths (renames
// contribute their old path too), the patch and its truncation flag. found is
// false when no git_diff event exists; err non-nil when the last one does not
// decode.
func lastGitDiff(events []agent.Event) (changed []string, patch string, truncated, found bool, err error) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "git_diff" {
			continue
		}
		var p gitDiffPayload
		if uerr := json.Unmarshal(events[i].Payload, &p); uerr != nil {
			return nil, "", false, true, uerr
		}
		for _, f := range p.Files {
			changed = append(changed, f.Path)
			if f.OldPath != "" {
				changed = append(changed, f.OldPath)
			}
		}
		return changed, p.Patch, p.PatchTruncated, true, nil
	}
	return nil, "", false, false, nil
}

// emitFixupCommitClaims runs the cross-check for a fix-up pass and appends a
// fixupCommitClaimsUnbackedEvent to res.Events when at least one claim is
// unbacked. It PEEKS the sidecar — never deletes it; the commit-time loader
// still consumes it. Every precondition miss logs one
// fixup_commit_claims_unchecked {reason} line and emits nothing. EVIDENCE ONLY.
func emitFixupCommitClaims(cfg config, res *agent.Result, logSink io.Writer) {
	if !cfg.fixup {
		return
	}
	unchecked := func(reason, detail string) {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"fixup_commit_claims_unchecked","run_id":%q,"stage_id":%q,"reason":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, reason, detail)
	}
	raw, err := readSidecarBounded(fixupCommitMessagePath(cfg.runID, cfg.stageID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			unchecked(claimsUncheckedSidecarAbsent, "")
		} else {
			unchecked(claimsUncheckedSidecarUnreadable, err.Error())
		}
		return
	}
	_, body, ok := parseCommitMessageSidecar(raw)
	if !ok {
		unchecked(claimsUncheckedSidecarEmpty, "")
		return
	}
	changed, patch, patchTruncated, found, derr := lastGitDiff(res.Events)
	if !found {
		unchecked(claimsUncheckedNoGitDiff, "")
		return
	}
	if derr != nil {
		unchecked(claimsUncheckedGitDiffUndecodable, derr.Error())
		return
	}
	claims, truncated := checkFixupCommitClaims(body, changed, patch, patch != "" && !patchTruncated)
	if len(claims) == 0 {
		return
	}
	ev := fixupCommitClaimsEvidence{Truncated: truncated}
	for _, c := range claims {
		ev.Claims = append(ev.Claims, fixupCommitClaimEvidence(c))
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"fixup_commit_claims_unbacked","run_id":%q,"stage_id":%q,"claims":%d,"truncated":%t}`+"\n",
		cfg.runID, cfg.stageID, len(claims), truncated)
	res.Events = append(res.Events, agent.Event{
		Kind:    fixupCommitClaimsUnbackedEvent,
		Payload: agent.MakePayload(ev),
	})
}
