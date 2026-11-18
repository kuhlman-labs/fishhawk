package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// diffSecretsDetectedCategory is the origin audit entry of a diff secrets
// check raise (E80.3 / #3760): one entry per review round that raised at least
// one NEW server_check concern or recorded a new allowlist suppression
// (ADR-084 A2, #5230), system actor, stage-anchored. Its sequence is the
// concerns' OriginReviewSequence. INTERNAL: no issue-comment activity line,
// so it is not an issue-comment surface. The payload names locations and
// pattern classes only — never matched bytes.
const diffSecretsDetectedCategory = "diff_secrets_detected"

// diffSecretsRaiseMu serializes the read-check-insert of raiseDiffSecretConcerns
// so two concurrent rounds for one stage (the trace-time hook and the fix-up
// re-review backstop) cannot both observe a check key absent and mint a
// duplicate. It holds within ONE process only — a multi-replica deployment can
// still race (documented as a residual in server/README.md).
var diffSecretsRaiseMu sync.Mutex

// diffSecretsDetectedPayload is the diff_secrets_detected audit payload.
// HitCount and Groups name the FRESH groups the round raised (unchanged since
// #3760); the embedded diffsecrets.SuppressionSummary (ADR-084 A2, #5230)
// flattens beside them and carries the allowlist read's outcome and every
// suppressed hit, located by path, lines, pattern class, rule id and
// fingerprint — never bytes, never a glob.
type diffSecretsDetectedPayload struct {
	Check          string                    `json:"check"`
	HeadSHA        string                    `json:"head_sha,omitempty"`
	PatchTruncated bool                      `json:"patch_truncated"`
	HitCount       int                       `json:"hit_count"`
	Groups         []diffSecretsGroupPayload `json:"groups"`
	diffsecrets.SuppressionSummary
}

type diffSecretsGroupPayload struct {
	Path     string `json:"path"`
	Lines    []int  `json:"lines"`
	Pattern  string `json:"pattern"`
	CheckKey string `json:"check_key"`
}

// diffSecretsMarkerConcernCategory is the CONCERN category
// (concern.RaisedConcern, not an audit category) of the low, human-only
// concern each fresh marker-suppressed hit raises (ADR-084 A2, #5230). The
// ConcernCategory suffix keeps the audit registry's completeness sweep
// (TestKnownCategoriesCoversEmittedCategories) from reading it as an audit
// category.
const diffSecretsMarkerConcernCategory = "diff_secrets_marker_suppressed"

// diffSecretsRound is what one raiseDiffSecretConcerns round hands its caller.
//
//   - PersonaGroups is the persona-attachment seam (E80.5 / #3762, the A2
//     persona rule): every group still raised plus every marker-suppressed
//     group, NEVER an entry-suppressed one. nil when nothing matched.
//   - FreshGroups is the kept groups NOT already on record for the stage —
//     what the round newly found (suppressed groups and groups already raised
//     are excluded). The conflict-resolution skip row counts these.
//   - SuppressedHitCount is the hits the allowlist suppressed this round.
type diffSecretsRound struct {
	PersonaGroups      []diffsecrets.Group
	FreshGroups        []diffsecrets.Group
	SuppressedHitCount int
}

// raiseDiffSecretConcerns runs the deterministic diff secrets check (ADR-084
// D5 / rule 5, E80.3 / #3760) over the ADDED lines of diff.Patch with
// redaction.DefaultPatterns — no model call — and raises one server_check
// implement concern (severity high, category security) per (file, pattern)
// group not already on record for this stage.
//
// Allowlist (ADR-084 A2, #5230): once Scan finds a hit, the reviewed allowlist
// is read at the run's ADMISSION commit (diffSecretsAllowlist: fail closed,
// memoized per (run, head)) and Partition splits the hits. Entry-suppressed
// hits are counted only. Each marker-suppressed hit is listed and, when its
// MarkerCheckKey is not on record, raises one LOW, human-only server_check
// concern (diffSecretsMarkerConcernCategory) — non-blocking at the human
// merge verb, but like every open concern it holds delegated settlement.
//
// De-duplication: a group or marker hit is skipped when a server_check row
// with the same check_key exists on THIS stage in an open, waived or deferred
// state. A superseded or addressed row does not suppress a re-raise. A
// ListByRun error fails OPEN to raising: a duplicate concern is noise, a
// missed secret is silent.
//
// Append rule: the diff_secrets_detected entry is appended iff a group or a
// marker hit is fresh, OR the round's suppressed set is not a subset of the
// suppressed sets already recorded on this stage for the same NON-EMPTY head
// (C4a: a suppressed-only repeat at one head appends nothing). That lookup's
// error fails OPEN to appending: a duplicate summary is noise, a lost record
// is silent.
//
// Ordering: the diff_secrets_detected audit entry is appended FIRST and its
// sequence stamps the concerns (one InsertRaised for the high group concerns
// and the low marker concerns). An append failure WARN-logs and raises nothing
// (no concern without its origin record); an InsertRaised failure WARN-logs
// and leaves the audit entry as the authoritative record.
//
// Returns the round (diffSecretsRound); the zero value when nothing was
// scanned or nothing matched. Logs carry run/stage ids, counts, paths, line
// numbers, pattern names and the allowlist status slug only.
func (s *Server) raiseDiffSecretConcerns(ctx context.Context, runID, stageID uuid.UUID, diff policy.Diff, headSHA string) diffSecretsRound {
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil {
		return diffSecretsRound{}
	}
	if diff.Patch == "" {
		if len(diff.ChangedFiles) > 0 {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: diff carries no patch text — added lines not scanned",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int("changed_files", len(diff.ChangedFiles)),
			)
		}
		return diffSecretsRound{}
	}
	res := diffsecrets.Scan(diff.Patch, redaction.DefaultPatterns)
	if len(res.Hits) == 0 {
		return diffSecretsRound{}
	}
	// The allowlist read is a forge call: it runs BEFORE diffSecretsRaiseMu, so
	// a slow forge never serializes other runs' rounds.
	al, status := s.diffSecretsAllowlist(ctx, runID, headSHA)
	kept, suppressed := al.Partition(res.Hits)
	groups := diffsecrets.GroupHits(kept)
	summary := diffsecrets.NewSuppressionSummary(&status, suppressed)
	round := diffSecretsRound{PersonaGroups: diffSecretsPersonaGroups(kept, suppressed), SuppressedHitCount: len(suppressed)}

	diffSecretsRaiseMu.Lock()
	defer diffSecretsRaiseMu.Unlock()

	fresh := groups
	var freshMarkers []diffsecrets.SuppressionRecord
	for _, rec := range summary.Suppressed {
		if rec.By == diffsecrets.ByMarker {
			freshMarkers = append(freshMarkers, rec)
		}
	}
	if rows, err := s.cfg.ConcernRepo.ListByRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: list concerns failed — raising without de-duplication",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
	} else {
		recorded := map[string]bool{}
		for _, row := range rows {
			if row == nil || row.StageID != stageID || !row.IsServerCheck() || row.CheckKey == "" {
				continue
			}
			if row.State.IsOpen() || row.State == concern.StateWaived || row.State == concern.StateDeferred {
				recorded[row.CheckKey] = true
			}
		}
		fresh = nil
		for _, g := range groups {
			if !recorded[g.Key()] {
				fresh = append(fresh, g)
			}
		}
		markers := freshMarkers
		freshMarkers = nil
		for _, rec := range markers {
			if !recorded[rec.CheckKey] {
				freshMarkers = append(freshMarkers, rec)
			}
		}
	}
	round.FreshGroups = fresh
	if len(fresh) == 0 && len(freshMarkers) == 0 && s.diffSecretsSuppressionOnRecord(ctx, runID, stageID, headSHA, summary.Suppressed) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "diff secrets check: every hit is already on record for this stage",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("hit_count", len(res.Hits)),
			slog.Int("group_count", len(groups)),
			slog.Int("suppressed_hit_count", summary.SuppressedHitCount),
			slog.String("allowlist_status", status.Status),
		)
		return round
	}

	hitCount := 0
	payloadGroups := make([]diffSecretsGroupPayload, 0, len(fresh))
	for _, g := range fresh {
		hitCount += len(g.Lines)
		payloadGroups = append(payloadGroups, diffSecretsGroupPayload{
			Path: g.Path, Lines: g.Lines, Pattern: g.Pattern, CheckKey: g.Key(),
		})
	}
	payload, _ := json.Marshal(diffSecretsDetectedPayload{
		Check:              diffsecrets.CheckName,
		HeadSHA:            headSHA,
		PatchTruncated:     diff.PatchTruncated,
		HitCount:           hitCount,
		Groups:             payloadGroups,
		SuppressionSummary: summary,
	})
	systemKind := audit.ActorKind("system")
	entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  diffSecretsDetectedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: append diff_secrets_detected failed — no concern raised this round",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int("group_count", len(fresh)),
			slog.Int("marker_count", len(freshMarkers)),
			slog.String("error", err.Error()),
		)
		return round
	}

	raised := make([]concern.RaisedConcern, 0, len(fresh)+len(freshMarkers))
	for _, g := range fresh {
		raised = append(raised, concern.RaisedConcern{
			Severity: "high",
			Category: "security",
			Note:     diffsecrets.Note(g),
			CheckKey: g.Key(),
		})
	}
	for _, rec := range freshMarkers {
		raised = append(raised, concern.RaisedConcern{
			Severity: "low",
			Category: diffSecretsMarkerConcernCategory,
			Note:     diffsecrets.MarkerNote(rec),
			CheckKey: rec.CheckKey,
		})
	}
	level, msg := slog.LevelInfo, "diff secrets check: suppressed-only round recorded"
	if len(raised) > 0 {
		if _, ierr := s.cfg.ConcernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
			RunID:                runID,
			StageID:              stageID,
			StageKind:            concern.StageKindImplement,
			Provenance:           concern.ProvenanceServerCheck,
			OriginReviewSequence: entry.Sequence,
			Concerns:             raised,
		}); ierr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: insert concerns failed — the diff_secrets_detected entry is the record",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.Int64("origin_sequence", entry.Sequence),
				slog.String("error", ierr.Error()),
			)
			return round
		}
		level, msg = slog.LevelWarn, "diff secrets check: credential-shaped additions raised as server_check concerns"
	}
	rejected := 0
	if summary.Allowlist != nil {
		rejected = summary.Allowlist.RejectedCount
	}
	s.cfg.Logger.LogAttrs(ctx, level, msg,
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.Int("hit_count", hitCount),
		slog.Int("raised", len(fresh)),
		slog.Int("marker_raised", len(freshMarkers)),
		slog.String("locations", diffSecretsLocations(fresh)),
		slog.Bool("patch_truncated", diff.PatchTruncated),
		slog.Int("suppressed_hit_count", summary.SuppressedHitCount),
		slog.Int("marker_suppressed_hit_count", summary.MarkerSuppressedHitCount),
		slog.String("allowlist_status", status.Status),
		slog.Int("rejected_count", rejected),
	)
	return round
}

// diffSecretsPersonaGroups is the A2 persona rule: the kept hits plus the
// marker-suppressed hits, grouped; an entry-suppressed hit never attaches the
// security persona. nil when neither exists.
func diffSecretsPersonaGroups(kept []diffsecrets.Hit, suppressed []diffsecrets.Suppression) []diffsecrets.Group {
	hits := append([]diffsecrets.Hit(nil), kept...)
	for _, x := range suppressed {
		if x.By == diffsecrets.ByMarker {
			hits = append(hits, x.Hit)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return diffsecrets.GroupHits(hits)
}

// diffSecretsSuppressionOnRecord reports whether every record in sup is
// already on a diff_secrets_detected entry for THIS stage at the same
// NON-EMPTY head (C4a). An empty sup is trivially on record. An empty head, a
// list error (WARN) or no matching entry is false, so the caller appends.
func (s *Server) diffSecretsSuppressionOnRecord(ctx context.Context, runID, stageID uuid.UUID, headSHA string, sup []diffsecrets.SuppressionRecord) bool {
	if len(sup) == 0 {
		return true
	}
	if headSHA == "" {
		return false
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, diffSecretsDetectedCategory)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "diff secrets check: list diff_secrets_detected failed — recording the suppression summary again",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return false
	}
	onRecord := map[string]bool{}
	for _, e := range entries {
		if e == nil || e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p diffSecretsDetectedPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA != headSHA {
			continue
		}
		for _, rec := range p.Suppressed {
			onRecord[diffSecretsSuppressionKey(rec)] = true
		}
	}
	for _, rec := range sup {
		if !onRecord[diffSecretsSuppressionKey(rec)] {
			return false
		}
	}
	return true
}

// diffSecretsSuppressionKey is a SuppressionRecord's identity for the C4a
// subset check: every field, lines included.
func diffSecretsSuppressionKey(rec diffsecrets.SuppressionRecord) string {
	lines := make([]string, 0, len(rec.Lines))
	for _, l := range rec.Lines {
		lines = append(lines, strconv.Itoa(l))
	}
	return strings.Join([]string{rec.Path, rec.Pattern, rec.By, rec.RuleID, rec.RuleFingerprint, rec.CheckKey, strings.Join(lines, ",")}, "\x00")
}

// The allowlist read's fixed detail classes (AllowlistStatusRecord.Detail):
// structural slugs, never error text.
const (
	diffSecretsAllowlistRunRepoUnwired  = "run_repo_unwired"
	diffSecretsAllowlistRunUnreadable   = "run_unreadable"
	diffSecretsAllowlistBaseUnrecorded  = "base_commit_unrecorded"
	diffSecretsAllowlistForgeNotWired   = "forge_not_wired"
	diffSecretsAllowlistFetchTimeout    = 15 * time.Second
	diffSecretsAllowlistMemoCap         = 128
	diffSecretsAllowlistAdmissionCommit = 40 // hex length of a recorded admission commit
)

// diffSecretsAllowlist reads the reviewed allowlist at the run's ADMISSION
// commit (run.DocumentBaseCommit) — never at headSHA or a pass base, so a run
// cannot widen the allowlist it is judged by. It FAILS CLOSED: every outcome
// but loaded returns a nil allowlist (nothing is suppressed) with the status
// that names why:
//
//	unavailable/run_repo_unwired      no RunRepo
//	unavailable/run_unreadable        GetRun failed
//	unavailable/base_commit_unrecorded DocumentBaseCommit nil or not 40-hex
//	unavailable/forge_not_wired       fileFetcherFor named a reason
//	fetch_failed                      FetchFile failed (bounded timeout)
//	absent                            forge.ErrNotFound
//	parse_error                       ParseAllowlist failed (no error text kept)
//	loaded                            parsed; rejections mapped, capped
//
// A COMPLETED forge read (loaded, absent, parse_error) is memoized per
// (run, head) (C4c); fetch_failed is not, so a transient failure retries next
// round, and the config rungs make no forge call. Staleness (A2, C4d): a long
// run, or a child inheriting DocumentBaseCommit, keeps the allowlist as
// admitted — a widening never applies inside the run that makes it.
func (s *Server) diffSecretsAllowlist(ctx context.Context, runID uuid.UUID, headSHA string) (*diffsecrets.Allowlist, diffsecrets.AllowlistStatusRecord) {
	st := diffsecrets.AllowlistStatusRecord{Path: diffsecrets.AllowlistPath, Status: diffsecrets.StatusUnavailable}
	if s.cfg.RunRepo == nil {
		st.Detail = diffSecretsAllowlistRunRepoUnwired
		return nil, st
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		st.Detail = diffSecretsAllowlistRunUnreadable
		return nil, st
	}
	if runRow.DocumentBaseCommit == nil || !isHexSHA(*runRow.DocumentBaseCommit, diffSecretsAllowlistAdmissionCommit) {
		st.Detail = diffSecretsAllowlistBaseUnrecorded
		return nil, st
	}
	ref := *runRow.DocumentBaseCommit
	st.Ref = ref
	f, scope, repo, reason := s.fileFetcherFor(runRow)
	if reason != "" {
		st.Detail = diffSecretsAllowlistForgeNotWired
		return nil, st
	}
	memoKey := runID.String() + "|" + headSHA
	if m, ok := diffSecretsAllowlistReads.get(memoKey); ok {
		return m.al, m.status
	}
	fctx, cancel := context.WithTimeout(ctx, diffSecretsAllowlistFetchTimeout)
	defer cancel()
	fc, err := f.FetchFile(fctx, scope, repo, diffsecrets.AllowlistPath, ref)
	var al *diffsecrets.Allowlist
	switch {
	case errors.Is(err, forge.ErrNotFound):
		st.Status = diffsecrets.StatusAbsent
	case err != nil || fc == nil:
		st.Status = diffsecrets.StatusFetchFailed
		return nil, st // never memoized: a transient failure retries
	default:
		parsed, rej, perr := diffsecrets.ParseAllowlist(fc.Content, redaction.DefaultPatterns)
		if perr != nil {
			st.Status = diffsecrets.StatusParseError
		} else {
			al = parsed
			st.Status = diffsecrets.StatusLoaded
			st.RejectedCount = len(rej)
			if len(rej) > 0 {
				st.Rejected = diffsecrets.RejectionRecords(rej)
			}
		}
	}
	diffSecretsAllowlistReads.put(memoKey, diffSecretsAllowlistRead{al: al, status: st})
	return al, st
}

// isHexSHA reports whether v is exactly n lowercase-or-uppercase hex runes.
func isHexSHA(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, c := range v {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// diffSecretsAllowlistRead is one memoized completed allowlist read.
type diffSecretsAllowlistRead struct {
	al     *diffsecrets.Allowlist
	status diffsecrets.AllowlistStatusRecord
}

// diffSecretsAllowlistMemo is the per-(run, head) memo of completed allowlist
// reads (C4c): mutex-guarded, bounded at cap entries with oldest-first
// eviction. Process-local, like diffSecretsRaiseMu. Two concurrent rounds at
// one head may both miss and read once each — still at most once per round.
type diffSecretsAllowlistMemo struct {
	mu    sync.Mutex
	cap   int
	order []string
	reads map[string]diffSecretsAllowlistRead
}

func newDiffSecretsAllowlistMemo(capacity int) *diffSecretsAllowlistMemo {
	return &diffSecretsAllowlistMemo{cap: capacity, reads: map[string]diffSecretsAllowlistRead{}}
}

func (m *diffSecretsAllowlistMemo) get(k string) (diffSecretsAllowlistRead, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.reads[k]
	return r, ok
}

func (m *diffSecretsAllowlistMemo) put(k string, r diffSecretsAllowlistRead) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reads[k]; !ok {
		m.order = append(m.order, k)
	}
	m.reads[k] = r
	for len(m.order) > m.cap {
		delete(m.reads, m.order[0])
		m.order = m.order[1:]
	}
}

func (m *diffSecretsAllowlistMemo) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reads)
}

// diffSecretsAllowlistReads is the process's allowlist-read memo.
var diffSecretsAllowlistReads = newDiffSecretsAllowlistMemo(diffSecretsAllowlistMemoCap)

// diffSecretsLocations renders "path:l1,l2 (pattern)" per group for the log.
func diffSecretsLocations(groups []diffsecrets.Group) string {
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		lines := make([]string, 0, len(g.Lines))
		for _, l := range g.Lines {
			lines = append(lines, strconv.Itoa(l))
		}
		parts = append(parts, g.Path+":"+strings.Join(lines, ",")+" ("+g.Pattern+")")
	}
	return strings.Join(parts, "; ")
}

// redactReviewPatch passes the review prompt's diff through
// redaction.RedactDefault so a credential the diff secrets check found never
// reaches a model reviewer verbatim (E80.3 / #3760). A diff with no
// credential-shaped string comes back byte-identical. Logs per-pattern hit
// counts only.
func (s *Server) redactReviewPatch(ctx context.Context, runID, stageID uuid.UUID, patch string) string {
	if patch == "" {
		return patch
	}
	out, hits := redaction.RedactDefault([]byte(patch))
	if len(hits) == 0 {
		return patch
	}
	counts := make([]string, 0, len(hits))
	for _, h := range hits {
		counts = append(counts, h.Pattern+"="+strconv.Itoa(h.Count))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "implement review: redacted credential-shaped strings from the review diff",
		slog.String("run_id", runID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("hits", strings.Join(counts, ",")),
	)
	return string(out)
}
