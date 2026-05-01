package version

import "testing"

func TestVersionNotEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty; -ldflags overrides should never produce an empty string")
	}
}
