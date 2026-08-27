package splitfiling

import (
	"strconv"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// This file composes the generic workmgmt idempotency-key primitives into the
// SPLIT-FILING keys and the two pure lookups the server hook drives (#2064,
// E50.7). It stays free of any HTTP or provider dependency — it is handed
// already-fetched children and already-fetched comment bodies — so every rule
// below is unit-testable without a server or a forge.
//
// THE WINDOW THIS CLOSES. The split-filing hook dedups on the AUDIT LOG: a
// per-ordinal work_item_filed marker written immediately AFTER provider.File.
// The residual is the interleaving where File SUCCEEDS and the marker append
// then FAILS — the ordinal has no durable record, so a re-approval re-files a
// DUPLICATE child. Stamping a deterministic key into each filed child's body
// and querying the parent's children for that key before filing turns the
// forge itself into the durable record.

const (
	// splitChildKeyNamespace namespaces the per-ordinal child keys.
	splitChildKeyNamespace = "fishhawk-split-child"
	// splitCommentKeyNamespace namespaces the parent-comment keys.
	splitCommentKeyNamespace = "fishhawk-split-comment"
	// splitParentCloseKeyNamespace namespaces the parent-close linking-comment
	// keys the E50.6 watcher stamps (#2062). It is deliberately DISTINCT from
	// splitCommentKeyNamespace: the acceptance-carrier comment and the
	// parent-close comment land on the same parent thread and must never dedup
	// against each other.
	splitParentCloseKeyNamespace = "fishhawk-split-parent-close"
)

// Comment kinds passed to SplitCommentKey. They are distinct so the acceptance
// carrier and the #2412 over-cap refusal — which arise from the SAME
// re-approval window and post on the same parent thread — never dedup against
// each other.
const (
	// CommentKindAcceptance is the parent acceptance-carrier comment posted
	// after a complete filing.
	CommentKindAcceptance = "acceptance"
	// CommentKindRefusal is the parent over-cap refusal comment (#2412).
	CommentKindRefusal = "refusal"
)

// SplitChildKey mints the deterministic idempotency key for one phase ordinal
// of one run's split filing. It is a pure function of (run id, phase index), so
// a re-approval re-derives exactly the key the first pass stamped.
func SplitChildKey(runID string, phaseIndex int) string {
	return workmgmt.MintIdempotencyKey(splitChildKeyNamespace, runID, strconv.Itoa(phaseIndex))
}

// SplitCommentKey mints the deterministic idempotency key for one of the
// parent-thread comments this hook posts (see the CommentKind* constants).
func SplitCommentKey(runID, kind string) string {
	return workmgmt.MintIdempotencyKey(splitCommentKeyNamespace, runID, kind)
}

// ParentCloseCommentKey mints the deterministic idempotency key for the ONE
// linking comment the parent-close watcher posts on a split parent before
// closing it (#2062, E50.6).
//
// It takes NO RUN ID, deliberately asymmetric with [SplitCommentKey] above.
// The watcher is driven by an `issues.closed` webhook and resolves its linkage
// as a pure audit-payload read — it never resolves a run — so it must be able
// to re-derive the EXACT key a prior delivery stamped from forge facts alone:
// the parent's repository, the parent's issue number, and the contract child's
// number. A run-keyed key would be underivable on that path, leaving the dedup
// permanently inert and re-posting the comment on every redelivery.
//
// parentRepo is inside the digest, so the same (parent, child) numbers in a
// DIFFERENT repository mint a different key — issue numbers are per-repo, and a
// cross-repo collision must never look like an already-posted comment.
func ParentCloseCommentKey(parentRepo string, parentIssue, contractChildNumber int) string {
	return workmgmt.MintIdempotencyKey(splitParentCloseKeyNamespace, parentRepo,
		strconv.Itoa(parentIssue), strconv.Itoa(contractChildNumber))
}

// StampComment returns body with the hidden idempotency marker for key
// appended. It is the OUTBOUND half of the comment dedup: ThreadHasComment can
// only ever find a marker that this function put there, so a comment posted
// without it would leave the dedup permanently inert.
func StampComment(body, key string) string {
	return workmgmt.StampIdempotencyKey(body, key)
}

// FindAdoptableChild returns the child whose body bears key, and whether one
// was found.
//
// It consumes the WHOLE slice it is handed. When MORE THAN ONE child bears the
// same key — possible only if a prior concurrent re-approval already created a
// duplicate (the named TOCTOU residual) — the tie-break is DETERMINISTIC:
// LOWEST NUMBER WINS. That is an intentional choice, not an accident of
// iteration order: the lowest-numbered child is the earliest-created one, so
// adoption converges on the same child on every subsequent pass regardless of
// what order the forge returns them in. The second child is NOT reconciled —
// this function neither closes nor links it; an operator resolves the duplicate.
// A non-deterministic tie-break would be worse than the duplicate itself: two
// passes could adopt different children and record different numbers for the
// same ordinal.
//
// Matching is the whole-line marker match of workmgmt.BodyHasIdempotencyKey, so
// a child whose key is a strict PREFIX of the sought key is NOT adopted. State
// is irrelevant: a CLOSED child bearing the key is adoptable — it was filed, so
// re-filing it would duplicate it.
func FindAdoptableChild(children []workmgmt.EpicChild, key string) (workmgmt.EpicChild, bool) {
	if key == "" {
		return workmgmt.EpicChild{}, false
	}
	var best workmgmt.EpicChild
	found := false
	for _, c := range children {
		if !workmgmt.BodyHasIdempotencyKey(c.Body, key) {
			continue
		}
		if !found || c.Number < best.Number {
			best = c
			found = true
		}
	}
	return best, found
}

// ThreadHasComment reports whether the already-fetched comment thread carries a
// comment bearing key. It consumes the WHOLE slice it is handed — a match in
// the LAST element is found — because the caller pages the entire thread and a
// head-only inspection would re-post a comment that is already there, several
// pages down.
func ThreadHasComment(bodies []string, key string) bool {
	if key == "" {
		return false
	}
	for _, b := range bodies {
		if workmgmt.BodyHasIdempotencyKey(b, key) {
			return true
		}
	}
	return false
}
