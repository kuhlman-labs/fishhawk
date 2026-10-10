package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Post-merge head read outcomes (#4199). readPostMergeHead classifies what the
// bounded post-merge PR re-read observed; the rebase handler resolves the new
// head from that classification by PROVENANCE, and ships the outcome on the
// 200 (post_merge_head_read) and in the branch_rebased audit payload.
const (
	// postMergeHeadReadConverged: a read returned the merge commit sha.
	postMergeHeadReadConverged = "converged"
	// postMergeHeadReadReadAfterWriteLag: every successful read returned the
	// PRE-merge head — the forge's PR head lagging the branch-ref update.
	postMergeHeadReadReadAfterWriteLag = "read_after_write_lag"
	// postMergeHeadReadConcurrentPush: with a decoded merge sha, a read
	// returned a head that is neither the pre-merge head nor the merge commit.
	postMergeHeadReadConcurrentPush = "concurrent_push"
	// postMergeHeadReadUnreadable: no read succeeded.
	postMergeHeadReadUnreadable = "unreadable"
	// postMergeHeadReadReadBack: the merge sha did NOT decode, and a read
	// returned a head that differs from the pre-merge head.
	postMergeHeadReadReadBack = "read_back"
)

// defaultPostMergeHeadReadBackoff is the production re-read schedule: an
// initial read plus one re-read per entry, ~10s cumulative. Every recorded
// #4199 incident showed the merge commit on the PR within ~5s.
var defaultPostMergeHeadReadBackoff = []time.Duration{
	500 * time.Millisecond, time.Second, 2 * time.Second, 3 * time.Second, 3500 * time.Millisecond,
}

// rebasePostMergeTailBudget bounds the rebase handler's post-merge tail
// (re-park, branch_rebased, attribution, republish, notify, merge-candidate
// pass), which runs detached from request cancellation once a merge was
// performed, so a caller that times out during the re-read cannot strand the
// landed merge without its audit row and attribution.
const rebasePostMergeTailBudget = 60 * time.Second

// errPostMergeEmptyHead is recorded as the read error when a post-merge PR
// read succeeded but carried an empty head sha.
var errPostMergeEmptyHead = errors.New("the post-merge PR re-read returned an empty head")

// postMergeHeadRead is the classified result of the bounded post-merge read.
// Observed is the last non-empty head a read returned (empty when none did);
// Attempts counts the reads made; LastErr is the last read error, if any.
type postMergeHeadRead struct {
	Outcome  string
	Observed string
	Attempts int
	LastErr  error
}

// readPostMergeHead re-reads the PR head after a performed base merge, up to
// 1+len(backoff) times, waiting backoff[i-1] (ctx-aware) before read i, and
// classifies what it saw instead of trusting a single read (#4199):
//
//   - a read equal to mergeSHA (decoded) → converged, immediately;
//   - a read equal to priorHead → lag; keep re-reading;
//   - any other head with a decoded mergeSHA → concurrent_push, immediately;
//   - any other head with mergeSHA == "" → read_back, immediately;
//   - an error or empty head → recorded in LastErr; keep re-reading.
//
// After the budget the outcome is read_after_write_lag when any successful
// read returned priorHead, and unreadable otherwise.
//
// PROVENANCE RULE (applied by resolvePostMergeHead): a merge sha the merges
// endpoint decoded is the authority for the new head; the re-read only
// CLASSIFIES. Only on the benign undecodable ("", nil) shape does the re-read
// supply the head, and then only when it differs from priorHead — a read stuck
// at the pre-merge head is never accepted as the new head.
//
// The lag signature is deliberately EXACTLY "read == priorHead", not "an
// ancestor of the merge commit": the lease re-check read priorHead immediately
// before the merge, every recorded incident returned exactly priorHead, and an
// ancestry probe would classify a foreign force-push that REWINDS the branch
// as benign lag and suppress the concurrent-push signal. Any other head warns.
func readPostMergeHead(ctx context.Context, read func(context.Context) (string, error),
	priorHead, mergeSHA string, backoff []time.Duration) postMergeHeadRead {
	var out postMergeHeadRead
	lagSeen := false
	for i := 0; i <= len(backoff); i++ {
		if i > 0 && !waitPostMergeBackoff(ctx, backoff[i-1]) {
			break
		}
		out.Attempts++
		head, err := read(ctx)
		if err == nil && head == "" {
			err = errPostMergeEmptyHead
		}
		if err != nil {
			out.LastErr = err
			continue
		}
		if mergeSHA != "" && head == mergeSHA {
			out.Outcome, out.Observed = postMergeHeadReadConverged, head
			return out
		}
		if head == priorHead {
			out.Observed, lagSeen = head, true
			continue
		}
		out.Observed = head
		if mergeSHA != "" {
			out.Outcome = postMergeHeadReadConcurrentPush
			return out
		}
		out.Outcome = postMergeHeadReadReadBack
		return out
	}
	if lagSeen {
		out.Outcome = postMergeHeadReadReadAfterWriteLag
	} else {
		out.Outcome = postMergeHeadReadUnreadable
	}
	return out
}

// waitPostMergeBackoff waits d, returning false when ctx ends first.
func waitPostMergeBackoff(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// resolvePostMergeHead resolves the new head of a performed merge by
// PROVENANCE. With a decoded mergeSHA the merge commit is the new head for
// converged, read_after_write_lag and unreadable; only a genuine
// concurrent_push keeps the observed head (the merge commit is then already
// superseded on the branch). With mergeSHA == "" the observed head is
// accepted only on read_back; otherwise the head is unresolved ("").
func resolvePostMergeHead(pm postMergeHeadRead, mergeSHA string) string {
	if mergeSHA != "" {
		if pm.Outcome == postMergeHeadReadConcurrentPush {
			return pm.Observed
		}
		return mergeSHA
	}
	if pm.Outcome == postMergeHeadReadReadBack {
		return pm.Observed
	}
	return ""
}

// postMergeHeadUnresolvedReason names why an undecodable merge's new head
// stayed unresolved, for the republish warning.
func postMergeHeadUnresolvedReason(pm postMergeHeadRead, priorHead string) string {
	if pm.Outcome == postMergeHeadReadReadAfterWriteLag {
		return fmt.Sprintf("the merges endpoint returned no merge commit sha and all %d post-merge PR re-reads still returned the pre-merge head %s (read-after-write lag)",
			pm.Attempts, priorHead)
	}
	if pm.LastErr != nil {
		return pm.LastErr.Error()
	}
	return errPostMergeEmptyHead.Error()
}

// postMergeHeadReadNote is the post_merge_head_read_note shipped on a 200 for
// the degraded outcomes. Converged, read_back and concurrent_push carry no
// note: the last is reported by lineage_attribution_warning.
func postMergeHeadReadNote(pm postMergeHeadRead, priorHead, mergeSHA string) string {
	switch pm.Outcome {
	case postMergeHeadReadReadAfterWriteLag:
		if mergeSHA != "" {
			return fmt.Sprintf("post_merge_head_read=read_after_write_lag: all %d post-merge PR reads still returned the pre-merge head %s instead of the merge commit %s — the forge's PR head lags the branch-ref update. This is NOT a concurrent push: %s is the commit the merges endpoint reported creating, so it was attributed and anchors both the fishhawk_audit_complete check and the merge-candidate verify pass; no fishhawk_vouch_commit is needed.",
				pm.Attempts, priorHead, mergeSHA, mergeSHA)
		}
		return fmt.Sprintf("post_merge_head_read=read_after_write_lag: the merges endpoint returned no merge commit sha and all %d post-merge PR reads still returned the pre-merge head %s, so the new head is unresolved and was not published, attributed or verified (see audit_check_republish_warning and lineage_attribution_warning).",
			pm.Attempts, priorHead)
	case postMergeHeadReadUnreadable:
		errText := errPostMergeEmptyHead.Error()
		if pm.LastErr != nil {
			errText = pm.LastErr.Error()
		}
		if mergeSHA != "" {
			return fmt.Sprintf("post_merge_head_read=unreadable: no post-merge PR read succeeded in %d attempts (%s), so post-merge divergence could not be checked; the fishhawk_audit_complete check and the merge-candidate verify pass are anchored on the merge commit %s the merges endpoint reported creating.",
				pm.Attempts, errText, mergeSHA)
		}
		return fmt.Sprintf("post_merge_head_read=unreadable: no post-merge PR read succeeded in %d attempts (%s) and the merges endpoint returned no merge commit sha, so the new head is unresolved and was not published, attributed or verified (see audit_check_republish_warning and lineage_attribution_warning).",
			pm.Attempts, errText)
	}
	return ""
}

// postMergeHeadReadSchedule is the re-read schedule the rebase handler uses:
// the test seam when set, the production schedule otherwise.
func (s *Server) postMergeHeadReadSchedule() []time.Duration {
	if s.postMergeHeadReadBackoff != nil {
		return s.postMergeHeadReadBackoff
	}
	return defaultPostMergeHeadReadBackoff
}
