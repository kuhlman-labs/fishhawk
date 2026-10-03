package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/permdrift"
)

// The permission-drift check's audit categories (E80.4 / #3761). All three are
// INTERNAL: no issue-comment activity line, so none is an issue-comment
// surface. Payloads carry surfaces, paths, keys and grant values only — never
// file bytes.
const (
	// permissionDriftDetectedCategory is the origin entry of a raise: one per
	// check that raised at least one NEW server_check concern, system actor,
	// stage-anchored. Its sequence is the concerns' OriginReviewSequence.
	permissionDriftDetectedCategory = "permission_drift_detected"
	// permissionNarrowingNoticedCategory records the narrowings one check
	// saw. A narrowing is never a concern.
	permissionNarrowingNoticedCategory = "permission_narrowing_noticed"
	// permissionDriftRaiseFailedCategory records that InsertRaised failed
	// twice after the detected entry landed, so the gap between the origin
	// entry and the concern store is visible beyond a log line.
	permissionDriftRaiseFailedCategory = "permission_drift_raise_failed"
)

// The checkPermissionDrift triggers, recorded on the detected entry.
const (
	permissionDriftTriggerPROpened           = "pull_request_opened"
	permissionDriftTriggerFixupPushed        = "fixup_pushed"
	permissionDriftTriggerConflictResolution = "conflict_resolution_pushed"
	permissionDriftTriggerConsolidated       = "consolidated_review"
)

// permissionDriftAllSurfaces is the pseudo-surface a check-wide failure (the
// forge compare itself failed) is raised against: no single surface file can
// be named, so the concern covers every one.
var permissionDriftAllSurfaces = permdrift.Surface{
	ID:          "all-surfaces",
	Severity:    permdrift.SeverityHigh,
	Description: "every declared permission surface",
}

// permissionDriftRaiseMu serializes the read-check-insert of the raise so two
// concurrent checks for one stage cannot both observe a check key absent and
// mint a duplicate. One process only — a multi-replica deployment can still
// race (server/README.md residual, shared with the diff secrets check).
var permissionDriftRaiseMu sync.Mutex

// permissionDriftRequest is one check: the run and the stage concerns attach
// to, the base and head commits to compare, and what triggered it.
type permissionDriftRequest struct {
	RunID   uuid.UUID
	StageID uuid.UUID
	Base    string
	Head    string
	Trigger string
	// IntersectRef, when set (a conflict-resolution push), is the run's base
	// BRANCH. The forge has no merge-base primitive, so each changed surface
	// file is ALSO evaluated IntersectRef → Head and only widenings present in
	// BOTH comparisons raise (keyed by check key): a widening the base branch
	// itself introduced is absent from the second comparison and is dropped,
	// one the resolution introduced survives.
	IntersectRef string
	// IntersectUnresolved records that a conflict-resolution push's base
	// branch could not be read, so the check ran the single comparison (a
	// superset: base-branch widenings raise too — noise, never a miss).
	IntersectUnresolved bool
}

// permissionDriftDetectedPayload is the permission_drift_detected payload.
type permissionDriftDetectedPayload struct {
	Check               string                       `json:"check"`
	SurfacesVersion     int                          `json:"surfaces_version"`
	Trigger             string                       `json:"trigger"`
	BaseSHA             string                       `json:"base_sha"`
	HeadSHA             string                       `json:"head_sha"`
	IntersectRef        string                       `json:"intersect_ref,omitempty"`
	IntersectUnresolved bool                         `json:"intersect_unresolved,omitempty"`
	CompareTruncated    bool                         `json:"compare_truncated"`
	Widenings           []permissionDriftWidening    `json:"widenings"`
	Unevaluable         []permissionDriftUnevaluable `json:"unevaluable"`
	ExtensionRejected   []permdrift.Rejection        `json:"extension_rejected,omitempty"`
	ExtensionError      string                       `json:"extension_error,omitempty"`
}

type permissionDriftWidening struct {
	Surface  string `json:"surface"`
	Path     string `json:"path"`
	Key      string `json:"key"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Severity string `json:"severity"`
	CheckKey string `json:"check_key"`

	surface permdrift.Surface
	change  permdrift.Change
}

type permissionDriftUnevaluable struct {
	Surface  string `json:"surface"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
	CheckKey string `json:"check_key"`

	surface permdrift.Surface
}

type permissionNarrowing struct {
	Surface string `json:"surface"`
	Path    string `json:"path"`
	Key     string `json:"key"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

// permissionDriftFindings is one check's evaluation before de-duplication.
type permissionDriftFindings struct {
	widenings   []permissionDriftWidening
	unevaluable []permissionDriftUnevaluable
	narrowings  []permissionNarrowing
	truncated   bool
	extRejected []permdrift.Rejection
	extError    string
}

// checkPermissionDrift runs the deterministic permission-drift check (ADR-084
// D5 / rule 5, E80.4 / #3761) on a detached, shutdown-tracked goroutine, so the
// caller's report latency is unchanged. A nil ConcernRepo, AuditRepo or
// RunRepo, or a request without both commits, is a no-op. See
// runPermissionDriftCheck for the check itself.
func (s *Server) checkPermissionDrift(ctx context.Context, req permissionDriftRequest) {
	if !s.permissionDriftRunnable(ctx, req) {
		return
	}
	bg := context.WithoutCancel(ctx)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		s.runPermissionDriftCheck(bg, req)
	}()
}

// permissionDriftRunnable reports whether a check can run: false (a no-op) for
// a nil ConcernRepo, AuditRepo or RunRepo, or a request without both commits.
func (s *Server) permissionDriftRunnable(ctx context.Context, req permissionDriftRequest) bool {
	if s.cfg.ConcernRepo == nil || s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return false
	}
	if req.Base == "" || req.Head == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "permission-drift check: base or head commit missing — skipped",
			slog.String("run_id", req.RunID.String()),
			slog.String("stage_id", req.StageID.String()),
			slog.String("trigger", req.Trigger))
		return false
	}
	return true
}

// runPermissionDriftCheck is the synchronous body of checkPermissionDrift
// (the consolidated review calls it directly, inside its own background
// goroutine, so the check and that review's compare never run concurrently).
//
//  1. Resolve the forge compare and file fetcher; either unavailable is an
//     INFO-logged skip (the CLI/dev posture, as the consolidated review).
//  2. Read .fishhawk/permission-surfaces.yaml at BASE only (404 = none; an
//     unreadable or unparseable file = the product list only, recorded as
//     extension_error; rejected entries recorded).
//  3. ComparePatch(base, head) for the changed files. A compare error raises
//     ONE check-wide compare_failed concern. When the compare is truncated, or
//     reports a rename (whose source path it does not list), every exact-path
//     surface is probed at both refs anyway; on truncation each glob surface
//     also raises one compare_truncated concern.
//  4. For each (surface, changed path) fetch base and head (ErrNotFound = side
//     absent; any other error = fetch_failed for that pair) and Detect.
//  5. raisePermissionDrift de-duplicates and raises; narrowings go to one
//     permission_narrowing_noticed entry, never to a concern.
func (s *Server) runPermissionDriftCheck(ctx context.Context, req permissionDriftRequest) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, req.RunID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: get run failed — skipped",
			slog.String("run_id", req.RunID.String()), slog.String("error", err.Error()))
		return
	}
	comparer, scope, repo, reason := s.forgeCompareFor(runRow)
	if reason != "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "permission-drift check: forge compare not wired — skipped",
			slog.String("run_id", req.RunID.String()), slog.String("reason", reason))
		return
	}
	fetcher, fscope, frepo, freason := s.fileFetcherFor(runRow)
	if freason != "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "permission-drift check: forge file fetch not wired — skipped",
			slog.String("run_id", req.RunID.String()), slog.String("reason", freason))
		return
	}
	reader := &permissionDriftReader{ctx: ctx, f: fetcher, scope: fscope, repo: frepo, cache: map[[2]string]driftSide{}}

	var fx permissionDriftFindings
	surfaces := s.permissionDriftSurfaces(ctx, req, reader, &fx)

	cmp, cerr := comparer.ComparePatch(ctx, scope, repo, req.Base, req.Head)
	if cerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: forge compare failed — raising unevaluable",
			slog.String("run_id", req.RunID.String()), slog.String("error", cerr.Error()))
		fx.unevaluable = append(fx.unevaluable, newUnevaluable(permissionDriftAllSurfaces,
			req.Base+".."+req.Head, permdrift.ReasonCompareFailed))
		s.raisePermissionDrift(ctx, req, fx)
		return
	}
	fx.truncated = cmp.Truncated

	type pair struct {
		s    permdrift.Surface
		path string
	}
	var pairs []pair
	seen := map[string]bool{}
	add := func(sf permdrift.Surface, path string) {
		k := sf.ID + "\x00" + path
		if !seen[k] {
			seen[k] = true
			pairs = append(pairs, pair{sf, path})
		}
	}
	renamed := false
	for _, f := range cmp.Files {
		if f.Status == "renamed" {
			renamed = true
		}
		for _, sf := range permdrift.MatchSurfaces(surfaces, f.Path) {
			add(sf, f.Path)
		}
	}
	if cmp.Truncated || renamed {
		for _, sf := range surfaces {
			glob := false
			for _, p := range sf.Paths {
				if isGlobPath(p) {
					glob = true
					continue
				}
				add(sf, p)
			}
			if glob && cmp.Truncated {
				fx.unevaluable = append(fx.unevaluable, newUnevaluable(sf, "", permdrift.ReasonCompareTruncated))
			}
		}
	}

	for _, p := range pairs {
		res := reader.detect(p.s, p.path, req.Base, req.Head)
		if req.IntersectRef != "" && res.Unevaluable == "" {
			res = intersectDrift(p.s, p.path, res, reader.detect(p.s, p.path, req.IntersectRef, req.Head))
		}
		if res.Unevaluable != "" {
			fx.unevaluable = append(fx.unevaluable, newUnevaluable(p.s, p.path, res.Unevaluable))
			continue
		}
		for _, c := range res.Widened {
			fx.widenings = append(fx.widenings, permissionDriftWidening{
				Surface: p.s.ID, Path: p.path, Key: c.Key, Before: c.Before, After: c.After,
				Severity: string(p.s.Severity), CheckKey: permdrift.CheckKey(p.s.ID, p.path, c.Key, c.After),
				surface: p.s, change: c,
			})
		}
		for _, c := range res.Narrowed {
			fx.narrowings = append(fx.narrowings, permissionNarrowing{
				Surface: p.s.ID, Path: p.path, Key: c.Key, Before: c.Before, After: c.After,
			})
		}
	}
	s.raisePermissionDrift(ctx, req, fx)
}

// permissionDriftSurfaces returns the product surfaces merged with the
// repository's extension, read at req.Base ONLY so a change cannot remove its
// own surface. Absence is no extension; any other read failure or a parse
// error leaves the product list and records extension_error.
func (s *Server) permissionDriftSurfaces(ctx context.Context, req permissionDriftRequest, reader *permissionDriftReader, fx *permissionDriftFindings) []permdrift.Surface {
	product := permdrift.DefaultSurfaces()
	side := reader.read(permdrift.RepoSurfacesPath, req.Base)
	switch {
	case side.err != nil:
		fx.extError = permdrift.ReasonFetchFailed
	case !side.f.Exists:
		return product
	default:
		accepted, rejected, err := permdrift.ParseRepoSurfaces(side.f.Content)
		if err != nil {
			fx.extError = permdrift.ReasonParseError
			break
		}
		fx.extRejected = rejected
		return permdrift.MergeSurfaces(product, accepted)
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: repository surface extension unreadable — product surfaces only",
		slog.String("run_id", req.RunID.String()),
		slog.String("path", permdrift.RepoSurfacesPath),
		slog.String("reason", fx.extError))
	return product
}

// isGlobPath reports whether a surface path carries doublestar metacharacters.
func isGlobPath(p string) bool { return strings.ContainsAny(p, "*?[{\\") }

func newUnevaluable(sf permdrift.Surface, path, reason string) permissionDriftUnevaluable {
	return permissionDriftUnevaluable{
		Surface: sf.ID, Path: path, Reason: reason,
		CheckKey: permdrift.UnevaluableKey(sf.ID, path), surface: sf,
	}
}

// intersectDrift keeps the changes of first (previous head → pushed head)
// that second (base-branch tip → pushed head) also reports, keyed by check
// key. An unevaluable second comparison fails the pair closed.
func intersectDrift(sf permdrift.Surface, path string, first, second permdrift.Result) permdrift.Result {
	if second.Unevaluable != "" {
		return second
	}
	keys := map[string]bool{}
	for _, c := range append(append([]permdrift.Change(nil), second.Widened...), second.Narrowed...) {
		keys[permdrift.CheckKey(sf.ID, path, c.Key, c.After)] = true
	}
	keep := func(in []permdrift.Change) []permdrift.Change {
		var out []permdrift.Change
		for _, c := range in {
			if keys[permdrift.CheckKey(sf.ID, path, c.Key, c.After)] {
				out = append(out, c)
			}
		}
		return out
	}
	return permdrift.Result{Widened: keep(first.Widened), Narrowed: keep(first.Narrowed)}
}

// driftSide is one fetched file side; err is set for a read failure other
// than absence.
type driftSide struct {
	f   permdrift.FileSide
	err error
}

// permissionDriftReader fetches surface files once per (path, ref).
type permissionDriftReader struct {
	ctx   context.Context
	f     forge.FileFetcher
	scope forge.CredentialScope
	repo  forge.RepoRef
	cache map[[2]string]driftSide
}

func (r *permissionDriftReader) read(path, ref string) driftSide {
	k := [2]string{path, ref}
	if d, ok := r.cache[k]; ok {
		return d
	}
	var d driftSide
	fc, err := r.f.FetchFile(r.ctx, r.scope, r.repo, path, ref)
	switch {
	case errors.Is(err, forge.ErrNotFound):
		d.f = permdrift.FileSide{Exists: false}
	case err != nil:
		d.err = err
	default:
		d.f = permdrift.FileSide{Content: fc.Content, Exists: true}
	}
	r.cache[k] = d
	return d
}

// detect fetches path at both refs and evaluates sf; a read failure on either
// side is fetch_failed (never an absent side, which would read as narrowing).
func (r *permissionDriftReader) detect(sf permdrift.Surface, path, base, head string) permdrift.Result {
	b, h := r.read(path, base), r.read(path, head)
	if b.err != nil || h.err != nil {
		return permdrift.Result{Unevaluable: permdrift.ReasonFetchFailed}
	}
	return permdrift.Detect(sf, b.f, h.f)
}

// raisePermissionDrift records a check's findings: one
// permission_narrowing_noticed entry for any narrowings, then — under
// permissionDriftRaiseMu — the widenings and unevaluables not already on
// record for the stage as a server_check row in an open, waived or deferred
// state (superseded/addressed do not suppress; a ListByRun error fails OPEN to
// raising). The permission_drift_detected entry is appended FIRST and stamps
// the concerns' origin sequence; an append failure raises nothing. InsertRaised
// is retried once; a second failure appends permission_drift_raise_failed and
// the detected entry stands as the record.
func (s *Server) raisePermissionDrift(ctx context.Context, req permissionDriftRequest, fx permissionDriftFindings) {
	systemKind := audit.ActorKind("system")
	stageID := req.StageID
	if len(fx.narrowings) > 0 {
		payload, _ := json.Marshal(map[string]any{
			"check":            permdrift.CheckName,
			"surfaces_version": permdrift.SurfacesVersion,
			"trigger":          req.Trigger,
			"base_sha":         req.Base,
			"head_sha":         req.Head,
			"narrowings":       fx.narrowings,
		})
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: req.RunID, StageID: &stageID, Timestamp: time.Now().UTC(),
			Category: permissionNarrowingNoticedCategory, ActorKind: &systemKind, Payload: payload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: append permission_narrowing_noticed failed",
				slog.String("run_id", req.RunID.String()), slog.String("error", err.Error()))
		}
	}
	if len(fx.widenings) == 0 && len(fx.unevaluable) == 0 {
		return
	}

	permissionDriftRaiseMu.Lock()
	defer permissionDriftRaiseMu.Unlock()

	widenings, unevaluable := fx.widenings, fx.unevaluable
	if rows, err := s.cfg.ConcernRepo.ListByRun(ctx, req.RunID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: list concerns failed — raising without de-duplication",
			slog.String("run_id", req.RunID.String()), slog.String("error", err.Error()))
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
		widenings, unevaluable = nil, nil
		for _, w := range fx.widenings {
			if !recorded[w.CheckKey] {
				recorded[w.CheckKey] = true
				widenings = append(widenings, w)
			}
		}
		for _, u := range fx.unevaluable {
			if !recorded[u.CheckKey] {
				recorded[u.CheckKey] = true
				unevaluable = append(unevaluable, u)
			}
		}
	}
	if len(widenings) == 0 && len(unevaluable) == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "permission-drift check: every finding is already on record for this stage",
			slog.String("run_id", req.RunID.String()), slog.String("stage_id", stageID.String()))
		return
	}

	payload, _ := json.Marshal(permissionDriftDetectedPayload{
		Check:               permdrift.CheckName,
		SurfacesVersion:     permdrift.SurfacesVersion,
		Trigger:             req.Trigger,
		BaseSHA:             req.Base,
		HeadSHA:             req.Head,
		IntersectRef:        req.IntersectRef,
		IntersectUnresolved: req.IntersectUnresolved,
		CompareTruncated:    fx.truncated,
		Widenings:           nonNilSlice(widenings),
		Unevaluable:         nonNilSlice(unevaluable),
		ExtensionRejected:   fx.extRejected,
		ExtensionError:      fx.extError,
	})
	entry, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: req.RunID, StageID: &stageID, Timestamp: time.Now().UTC(),
		Category: permissionDriftDetectedCategory, ActorKind: &systemKind, Payload: payload,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: append permission_drift_detected failed — no concern raised",
			slog.String("run_id", req.RunID.String()),
			slog.Int("widenings", len(widenings)), slog.Int("unevaluable", len(unevaluable)),
			slog.String("error", err.Error()))
		return
	}

	raised := make([]concern.RaisedConcern, 0, len(widenings)+len(unevaluable))
	keys := make([]string, 0, cap(raised))
	for _, w := range widenings {
		raised = append(raised, concern.RaisedConcern{
			Severity: string(w.surface.Severity), Category: "security",
			Note: permdrift.Note(w.surface, w.Path, w.change), CheckKey: w.CheckKey,
		})
		keys = append(keys, w.CheckKey)
	}
	for _, u := range unevaluable {
		raised = append(raised, concern.RaisedConcern{
			Severity: string(u.surface.Severity), Category: "security",
			Note: permdrift.NoteUnevaluable(u.surface, u.Path, u.Reason), CheckKey: u.CheckKey,
		})
		keys = append(keys, u.CheckKey)
	}
	params := concern.InsertRaisedParams{
		RunID: req.RunID, StageID: stageID, StageKind: concern.StageKindImplement,
		Provenance: concern.ProvenanceServerCheck, OriginReviewSequence: entry.Sequence, Concerns: raised,
	}
	_, ierr := s.cfg.ConcernRepo.InsertRaised(ctx, params)
	if ierr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: insert concerns failed — retrying once",
			slog.String("run_id", req.RunID.String()), slog.String("error", ierr.Error()))
		_, ierr = s.cfg.ConcernRepo.InsertRaised(ctx, params)
	}
	if ierr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: insert concerns failed twice — the permission_drift_detected entry is the record",
			slog.String("run_id", req.RunID.String()),
			slog.Int64("origin_sequence", entry.Sequence),
			slog.String("error", ierr.Error()))
		failPayload, _ := json.Marshal(map[string]any{
			"check":           permdrift.CheckName,
			"origin_sequence": entry.Sequence,
			"check_keys":      keys,
			"error":           ierr.Error(),
		})
		if _, ferr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: req.RunID, StageID: &stageID, Timestamp: time.Now().UTC(),
			Category: permissionDriftRaiseFailedCategory, ActorKind: &systemKind, Payload: failPayload,
		}); ferr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "permission-drift check: append permission_drift_raise_failed failed",
				slog.String("run_id", req.RunID.String()), slog.String("error", ferr.Error()))
		}
		return
	}
	sort.Strings(keys)
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: raised server_check concerns",
		slog.String("run_id", req.RunID.String()),
		slog.String("stage_id", stageID.String()),
		slog.String("trigger", req.Trigger),
		slog.Int("widenings", len(widenings)),
		slog.Int("unevaluable", len(unevaluable)),
		slog.String("check_keys", strings.Join(keys, "; ")))
}

// nonNilSlice renders a nil slice as [] in JSON.
func nonNilSlice[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

// conflictResolutionDriftRequest builds the check for a conflict-resolution
// push: base is the pre-merge branch tip the runner reports, and IntersectRef
// is the base branch named on the stage's newest
// stage_conflict_resolution_triggered entry. When that entry cannot be read or
// names no base ref, the request runs the single comparison and records
// IntersectUnresolved (a superset — the base branch's own widenings raise too).
func (s *Server) conflictResolutionDriftRequest(ctx context.Context, runID, stageID uuid.UUID, pr *pullRequestBody) permissionDriftRequest {
	req := permissionDriftRequest{
		RunID: runID, StageID: stageID, Base: pr.BaseSHA, Head: pr.HeadSHA,
		Trigger: permissionDriftTriggerConflictResolution,
	}
	var trigger conflictResolutionTrigger
	if entry, ok := s.newestStageEntry(ctx, runID, stageID, CategoryStageConflictResolutionTriggered); ok && entry != nil &&
		json.Unmarshal(entry.Payload, &trigger) == nil && trigger.BaseRef != "" {
		req.IntersectRef = trigger.BaseRef
		return req
	}
	req.IntersectUnresolved = true
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: conflict-resolution base branch unresolved — checking the full merge delta",
		slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()))
	return req
}
