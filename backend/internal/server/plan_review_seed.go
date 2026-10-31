package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"

	"github.com/google/uuid"
)

// Plan-review seed (#4436): the plan review exports its read-only tree at the
// commit the plan agent actually READ, never at the shared checkout's HEAD.
//
// Since #3973 the plan agent runs in a lineage worktree seeded from the freshly
// fetched declared base, while run.WorkingDir (the operator's shared checkout)
// can sit behind it after any unpulled merge. Exporting that checkout's HEAD
// made reviewers read an older tree and reject current plans for citing
// "nonexistent" symbols. The runner therefore ships the plan seed on the plan
// upload in planSeedHeader; handleShipPlan records it as plan_generated.seed_sha
// (recordPlanSeedHeader), and runPlanReviews resolves it from the stage's
// newest plan_generated row (resolvePlanReviewSeed) and exports there.
//
// It fails CLOSED: with no recorded, a malformed, or an unreadable seed the
// round exports NO tree (the empty ref reaches exportReviewTree's no_ref
// degrade) and records why on plan_review_started.seed_unresolved. It never
// substitutes the checkout HEAD.

// planSeedHeader carries the runner-asserted plan seed on POST
// /v0/runs/{run_id}/plan. CROSS-MODULE twin of runner/internal/upload
// PlanSeedHeader; both are pinned to testdata/wire/plan_seed_header.json. Like
// reachabilityHeader it rides beside the signed body and is NOT covered by
// X-Fishhawk-Signature.
const planSeedHeader = "X-Fishhawk-Plan-Seed-SHA"

// planSeedSHAPattern accepts a full lowercase SHA-1 (40) or SHA-256 (64) object
// name. Hex-only also guarantees the value can never be read as a git option
// when it reaches `git rev-parse` / `git archive` / `git fetch`.
var planSeedSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// validPlanSeedSHA reports whether s is a well-formed plan seed.
func validPlanSeedSHA(s string) bool {
	return planSeedSHAPattern.MatchString(s)
}

// recordPlanSeedHeader stamps the plan seed header's value onto the
// plan_generated payload: an empty header records nothing (a pre-#4436 runner),
// a valid one records seed_sha, and anything else records seed_sha_malformed
// WITHOUT storing the raw value. A malformed header never fails the upload.
func recordPlanSeedHeader(payload map[string]any, raw string) {
	if raw == "" {
		return
	}
	if validPlanSeedSHA(raw) {
		payload["seed_sha"] = raw
		return
	}
	payload["seed_sha_malformed"] = true
}

// The closed set of reasons a plan round resolved no seed, recorded on
// plan_review_started.seed_unresolved.
const (
	// planSeedUnresolvedLookupFailed: the plan_generated read failed.
	planSeedUnresolvedLookupFailed = "seed_lookup_failed"
	// planSeedUnresolvedNotRecorded: no plan_generated row on the stage, or
	// one carrying no seed_sha (a runner predating #4436).
	planSeedUnresolvedNotRecorded = "seed_not_recorded"
	// planSeedUnresolvedMalformed: the row carries seed_sha_malformed, or a
	// seed_sha that fails validPlanSeedSHA on read.
	planSeedUnresolvedMalformed = "seed_malformed"
)

// planReviewSeed is a plan round's resolved seed: SHA when one resolved, else
// Unresolved names why (one of the planSeedUnresolved* reasons).
type planReviewSeed struct {
	SHA        string
	Unresolved string
}

// resolvePlanReviewSeed reads the plan seed for a plan-review round from the
// stage's NEWEST plan_generated row. Newest-row is the reviewed plan on both
// production paths: handleShipPlan appends the row immediately before calling
// runPlanReviews, and the boot re-dispatch refuses a plan round once a newer
// plan_generated exists (redispatchSlugSupersededByNewPlan). It deliberately
// does NOT match on sha256(planBody): artifact content is stored as JSONB, so
// the re-dispatch path's re-read body is not byte-equal to the upload.
//
// Every unresolved outcome is WARN-logged and returned with its reason; the
// caller then exports no tree.
func (s *Server) resolvePlanReviewSeed(ctx context.Context, runID, stageID uuid.UUID) planReviewSeed {
	unresolved := func(reason string) planReviewSeed {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: no plan seed resolved — review runs diff-only",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("reason", reason),
		)
		return planReviewSeed{Unresolved: reason}
	}
	entry, ok := s.newestStageEntry(ctx, runID, stageID, "plan_generated")
	if !ok {
		return unresolved(planSeedUnresolvedLookupFailed)
	}
	if entry == nil {
		return unresolved(planSeedUnresolvedNotRecorded)
	}
	var p struct {
		SeedSHA          string `json:"seed_sha"`
		SeedSHAMalformed bool   `json:"seed_sha_malformed"`
	}
	if err := json.Unmarshal(entry.Payload, &p); err != nil {
		return unresolved(planSeedUnresolvedMalformed)
	}
	if p.SeedSHAMalformed || (p.SeedSHA != "" && !validPlanSeedSHA(p.SeedSHA)) {
		return unresolved(planSeedUnresolvedMalformed)
	}
	if p.SeedSHA == "" {
		return unresolved(planSeedUnresolvedNotRecorded)
	}
	return planReviewSeed{SHA: p.SeedSHA}
}

// planReviewSeedCtxKey is unexported so only this package sets the marker.
type planReviewSeedCtxKey struct{}

// withPlanReviewSeed returns ctx carrying the plan round's resolved seed, so
// emitReviewStarted can record it on plan_review_started.
func withPlanReviewSeed(ctx context.Context, seed planReviewSeed) context.Context {
	return context.WithValue(ctx, planReviewSeedCtxKey{}, seed)
}

// planReviewSeedFrom returns the plan seed ctx carries; ok=false when none.
func planReviewSeedFrom(ctx context.Context) (planReviewSeed, bool) {
	seed, ok := ctx.Value(planReviewSeedCtxKey{}).(planReviewSeed)
	return seed, ok
}
