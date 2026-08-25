package cmdinfo

import (
	"strings"
	"testing"
)

func TestCommandsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Commands() {
		if c.Key == "" {
			t.Errorf("command with empty key: %+v", c)
		}
		if seen[c.Key] {
			t.Errorf("duplicate command key %q", c.Key)
		}
		seen[c.Key] = true
		if c.Synopsis == "" {
			t.Errorf("command %q has empty synopsis", c.Key)
		}
		fseen := map[string]bool{}
		for _, f := range c.Flags {
			if f == "" || f[0] == '-' {
				t.Errorf("command %q flag %q must be a bare name (no leading dash)", c.Key, f)
			}
			if fseen[f] {
				t.Errorf("command %q lists flag %q twice", c.Key, f)
			}
			fseen[f] = true
		}
	}
}

// TestGroupsMatchCommandKeys asserts every group subcommand corresponds
// to a real command key, AND — the "vice versa" the doc comment claims but
// the forward direction alone did not enforce — that every grouped command
// key (one containing a space) is listed by exactly the group it names. A
// grouped command added to Commands() without a Groups() entry, or a Groups()
// subcommand with no command key, both redden.
func TestGroupsMatchCommandKeys(t *testing.T) {
	keys := map[string]bool{}
	for _, c := range Commands() {
		keys[c.Key] = true
	}
	grouped := map[string]bool{}
	for _, g := range Groups() {
		for _, sub := range g.Subcommands {
			key := g.Name + " " + sub
			if !keys[key] {
				t.Errorf("group %q lists subcommand %q with no matching command key %q", g.Name, sub, key)
			}
			grouped[key] = true
		}
	}
	// Reverse direction: every grouped command key is covered by a group.
	for _, c := range Commands() {
		if strings.Contains(c.Key, " ") && !grouped[c.Key] {
			t.Errorf("command %q is grouped (has a space) but no Group lists it", c.Key)
		}
	}
}
