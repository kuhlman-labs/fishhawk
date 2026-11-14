package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// vouchHeadAdvance is probeVouchHeadAdvance's answer (#4856). Verified true
// means the vouch handler may stamp prior_head_sha = PriorHeadSHA and
// head_advance_verified = true on the operator_commit_vouched row, which makes
// the row a head edge in walkRecordedBaseMerges. Note names, on every
// non-advance answer, why the head did not advance and the remedy.
//
// LiveHeadRead reports that the probe read the run's live pull-request head
// (resolveVouchPublishHead); LiveHead and LiveHeadErr are then its NEWEST read
// (the post-compare re-read when the probe got that far). The handler's
// audit-check publish bound reuses a successful read instead of reading the
// pull request again.
type vouchHeadAdvance struct {
	PriorHeadSHA string
	Verified     bool
	Note         string

	LiveHeadRead bool
	LiveHead     string
	LiveHeadErr  error
}

// defaultVouchHeadAdvanceProbeTimeout bounds the whole head-advance probe:
// its audit reads, both live-head reads and both compares (#4856). The vouch
// records its row only after the probe returns, so an unbounded probe on a
// hanging forge could outlast the caller's deadline (the MCP client's 30s) and
// the row would never be appended. On expiry the probe answers a non-advance
// note and the vouch records today's three-key row. It matches the 5s
// staleness read bound (defaultMergeCandidateStalenessReadTimeout).
const defaultVouchHeadAdvanceProbeTimeout = 5 * time.Second

// vouchHeadAdvanceProbeTimeout is the bound the vouch handler applies. Tests
// shorten it; production never writes it.
var vouchHeadAdvanceProbeTimeout = defaultVouchHeadAdvanceProbeTimeout

// vouchProbeErrText renders a probe read failure for a note, naming the probe
// deadline when it is what cut the read short.
func vouchProbeErrText(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "the head-advance probe's " + vouchHeadAdvanceProbeTimeout.String() + " deadline expired: " + err.Error()
	}
	return err.Error()
}

// vouchNoAdvancePrefix opens every non-advance note, so an operator reading
// recorded_head_advance_note sees first that the declaration itself landed.
const vouchNoAdvancePrefix = "the vouch is recorded, but the run's recorded fix-up head did NOT advance: "

// vouchNoAdvance builds a non-advance answer.
func vouchNoAdvance(reason string) vouchHeadAdvance {
	return vouchHeadAdvance{Note: vouchNoAdvancePrefix + reason}
}

// probeVouchHeadAdvance decides whether a vouch of sha may ADVANCE the run's
// recorded head (#4856). It is READ-ONLY (no appends, no transitions) and
// never returns an error: every rung that cannot verify answers Verified=false
// with a note naming the reason and the remedy. The caller bounds ctx
// (vouchHeadAdvanceProbeTimeout). The rungs run in this order:
//
//	(a) the forge cannot be probed (no GitHub client, no installation, an
//	    unparseable repo);
//	(b) the recorded head cannot be read, or the fix-up walk and the
//	    acceptance-seeded walk disagree on it (vouchPriorHead);
//	(c) the run has no recorded head (a vouch never invents one);
//	(d) the recorded head is not a full commit id (isDeployCommitSHA): a
//	    reported head_sha is only required to be non-empty, and a ref name
//	    would make a compare operand movable, so it never reaches the forge;
//	(e) sha already IS the recorded head;
//	(f) the run's live pull-request head (resolveVouchPublishHead, the
//	    publish bound's ladder) cannot be resolved, or is not sha: only the
//	    live head may advance the recorded head (the Captain's 2026-11-14
//	    ruling on #4856/#4874);
//	(g) compare(sha...prior) is not empty: sha is not a fast-forward of the
//	    recorded head, and a history rewrite is never admitted;
//	(h) compare(prior...sha) is empty (sha is not ahead) or at the 250-commit
//	    cap (too many commits to prove);
//	(i) the run-lineage closure ledger is incomplete;
//	(j) a commit between prior and sha (other than sha itself) is not run
//	    lineage under the closure ledger (vouchClosureLedger);
//	(k) the live head, re-read after the compares, cannot be resolved or is no
//	    longer sha: it moved during the probe, so the stamp is refused;
//	(l) otherwise Verified=true with PriorHeadSHA=prior.
//
// Notes render the recorded head through formatReportedHeadObjectID, the
// diagnostic form of a runner-reported value (reported_head.go).
//
// The compares use GitHub's three-dot semantics (base...head = commits
// reachable from head and not from the merge base), the same reading
// merge_candidate_vouch.go relies on: an empty compare(sha...prior) means prior
// is an ancestor of sha.
func (s *Server) probeVouchHeadAdvance(ctx context.Context, runRow *run.Run, sha string) vouchHeadAdvance {
	// (a) Cannot probe.
	if s.cfg.GitHub == nil {
		return vouchNoAdvance("cannot probe the forge: no GitHub client is wired, so the fast-forward cannot be verified; the vouch is membership-only")
	}
	if runRow == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return vouchNoAdvance("cannot probe the forge: the run has no installation, so the fast-forward cannot be verified; the vouch is membership-only")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return vouchNoAdvance("cannot probe the forge: the run repo is unparseable (" + err.Error() + "); the vouch is membership-only")
	}

	// (b) The ONE prior-head answer both recorded walks resolve.
	prior, disagree, err := s.vouchPriorHead(ctx, runRow.ID)
	if err != nil {
		return vouchNoAdvance("the recorded head could not be read (" + vouchProbeErrText(ctx, err) + "); re-invoke fishhawk_vouch_commit once the audit chain is readable")
	}
	if disagree != "" {
		return vouchNoAdvance(disagree)
	}
	// (c) No recorded head.
	if prior == "" {
		return vouchNoAdvance("the run has no recorded head, and a vouch never invents one; the vouch is membership-only")
	}
	// (d) The recorded head must be a full commit id before it reaches the
	// forge as a compare operand or a note.
	if !isDeployCommitSHA(prior) {
		return vouchNoAdvance("the run's recorded head is not a full commit id, so it cannot anchor a fast-forward proof; record a fresh push or reset, then re-vouch")
	}
	priorText := formatReportedHeadObjectID(prior)
	// (e) Already the recorded head.
	if strings.EqualFold(prior, sha) {
		return vouchNoAdvance("the vouched commit " + sha + " is already the run's recorded head")
	}

	// (f) Only the run's live pull-request head may advance the recorded head.
	adv := vouchHeadAdvance{LiveHeadRead: true}
	adv.LiveHead, adv.LiveHeadErr = s.resolveVouchPublishHead(ctx, runRow)
	if adv.LiveHeadErr != nil {
		return adv.noAdvance("the run's live pull-request head could not be resolved (" + vouchProbeErrText(ctx, adv.LiveHeadErr) + "), so the vouched commit " + sha + " cannot be verified as it; re-invoke fishhawk_vouch_commit once the pull request is readable")
	}
	if !shaEqualsHead(sha, adv.LiveHead) {
		return adv.noAdvance("the vouched commit " + sha + " is not the run's live pull-request head " + adv.LiveHead + ", and only the live head may advance the recorded head; re-invoke fishhawk_vouch_commit naming the live head")
	}

	// (g) Fast-forward: nothing on prior is missing from sha.
	behind, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, sha, prior)
	if err != nil {
		return adv.noAdvance("comparing the vouched commit " + sha + " against the recorded head " + priorText + " failed (" + vouchProbeErrText(ctx, err) + "); re-invoke fishhawk_vouch_commit once the forge answers")
	}
	if len(behind) != 0 {
		return adv.noAdvance("the vouched commit " + sha + " is not a fast-forward of the recorded head " + priorText + " (" + strconv.Itoa(len(behind)) + " recorded commit(s) are not in its history), and a history rewrite is never admitted; use fishhawk_reset_run_branch or fishhawk_rebase_run_branch instead")
	}

	// (h) The commits the vouch would add.
	ahead, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, prior, sha)
	if err != nil {
		return adv.noAdvance("comparing the recorded head " + priorText + " against the vouched commit " + sha + " failed (" + vouchProbeErrText(ctx, err) + "); re-invoke fishhawk_vouch_commit once the forge answers")
	}
	if len(ahead) == 0 {
		return adv.noAdvance("the vouched commit " + sha + " is not ahead of the recorded head " + priorText)
	}
	if vouchProbeAtCap(len(ahead)) {
		return adv.noAdvance("too many commits to prove: the compare from the recorded head " + priorText + " to " + sha + " reached the " + strconv.Itoa(vouchProbeCompareCap) + "-commit cap and may be truncated")
	}

	// (i) The closure ledger must be complete.
	ledger, complete := s.vouchClosureLedger(ctx, runRow)
	if !complete {
		return adv.noAdvance("the run lineage ledger could not be read completely, so the commits between the recorded head " + priorText + " and " + sha + " cannot be proved run lineage; re-invoke fishhawk_vouch_commit once the audit chain is readable")
	}
	// (j) Every intermediate commit is already run lineage.
	if offender := firstUnattributedCommit(ahead, ledger, sha); offender != "" {
		return adv.noAdvance("the commit " + offender + " between the recorded head " + priorText + " and " + sha + " is not run lineage; vouch " + offender + " first, then re-vouch " + sha + " (for a hand merge of the base, which brings in base commits, use fishhawk_rebase_run_branch instead)")
	}

	// (k) Re-read the live head: a push during the compares refuses the stamp.
	adv.LiveHead, adv.LiveHeadErr = s.resolveVouchPublishHead(ctx, runRow)
	if adv.LiveHeadErr != nil {
		return adv.noAdvance("the run's live pull-request head could not be re-read after the compares (" + vouchProbeErrText(ctx, adv.LiveHeadErr) + "), so the vouched commit " + sha + " cannot be confirmed as it; re-invoke fishhawk_vouch_commit once the pull request is readable")
	}
	if !shaEqualsHead(sha, adv.LiveHead) {
		return adv.noAdvance("the run's live pull-request head moved from " + sha + " to " + adv.LiveHead + " while the vouch was being verified, so the stamp is refused; re-invoke fishhawk_vouch_commit naming the live head")
	}
	// (l) Verified.
	adv.PriorHeadSHA = prior
	adv.Verified = true
	return adv
}

// noAdvance turns a probe answer that has read the live head into a
// non-advance answer, keeping the live-head read for the publish bound.
func (a vouchHeadAdvance) noAdvance(reason string) vouchHeadAdvance {
	out := vouchNoAdvance(reason)
	out.LiveHeadRead, out.LiveHead, out.LiveHeadErr = a.LiveHeadRead, a.LiveHead, a.LiveHeadErr
	return out
}

// vouchPriorHead is the ONE prior-head answer the probe stamps (#4856, approval
// condition C4): the head BOTH recorded walks resolve. The fix-up walk is
// recordedFixupHead (the timestamp-ordered reported-head seed). The second walk
// seeds the way acceptanceHeadSeed does (reported-head rows plus a decomposed
// parent's integration_commit_recorded rows, by chain sequence) and walks the
// SAME rows (fixupHeadWalkCategories), which is the walk the acceptance head
// runs once #4874 delivers vouch rows to it.
//
//   - Any read error is returned: a prior from a partial read is never stamped.
//   - Neither walk has a seed: ("", "", nil), the no-recorded-head rung.
//   - Only the acceptance-seeded walk has a seed (a decomposed parent whose head
//     is a fan-in integration commit): its head is the prior. The fix-up walk
//     has no seed, so it never reads the stamp and cannot reach a different
//     answer. The stamp is then recorded for the acceptance walk (#4874 reads
//     vouch rows); no fix-up expectation exists yet, so
//     resolveFixupExpectedHeadSHA still answers "" and the runner keeps its
//     checkout-only path.
//   - Both have a seed and they differ: disagree names both heads, and the
//     vouch is recorded unverified. A prior one walk would not reach is never
//     stamped.
func (s *Server) vouchPriorHead(ctx context.Context, runID uuid.UUID) (prior, disagree string, err error) {
	if s.cfg.AuditRepo == nil {
		return "", "", nil
	}
	fixupHead, err := s.recordedFixupHead(ctx, runID)
	if err != nil {
		return "", "", err
	}
	seed, seedSHA, err := s.acceptanceHeadSeed(ctx, runID, 0, false)
	if err != nil {
		return "", "", err
	}
	if seed == nil {
		return fixupHead, "", nil
	}
	rows, err := s.fixupHeadWalkRows(ctx, runID)
	if err != nil {
		return "", "", err
	}
	seededHead := walkRecordedBaseMerges(rows, seed.Sequence, seedSHA)
	if fixupHead == "" {
		return seededHead, "", nil
	}
	if fixupHead != seededHead {
		return "", "the fix-up walk resolves the recorded head " + formatReportedHeadObjectID(fixupHead) + " but the acceptance-seeded walk resolves " + formatReportedHeadObjectID(seededHead) + ", so no single prior head can be stamped; the vouch is membership-only (record a fresh push or reset so both walks agree, then re-vouch)", nil
	}
	return fixupHead, "", nil
}

// firstUnattributedCommit returns the first commit in ahead that is neither
// sha itself (case-insensitively) nor a closure-ledger member, or "" when
// every commit is attributed.
func firstUnattributedCommit(ahead []string, ledger map[string]struct{}, sha string) string {
	for _, c := range ahead {
		if strings.EqualFold(c, sha) {
			continue
		}
		if _, ok := ledger[strings.ToLower(c)]; ok {
			continue
		}
		return c
	}
	return ""
}

// vouchClosureLedger is the run-lineage set the probe's closure check reads
// (#4856, approval condition C5). It is deliberately NARROWER than the ADR-035
// reported-head ledger (buildReportedHeadLedger), which unions EVERY vouched
// sha: trust here is transitive only through verified edges. A commit counts
// only when it is
//
//   - a run-reported push head (lineageLedgerCategories on the run's own chain,
//     lineageChildLedgerCategories on its decomposition children);
//   - a server-recorded merge: the merge commit of a CLEAN recorded
//     branch_rebased row (isCleanAdvance), or a fan-in integration merge
//     (slices_integrated, integration_commit_recorded). A branch_reset row
//     moves the branch to an existing commit and adds no member;
//   - the vouched sha of an EARLIER verified vouch (isVerifiedVouchAdvance) on
//     the run or one of its children.
//
// A commit present only through an unverified vouch does NOT count, so vouching
// a foreign commit A unverified and then vouching V over A never verifies V.
// Keys are lowercased. complete is false when any read failed; the probe then
// refuses the advance.
func (s *Server) vouchClosureLedger(ctx context.Context, runRow *run.Run) (map[string]struct{}, bool) {
	ledger := map[string]struct{}{}
	add := func(sha string) {
		if sha != "" {
			ledger[strings.ToLower(sha)] = struct{}{}
		}
	}
	read := func(runID uuid.UUID, cat string) ([]*audit.Entry, bool) {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"vouch-commit: list closure ledger rows failed; the recorded head will not advance",
				slog.String("run_id", runRow.ID.String()),
				slog.String("ledger_run_id", runID.String()),
				slog.String("category", cat),
				slog.String("error", err.Error()))
			return nil, false
		}
		return entries, true
	}
	addField := func(entries []*audit.Entry, field func(json.RawMessage) []string) {
		for _, e := range entries {
			if e == nil {
				continue
			}
			for _, sha := range field(e.Payload) {
				add(sha)
			}
		}
	}

	for _, cat := range lineageLedgerCategories {
		entries, ok := read(runRow.ID, cat)
		if !ok {
			return ledger, false
		}
		addField(entries, headSHAField)
	}
	rebased, ok := read(runRow.ID, CategoryBranchRebased)
	if !ok {
		return ledger, false
	}
	addField(rebased, cleanMergeCommitField)
	integrated, ok := read(runRow.ID, lineageIntegrationLedgerCategory)
	if !ok {
		return ledger, false
	}
	addField(integrated, integrationCommitSHAsField)
	recorded, ok := read(runRow.ID, lineageIntegrationCommitCategory)
	if !ok {
		return ledger, false
	}
	addField(recorded, integrationMergeSHAField)
	vouched, ok := read(runRow.ID, CategoryOperatorCommitVouched)
	if !ok {
		return ledger, false
	}
	addField(vouched, verifiedVouchField)

	if s.cfg.RunRepo == nil {
		return ledger, false
	}
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		DecomposedFrom: &runRow.ID,
		Limit:          lineageChildRunsLimit,
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"vouch-commit: list decomposition children failed; the recorded head will not advance",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return ledger, false
	}
	for _, child := range children {
		if child == nil {
			continue
		}
		for _, cat := range lineageChildLedgerCategories {
			entries, ok := read(child.ID, cat)
			if !ok {
				return ledger, false
			}
			addField(entries, headSHAField)
		}
		childVouched, ok := read(child.ID, CategoryOperatorCommitVouched)
		if !ok {
			return ledger, false
		}
		addField(childVouched, verifiedVouchField)
	}
	return ledger, true
}

// headSHAField decodes a reported-head row's head_sha.
func headSHAField(raw json.RawMessage) []string {
	var p struct {
		HeadSHA string `json:"head_sha"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return []string{p.HeadSHA}
}

// cleanMergeCommitField decodes a branch_rebased row's merge commit, only when
// the row is a CLEAN advance (isCleanAdvance).
func cleanMergeCommitField(raw json.RawMessage) []string {
	var p branchRebasedAdvance
	if json.Unmarshal(raw, &p) != nil || !isCleanAdvance(p) {
		return nil
	}
	return []string{p.MergeCommitSHA}
}

// integrationCommitSHAsField decodes a slices_integrated row's merge commits.
func integrationCommitSHAsField(raw json.RawMessage) []string {
	var p struct {
		IntegrationCommitSHAs []string `json:"integration_commit_shas"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return p.IntegrationCommitSHAs
}

// integrationMergeSHAField decodes an integration_commit_recorded row's merge.
func integrationMergeSHAField(raw json.RawMessage) []string {
	var p struct {
		MergeSHA string `json:"merge_sha"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return []string{p.MergeSHA}
}

// verifiedVouchField decodes a vouch row's vouched_sha, only when the row is a
// server-verified advance (isVerifiedVouchAdvance).
func verifiedVouchField(raw json.RawMessage) []string {
	var p vouchAdvanceRow
	if json.Unmarshal(raw, &p) != nil || !isVerifiedVouchAdvance(p) {
		return nil
	}
	return []string{p.VouchedSHA}
}

// vouchAdvanceLogAttrs are the attributes the vouch handler logs its advance
// outcome with.
func vouchAdvanceLogAttrs(runID uuid.UUID, sha string, adv vouchHeadAdvance) []slog.Attr {
	return []slog.Attr{
		slog.String("run_id", runID.String()),
		slog.String("vouched_sha", sha),
		slog.String("prior_head_sha", adv.PriorHeadSHA),
		slog.Bool("recorded_head_advanced", adv.Verified),
		slog.String("note", adv.Note),
	}
}
