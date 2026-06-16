package server

import "testing"

// TestValidateBindingAssertions covers the #1171 declaration-validation matrix:
// each valid type passes, and each malformed declaration (unknown type, missing
// path, non-repo-relative path, empty literal, a test_asserts path not ending
// in _test.go) is rejected. An empty/nil slice is the byte-identical
// no-declaration path and must pass.
func TestValidateBindingAssertions(t *testing.T) {
	cases := []struct {
		name       string
		assertions []bindingAssertion
		wantErr    bool
	}{
		{
			name:       "nil slice valid",
			assertions: nil,
			wantErr:    false,
		},
		{
			name:       "empty slice valid",
			assertions: []bindingAssertion{},
			wantErr:    false,
		},
		{
			name: "valid file_contains",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "backend/internal/yaml/pad.go", Literal: "pad: 3"},
			},
			wantErr: false,
		},
		{
			name: "valid test_asserts",
			assertions: []bindingAssertion{
				{Type: "test_asserts", Path: "backend/internal/yaml/pad_test.go", Literal: "TestPad"},
			},
			wantErr: false,
		},
		{
			name: "multiple valid",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: "x"},
				{Type: "test_asserts", Path: "a/b_test.go", Literal: "y"},
			},
			wantErr: false,
		},
		{
			name: "unknown type rejected",
			assertions: []bindingAssertion{
				{Type: "file_matches", Path: "a/b.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "empty type rejected",
			assertions: []bindingAssertion{
				{Type: "", Path: "a/b.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "missing path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "absolute path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "/etc/passwd", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "parent-traversal path rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "../secrets.go", Literal: "x"},
			},
			wantErr: true,
		},
		{
			name: "empty literal rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: ""},
			},
			wantErr: true,
		},
		{
			name: "test_asserts non-test path rejected",
			assertions: []bindingAssertion{
				{Type: "test_asserts", Path: "a/b.go", Literal: "TestX"},
			},
			wantErr: true,
		},
		{
			name: "first valid second invalid rejected",
			assertions: []bindingAssertion{
				{Type: "file_contains", Path: "a/b.go", Literal: "x"},
				{Type: "test_asserts", Path: "a/b.go", Literal: "y"},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBindingAssertions(tc.assertions)
			if tc.wantErr && err == nil {
				t.Fatalf("validateBindingAssertions(%v) = nil, want error", tc.assertions)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateBindingAssertions(%v) = %v, want nil", tc.assertions, err)
			}
		})
	}
}
