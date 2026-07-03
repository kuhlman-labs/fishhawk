package server

// audit_export.go implements GET /v0/audit/export — the producer half
// of the compliance-export contract (ADR-008 / ADR-054, #1604). It
// assembles the EXACT Export v1 wire shape the external verifier
// consumes.
//
// LOCKSTEP CONTRACT: the wire structs below mirror
// verifier/internal/audit/export.go field-for-field and tag-for-tag.
// That verifier file is the BINDING definition (ADR-054): its
// ParseExport calls json.Decoder.DisallowUnknownFields, so ANY extra
// field anywhere in the body — including nested objects — makes the
// verifier reject the export. Continuation/partiality markers
// therefore ride RESPONSE HEADERS (X-Fishhawk-Export-Complete /
// X-Fishhawk-Export-Next-Cursor), never the body; the body stays the
// pure three-field Export v1 so every page byte-parses through
// ParseExport and, being whole-run bounded, independently passes
// VerifyExport. The exact-mirror-struct + strict-decode + hash
// recompute test in audit_export_test.go fails if the producer ever
// drifts from this contract. The verifier package is `internal`, so
// it cannot be imported here (Go internal-package rule); the byte
// compatibility is pinned instead by backend/internal/audit's
// canonical fixture, shared with verifier/internal/audit.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// exportSchemaV1 is the schema string the verifier's ExportSchemaV1
// recognizes. Duplicated as a literal (not imported) because the
// verifier package is internal to the verifier module; drift is caught
// by the strict-decode mirror test asserting schema == "v1".
const exportSchemaV1 = "v1"

// exportGlobalChainKey is the reserved runs-map key under which the
// global (run-less) chain partition is exported. The nil UUID parses
// as a valid UUID in the verifier's VerifyExport run walk; the
// partition's entries carry run_id:null — exactly the value their
// hashes were computed over via HashInputs — so the chain verifies
// with no verifier change. Real run IDs are v4/v7 UUIDs, so this
// reserved key cannot collide with a real run.
const exportGlobalChainKey = "00000000-0000-0000-0000-000000000000"

// Export-page bounding is at WHOLE-RUN granularity: a run's entire
// chain is emitted on one page (never split), so every page
// independently passes VerifyExport from each run's nil-prev_hash
// genesis entry. The limit bounds the number of RUNS per page.
const (
	exportDefaultRunLimit = 50
	exportMaxRunLimit     = 200
	// exportRunPageSize bounds the repository ListRuns page while
	// materializing the filtered run set in memory. v0 volumes; the
	// same in-memory posture calibration.go / reads.go document. SQL
	// push-down of the created_at range + keyset is a follow-up when
	// volume demands (it would require an sqlc regeneration this
	// change deliberately avoids).
	exportRunPageSize = 500
)

// exportResponse mirrors verifier/internal/audit/audit.Export.
type exportResponse struct {
	Schema     string                   `json:"schema"`
	ExportedAt time.Time                `json:"exported_at"`
	Runs       map[string]exportRunData `json:"runs"`
}

// exportRunData mirrors verifier/internal/audit/audit.RunData.
// SigningKey carries omitempty so a run that never issued a key
// serializes without the field, matching the verifier's optional
// signing_key (its chain walk does not require it).
type exportRunData struct {
	SigningKey   *exportSigningKey `json:"signing_key,omitempty"`
	AuditEntries []exportEntry     `json:"audit_entries"`
}

// exportSigningKey mirrors verifier/internal/audit/audit.SigningKey.
// PublicKey is base64 so the export stays text-safe.
type exportSigningKey struct {
	PublicKey string    `json:"public_key"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// exportEntry mirrors verifier/internal/audit/audit.Entry. The
// nullable pointer fields carry NO omitempty so they serialize as
// explicit JSON nulls — exactly the value their hashes were computed
// over, and exactly what the verifier's Entry expects to decode.
type exportEntry struct {
	ID           uuid.UUID       `json:"id"`
	Sequence     int64           `json:"sequence"`
	RunID        *uuid.UUID      `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *string         `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
}

// exportCursor is the keyset continuation token. base64(JSON) of the
// last run on the prior page; resume scans the materialized order for
// the first run strictly after this (created_at, id) pair. Value-based
// (not offset-based) so a run created between pages — which sorts
// before the cursor position in created_at DESC order — is skipped by
// value rather than shifting an index.
type exportCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// exportPage is the resolved run-selection page shared by the JSON and
// CSV export handlers. It is the single run-selection code path: query
// parsing, run_id XOR repo/date mutual exclusion, limit/cursor
// validation, created_at DESC keyset paging. Both handlers project the
// SAME page — the CSV endpoint is a projection over this shared layer,
// never a parallel query path.
type exportPage struct {
	page          []*run.Run
	includeGlobal bool
	firstPage     bool // this is the first page (empty cursor)
	complete      bool
	nextCursor    string
}

// resolveExportPage performs the shared run-selection and keyset paging
// for both export handlers. It parses the query, enforces the ADR-054
// run_id/repo-date mutual exclusion, validates limit and cursor,
// materializes the selected run set, sorts it created_at DESC / id DESC,
// and returns the bounded page. On any validation or selection error it
// writes the error response itself and returns ok=false, so the caller
// must return immediately without writing anything more. The nil-repo
// fail-closed 503 check stays in each handler (before this call).
func (s *Server) resolveExportPage(w http.ResponseWriter, r *http.Request) (*exportPage, bool) {
	q := r.URL.Query()
	rawRunIDs := q["run_id"]
	explicitMode := len(rawRunIDs) > 0
	repo := q.Get("repo")
	rawFrom := q.Get("from")
	rawTo := q.Get("to")

	// The explicit run-id set and the filter shape (repo/date) are the
	// two mutually exclusive filtering modes per ADR-054.
	if explicitMode && (repo != "" || rawFrom != "" || rawTo != "") {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id cannot be combined with repo, from, or to",
			map[string]any{"field": "run_id"})
		return nil, false
	}

	limit, err := parseLimit(q.Get("limit"), exportDefaultRunLimit, exportMaxRunLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return nil, false
	}

	includeGlobal := q.Get("include_global") != "false"

	cursor, err := decodeExportCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid", err.Error(), nil)
		return nil, false
	}

	var selected []*run.Run
	if explicitMode {
		selected, err = s.selectExplicitRuns(r.Context(), w, r, rawRunIDs)
		if err != nil {
			// selectExplicitRuns already wrote the error response.
			return nil, false
		}
	} else {
		selected, err = s.selectFilteredRuns(r.Context(), w, r, repo, rawFrom, rawTo)
		if err != nil {
			return nil, false
		}
	}

	// Deterministic order for keyset paging: created_at DESC, id DESC
	// (the repository's order + tiebreak).
	sort.Slice(selected, func(i, j int) bool {
		if !selected[i].CreatedAt.Equal(selected[j].CreatedAt) {
			return selected[i].CreatedAt.After(selected[j].CreatedAt)
		}
		return selected[i].ID.String() > selected[j].ID.String()
	})

	// Keyset continuation: skip to the first run strictly after the
	// cursor pair, then take up to `limit` runs. More remaining ⇒ this
	// page is partial.
	start := 0
	if cursor != nil {
		for start < len(selected) && !runAfterCursor(selected[start], cursor) {
			start++
		}
	}
	remaining := selected[start:]
	page := remaining
	complete := true
	var nextCursor string
	if len(remaining) > limit {
		page = remaining[:limit]
		complete = false
		last := page[len(page)-1]
		nextCursor = encodeExportCursor(exportCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}

	return &exportPage{
		page:          page,
		includeGlobal: includeGlobal,
		firstPage:     cursor == nil,
		complete:      complete,
		nextCursor:    nextCursor,
	}, true
}

// handleAuditExport implements GET /v0/audit/export.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	// Fail closed: a compliance artifact must not silently omit its
	// inputs. All three repositories are required.
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil || s.cfg.SigningRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_export_unconfigured",
			"audit export requires configured audit, run, and signing repositories", nil)
		return
	}

	ep, ok := s.resolveExportPage(w, r)
	if !ok {
		// resolveExportPage already wrote the error response.
		return
	}

	resp := exportResponse{
		Schema:     exportSchemaV1,
		ExportedAt: s.nowFunc().UTC(),
		Runs:       make(map[string]exportRunData, len(ep.page)+1),
	}
	for _, rn := range ep.page {
		rd, aerr := s.assembleRunData(r.Context(), rn.ID)
		if aerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"assemble run export failed", map[string]any{"error": aerr.Error(), "run_id": rn.ID.String()})
			return
		}
		resp.Runs[rn.ID.String()] = rd
	}

	// Global (run-less) chain partition: first page only (empty
	// cursor). Emitted even when the chain is empty so consumers can
	// distinguish "no global entries" from "not included" — never
	// silently dropped (ADR-054 consequence). Absent on continuation
	// pages (whole-chain bounding: it can't be split) and when
	// include_global=false.
	if ep.includeGlobal && ep.firstPage {
		globalEntries, gerr := s.cfg.AuditRepo.ListGlobal(r.Context())
		if gerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list global chain failed", map[string]any{"error": gerr.Error()})
			return
		}
		resp.Runs[exportGlobalChainKey] = exportRunData{AuditEntries: toExportEntries(globalEntries)}
	}

	// Continuation rides headers because ParseExport's
	// DisallowUnknownFields forbids any extra body field.
	w.Header().Set("X-Fishhawk-Export-Complete", strconv.FormatBool(ep.complete))
	if !ep.complete {
		w.Header().Set("X-Fishhawk-Export-Next-Cursor", ep.nextCursor)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// selectExplicitRuns resolves an explicit run-id set. A compliance
// export must fail closed: a requested run that does not exist is a
// 404, never a silent omission. On any error it writes the response
// and returns a non-nil error so the caller stops.
func (s *Server) selectExplicitRuns(ctx context.Context, w http.ResponseWriter, r *http.Request, rawRunIDs []string) ([]*run.Run, error) {
	selected := make([]*run.Run, 0, len(rawRunIDs))
	for _, raw := range rawRunIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"run_id must be a valid UUID", map[string]any{"field": "run_id", "got": raw})
			return nil, err
		}
		rn, err := s.cfg.RunRepo.GetRun(ctx, id)
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				s.writeError(w, r, http.StatusNotFound, "run_not_found",
					"requested run does not exist", map[string]any{"run_id": id.String()})
				return nil, err
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"get run failed", map[string]any{"error": err.Error()})
			return nil, err
		}
		selected = append(selected, rn)
	}
	return selected, nil
}

// selectFilteredRuns materializes the repo-filtered run set and
// applies the created_at date range in memory. On any error it writes
// the response and returns a non-nil error so the caller stops.
func (s *Server) selectFilteredRuns(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, rawFrom, rawTo string) ([]*run.Run, error) {
	var from, to time.Time
	var haveFrom, haveTo bool
	if rawFrom != "" {
		t, perr := time.Parse(time.RFC3339, rawFrom)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"from must be an RFC3339 timestamp", map[string]any{"field": "from", "got": rawFrom})
			return nil, perr
		}
		from, haveFrom = t, true
	}
	if rawTo != "" {
		t, perr := time.Parse(time.RFC3339, rawTo)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"to must be an RFC3339 timestamp", map[string]any{"field": "to", "got": rawTo})
			return nil, perr
		}
		to, haveTo = t, true
	}
	if haveFrom && haveTo && from.After(to) {
		err := errors.New("from is after to")
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"from must not be after to", map[string]any{"from": rawFrom, "to": rawTo})
		return nil, err
	}

	all, err := s.materializeRuns(ctx, repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return nil, err
	}
	selected := make([]*run.Run, 0, len(all))
	for _, rn := range all {
		if haveFrom && rn.CreatedAt.Before(from) {
			continue
		}
		if haveTo && rn.CreatedAt.After(to) {
			continue
		}
		selected = append(selected, rn)
	}
	return selected, nil
}

// materializeRuns loops ListRuns (Repo pushed down) until a short page,
// gathering every matching run. In-memory posture per calibration.go /
// reads.go; acceptable at v0 volumes.
func (s *Server) materializeRuns(ctx context.Context, repo string) ([]*run.Run, error) {
	var out []*run.Run
	offset := 0
	for {
		page, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			Repo:   repo,
			Limit:  exportRunPageSize,
			Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < exportRunPageSize {
			break
		}
		offset += exportRunPageSize
	}
	return out, nil
}

// assembleRunData builds one run's export portion: its full audit
// chain plus its signing key (omitted when the run never issued one —
// e.g. local runs — which the verifier's chain walk tolerates).
func (s *Server) assembleRunData(ctx context.Context, runID uuid.UUID) (exportRunData, error) {
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		return exportRunData{}, err
	}
	rd := exportRunData{AuditEntries: toExportEntries(entries)}

	key, err := s.cfg.SigningRepo.Get(ctx, runID)
	switch {
	case err == nil:
		rd.SigningKey = &exportSigningKey{
			PublicKey: base64.StdEncoding.EncodeToString(key.PublicKey),
			IssuedAt:  key.IssuedAt.UTC(),
			ExpiresAt: key.ExpiresAt.UTC(),
		}
	case errors.Is(err, signing.ErrNotFound):
		// No key issued for this run: export with signing_key absent.
	default:
		return exportRunData{}, err
	}
	return rd, nil
}

// toExportEntries maps stored audit entries to the wire shape. Always
// returns a non-nil slice so an empty chain serializes as `[]` rather
// than `null`.
func toExportEntries(entries []*audit.Entry) []exportEntry {
	out := make([]exportEntry, 0, len(entries))
	for _, e := range entries {
		var actorKind *string
		if e.ActorKind != nil {
			v := string(*e.ActorKind)
			actorKind = &v
		}
		out = append(out, exportEntry{
			ID:           e.ID,
			Sequence:     e.Sequence,
			RunID:        e.RunID,
			StageID:      e.StageID,
			Timestamp:    e.Timestamp,
			Category:     e.Category,
			ActorKind:    actorKind,
			ActorSubject: e.ActorSubject,
			Payload:      e.Payload,
			PrevHash:     e.PrevHash,
			EntryHash:    e.EntryHash,
		})
	}
	return out
}

// runAfterCursor reports whether rn sorts strictly after the cursor
// pair under created_at DESC, id DESC ordering.
func runAfterCursor(rn *run.Run, c *exportCursor) bool {
	if !rn.CreatedAt.Equal(c.CreatedAt) {
		return rn.CreatedAt.Before(c.CreatedAt)
	}
	return rn.ID.String() < c.ID.String()
}

func encodeExportCursor(c exportCursor) string {
	raw, _ := json.Marshal(c) // cannot fail for these fields
	return base64.URLEncoding.EncodeToString(raw)
}

// decodeExportCursor returns nil for an empty cursor (first page) or a
// decoded cursor. A malformed cursor is a 400, never a silent reset to
// the first page.
func decodeExportCursor(raw string) (*exportCursor, error) {
	if raw == "" {
		return nil, nil
	}
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("cursor is not valid base64")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c exportCursor
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("cursor is not in the expected shape")
	}
	// Both keyset components are required: a decodable-but-incomplete
	// cursor (e.g. base64("{}")) leaves a zero created_at / nil id that
	// runAfterCursor would silently mis-order into an empty complete
	// page. Reject it as malformed rather than reset to the first page.
	if c.CreatedAt.IsZero() || c.ID == uuid.Nil {
		return nil, errors.New("cursor is missing created_at or id")
	}
	return &c, nil
}
