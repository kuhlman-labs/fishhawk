package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrBindingAssertionUnsatisfied is the pre-push binding-assertion gate
// sentinel (#1171). An operator declares deterministic substring checks at
// plan-approval time (fishhawk_approve_plan binding_assertions); the backend
// echoes them on the implement prompt-response and the runner evaluates each
// against the committed scope-only tree BEFORE the push. Any unsatisfied
// assertion wraps this sentinel, which the runner classifies category-B
// (artifact does not meet a declared binding condition → park for
// re-scope/re-plan), the same chain as ErrScopeFilesMissing.
var ErrBindingAssertionUnsatisfied = errors.New("gitops: declared binding assertion(s) not satisfied by the committed tree")

// BindingAssertionsUnsatisfiedError is the typed carrier for the
// binding-assertion shortfall (#2501), mirroring *ScopeFilesMissingError
// field-for-field. Unwrap returns the ErrBindingAssertionUnsatisfied sentinel,
// so every existing errors.Is(err, ErrBindingAssertionUnsatisfied)
// classification — including the runner's category-B mapping — keeps working
// unchanged.
//
// The runner returns it from the verifyCommit closure ONLY after every OTHER
// committed-tree gate (created-out-of-scope, missing-declared-scope-file,
// compile/test, verified-tree) has already passed, so when CommitAndPush sees
// it with ParkOnAssertionShortfall set it is provably the SOLE gate failure —
// the unsatisfied-binding-assertion-ONLY class. That class PARKS for an
// operator exempt/fail decision rather than failing category-B: CommitAndPush
// pushes the verified commit to the run branch anyway (so the held commit
// survives the runner exit and a later exempt resolution can open the PR from
// it, ADR-035) and surfaces AssertionShortfall instead of aborting. Any
// compound failure (an unsatisfied assertion + another gate) never reaches this
// type because the runner's terminal resolver returns an UNTYPED sentinel wrap
// instead, keeping today's category-B.
//
// Error() has ONE rule, byte-identical to ScopeFilesMissingError.Error(): it
// returns Message VERBATIM when Message is non-empty, and the
// ErrBindingAssertionUnsatisfied sentinel text when Message is empty.
type BindingAssertionsUnsatisfiedError struct {
	// Unsatisfied is the order-preserving list of declared assertions the
	// committed tree did not satisfy.
	Unsatisfied []BindingAssertionResult
	// Message is the full human-readable gate message. Error returns it
	// verbatim so the runner's failure-report narrative is byte-identical to
	// the pre-#2501 wrap.
	Message string
}

func (e *BindingAssertionsUnsatisfiedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return ErrBindingAssertionUnsatisfied.Error()
}

// Unwrap returns the ErrBindingAssertionUnsatisfied sentinel so
// errors.Is(err, ErrBindingAssertionUnsatisfied) holds for the typed error.
func (*BindingAssertionsUnsatisfiedError) Unwrap() error { return ErrBindingAssertionUnsatisfied }

// BindingAssertion is one operator-declared deterministic check to evaluate
// against a committed tree. It mirrors the wire fields the runner decodes from
// the prompt-response (upload.BindingAssertion); the caller converts so this
// package stays free of the upload import. Type is carried for evidence/report
// fidelity but does not change the check: both v0 types (file_contains,
// test_asserts) are the identical substring primitive — the *_test.go path
// constraint that distinguishes test_asserts is enforced backend-side at
// declaration time, so the runner never re-validates it.
type BindingAssertion struct {
	Type    string
	Path    string
	Literal string
}

// BindingAssertionResult is the per-assertion outcome. Satisfied is true when
// Literal appears as a substring in the committed content of Path. The full
// result set (satisfied and unsatisfied) is returned so the caller can both
// surface every outcome in the gate evidence AND fail on the unsatisfied
// subset.
type BindingAssertionResult struct {
	Type      string
	Path      string
	Literal   string
	Satisfied bool
}

// EvaluateBindingAssertions reads the committed content of each assertion's
// Path at ref (any tree-ish: a commit SHA for the pre-push gate, a tree object
// hash for the evidence path) via `git show <ref>:<path>` and reports whether
// the declared Literal is present as a deterministic substring. A path absent
// from the committed tree (git show exits non-zero) counts as unsatisfied
// rather than an error — a declared assertion against a file the commit never
// produced is a real shortfall, not an infra failure. A genuine git invocation
// failure on an existing path is indistinguishable from absence at this layer,
// so both fail closed (Satisfied=false); the gate is deliberately conservative.
//
// An empty (or nil) assertion slice returns no results and no error — the
// byte-identical no-declaration no-op. Package-level (not a *Pusher method)
// like MissingScopeFiles/UntrackedPaths: the caller is the runner's pre-push
// path with no *Pusher in scope.
func EvaluateBindingAssertions(ctx context.Context, repoDir, ref string, assertions []BindingAssertion) ([]BindingAssertionResult, error) {
	if len(assertions) == 0 {
		return nil, nil
	}
	results := make([]BindingAssertionResult, 0, len(assertions))
	for _, a := range assertions {
		res := BindingAssertionResult{Type: a.Type, Path: a.Path, Literal: a.Literal}
		// `git show <ref>:<path>` prints the blob at that path in the tree;
		// it exits non-zero when the path is absent from the tree. An absent
		// path is an unsatisfied assertion, not a gate error.
		out, err := (&Pusher{}).runOut(ctx, repoDir, "show", ref+":"+a.Path)
		if err == nil && strings.Contains(out, a.Literal) {
			res.Satisfied = true
		}
		results = append(results, res)
	}
	return results, nil
}

// UnsatisfiedBindingAssertions returns the subset of results whose assertion
// was not satisfied, preserving order. The caller fails the stage when this is
// non-empty.
func UnsatisfiedBindingAssertions(results []BindingAssertionResult) []BindingAssertionResult {
	var unsatisfied []BindingAssertionResult
	for _, r := range results {
		if !r.Satisfied {
			unsatisfied = append(unsatisfied, r)
		}
	}
	return unsatisfied
}

// FormatUnsatisfied renders an actionable per-assertion message for the
// category-B failure. Each line names the type, path, and the literal the
// committed tree did not contain.
func FormatUnsatisfied(unsatisfied []BindingAssertionResult) string {
	parts := make([]string, 0, len(unsatisfied))
	for _, r := range unsatisfied {
		parts = append(parts, fmt.Sprintf("%s on %s did not find %q", r.Type, r.Path, r.Literal))
	}
	return strings.Join(parts, "; ")
}
