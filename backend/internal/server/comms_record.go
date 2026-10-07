package server

// The comms scan's RECORD and SUPPRESSION contracts (E81.5 / #3775, phase 4
// #4014): the comms_scan_gathered row the comms gather writes on a signed
// prompt serve, its digest and dedupe-append, the lookups phases 5 and 7 bind
// to, the suppression memory and draft-filed reads, marker trust, and the
// cursor hold-back phase 7 applies. The gather itself (comms_scan.go) builds
// the payload; this file only records and reads. Contract: README.md
// § "Comms scan gather".

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
)

// The comms audit categories this file reads and writes. All three are
// registered in audit.KnownCategories by #4012; comms_scan_gathered is written
// here, comms_apply_completed and comms_draft_filed are written by the comms
// apply (phase 7, #4017) and only READ here.
const (
	CategoryCommsScanGathered   = "comms_scan_gathered"
	CategoryCommsApplyCompleted = "comms_apply_completed"
	CategoryCommsDraftFiled     = "comms_draft_filed"
)

// commsScanGatheredPayload is the comms_scan_gathered row: what one gather
// showed, omitted and suppressed, and the cursor bounds phase 7 advances to.
//
// The gather builds every field except GatherDigest, which
// recordCommsScanGathered stamps. StageID and StageAttempt are part of the
// digest, so a new stage attempt over the same reports records a new row.
// Omitted and Suppressed are capped by the gather (OmittedCount and
// SuppressedCount carry the true totals). PendingCursor is nil when the scan
// did not complete: there is nothing phase 7 may advance to.
type commsScanGatheredPayload struct {
	StageID          uuid.UUID                `json:"stage_id"`
	StageAttempt     string                   `json:"stage_attempt"`
	Repo             string                   `json:"repo"`
	GatherDigest     string                   `json:"gather_digest"`
	Shown            []commsShownReport       `json:"shown"`
	Omitted          []string                 `json:"omitted"`
	OmittedCount     int                      `json:"omitted_count"`
	Suppressed       []commsSuppression       `json:"suppressed"`
	SuppressedCount  int                      `json:"suppressed_count"`
	ClassExcluded    commsClassExcluded       `json:"class_excluded"`
	Degradations     []commsGatherDegradation `json:"degradations"`
	MalformedMarkers int                      `json:"malformed_markers"`
	Charter          commsCharterRecord       `json:"charter"`
	PendingCursor    *commsPendingCursor      `json:"pending_cursor,omitempty"`
}

// commsShownReport is one report the served prompt rendered. ContentHash is
// userreport.ContentHash of the report as gathered; UpdatedAt is the forge
// updated_at commsCursorHoldBack holds the cursor back to.
type commsShownReport struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	IssueNumber int       `json:"issue_number"`
	CommentID   int64     `json:"comment_id"`
	ContentHash string    `json:"content_hash"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// commsClassExcluded counts the gathered items each non-external class
// removed before suppression.
type commsClassExcluded struct {
	FishhawkFiled int `json:"fishhawk_filed"`
	Bot           int `json:"bot"`
	Internal      int `json:"internal"`
}

// commsGatherDegradation is one named partial-gather reason: the source it
// affected, the reason token and how often it hit.
type commsGatherDegradation struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// commsCharterRecord names the charter the gather read and the rubric and
// non-goal ids the served prompt rendered.
type commsCharterRecord struct {
	Path        string   `json:"path"`
	ContentHash string   `json:"content_hash"`
	RubricIDs   []string `json:"rubric_ids"`
	NonGoalIDs  []string `json:"non_goal_ids"`
}

// commsPendingCursor is the scan's read bounds (Since, NoteSince) and the
// bounds phase 7 may advance to once every shown report is accounted for
// (Cursor, NoteCursor). The gather holds Cursor back to the earliest omitted
// report, and NoteCursor <= Cursor.
type commsPendingCursor struct {
	Since      time.Time `json:"since"`
	NoteSince  time.Time `json:"note_since"`
	Cursor     time.Time `json:"cursor"`
	NoteCursor time.Time `json:"note_cursor"`
}

// normalized returns a copy whose list fields are empty arrays rather than
// nil, so a nil and an empty list record — and digest — identically.
func (p commsScanGatheredPayload) normalized() commsScanGatheredPayload {
	if p.Shown == nil {
		p.Shown = []commsShownReport{}
	}
	if p.Omitted == nil {
		p.Omitted = []string{}
	}
	if p.Suppressed == nil {
		p.Suppressed = []commsSuppression{}
	}
	if p.Degradations == nil {
		p.Degradations = []commsGatherDegradation{}
	}
	if p.Charter.RubricIDs == nil {
		p.Charter.RubricIDs = []string{}
	}
	if p.Charter.NonGoalIDs == nil {
		p.Charter.NonGoalIDs = []string{}
	}
	return p
}

// commsGatherDigest is the lowercase-hex sha256 of json.Marshal of p
// (normalized) with GatherDigest blank. The payload is a closed shape of
// strings, ints, times and uuids, so Marshal cannot fail.
func commsGatherDigest(p commsScanGatheredPayload) string {
	p = p.normalized()
	p.GatherDigest = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// commsRecordMu serialises recordCommsScanGathered's list-then-append so two
// concurrent serves of one gather in this process record one row. Two
// fishhawkd replicas can still append twice; the rows are content-identical
// (the digest covers the whole payload), so the lookups stay equivalent.
var commsRecordMu sync.Mutex

// recordCommsScanGathered stamps stageID and the gather digest onto payload
// and appends ONE comms_scan_gathered row per distinct digest for the run. It
// returns the appended row (appended true) or the existing row carrying the
// digest (appended false). A nil AuditRepo, a list failure or an append
// failure is an error: the caller must not serve a prompt it cannot bind.
func (s *Server) recordCommsScanGathered(ctx context.Context, runID, stageID uuid.UUID, payload commsScanGatheredPayload) (*audit.Entry, bool, error) {
	if s.cfg.AuditRepo == nil {
		return nil, false, errors.New("record comms scan: audit repository not configured")
	}
	payload = payload.normalized()
	payload.StageID = stageID
	digest := commsGatherDigest(payload)
	payload.GatherDigest = digest
	body, _ := json.Marshal(payload)

	commsRecordMu.Lock()
	defer commsRecordMu.Unlock()

	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCommsScanGathered)
	if err != nil {
		return nil, false, fmt.Errorf("record comms scan: list prior rows: %w", err)
	}
	for _, e := range entries {
		if commsEntryDigest(e) == digest {
			return e, false, nil
		}
	}
	systemKind := audit.ActorKind("system")
	entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryCommsScanGathered,
		ActorKind: &systemKind,
		Payload:   body,
	})
	if err != nil {
		return nil, false, fmt.Errorf("record comms scan: append: %w", err)
	}
	return entry, true, nil
}

// commsEntryDigest reads only a row's gather_digest, leniently; "" when the
// row does not decode.
func commsEntryDigest(e *audit.Entry) string {
	var d struct {
		GatherDigest string `json:"gather_digest"`
	}
	if json.Unmarshal(e.Payload, &d) != nil {
		return ""
	}
	return d.GatherDigest
}

// decodeCommsScanGathered strictly decodes a comms_scan_gathered payload:
// unknown fields and trailing data are refused.
func decodeCommsScanGathered(raw json.RawMessage) (*commsScanGatheredPayload, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p commsScanGatheredPayload
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode comms_scan_gathered payload: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode comms_scan_gathered payload: trailing data after the object")
	}
	return &p, nil
}

// latestCommsScanGathered returns the stage's highest-sequence
// comms_scan_gathered row and its strictly decoded payload. Rows are selected
// by the entry's server-written stage column; ties on sequence go to the later
// listed row. The selected row must decode and name the same stage, and no row
// for the stage is an error — phases 5 and 7 bind to what was served, never
// to a guess.
func (s *Server) latestCommsScanGathered(ctx context.Context, runID, stageID uuid.UUID) (*audit.Entry, *commsScanGatheredPayload, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil, errors.New("latest comms scan: audit repository not configured")
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCommsScanGathered)
	if err != nil {
		return nil, nil, fmt.Errorf("latest comms scan: list rows: %w", err)
	}
	var latest *audit.Entry
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if latest == nil || e.Sequence >= latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return nil, nil, fmt.Errorf("latest comms scan: no %s row for stage %s", CategoryCommsScanGathered, stageID)
	}
	p, err := decodeCommsScanGathered(latest.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("latest comms scan: %w", err)
	}
	if p.StageID != stageID {
		return nil, nil, fmt.Errorf("latest comms scan: row names stage %s, want %s", p.StageID, stageID)
	}
	return latest, p, nil
}

// commsScanGatheredByDigest returns the run's comms_scan_gathered row whose
// gather_digest equals digest exactly, strictly decoded. No such row, or a
// match that does not decode, is an error.
func (s *Server) commsScanGatheredByDigest(ctx context.Context, runID uuid.UUID, digest string) (*audit.Entry, *commsScanGatheredPayload, error) {
	if s.cfg.AuditRepo == nil {
		return nil, nil, errors.New("comms scan by digest: audit repository not configured")
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCommsScanGathered)
	if err != nil {
		return nil, nil, fmt.Errorf("comms scan by digest: list rows: %w", err)
	}
	for _, e := range entries {
		if digest == "" || commsEntryDigest(e) != digest {
			continue
		}
		p, err := decodeCommsScanGathered(e.Payload)
		if err != nil {
			return nil, nil, fmt.Errorf("comms scan by digest: %w", err)
		}
		return e, p, nil
	}
	return nil, nil, fmt.Errorf("comms scan by digest: no %s row with digest %q", CategoryCommsScanGathered, digest)
}

// commsSuppression is one suppressed report: its canonical id, the content
// hash it was suppressed at, and why.
type commsSuppression struct {
	ID    string `json:"id"`
	Hash  string `json:"hash"`
	Basis string `json:"basis"`
}

// The suppression bases. filed, rejected and n_drift are prior comms apply
// outcomes (comms_apply_completed); marker is a trusted draft marker.
const (
	commsSuppressionBasisFiled    = "filed"
	commsSuppressionBasisRejected = "rejected"
	commsSuppressionBasisNDrift   = "n_drift"
	commsSuppressionBasisMarker   = "marker"
)

// commsApplyCompletedRecord is the READ contract of a comms_apply_completed
// row (written by phase 7, #4017). Extra fields are tolerated.
type commsApplyCompletedRecord struct {
	Repo         string             `json:"repo"`
	Suppressions []commsSuppression `json:"suppressions"`
}

// commsDraftFiledRecord is the READ contract of a comms_draft_filed row
// (written by phase 7, #4017): the filed item (CommentID 0 for an issue) and
// the reports its marker names. Extra fields are tolerated.
type commsDraftFiledRecord struct {
	Repo        string                    `json:"repo"`
	IssueNumber int                       `json:"issue_number"`
	CommentID   int64                     `json:"comment_id"`
	Reports     []userreport.MarkedReport `json:"reports"`
}

// commsSuppressionKey is the (report id, content hash) a suppression matches
// on. A materially edited report hashes differently and re-enters.
type commsSuppressionKey struct {
	ID   string
	Hash string
}

// commsSuppressionMemory is a set of suppressions keyed by (id, hash). The
// first suppression added for a key keeps its basis.
type commsSuppressionMemory map[commsSuppressionKey]commsSuppression

// add records sup unless its key is already present or it lacks an id or
// hash (such an entry could only ever match an unidentifiable report).
func (m commsSuppressionMemory) add(sup commsSuppression) {
	if sup.ID == "" || sup.Hash == "" {
		return
	}
	k := commsSuppressionKey{ID: sup.ID, Hash: sup.Hash}
	if _, ok := m[k]; ok {
		return
	}
	m[k] = sup
}

// lookup reports the suppression for a report at its CURRENT content hash.
func (m commsSuppressionMemory) lookup(id, hash string) (commsSuppression, bool) {
	sup, ok := m[commsSuppressionKey{ID: id, Hash: hash}]
	return sup, ok
}

// The gather degradations the suppression and draft-filed reads name.
const (
	commsDegradeSourceAudit                  = "audit"
	commsDegradeSuppressionMemoryUnavailable = "suppression_memory_unavailable"
	commsDegradeDraftFiledUnavailable        = "draft_filed_unavailable"
)

// commsMemoryUnavailableError is the named degrade a suppression or
// draft-filed read returns: the gather proceeds with empty memory (nothing is
// suppressed, the safe direction) and records Degradation().
type commsMemoryUnavailableError struct {
	Reason string
	Err    error
}

func (e *commsMemoryUnavailableError) Error() string {
	return fmt.Sprintf("comms gather: %s: %v", e.Reason, e.Err)
}

func (e *commsMemoryUnavailableError) Unwrap() error { return e.Err }

// Degradation is the payload and prompt degradation the error names.
func (e *commsMemoryUnavailableError) Degradation() commsGatherDegradation {
	return commsGatherDegradation{Source: commsDegradeSourceAudit, Reason: e.Reason, Count: 1}
}

// listCommsCategoryForAccount reads EVERY row of category and keeps those
// whose entry account equals accountID (nil matches only nil). The walk is
// bounded by the category's rows, never by a count of prior runs, so an old
// suppression survives any number of newer runs. ListAll's AccountID also
// admits NULL-account rows (the #1829 window), so the equality filter here is
// the account boundary, not the query.
func (s *Server) listCommsCategoryForAccount(ctx context.Context, category, reason string, accountID *uuid.UUID) ([]*audit.Entry, error) {
	if s.cfg.AuditRepo == nil {
		return nil, &commsMemoryUnavailableError{Reason: reason, Err: errors.New("audit repository not configured")}
	}
	params := audit.ListAllParams{Category: &category}
	if accountID != nil {
		params.AccountID = accountID.String()
	}
	entries, err := s.cfg.AuditRepo.ListAll(ctx, params)
	if err != nil {
		return nil, &commsMemoryUnavailableError{Reason: reason, Err: err}
	}
	var out []*audit.Entry
	for _, e := range entries {
		if !sameAccount(e.AccountID, accountID) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// sameAccount holds when a and b name the same account, or are both nil.
func sameAccount(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// loadCommsSuppressionMemory returns the suppressions every prior
// comms_apply_completed row for repo under accountID recorded. Undecodable
// rows are skipped. A nil AuditRepo or a list failure returns empty memory
// and a *commsMemoryUnavailableError (suppression_memory_unavailable).
func (s *Server) loadCommsSuppressionMemory(ctx context.Context, accountID *uuid.UUID, repo string) (commsSuppressionMemory, error) {
	mem := commsSuppressionMemory{}
	entries, err := s.listCommsCategoryForAccount(ctx, CategoryCommsApplyCompleted, commsDegradeSuppressionMemoryUnavailable, accountID)
	if err != nil {
		return mem, err
	}
	for _, e := range entries {
		var rec commsApplyCompletedRecord
		if json.Unmarshal(e.Payload, &rec) != nil || rec.Repo != repo {
			continue
		}
		for _, sup := range rec.Suppressions {
			mem.add(sup)
		}
	}
	return mem, nil
}

// loadCommsDraftFiled returns every comms_draft_filed record for repo under
// accountID. Undecodable rows are skipped. A nil AuditRepo or a list failure
// returns nil and a *commsMemoryUnavailableError (draft_filed_unavailable).
func (s *Server) loadCommsDraftFiled(ctx context.Context, accountID *uuid.UUID, repo string) ([]commsDraftFiledRecord, error) {
	entries, err := s.listCommsCategoryForAccount(ctx, CategoryCommsDraftFiled, commsDegradeDraftFiledUnavailable, accountID)
	if err != nil {
		return nil, err
	}
	var out []commsDraftFiledRecord
	for _, e := range entries {
		var rec commsDraftFiledRecord
		if json.Unmarshal(e.Payload, &rec) != nil || rec.Repo != repo {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// commsFiledItemKey identifies a filed item: its issue number and comment id
// (0 for an issue).
type commsFiledItemKey struct {
	IssueNumber int
	CommentID   int64
}

// trustedCommsMarkers returns the marker suppressions the server trusts, and
// the malformed-marker count (observability only).
//
// A comms marker is attacker-writable body text, so it is honoured only at the
// server-controlled position:
//
//  1. ParseDraftMarkers runs ONLY on an item classified fishhawk_filed. An
//     external item — including one with MarkerFromExternal set — a bot item
//     and an internal item are never parsed.
//  2. A parsed entry is honoured ONLY when a comms_draft_filed row for the
//     same (issue number, comment id) names that exact (id, content hash).
//
// The result is the intersection, so a marker planted in an attacker title
// that a filed draft's intake Derives-from block quotes suppresses nothing.
func trustedCommsMarkers(items []userreport.ReportItem, draftFiled []commsDraftFiledRecord) (commsSuppressionMemory, int) {
	filed := make(map[commsFiledItemKey]map[userreport.MarkedReport]struct{}, len(draftFiled))
	for _, rec := range draftFiled {
		k := commsFiledItemKey{IssueNumber: rec.IssueNumber, CommentID: rec.CommentID}
		if filed[k] == nil {
			filed[k] = map[userreport.MarkedReport]struct{}{}
		}
		for _, r := range rec.Reports {
			filed[k][r] = struct{}{}
		}
	}
	trusted := commsSuppressionMemory{}
	malformed := 0
	for _, it := range items {
		if it.Classification != userreport.ClassFishhawkFiled {
			continue
		}
		entries, bad := userreport.ParseDraftMarkers(it.Body)
		malformed += bad
		named := filed[commsFiledItemKey{IssueNumber: it.IssueNumber, CommentID: it.CommentID}]
		for _, e := range entries {
			if _, ok := named[e]; !ok {
				continue
			}
			trusted.add(commsSuppression{ID: e.ID, Hash: e.ContentHash, Basis: commsSuppressionBasisMarker})
		}
	}
	return trusted, malformed
}

// commsCursorHoldBack returns the bounds phase 7 advances the cursor to when
// the reports named by ids are NOT accounted for (unaccounted reports, and
// the sources of approved-but-unfiled drafts): cursor = min(pending cursor,
// updated_at of each named shown report) and noteCursor = min(pending note
// cursor, cursor), each clamped to never retreat behind the scan's read
// bounds. An id the row does not show returns the read bounds (Since,
// NoteSince) — no movement, the fail-closed direction. ok is false when the
// row has no pending cursor: the scan did not complete and phase 7 must not
// advance at all.
func commsCursorHoldBack(p commsScanGatheredPayload, ids []string) (cursor, noteCursor time.Time, ok bool) {
	pc := p.PendingCursor
	if pc == nil {
		return time.Time{}, time.Time{}, false
	}
	shown := make(map[string]time.Time, len(p.Shown))
	for _, r := range p.Shown {
		shown[r.ID] = r.UpdatedAt
	}
	cursor = pc.Cursor
	for _, id := range ids {
		at, found := shown[id]
		if !found {
			return pc.Since, pc.NoteSince, true
		}
		if at.Before(cursor) {
			cursor = at
		}
	}
	noteCursor = pc.NoteCursor
	if cursor.Before(noteCursor) {
		noteCursor = cursor
	}
	if cursor.Before(pc.Since) {
		cursor = pc.Since
	}
	if noteCursor.Before(pc.NoteSince) {
		noteCursor = pc.NoteSince
	}
	if noteCursor.After(cursor) {
		noteCursor = cursor // a row whose NoteSince exceeds Since: keep note <= cursor
	}
	return cursor, noteCursor, true
}
