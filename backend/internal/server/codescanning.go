package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
)

// codeScanningLister is the subset of *githubclient.Client the
// code_scanning_alert ingest needs (#1096). Declared as an interface
// so the ingest core is unit-testable with a small fake
// (codescanning_test.go) without standing up a real REST client +
// token provider; production passes s.cfg.GitHub, which satisfies it.
type codeScanningLister interface {
	ListCodeScanningAlertsScoped(ctx context.Context, scope forge.CredentialScope, repo githubclient.RepoRef, ref string) ([]securityscan.Finding, error)
}

// codeScanningAlertPayload is the slice of GitHub's code_scanning_alert
// webhook payload the ingest reads. We re-fetch the full open-alert set
// from the REST API on every delivery (the payload carries one alert;
// the gate needs the whole set), so only the routing fields matter:
// the repo, the ref/commit the scan ran against (to match a run), and
// the installation. Doc:
// https://docs.github.com/en/webhooks/webhook-events-and-payloads#code_scanning_alert
type codeScanningAlertPayload struct {
	Action    string `json:"action"`
	Ref       string `json:"ref"`
	CommitOID string `json:"commit_oid"`
	Alert     struct {
		Number             int `json:"number"`
		MostRecentInstance struct {
			Ref       string `json:"ref"`
			CommitSHA string `json:"commit_sha"`
		} `json:"most_recent_instance"`
	} `json:"alert"`
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// securityScanAuditPayload is the body of the securityscan audit entry
// the ingest records (category securityscan.AuditCategorySecurityFindings).
// The merge gate (auditcomplete) reads Findings to decide whether to
// hold the merge, and the run-status / REST surfaces render them. A
// non-empty Findings means "high-severity findings on the implement
// diff are unresolved"; an entry is recorded only when the filtered
// state is meaningful (see recordSecurityScan).
type securityScanAuditPayload struct {
	HeadSHA  string                 `json:"head_sha"`
	Ref      string                 `json:"ref"`
	PRNumber int                    `json:"pr_number,omitempty"`
	Findings []securityscan.Finding `json:"findings"`
}

// ingestCodeScanningAlert handles a GitHub `code_scanning_alert` event
// (#1096): it matches the alert to a Fishhawk run, fetches + filters the
// repo's open code-scanning alerts to the high-severity findings on the
// run's implement diff, and records ONE idempotent securityscan audit
// entry (floored on the latest fix-up) the merge gate reads. This is
// what catches a new CodeQL/SAST finding on the implement diff in-loop
// (routable via fixup_stage) instead of first as a blocked required
// check at merge.
//
// Best-effort throughout — audit-only, never a 5xx. Wired off s.cfg.GitHub,
// which exposes ListCodeScanningAlerts; nil in the dev posture (no GitHub
// App), in which case the fetch is skipped and nothing is recorded.
func (s *Server) ingestCodeScanningAlert(ctx context.Context, raw []byte) {
	var lister codeScanningLister
	if s.cfg.GitHub != nil {
		lister = s.cfg.GitHub
	}
	s.ingestCodeScanningAlertWith(ctx, raw, lister)
}

// ingestCodeScanningAlertWith is ingestCodeScanningAlert with the
// code-scanning client injected, so the run-matching / filtering /
// idempotent-record core is exercised in unit tests with a fake lister.
func (s *Server) ingestCodeScanningAlertWith(ctx context.Context, raw []byte, lister codeScanningLister) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p codeScanningAlertPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: payload parse failed",
			slog.String("error", err.Error()))
		return
	}

	// Resolve the ref + head SHA. Prefer the top-level event fields;
	// fall back to the alert's most-recent instance.
	ref := p.Ref
	if ref == "" {
		ref = p.Alert.MostRecentInstance.Ref
	}
	headSHA := p.CommitOID
	if headSHA == "" {
		headSHA = p.Alert.MostRecentInstance.CommitSHA
	}

	// Match a run by PR. A code_scanning_alert carries no PR URL, so
	// extract the PR number from a refs/pull/{n}/... ref and rebuild the
	// canonical PR URL the run was denormalized with (#216). A branch-ref
	// alert (e.g. a post-merge default-branch scan) maps to no PR and is
	// a no-op — this feature gates PR-stage findings before merge.
	prNumber, ok := pullNumberFromRef(ref)
	if !ok {
		return
	}
	if p.Repository.FullName == "" {
		return
	}
	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", p.Repository.FullName, prNumber)
	target := s.findRunByPullRequestURL(ctx, prURL, "code_scanning_alert")
	if target == nil {
		return
	}

	// The implement diff: the files this run actually changed. A finding
	// on an untouched file is real but pre-existing repo debt, not
	// introduced here, so it must not gate. We use the approved plan's
	// scope.files — the runner's scope enforcement bounds the commit to
	// exactly these, so they are a faithful diff proxy.
	diffFiles := s.runDiffFiles(ctx, target.ID)

	// Resolve the installation: prefer the run's owner, fall back to the
	// delivery's installation id.
	var installID int64
	if target.InstallationID != nil {
		installID = *target.InstallationID
	}
	if installID == 0 {
		installID = p.Installation.ID
	}

	if lister == nil {
		// No GitHub REST client wired (dev posture). We can't fetch the
		// alert set; record nothing. A run with no securityscan entry is
		// treated as not-blocking by the gate.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: no GitHub client; skipping fetch",
			slog.String("run_id", target.ID.String()))
		return
	}

	alerts, err := lister.ListCodeScanningAlertsScoped(ctx, forge.FromGitHubInstallationID(installID), repoRefFromPayload(p), ref)
	if err != nil {
		// Best-effort: a fetch error records no entry this delivery; a
		// later delivery re-fetches. (The gate's fail-OPEN on a
		// securityscan read/decode error is auditcomplete's concern.)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: list alerts failed",
			slog.String("run_id", target.ID.String()),
			slog.String("ref", ref),
			slog.String("error", err.Error()))
		return
	}

	// High-severity findings intersecting the implement diff. The two
	// filters are securityscan's pure, single-authority helpers.
	findings := securityscan.FilterToDiffFiles(securityscan.FilterHighSeverity(alerts), diffFiles)

	s.recordSecurityScan(ctx, target.ID, headSHA, ref, prNumber, findings)
}

// recordSecurityScan writes ONE idempotent securityscan audit entry for
// the run, floored on the latest stage_fixup_triggered (#1096). The
// floor is what lets a clean re-scan after a fix-up clear the gate: only
// securityscan entries recorded AFTER the most recent fix-up count as
// the current window.
//
// Idempotency: GitHub delivers one code_scanning_alert per alert, and
// every delivery re-fetches the same full alert set, so N alerts would
// otherwise write N identical entries. We compare the current filtered
// state to the most-recent in-window entry and skip an identical
// re-delivery. When there is no in-window entry yet, a clean (no-finding)
// scan records nothing — absence already means "not blocking" — while a
// finding records a fresh blocking entry.
func (s *Server) recordSecurityScan(ctx context.Context, runID uuid.UUID, headSHA, ref string, prNumber int, findings []securityscan.Finding) {
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: list run audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}

	// Floor on the latest fix-up, then find the most-recent securityscan
	// entry strictly after that floor (the current window).
	var floorSeq int64
	for _, e := range entries {
		if e.Category == CategoryStageFixupTriggered && e.Sequence > floorSeq {
			floorSeq = e.Sequence
		}
	}
	var lastInWindow *audit.Entry
	for _, e := range entries {
		if e.Category != securityscan.AuditCategorySecurityFindings || e.Sequence <= floorSeq {
			continue
		}
		if lastInWindow == nil || e.Sequence > lastInWindow.Sequence {
			lastInWindow = e
		}
	}

	current := scanFingerprint(headSHA, findings)
	if lastInWindow != nil {
		var prev securityScanAuditPayload
		if json.Unmarshal(lastInWindow.Payload, &prev) == nil &&
			scanFingerprint(prev.HeadSHA, prev.Findings) == current {
			// Identical to the most-recent in-window scan: a duplicate
			// delivery (or an unchanged re-scan). Nothing new to record.
			return
		}
	} else if len(findings) == 0 {
		// No in-window entry and a clean scan: nothing meaningful to
		// record (a run with no securityscan entry does not block).
		return
	}

	payload, err := json.Marshal(securityScanAuditPayload{
		HeadSHA:  headSHA,
		Ref:      ref,
		PRNumber: prNumber,
		Findings: findings,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: marshal payload failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	actor := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  securityscan.AuditCategorySecurityFindings,
		ActorKind: &actor,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "code_scanning_alert: append audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "code_scanning_alert: recorded securityscan entry",
		slog.String("run_id", runID.String()),
		slog.String("head_sha", headSHA),
		slog.Int("pr_number", prNumber),
		slog.Int("findings", len(findings)))
}

// runDiffFiles returns the run's implement-diff file set — the approved
// plan's scope.files paths. Empty when no plan is available (the filter
// then intersects nothing, conservatively gating no findings).
func (s *Server) runDiffFiles(ctx context.Context, runID uuid.UUID) []string {
	p, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil || p == nil {
		return nil
	}
	files := make([]string, 0, len(p.Scope.Files))
	for _, f := range p.Scope.Files {
		if f.Path != "" {
			files = append(files, f.Path)
		}
	}
	return files
}

// scanFingerprint is the idempotency key for a scan: the head SHA plus
// the sorted set of finding numbers. Two deliveries with the same head
// and the same alert numbers describe the same scan state.
func scanFingerprint(headSHA string, findings []securityscan.Finding) string {
	nums := make([]int, 0, len(findings))
	for _, f := range findings {
		nums = append(nums, f.Number)
	}
	sort.Ints(nums)
	return headSHA + "|" + fmt.Sprint(nums)
}

// pullNumberFromRef extracts {n} from a "refs/pull/{n}/merge" or
// "refs/pull/{n}/head" ref. Returns ok=false for any other ref shape
// (e.g. a branch ref), which the ingest treats as "no PR to match".
func pullNumberFromRef(ref string) (int, bool) {
	const prefix = "refs/pull/"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(ref, prefix)
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:slash])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// repoRefFromPayload builds the typed repo ref from the payload, taking
// the structured owner/name when present and otherwise splitting
// full_name ("owner/repo").
func repoRefFromPayload(p codeScanningAlertPayload) githubclient.RepoRef {
	if p.Repository.Owner.Login != "" && p.Repository.Name != "" {
		return githubclient.RepoRef{Owner: p.Repository.Owner.Login, Name: p.Repository.Name}
	}
	owner, name, _ := strings.Cut(p.Repository.FullName, "/")
	return githubclient.RepoRef{Owner: owner, Name: name}
}
