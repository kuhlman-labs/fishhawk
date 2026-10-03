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
// forge compare itself failed, or a commit to compare is missing) is raised
// against: no single surface file can be named, so the concern covers every
// one.
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

// permissionDriftWidening is one widening. The exported (payload) Path, Key,
// Before and After are permdrift.Display renderings (control-free, bounded);
// CheckKey is the escaped, digest-bounded permdrift.CheckKey of the RAW
// values; the unexported fields keep the raw values for the concern note.
type permissionDriftWidening struct {
	Surface  string `json:"surface"`
	Path     string `json:"path"`
	Key      string `json:"key"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Severity string `json:"severity"`
	CheckKey string `json:"check_key"`

	surface permdrift.Surface
	path    string
	change  permdrift.Change
}

// permissionDriftUnevaluable is one unevaluable surface file. head is the
// commit SHA its CheckKey carries ("" when the check could not resolve one —
// then only an OPEN row suppresses it, never a waived or deferred one).
type permissionDriftUnevaluable struct {
	Surface  string `json:"surface"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
	CheckKey string `json:"check_key"`

	surface permdrift.Surface
	path    string
	head    string
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
// RunRepo is a no-op; a request without both commits raises commit_missing
// (runPermissionDriftCheck). See runPermissionDriftCheck for the check itself.
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
// a nil ConcernRepo, AuditRepo or RunRepo — there is nowhere to record a
// finding. A missing commit is NOT a no-op: runPermissionDriftCheck raises
// commit_missing for it once the forge resolves.
func (s *Server) permissionDriftRunnable(_ context.Context, _ permissionDriftRequest) bool {
	return s.cfg.ConcernRepo != nil && s.cfg.AuditRepo != nil && s.cfg.RunRepo != nil
}

// runPermissionDriftCheck is the synchronous body of checkPermissionDrift
// (the consolidated review calls it directly, inside its own background
// goroutine, so the check and that review's compare never run concurrently).
//
//  1. Resolve the forge compare and file fetcher; either unavailable is an
//     INFO-logged skip (the CLI/dev posture, as the consolidated review).
//  2. A missing base or head commit raises ONE check-wide commit_missing
//     concern and stops (no forge call is made).
//  3. ComparePatch(base, head) for the changed files, and resolve the head to
//     the commit SHA every unevaluable key carries (permissionDriftKeyHead);
//     the consolidated trigger's surface files are then read AT that commit.
//  4. Read .fishhawk/permission-surfaces.yaml at BASE only (404 = none). An
//     unreadable file (fetch_failed) or an unparseable one
//     (extension_parse_error) leaves the product list only, records
//     extension_error AND raises one unevaluable concern on the
//     permission-surface-declarations surface; rejected entries are recorded.
//  5. A compare error raises ONE check-wide compare_failed concern. A renamed
//     file's SOURCE (PreviousPath) is evaluated against every surface it
//     matches; a rename whose source the compare does not name raises one
//     rename_source_unknown concern per glob surface. When the compare is
//     truncated or reports a rename, every exact-path surface is probed at
//     both refs anyway; on truncation each glob surface also raises one
//     compare_truncated concern.
//  6. For each (surface, path) fetch base and head (ErrNotFound = side
//     absent; any other error = fetch_failed for that pair) and Detect.
//  7. raisePermissionDrift de-duplicates and raises; narrowings go to one
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
	var fx permissionDriftFindings
	if req.Base == "" || req.Head == "" {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: base or head commit missing — raising unevaluable",
			slog.String("run_id", req.RunID.String()),
			slog.String("stage_id", req.StageID.String()),
			slog.String("trigger", req.Trigger))
		fx.unevaluable = append(fx.unevaluable, newUnevaluable(permissionDriftAllSurfaces,
			req.Base+".."+req.Head, permdrift.ReasonCommitMissing, s.permissionDriftKeyHead(ctx, req, "", false)))
		s.raisePermissionDrift(ctx, req, fx)
		return
	}
	reader := &permissionDriftReader{ctx: ctx, f: fetcher, scope: fscope, repo: frepo, cache: map[[2]string]driftSide{}}

	cmp, cerr := comparer.ComparePatch(ctx, scope, repo, req.Base, req.Head)
	// cmpHead is the compare's head commit only when the forge listed every
	// commit: past a capped listing the last listed commit is not the tip.
	cmpHead := ""
	if cerr == nil && !cmp.CommitsTruncated {
		cmpHead = cmp.HeadSHA
	}
	keyHead := s.permissionDriftKeyHead(ctx, req, cmpHead, cerr == nil)
	// evalHead is the ref every surface file is read at. The consolidated
	// trigger's Head is a branch name, so it is pinned to the compare's head
	// commit when one resolved: the files read are then exactly the commit
	// the unevaluable keys carry, even if the branch moves mid-check.
	evalHead := req.Head
	if req.Trigger == permissionDriftTriggerConsolidated && cmpHead != "" {
		evalHead = cmpHead
	}
	surfaces := s.permissionDriftSurfaces(ctx, req, reader, &fx, keyHead)

	if cerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: forge compare failed — raising unevaluable",
			slog.String("run_id", req.RunID.String()), slog.String("error", cerr.Error()))
		fx.unevaluable = append(fx.unevaluable, newUnevaluable(permissionDriftAllSurfaces,
			req.Base+".."+req.Head, permdrift.ReasonCompareFailed, keyHead))
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
		for _, sf := range permdrift.MatchSurfaces(surfaces, f.Path) {
			add(sf, f.Path)
		}
		if f.Status != "renamed" {
			continue
		}
		renamed = true
		// The rename SOURCE vanished at head: evaluate it against every
		// surface it matches (glob or exact), so moving a governed file out of
		// a surface reads as its removal there.
		if f.PreviousPath != "" {
			for _, sf := range permdrift.MatchSurfaces(surfaces, f.PreviousPath) {
				add(sf, f.PreviousPath)
			}
			continue
		}
		// The source is unknown: an exact-path surface is still probed below,
		// but no glob surface can be — fail each one CLOSED, keyed by the
		// destination path.
		for _, sf := range surfaces {
			if hasGlobPath(sf) {
				fx.unevaluable = append(fx.unevaluable, newUnevaluable(sf, f.Path, permdrift.ReasonRenameSourceUnknown, keyHead))
			}
		}
	}
	if cmp.Truncated || renamed {
		for _, sf := range surfaces {
			for _, p := range sf.Paths {
				if !isGlobPath(p) {
					add(sf, p)
				}
			}
			if cmp.Truncated && hasGlobPath(sf) {
				fx.unevaluable = append(fx.unevaluable, newUnevaluable(sf, "", permdrift.ReasonCompareTruncated, keyHead))
			}
		}
	}

	for _, p := range pairs {
		res := reader.detect(p.s, p.path, req.Base, evalHead)
		if req.IntersectRef != "" && res.Unevaluable == "" {
			res = intersectDrift(p.s, p.path, res, reader.detect(p.s, p.path, req.IntersectRef, evalHead))
		}
		if res.Unevaluable != "" {
			fx.unevaluable = append(fx.unevaluable, newUnevaluable(p.s, p.path, res.Unevaluable, keyHead))
			continue
		}
		for _, c := range res.Widened {
			fx.widenings = append(fx.widenings, permissionDriftWidening{
				Surface: p.s.ID, Path: permdrift.Display(p.path), Key: permdrift.Display(c.Key),
				Before: permdrift.Display(c.Before), After: permdrift.Display(c.After),
				Severity: string(p.s.Severity), CheckKey: permdrift.CheckKey(p.s.ID, p.path, c.Key, c.After),
				surface: p.s, path: p.path, change: c,
			})
		}
		for _, c := range res.Narrowed {
			fx.narrowings = append(fx.narrowings, permissionNarrowing{
				Surface: p.s.ID, Path: permdrift.Display(p.path), Key: permdrift.Display(c.Key),
				Before: permdrift.Display(c.Before), After: permdrift.Display(c.After),
			})
		}
	}
	s.raisePermissionDrift(ctx, req, fx)
}

// permissionDriftSurfaces returns the product surfaces merged with the
// repository's extension, read at req.Base ONLY so a change cannot remove its
// own surface. Absence is no extension. Any other read failure (fetch_failed)
// or a parse error (extension_parse_error) leaves the product list, records
// extension_error, and FAILS CLOSED: one unevaluable concern on the
// permission-surface-declarations surface at RepoSurfacesPath, keyed by
// keyHead — whatever surface the extension declares went unchecked.
func (s *Server) permissionDriftSurfaces(ctx context.Context, req permissionDriftRequest, reader *permissionDriftReader, fx *permissionDriftFindings, keyHead string) []permdrift.Surface {
	product := permdrift.DefaultSurfaces()
	side := reader.read(permdrift.RepoSurfacesPath, req.Base)
	reason := ""
	switch {
	case side.err != nil:
		fx.extError, reason = permdrift.ReasonFetchFailed, permdrift.ReasonFetchFailed
	case !side.f.Exists:
		return product
	default:
		accepted, rejected, err := permdrift.ParseRepoSurfaces(side.f.Content)
		if err != nil {
			fx.extError, reason = permdrift.ReasonParseError, permdrift.ReasonExtensionParseError
			break
		}
		fx.extRejected = rejected
		return permdrift.MergeSurfaces(product, accepted)
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "permission-drift check: repository surface extension unreadable — product surfaces only, raising unevaluable",
		slog.String("run_id", req.RunID.String()),
		slog.String("path", permdrift.RepoSurfacesPath),
		slog.String("reason", reason))
	for _, sf := range product {
		if sf.ID == permdrift.SurfaceIDSurfaceDeclarations {
			fx.unevaluable = append(fx.unevaluable, newUnevaluable(sf, permdrift.RepoSurfacesPath, reason, keyHead))
		}
	}
	return product
}

// permissionDriftKeyHead resolves the commit SHA a check's unevaluable keys
// carry, so a waived or deferred unevaluable row suppresses only a repeat at
// the SAME commit. An empty req.Head resolves "" (commit_missing). A
// push-report trigger's Head is the commit SHA the runner reported. The
// consolidated trigger's Head is the consolidated BRANCH name, which never
// changes across checks, so it is never keyed on: it resolves to the forge
// compare's head commit (cmpHead — the branch tip the compare evaluated, and
// the commit the check then reads its surface files at, so a fix-up pushed
// to the parent after the fan-in is distinguished). A compare that answered
// but named no provable tip (cmpHead "" because its commit listing was capped,
// or it listed no commit) resolves "": the integration ledger may predate the
// tip that compare evaluated, so it is not consulted. Only a compare that
// FAILED (compared false) falls back to the newest integration_commit_recorded
// merge_sha on the parent, else "".
func (s *Server) permissionDriftKeyHead(ctx context.Context, req permissionDriftRequest, cmpHead string, compared bool) string {
	switch {
	case req.Head == "":
		return ""
	case req.Trigger != permissionDriftTriggerConsolidated:
		return req.Head
	case cmpHead != "":
		return cmpHead
	case compared:
		return ""
	}
	if sha, ok := s.resolveConsolidatedFanInHeadSHA(ctx, req.RunID, 0, false); ok {
		return sha
	}
	return ""
}

// isGlobPath reports whether a surface path carries doublestar metacharacters.
func isGlobPath(p string) bool { return strings.ContainsAny(p, "*?[{\\") }

// hasGlobPath reports whether any of sf's paths is a glob.
func hasGlobPath(sf permdrift.Surface) bool {
	for _, p := range sf.Paths {
		if isGlobPath(p) {
			return true
		}
	}
	return false
}

// newUnevaluable builds one unevaluable finding keyed by head (the resolved
// commit SHA, "" when unresolved). The payload path is a permdrift.Display
// rendering; the key and the note use the raw path.
func newUnevaluable(sf permdrift.Surface, path, reason, head string) permissionDriftUnevaluable {
	return permissionDriftUnevaluable{
		Surface: sf.ID, Path: permdrift.Display(path), Reason: reason,
		CheckKey: permdrift.UnevaluableKey(sf.ID, path, head), surface: sf, path: path, head: head,
	}
}

// intersectDrift keeps the changes of first (previous head → pushed head)
// that second (base-branch tip → pushed head) also reports IN THE SAME
// DIRECTION, keyed by check key: a widening survives only when second also
// widens that key to that value, a narrowing only when second also narrows
// it. (One union set would let first's widening to "read" survive on second's
// NARROWING of the same key to "read" — a different change sharing the check
// key.) An unevaluable second comparison fails the pair closed.
func intersectDrift(sf permdrift.Surface, path string, first, second permdrift.Result) permdrift.Result {
	if second.Unevaluable != "" {
		return second
	}
	set := func(in []permdrift.Change) map[string]bool {
		out := map[string]bool{}
		for _, c := range in {
			out[permdrift.CheckKey(sf.ID, path, c.Key, c.After)] = true
		}
		return out
	}
	keep := func(in []permdrift.Change, keys map[string]bool) []permdrift.Change {
		var out []permdrift.Change
		for _, c := range in {
			if keys[permdrift.CheckKey(sf.ID, path, c.Key, c.After)] {
				out = append(out, c)
			}
		}
		return out
	}
	return permdrift.Result{
		Widened:  keep(first.Widened, set(second.Widened)),
		Narrowed: keep(first.Narrowed, set(second.Narrowed)),
	}
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
// raising). An unevaluable whose head could not be resolved to a commit SHA
// (head "") is suppressed by an OPEN row only — a waived or deferred row at
// an unknown commit never silences a later one. The permission_drift_detected entry is appended FIRST and stamps
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
		recorded, open := map[string]bool{}, map[string]bool{}
		for _, row := range rows {
			if row == nil || row.StageID != stageID || !row.IsServerCheck() || row.CheckKey == "" {
				continue
			}
			if row.State.IsOpen() {
				open[row.CheckKey] = true
			}
			if row.State.IsOpen() || row.State == concern.StateWaived || row.State == concern.StateDeferred {
				recorded[row.CheckKey] = true
			}
		}
		widenings, unevaluable = nil, nil
		for _, w := range fx.widenings {
			if !recorded[w.CheckKey] {
				recorded[w.CheckKey], open[w.CheckKey] = true, true
				widenings = append(widenings, w)
			}
		}
		for _, u := range fx.unevaluable {
			suppress := recorded
			if u.head == "" {
				suppress = open
			}
			if !suppress[u.CheckKey] {
				recorded[u.CheckKey], open[u.CheckKey] = true, true
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
		ExtensionRejected:   displayRejections(fx.extRejected),
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
			Note: permdrift.Note(w.surface, w.path, w.change), CheckKey: w.CheckKey,
		})
		keys = append(keys, w.CheckKey)
	}
	for _, u := range unevaluable {
		raised = append(raised, concern.RaisedConcern{
			Severity: string(u.surface.Severity), Category: "security",
			Note: permdrift.NoteUnevaluable(u.surface, u.path, u.Reason), CheckKey: u.CheckKey,
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

// displayRejections renders each rejected extension entry's id (as written in
// the file) through permdrift.Display for the audit payload.
func displayRejections(in []permdrift.Rejection) []permdrift.Rejection {
	if in == nil {
		return nil
	}
	out := make([]permdrift.Rejection, len(in))
	for i, r := range in {
		r.ID = permdrift.Display(r.ID)
		out[i] = r
	}
	return out
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
