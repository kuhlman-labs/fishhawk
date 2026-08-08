package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestEvaluateBindingAssertions covers the pre-push binding-assertion gate
// primitive (#1171): a declared literal present in the committed file is
// satisfied; a present-file-missing-literal and an absent-file are both
// unsatisfied; the test_asserts type uses the identical substring primitive;
// and an empty assertion slice is a no-op returning no results.
func TestEvaluateBindingAssertions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Build a repo whose committed tree carries a YAML file with `pad: 3` and a
	// Go test file asserting a named symbol.
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "config.yaml"), []byte("layout:\n  pad: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "thing_test.go"), []byte("package p\n\nfunc TestPad(t *testing.T) { _ = padWidth }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add config + test")
	head := mustGitOut(t, repo, "rev-parse", "HEAD")

	t.Run("satisfied literals pass", func(t *testing.T) {
		results, err := EvaluateBindingAssertions(context.Background(), repo, head, []BindingAssertion{
			{Type: "file_contains", Path: "config.yaml", Literal: "pad: 3"},
			{Type: "test_asserts", Path: "thing_test.go", Literal: "padWidth"},
		})
		if err != nil {
			t.Fatalf("EvaluateBindingAssertions: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("results = %d, want 2", len(results))
		}
		for _, r := range results {
			if !r.Satisfied {
				t.Errorf("%s on %s: satisfied = false, want true", r.Type, r.Path)
			}
		}
		if len(UnsatisfiedBindingAssertions(results)) != 0 {
			t.Errorf("unsatisfied = %v, want none", UnsatisfiedBindingAssertions(results))
		}
	})

	t.Run("present file missing literal is unsatisfied", func(t *testing.T) {
		results, err := EvaluateBindingAssertions(context.Background(), repo, head, []BindingAssertion{
			{Type: "file_contains", Path: "config.yaml", Literal: "pad: 99"},
		})
		if err != nil {
			t.Fatalf("EvaluateBindingAssertions: %v", err)
		}
		if len(results) != 1 || results[0].Satisfied {
			t.Errorf("results = %+v, want one unsatisfied", results)
		}
		unsat := UnsatisfiedBindingAssertions(results)
		if len(unsat) != 1 {
			t.Fatalf("unsatisfied = %d, want 1", len(unsat))
		}
		if msg := FormatUnsatisfied(unsat); msg == "" {
			t.Error("FormatUnsatisfied returned empty message")
		}
	})

	t.Run("absent file is unsatisfied", func(t *testing.T) {
		results, err := EvaluateBindingAssertions(context.Background(), repo, head, []BindingAssertion{
			{Type: "file_contains", Path: "does/not/exist.go", Literal: "anything"},
		})
		if err != nil {
			t.Fatalf("EvaluateBindingAssertions returned error for absent file, want unsatisfied result: %v", err)
		}
		if len(results) != 1 || results[0].Satisfied {
			t.Errorf("results = %+v, want one unsatisfied (absent path)", results)
		}
	})

	t.Run("empty slice is a no-op", func(t *testing.T) {
		results, err := EvaluateBindingAssertions(context.Background(), repo, head, nil)
		if err != nil {
			t.Fatalf("EvaluateBindingAssertions(nil): %v", err)
		}
		if results != nil {
			t.Errorf("results = %v, want nil for empty input", results)
		}
	})
}

// TestErrBindingAssertionUnsatisfiedIsWrappable confirms the sentinel survives
// fmt.Errorf %w wrapping so the runner's errors.Is category-B classification
// chain recognizes it.
func TestErrBindingAssertionUnsatisfiedIsWrappable(t *testing.T) {
	wrapped := errors.Join(ErrBindingAssertionUnsatisfied, errors.New("detail"))
	if !errors.Is(wrapped, ErrBindingAssertionUnsatisfied) {
		t.Error("wrapped error does not match ErrBindingAssertionUnsatisfied")
	}
}

// TestBindingAssertionsUnsatisfiedError_Contract pins the typed carrier the
// #2501 park mechanism keys on: BOTH branches of the single Error() rule
// (Message verbatim when non-empty, the sentinel text when empty), the
// errors.Is unwrap that keeps every existing category-B classification working,
// and errors.As extracting the order-preserving Unsatisfied set with
// Type/Path/Literal intact.
func TestBindingAssertionsUnsatisfiedError_Contract(t *testing.T) {
	unsatisfied := []BindingAssertionResult{
		{Type: "file_contains", Path: "a.go", Literal: "ALPHA"},
		{Type: "test_asserts", Path: "b_test.go", Literal: "BETA"},
	}
	withMsg := &BindingAssertionsUnsatisfiedError{Unsatisfied: unsatisfied, Message: "full narrative"}
	if got := withMsg.Error(); got != "full narrative" {
		t.Errorf("Error() with a Message must return it verbatim, got %q", got)
	}
	empty := &BindingAssertionsUnsatisfiedError{Unsatisfied: unsatisfied}
	if got := empty.Error(); got != ErrBindingAssertionUnsatisfied.Error() {
		t.Errorf("Error() with an empty Message must return the sentinel text, got %q", got)
	}

	// errors.Is keeps the existing category-B mapping working through the type.
	if !errors.Is(error(withMsg), ErrBindingAssertionUnsatisfied) {
		t.Error("the typed error must unwrap to ErrBindingAssertionUnsatisfied")
	}
	// ...including through an outer wrap.
	if !errors.Is(fmt.Errorf("commit+push: %w", error(withMsg)), ErrBindingAssertionUnsatisfied) {
		t.Error("a wrapped typed error must still match the sentinel")
	}

	var extracted *BindingAssertionsUnsatisfiedError
	if !errors.As(fmt.Errorf("commit+push: %w", error(withMsg)), &extracted) {
		t.Fatal("errors.As must extract the typed error through a wrap")
	}
	if len(extracted.Unsatisfied) != 2 {
		t.Fatalf("Unsatisfied len = %d, want 2", len(extracted.Unsatisfied))
	}
	if extracted.Unsatisfied[0] != unsatisfied[0] || extracted.Unsatisfied[1] != unsatisfied[1] {
		t.Errorf("Unsatisfied must be order-preserving with fields intact, got %+v", extracted.Unsatisfied)
	}
}
