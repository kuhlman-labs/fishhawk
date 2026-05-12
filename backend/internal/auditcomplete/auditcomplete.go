// Package auditcomplete derives the state of the
// `fishhawk_audit_complete` blocking check (#229). The check fails
// when the audit story for a run isn't intact: a missing plan
// artifact, a missing trace bundle, a missing pull_request
// artifact, or a tampered/missing audit-chain link. The reviewer
// can't approve until everything Fishhawk claims to record actually
// landed.
//
// Scope:
//   - Read-only. Compute pulls from the run, artifact, and audit
//     repos, runs the same hash algorithm the verifier uses, and
//     returns a state without writing anything.
//   - Compute-on-read (per #229's recommendation). The review-stage
//     read endpoint and the approval-handler enforcement both call
//     Compute directly. No persistence layer; verification cost
//     is bounded (single-digit ms on a normal run's chain) and the
//     freshest state is always what the reviewer sees.
//
// Failure categorization:
//   - State=fail with a non-empty `missing` list → audit story is
//     broken, gate refuses approval, reviewer sees what to fix.
//   - State=pending → some non-review stages haven't terminated
//     yet, so we can't say "done"; the reviewer waits.
//   - State=pass → every load-bearing artifact + audit entry is
//     present and the chain verifies.
package auditcomplete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// MissingKind names a category of audit-incompleteness. Stable,
// machine-readable; the SPA can localize / branch on it.
type MissingKind string

// MissingKind values.
const (
	MissingPlan          MissingKind = "plan_missing"        // plan stage didn't produce a standard_v1 artifact
	MissingTrace         MissingKind = "trace_missing"       // a non-review stage hasn't shipped both bundle variants
	MissingPullRequest   MissingKind = "pr_missing"          // implement stage didn't produce a pull_request artifact
	MissingChain         MissingKind = "chain_invalid"       // audit chain prev_hash → entry_hash links don't verify
	MissingChainBroken   MissingKind = "chain_unrecoverable" // chain read or hash recomputation errored
	MissingForeignCommit MissingKind = "foreign_commit"      // PR HEAD on GitHub isn't a Fishhawk-recorded head_sha (#282)
	MissingHeadFetchFail MissingKind = "head_fetch_failed"   // couldn't read PR HEAD from GitHub; result is pending, not fail (#282)
)

// MissingItem points at a specific gap. Detail is human-readable;
// callers that want to render structured info (per-stage breakdown,
// etc.) should branch on Kind.
type MissingItem struct {
	Kind   MissingKind `json:"kind"`
	Detail string      `json:"detail"`
}

// Deps groups the repository handles Compute needs. Production
// wires the postgres-backed repos; tests wire fakes. Defining the
// dependencies here lets Compute stay a pure function over data.
type Deps struct {
	Runs      run.Repository
	Artifacts artifact.Repository
	Audit     audit.Repository
	// PRHead resolves a PR number to its live HEAD sha on GitHub
	// (#282). Used by rule 5 (foreign-commit detection) to compare
	// what's on GitHub right now against the Fishhawk-recorded
	// head_shas across the run + parent chain. Nil disables the
	// rule — Compute treats that as "no live data, skip drift
	// check" rather than failing it. Production wires a closure
	// around `githubclient.Client.GetPullRequest`.
	PRHead PRHeadFetcher
}

// PRHeadFetcher is the signature for the live-HEAD callback. Errors
// flow into a `head_fetch_failed` MissingItem rather than failing
// Compute outright (GitHub flap shouldn't break the audit signal).
type PRHeadFetcher func(ctx context.Context, installationID int64, repo githubclient.RepoRef, prNumber int) (headSHA string, err error)

// Compute returns the audit-completeness state for the run plus a
// list of structured missing items. Both are returned together so
// the SPA can render "fail because: plan_missing, trace_missing
// (implement stage)" rather than just "fail."
//
// Errors are returned for transient I/O failures the caller should
// retry (DB unreachable, etc.). Logical gaps (missing artifact,
// failed chain) are encoded in the (state, missing) pair, never as
// errors.
func Compute(ctx context.Context, runID uuid.UUID, deps Deps) (stagecheck.State, []MissingItem, error) {
	if deps.Runs == nil || deps.Artifacts == nil || deps.Audit == nil {
		return stagecheck.StatePending, nil, errors.New("auditcomplete: incomplete deps")
	}

	stages, err := deps.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: list stages: %w", err)
	}

	// Sort the stages we care about by type. Review stages don't
	// produce traces or artifacts of their own — they consume the
	// implement stage's pull_request — so they're excluded from
	// the "every non-review stage must have shipped a trace" rule.
	var (
		planStage      *run.Stage
		implementStage *run.Stage
		nonReview      []*run.Stage
	)
	for _, s := range stages {
		if s.Type != run.StageTypeReview {
			nonReview = append(nonReview, s)
		}
		switch s.Type {
		case run.StageTypePlan:
			planStage = s
		case run.StageTypeImplement:
			implementStage = s
		}
	}

	// Mid-flight: if any non-review stage hasn't terminated, the
	// run isn't "done" — so neither is the audit. Pending rather
	// than fail; the reviewer waits.
	for _, s := range nonReview {
		if !s.State.IsTerminal() {
			return stagecheck.StatePending, nil, nil
		}
	}

	var missing []MissingItem

	// Rule 1: every plan stage in the run must have produced a
	// standard_v1 plan artifact. Workflows without a plan stage
	// (e.g. routine_change) skip this rule cleanly.
	if planStage != nil {
		ok, err := hasStandardV1Plan(ctx, deps.Artifacts, planStage.ID)
		if err != nil {
			return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: plan artifacts: %w", err)
		}
		if !ok {
			missing = append(missing, MissingItem{
				Kind:   MissingPlan,
				Detail: fmt.Sprintf("plan stage %s has no kind=plan, schema_version=standard_v1 artifact", shortID(planStage.ID)),
			})
		}
	}

	// Rule 2: every non-review stage that completed must have a
	// trace_uploaded audit entry. The runner ships both raw and
	// redacted variants per stage (E2.4); both must land for the
	// chain to be considered complete.
	traceMisses, err := missingTraces(ctx, deps.Audit, runID, nonReview)
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: trace audit: %w", err)
	}
	missing = append(missing, traceMisses...)

	// Rule 3: implement stages must produce a pull_request
	// artifact. Workflows without an implement stage skip cleanly.
	if implementStage != nil {
		ok, err := hasPullRequest(ctx, deps.Artifacts, implementStage.ID)
		if err != nil {
			return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: pr artifacts: %w", err)
		}
		if !ok {
			missing = append(missing, MissingItem{
				Kind:   MissingPullRequest,
				Detail: fmt.Sprintf("implement stage %s has no kind=pull_request artifact", shortID(implementStage.ID)),
			})
		}
	}

	// Rule 4: the audit chain must verify. Recompute every
	// entry's hash from its inputs and check the link to the prior
	// entry. A single mismatch invalidates the run — the
	// integrity story doesn't tolerate "mostly correct."
	if chainErr := verifyChain(ctx, deps.Audit, runID); chainErr != nil {
		var kind MissingKind
		if errors.Is(chainErr, errChainInvalid) {
			kind = MissingChain
		} else {
			kind = MissingChainBroken
		}
		missing = append(missing, MissingItem{
			Kind:   kind,
			Detail: chainErr.Error(),
		})
	}

	// Rule 5: the PR's live HEAD on GitHub must match a Fishhawk-
	// recorded head_sha across the run + its parent_run_id chain
	// (#282 / ADR-017 spirit — Fishhawk's audit story is "every
	// commit on this branch was produced by Fishhawk").
	//
	// Skipped when:
	//   - PRHead callback isn't wired (dev / test posture without
	//     a GitHub client).
	//   - The implement stage hasn't produced a pull_request
	//     artifact yet (caught by Rule 3 above; rule 5 has nothing
	//     to compare against).
	//   - The PR couldn't be read from GitHub (head_fetch_failed).
	//     Surfaces as a "pending"-flavored missing item; doesn't
	//     flip the overall state to fail. A flapping GitHub signal
	//     mustn't break the audit gate.
	rule5(ctx, deps, runID, &missing)

	// Decide overall state. A `head_fetch_failed` item is
	// pending-flavored — if it's the ONLY thing in the missing
	// list, the audit isn't broken, we just couldn't verify the
	// drift rule against a live source. State stays pending so
	// branch protection re-evaluates on a successful follow-up
	// publish rather than tripping a misleading red.
	switch {
	case len(missing) == 0:
		return stagecheck.StatePass, nil, nil
	case onlyHeadFetchFailures(missing):
		return stagecheck.StatePending, missing, nil
	default:
		return stagecheck.StateFail, missing, nil
	}
}

// rule5 implements the foreign-commit detection. Appends missing
// items to `out` and never returns an error — fetch failures land
// as `head_fetch_failed` so the state-decision logic can treat them
// as pending rather than fail. Pure data-flow helper; the I/O is
// guarded by the PRHead callback being non-nil.
func rule5(ctx context.Context, deps Deps, runID uuid.UUID, out *[]MissingItem) {
	if deps.PRHead == nil {
		return
	}
	gather, ok, err := gatherForeignCommitInputs(ctx, deps, runID)
	if err != nil {
		// Walking the parent chain or reading artifacts failed —
		// log-equivalent as a fetch-failure missing item. The
		// caller treats it as pending for state decision.
		*out = append(*out, MissingItem{
			Kind:   MissingHeadFetchFail,
			Detail: fmt.Sprintf("could not gather Fishhawk-recorded head_shas: %v", err),
		})
		return
	}
	if !ok {
		// Either no implement stage yet, no installation, no PR
		// artifact, or no parseable PR number — Rule 3 covers the
		// missing artifact case; for the others rule 5 has nothing
		// to compare against.
		return
	}

	liveSHA, err := deps.PRHead(ctx, gather.installationID, gather.repo, gather.prNumber)
	if err != nil {
		*out = append(*out, MissingItem{
			Kind:   MissingHeadFetchFail,
			Detail: fmt.Sprintf("could not read PR HEAD from GitHub: %v", err),
		})
		return
	}
	if _, hit := gather.knownSHAs[liveSHA]; hit {
		return
	}

	known := make([]string, 0, len(gather.knownSHAs))
	for sha := range gather.knownSHAs {
		known = append(known, shortSHA(sha))
	}
	*out = append(*out, MissingItem{
		Kind: MissingForeignCommit,
		Detail: fmt.Sprintf(
			"PR HEAD %s is not a Fishhawk-recorded commit (known: %s)",
			shortSHA(liveSHA),
			strings.Join(known, ", ")),
	})
}

// foreignCommitInputs bundles the values rule5 needs to make the
// PRHead call + compose the missing-item detail.
type foreignCommitInputs struct {
	installationID int64
	repo           githubclient.RepoRef
	prNumber       int
	knownSHAs      map[string]struct{}
}

// gatherForeignCommitInputs walks runID upward via parent_run_id
// (#216) and collects every implement-stage `pull_request`
// artifact's head_sha + the PR's number. Returns (inputs, true, nil)
// when there's enough to call PRHead; (_, false, nil) when there's
// no implement stage / no installation / no PR yet; error only on
// transient I/O.
func gatherForeignCommitInputs(ctx context.Context, deps Deps, runID uuid.UUID) (foreignCommitInputs, bool, error) {
	known := make(map[string]struct{})
	var (
		installationID int64
		repoRef        githubclient.RepoRef
		prNumber       int
	)

	cursor := runID
	visited := map[uuid.UUID]struct{}{}
	for {
		if _, seen := visited[cursor]; seen {
			break // defensive against a corrupted parent loop
		}
		visited[cursor] = struct{}{}

		r, err := deps.Runs.GetRun(ctx, cursor)
		if err != nil {
			return foreignCommitInputs{}, false, fmt.Errorf("get run %s: %w", shortID(cursor), err)
		}

		// The original (head) run anchors installation + repo;
		// every ancestor shares them. Capture once.
		if installationID == 0 && r.InstallationID != nil {
			installationID = *r.InstallationID
			parsed, perr := parseRepo(r.Repo)
			if perr == nil {
				repoRef = parsed
			}
		}

		// Pull head_sha + pr_number from this run's implement stage.
		stages, err := deps.Runs.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return foreignCommitInputs{}, false, fmt.Errorf("list stages for %s: %w", shortID(r.ID), err)
		}
		var impl *run.Stage
		for _, s := range stages {
			if s.Type == run.StageTypeImplement {
				impl = s
				break
			}
		}
		if impl != nil {
			arts, err := deps.Artifacts.ListForStage(ctx, impl.ID)
			if err != nil {
				return foreignCommitInputs{}, false, fmt.Errorf("list artifacts for %s: %w", shortID(impl.ID), err)
			}
			for _, a := range arts {
				if a.Kind != artifact.KindPullRequest {
					continue
				}
				sha, num := decodePRArtifact(a.Content)
				if sha != "" {
					known[sha] = struct{}{}
				}
				// The newest run on the chain (we visit it first)
				// is authoritative for the PR number.
				if prNumber == 0 && num > 0 {
					prNumber = num
				}
			}
		}

		if r.ParentRunID == nil {
			break
		}
		cursor = *r.ParentRunID
	}

	if installationID == 0 || repoRef.Owner == "" || prNumber == 0 || len(known) == 0 {
		return foreignCommitInputs{}, false, nil
	}
	return foreignCommitInputs{
		installationID: installationID,
		repo:           repoRef,
		prNumber:       prNumber,
		knownSHAs:      known,
	}, true, nil
}

// decodePRArtifact pulls (head_sha, pr_number) out of a
// pull_request artifact's content. Mirrors the publisher's
// `decodeHeadSHA` but returns the number too. Empty / unparseable
// values come back as zero — the caller skips silently rather than
// surfacing the parse failure (the artifact validator is the right
// place for that complaint).
func decodePRArtifact(content []byte) (string, int) {
	if len(content) == 0 {
		return "", 0
	}
	var body struct {
		HeadSHA  string `json:"head_sha"`
		PRNumber int    `json:"pr_number"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return "", 0
	}
	return body.HeadSHA, body.PRNumber
}

// parseRepo splits "owner/name" into a RepoRef. Mirrors the
// helpers in other packages; duplicated here to keep auditcomplete
// import-free of higher layers.
func parseRepo(s string) (githubclient.RepoRef, error) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return githubclient.RepoRef{}, fmt.Errorf("auditcomplete: repo %q must be owner/name", s)
	}
	return githubclient.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
}

// shortSHA renders the leading 7 of a SHA for human-readable
// missing-item details. Leaves shorter SHAs untouched.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// onlyHeadFetchFailures returns true when every entry in `missing`
// is a `head_fetch_failed` row. Used to demote the overall state
// from fail to pending — a fetch failure isn't an audit gap, just
// "we don't know."
func onlyHeadFetchFailures(missing []MissingItem) bool {
	for _, m := range missing {
		if m.Kind != MissingHeadFetchFail {
			return false
		}
	}
	return len(missing) > 0
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func hasStandardV1Plan(ctx context.Context, repo artifact.Repository, stageID uuid.UUID) (bool, error) {
	arts, err := repo.ListForStage(ctx, stageID)
	if err != nil {
		return false, err
	}
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		return true, nil
	}
	return false, nil
}

func hasPullRequest(ctx context.Context, repo artifact.Repository, stageID uuid.UUID) (bool, error) {
	arts, err := repo.ListForStage(ctx, stageID)
	if err != nil {
		return false, err
	}
	for _, a := range arts {
		if a.Kind == artifact.KindPullRequest {
			return true, nil
		}
	}
	return false, nil
}

// missingTraces returns one MissingItem per non-review stage that
// didn't ship both raw + redacted bundles. The runner posts both
// variants per stage (E2.4); a missing variant still implies the
// audit chain is incomplete.
func missingTraces(ctx context.Context, repo audit.Repository, runID uuid.UUID, nonReview []*run.Stage) ([]MissingItem, error) {
	if len(nonReview) == 0 {
		return nil, nil
	}
	entries, err := repo.ListForRunByCategory(ctx, runID, "trace_uploaded")
	if err != nil {
		return nil, err
	}

	// Build (stage_id → set-of-variants) from the audit log.
	type variantSet struct{ raw, redacted bool }
	got := map[uuid.UUID]*variantSet{}
	for _, e := range entries {
		if e.StageID == nil {
			continue
		}
		v, ok := got[*e.StageID]
		if !ok {
			v = &variantSet{}
			got[*e.StageID] = v
		}
		// Variant comes from the audit payload; fall through if
		// the entry is shaped wrong (older format, etc.) — the
		// chain-verify rule will catch a tampered payload.
		switch traceVariantOf(e.Payload) {
		case "raw":
			v.raw = true
		case "redacted":
			v.redacted = true
		}
	}

	var out []MissingItem
	for _, s := range nonReview {
		// Only stages that actually executed need traces. A
		// stage that was cancelled before dispatch has nothing
		// to ship.
		if s.State == run.StageStatePending || s.State == run.StageStateCancelled {
			continue
		}
		v, ok := got[s.ID]
		if !ok {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) has no trace_uploaded audit entry", shortID(s.ID), s.Type),
			})
			continue
		}
		if !v.raw {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) is missing the raw trace bundle", shortID(s.ID), s.Type),
			})
		}
		if !v.redacted {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) is missing the redacted trace bundle", shortID(s.ID), s.Type),
			})
		}
	}
	return out, nil
}

// traceVariantOf reads the `variant` field out of a trace_uploaded
// audit entry's payload. Returns "" on parse failure or absent
// field — the caller treats that as "neither raw nor redacted"
// which counts as a missing variant.
func traceVariantOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Variant string `json:"variant"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Variant
}

// errChainInvalid signals that an entry's recomputed hash didn't
// match what's stored — the chain has been tampered with. Distinct
// from I/O errors so Compute can categorize the missing item.
var errChainInvalid = errors.New("audit chain mismatch")

func verifyChain(ctx context.Context, repo audit.Repository, runID uuid.UUID) error {
	entries, err := repo.ListForRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("list audit entries: %w", err)
	}
	var prev *string
	for _, e := range entries {
		// Recompute the hash from the entry's inputs. The
		// canonical algorithm lives in audit.ComputeEntryHash;
		// the verifier package mirrors it but is intended for
		// external callers. We use the backend's copy here so
		// we don't reach across the module boundary.
		runIDPtr := e.RunID
		got, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        runIDPtr,
			StageID:      e.StageID,
			Timestamp:    e.Timestamp,
			Category:     e.Category,
			ActorKind:    e.ActorKind,
			ActorSubject: e.ActorSubject,
			Payload:      e.Payload,
			PrevHash:     prev,
		})
		if err != nil {
			return fmt.Errorf("hash entry %s: %w", e.ID, err)
		}
		if got != e.EntryHash {
			return fmt.Errorf("%w: entry %s recomputed %q != stored %q",
				errChainInvalid, e.ID, got, e.EntryHash)
		}
		// PrevHash for the next entry is THIS entry's stored
		// hash, not the one we just recomputed — the link
		// integrity is the (prev, current) pair as stored, not
		// our recomputation.
		hash := e.EntryHash
		prev = &hash
	}
	return nil
}
