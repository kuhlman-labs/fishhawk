package plan

import "strings"

// Undecidable-cause classification (E72.63 / #4095). An acceptance criterion
// row the validator reports `undecidable` carries a free-prose
// undecidable_reason. The acceptance network sandbox admits only the target
// instance and Fishhawk's own endpoints, so a repository-local check that
// needs a database or container daemon (a pgtest/testcontainers-backed Go
// test, anything reading FISHHAWK_TEST_PG_URL, docker) cannot run there and
// fails `connect: operation not permitted`. The #4094 run reported four such
// rows with four differently worded reasons; ClassifyUndecidableReason names
// that one cause so the server ingest can record it as a structured
// `undecidable_causes` key on the acceptance_outcome_recorded payload.
//
// The constants live here — the plan package imports nothing from the repo —
// so the ingest, the acceptance prompt that asks for the explicit token and
// their tests share one spelling rather than free-typed strings.
const (
	// UndecidableCausesKey is the acceptance_outcome_recorded payload key
	// mapping each classified cause to the ids of the undecidable criterion
	// rows it explains, in row order: {"sandbox_denied_db": ["a", "b"]}. It is
	// emitted ONLY when at least one row is classified, so an unaffected
	// outcome marshals byte-identically to before, and it is DESCRIPTIVE ONLY —
	// it never changes the recorded verdict, triage routing or any gate.
	UndecidableCausesKey = "undecidable_causes"
	// UndecidableCauseSandboxDeniedDB is the cause naming a criterion left
	// undecided because the check that decides it needs a database or
	// container daemon the acceptance sandbox cannot reach. It is also the
	// explicit token the acceptance prompt asks the agent to lead such an
	// undecidable_reason with (`sandbox_denied_db: <the check it could not
	// run>`), which the token arm below matches verbatim.
	UndecidableCauseSandboxDeniedDB = "sandbox_denied_db"
)

// undecidableDatabaseMarkers name a database- or container-backed check in an
// undecidable_reason. Lowercase; matched as substrings of the lowered reason,
// so "Postgres", "PostgreSQL" and "DB-backed" all match.
var undecidableDatabaseMarkers = []string{
	"pgtest", "testcontainers", "postgres", "fishhawk_test_pg_url",
	"database", "db-backed", "docker",
}

// undecidableSandboxDenialSignature is the sandbox's own EPERM text for a
// refused connection (`dial tcp [::1]:5432: connect: operation not
// permitted`). It names a denial on its own, without the word "sandbox".
const undecidableSandboxDenialSignature = "operation not permitted"

// undecidableSandboxWord is the sandbox noun. It names a DENIAL only beside
// one of undecidableSandboxDenialVerbs: on its own it is a location ("the
// sandbox checkout lacks the database migration file the criterion names"),
// which is a different cause and must not classify (approval condition 2).
const undecidableSandboxWord = "sandbox"

// undecidableSandboxDenialVerbs are the refusal phrasings that turn a mention
// of the sandbox into a denial. They cover the four verbatim #4094 reasons
// ("the sandbox denies", "the sandbox cannot reach", "could not start in the
// sandbox", "the sandbox blocks"). "blocks"/"blocked" are listed instead of a
// "block" stem so the plan vocabulary's "blocking criterion" never counts as a
// refusal.
var undecidableSandboxDenialVerbs = []string{
	"deny", "denies", "denied", "denial",
	"blocks", "blocked",
	"cannot reach", "can't reach", "could not reach", "unreachable", "not reachable",
	"cannot start", "could not start",
	"cannot connect", "could not connect",
	"refused", "refuses", "not permitted", "not allowed", "forbidden",
}

// ClassifyUndecidableReason maps an undecidable criterion row's
// undecidable_reason to a cause, or "" when it names none. It returns
// UndecidableCauseSandboxDeniedDB when, case-insensitively, EITHER
//
//   - TOKEN: the reason contains the explicit `sandbox_denied_db` token the
//     acceptance prompt asks for; OR
//   - SIGNATURE: the reason names a database/container-backed check
//     (undecidableDatabaseMarkers) AND a sandbox denial — the EPERM signature,
//     or the word "sandbox" beside a refusal verb.
//
// Classification is DESCRIPTIVE: it never changes a verdict, a tally, triage
// or a gate, which is why a heuristic is acceptable here. The SIGNATURE arm is
// a heuristic over agent-written prose with two residuals. False positive: a
// reason naming a database, the sandbox and a refusal verb for an unrelated
// cause ("the sandbox checkout lacks the database migration, so the server
// could not start"). False negative: a reason naming neither the token nor
// both marker families. The prompt asks for the explicit token so the residual
// arm is the fallback, not the contract. Pure: no I/O, no state.
func ClassifyUndecidableReason(reason string) string {
	lowered := strings.ToLower(reason)
	if strings.Contains(lowered, UndecidableCauseSandboxDeniedDB) {
		return UndecidableCauseSandboxDeniedDB
	}
	if containsAnyPhrase(lowered, undecidableDatabaseMarkers) && namesSandboxDenial(lowered) {
		return UndecidableCauseSandboxDeniedDB
	}
	return ""
}

// namesSandboxDenial reports whether the lowered reason names a sandbox
// refusal: the EPERM signature on its own, or the sandbox noun together with a
// refusal verb.
func namesSandboxDenial(lowered string) bool {
	if strings.Contains(lowered, undecidableSandboxDenialSignature) {
		return true
	}
	return strings.Contains(lowered, undecidableSandboxWord) &&
		containsAnyPhrase(lowered, undecidableSandboxDenialVerbs)
}
