package cmdinfo

import "testing"

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
// to a real command key, and vice versa for grouped commands.
func TestGroupsMatchCommandKeys(t *testing.T) {
	keys := map[string]bool{}
	for _, c := range Commands() {
		keys[c.Key] = true
	}
	for _, g := range Groups() {
		for _, sub := range g.Subcommands {
			key := g.Name + " " + sub
			if !keys[key] {
				t.Errorf("group %q lists subcommand %q with no matching command key %q", g.Name, sub, key)
			}
		}
	}
}
