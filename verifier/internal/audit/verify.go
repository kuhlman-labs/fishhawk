package audit

import (
	"crypto/ed25519"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// IssueKind enumerates the verification failure modes.
type IssueKind string

// Issue kinds.
const (
	// IssueHashMismatch — entry's stored entry_hash does not equal
	// ComputeEntryHash(entry's inputs). The entry was tampered with
	// after signing, OR the signing algorithm has drifted (in which
	// case the canonical fixture test would also fail upstream).
	IssueHashMismatch IssueKind = "hash_mismatch"

	// IssueChainBroken — entry's prev_hash does not equal the prior
	// entry's entry_hash within the run. An entry was inserted,
	// removed, or reordered.
	IssueChainBroken IssueKind = "chain_broken"

	// IssueFirstEntryHasPrevHash — the first entry in a run carries
	// a non-nil prev_hash. By convention the chain starts at nil.
	IssueFirstEntryHasPrevHash IssueKind = "first_entry_has_prev_hash"

	// IssueSequenceNotMonotonic — entries within a run are not
	// strictly ascending by sequence. The sequence is BIGSERIAL on
	// the database side, so any non-monotonic ordering means the
	// export is malformed.
	IssueSequenceNotMonotonic IssueKind = "sequence_not_monotonic"

	// IssueMissingSigningKey — a run claimed in audit_entries has
	// no signing_keys row in the export. Without it the bundle
	// signature can't be verified.
	IssueMissingSigningKey IssueKind = "missing_signing_key"

	// IssueSignatureInvalid — Ed25519 verification failed for a
	// trace bundle.
	IssueSignatureInvalid IssueKind = "signature_invalid"
)

// Issue is one finding from VerifyExport. Aggregated into Result.
type Issue struct {
	RunID    uuid.UUID
	Sequence int64 // 0 when the issue isn't tied to a specific entry
	Kind     IssueKind
	Detail   string
}

// Result is the outcome of a verification run.
type Result struct {
	RunsVerified   int
	EntriesChecked int
	Issues         []Issue
}

// OK reports whether the verification turned up zero issues. Used
// for CLI exit-code logic.
func (r Result) OK() bool { return len(r.Issues) == 0 }

// VerifyExport walks every run in the export and checks the chain
// integrity for each. The result aggregates issues; the caller
// decides whether to print, exit non-zero, etc.
//
// This is the load-bearing function for the audit-grade pitch: an
// external party with the export and our public canonical hash
// algorithm can verify the chain without trusting the backend.
func VerifyExport(ex *Export) Result {
	res := Result{}
	if ex == nil {
		res.Issues = append(res.Issues, Issue{
			Kind:   "internal",
			Detail: "nil export passed to VerifyExport",
		})
		return res
	}

	// Sorted run order so issues report deterministically.
	runIDs := make([]string, 0, len(ex.Runs))
	for id := range ex.Runs {
		runIDs = append(runIDs, id)
	}
	sort.Strings(runIDs)

	for _, idStr := range runIDs {
		runID, err := uuid.Parse(idStr)
		if err != nil {
			res.Issues = append(res.Issues, Issue{
				Kind:   "internal",
				Detail: fmt.Sprintf("run id %q is not a valid UUID: %v", idStr, err),
			})
			continue
		}
		runData := ex.Runs[idStr]
		res.RunsVerified++

		issues := verifyRun(runID, runData)
		res.Issues = append(res.Issues, issues...)
		res.EntriesChecked += len(runData.AuditEntries)
	}
	return res
}

// verifyRun checks one run's chain. Issues collected are scoped to
// the run; they get appended into the parent Result.
func verifyRun(runID uuid.UUID, runData RunData) []Issue {
	var issues []Issue

	// Defensive: guarantee entries arrive sequence-ascending. The
	// canonical export emits them in order; if a tool re-shuffled
	// them, the chain check below would surface lots of false
	// positives. A separate issue-kind makes the diagnostic clean.
	for i := 1; i < len(runData.AuditEntries); i++ {
		if runData.AuditEntries[i].Sequence <= runData.AuditEntries[i-1].Sequence {
			issues = append(issues, Issue{
				RunID:    runID,
				Sequence: runData.AuditEntries[i].Sequence,
				Kind:     IssueSequenceNotMonotonic,
				Detail: fmt.Sprintf("sequence %d is not greater than prior sequence %d",
					runData.AuditEntries[i].Sequence, runData.AuditEntries[i-1].Sequence),
			})
		}
	}

	for i, entry := range runData.AuditEntries {
		// 1. The entry's own hash matches a recompute of its inputs.
		got, err := ComputeEntryHash(entry.ToHashInputs())
		if err != nil {
			issues = append(issues, Issue{
				RunID: runID, Sequence: entry.Sequence,
				Kind:   "internal",
				Detail: fmt.Sprintf("compute hash: %v", err),
			})
			continue
		}
		if got != entry.EntryHash {
			issues = append(issues, Issue{
				RunID: runID, Sequence: entry.Sequence,
				Kind: IssueHashMismatch,
				Detail: fmt.Sprintf("recomputed %s, stored %s",
					got, entry.EntryHash),
			})
		}

		// 2. The chain link points at the prior entry, or nil for
		// the first entry.
		if i == 0 {
			if entry.PrevHash != nil {
				issues = append(issues, Issue{
					RunID: runID, Sequence: entry.Sequence,
					Kind:   IssueFirstEntryHasPrevHash,
					Detail: fmt.Sprintf("prev_hash = %q, expected nil", *entry.PrevHash),
				})
			}
			continue
		}
		prior := runData.AuditEntries[i-1]
		if entry.PrevHash == nil {
			issues = append(issues, Issue{
				RunID: runID, Sequence: entry.Sequence,
				Kind:   IssueChainBroken,
				Detail: fmt.Sprintf("prev_hash is nil; expected %s (prior entry's entry_hash)", prior.EntryHash),
			})
			continue
		}
		if *entry.PrevHash != prior.EntryHash {
			issues = append(issues, Issue{
				RunID: runID, Sequence: entry.Sequence,
				Kind: IssueChainBroken,
				Detail: fmt.Sprintf("prev_hash %s != prior entry_hash %s",
					*entry.PrevHash, prior.EntryHash),
			})
		}
	}
	return issues
}

// VerifyBundleSignature checks an Ed25519 signature against a
// bundle's bytes using the run's public key. Caller is expected to
// have already extracted the public key from the export.
//
// Per ADR-008 (#72), the signed message is sha256(raw_bundle_bytes).
// Recomputing here means the verifier doesn't have to trust the
// caller about which bytes hashed to what.
func VerifyBundleSignature(publicKey, bundle, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("audit: public key length %d, want %d",
			len(publicKey), ed25519.PublicKeySize)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("audit: signature length %d, want %d",
			len(signature), ed25519.SignatureSize)
	}
	msg := computeMessage(bundle)
	if !ed25519.Verify(ed25519.PublicKey(publicKey), msg, signature) {
		return fmt.Errorf("audit: signature does not verify")
	}
	return nil
}

// computeMessage matches backend/internal/signing.ComputeMessage —
// sha256 of the raw bundle bytes. Re-implemented here, like
// ComputeEntryHash, so the verifier doesn't depend on backend code.
func computeMessage(bundle []byte) []byte {
	sum := sha256Sum(bundle)
	return sum[:]
}
