package server

// audit_export_csv.go implements GET /v0/audit/export.csv — a flat CSV
// rendering of the E9.1 compliance export (#1604) for spreadsheet
// compliance workflows.
//
// The CSV endpoint is a PROJECTION over the JSON export's assembly and
// filter layer (resolveExportPage + assembleRunData), never a parallel
// query path: it selects the SAME run page and reuses the SAME per-run
// assembly the JSON handler does, then flattens each audit entry to one
// row. The parity test (audit_export_csv_test.go) proves the CSV rows
// are a field-for-field projection of the JSON Export v1 body for the
// same run-level filter set — it fails if the CSV path ever grows
// divergent query logic.
//
// Two additional entry-level filters — approver and category — apply as
// in-memory row predicates. Entry-level filtering is CSV-ONLY by design:
// the JSON Export v1 body cannot drop entries without breaking the
// verifier's hash-chain walk (each entry's prev_hash binds its
// predecessor), so approver/category are not offered on the JSON
// endpoint.

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
)

// csvExportHeader is the fixed column order of the CSV export. Every row
// (run and global partition) is this exact shape.
var csvExportHeader = []string{
	"ts", "run_id", "repo", "category", "actor_kind",
	"actor_subject", "sequence", "entry_hash", "payload_summary",
}

// payloadSummaryRuneLimit bounds the payload_summary column. Full
// payloads remain JSON-export-only; the CSV carries a compacted,
// rune-bounded summary for spreadsheet legibility.
const payloadSummaryRuneLimit = 256

// payloadTruncationMarker is appended to a payload_summary that was cut
// at the rune boundary.
const payloadTruncationMarker = "...(truncated)"

// csvRowFilter is the ANDed in-memory entry-level predicate applied to
// every emitted row. Both fields are CSV-only (see file header).
type csvRowFilter struct {
	// approver, when non-empty, keeps only approval_submitted entries
	// whose actor_subject equals this value.
	approver string
	// category, when non-empty, keeps only entries whose category equals
	// this value (exact match, same param shape as the audit-list filter
	// in reads.go).
	category string
}

// keep reports whether an entry passes the ANDed row predicate. Both
// filters are exact-match: approver additionally requires the entry be
// an approval_submitted with a non-nil actor_subject.
func (f csvRowFilter) keep(e *exportEntry) bool {
	if f.approver != "" {
		if e.Category != "approval_submitted" || e.ActorSubject == nil || *e.ActorSubject != f.approver {
			return false
		}
	}
	if f.category != "" && e.Category != f.category {
		return false
	}
	return true
}

// handleAuditExportCSV implements GET /v0/audit/export.csv.
func (s *Server) handleAuditExportCSV(w http.ResponseWriter, r *http.Request) {
	// Same read:audit-export gate as the JSON handler (E9.5 / #1608):
	// the CSV is a projection of the same evidence, so it carries the
	// same exfiltration risk class. Auth before the config probe.
	if !s.requireWriteScope(w, r, scopeAuditExport) {
		return
	}
	// Fail closed: a compliance artifact must not silently omit its
	// inputs. All three repositories are required (identical posture to
	// the JSON handler).
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_export_unconfigured",
			"audit export requires configured audit, run, and signing repositories", nil)
		return
	}

	q := r.URL.Query()
	filter := csvRowFilter{
		approver: q.Get("approver"),
		category: q.Get("category"),
	}

	ep, ok := s.resolveExportPage(w, r)
	if !ok {
		// resolveExportPage already wrote the error response.
		return
	}
	// Account-scope the page (ADR-057 / E44.5): the CSV projection carries the
	// same tenant-isolation as the JSON export. The run-less global partition
	// below is unfiltered (it has no owning run).
	ep.page = accountVisiblePage(r, ep.page)

	// Render the WHOLE page to a buffer first, so any per-run assembly
	// error becomes a clean JSON writeError with no partial CSV bytes
	// ever leaving the server (buffer-before-write contract).
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	if err := cw.Write(csvExportHeader); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"write csv header failed", map[string]any{"error": err.Error()})
		return
	}

	for _, rn := range ep.page {
		rd, aerr := s.assembleRunData(r.Context(), rn.ID)
		if aerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"assemble run export failed", map[string]any{"error": aerr.Error(), "run_id": rn.ID.String()})
			return
		}
		for i := range rd.AuditEntries {
			e := &rd.AuditEntries[i]
			if !filter.keep(e) {
				continue
			}
			if err := cw.Write(csvRowFromExportEntry(e, rn.Repo)); err != nil {
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"write csv row failed", map[string]any{"error": err.Error(), "run_id": rn.ID.String()})
				return
			}
		}
	}

	// Run-less chain partition: first page only, scoped to the caller
	// (#2097). Its rows carry empty run_id and repo cells and pass
	// through the same predicate, emitted after the run rows. An
	// operator sees the full untenanted+tenant set; a tenant sees only
	// its own account partition; a malformed-account caller sees none.
	if ep.includeGlobal && ep.firstPage {
		globalEntries, gerr := s.callerRunlessEntries(r.Context(), r)
		if gerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list global chain failed", map[string]any{"error": gerr.Error()})
			return
		}
		for _, ge := range toExportEntries(globalEntries) {
			ge := ge
			if !filter.keep(&ge) {
				continue
			}
			if err := cw.Write(csvRowFromExportEntry(&ge, "")); err != nil {
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"write csv global row failed", map[string]any{"error": err.Error()})
				return
			}
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"flush csv failed", map[string]any{"error": err.Error()})
		return
	}

	// Success: set headers and write the buffered CSV once. Continuation
	// rides the SAME headers as the JSON endpoint (run-level cursor
	// semantics, unchanged).
	filename := "fishhawk-audit-export-" + s.nowFunc().UTC().Format("20060102T150405Z") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Fishhawk-Export-Complete", strconv.FormatBool(ep.complete))
	if !ep.complete {
		w.Header().Set("X-Fishhawk-Export-Next-Cursor", ep.nextCursor)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// csvRowFromExportEntry projects one exportEntry to its CSV row in
// csvExportHeader column order. runRepo is the owning run's Repo (empty
// for the global partition). Nil actor_kind/actor_subject and a nil
// run_id render as empty cells.
func csvRowFromExportEntry(e *exportEntry, runRepo string) []string {
	var runID string
	if e.RunID != nil {
		runID = e.RunID.String()
	}
	var actorKind string
	if e.ActorKind != nil {
		actorKind = *e.ActorKind
	}
	var actorSubject string
	if e.ActorSubject != nil {
		actorSubject = *e.ActorSubject
	}
	return []string{
		e.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
		runID,
		runRepo,
		e.Category,
		actorKind,
		actorSubject,
		strconv.FormatInt(e.Sequence, 10),
		e.EntryHash,
		payloadSummary(e.Payload),
	}
}

// payloadSummary compacts the payload JSON and bounds it at
// payloadSummaryRuneLimit runes, appending payloadTruncationMarker when
// cut. Truncation is rune-boundary safe: it never splits a multi-byte
// character. A payload that fails to compact (should not happen for
// stored JSON) falls back to its raw bytes, bounded identically.
func payloadSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var compact bytes.Buffer
	s := string(raw)
	if err := json.Compact(&compact, raw); err == nil {
		s = compact.String()
	}
	return truncateRunes(s, payloadSummaryRuneLimit)
}

// truncateRunes returns s bounded at limit runes, appending
// payloadTruncationMarker when it was cut. Iterating over runes keeps
// the cut on a rune boundary regardless of multi-byte characters.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + payloadTruncationMarker
}
