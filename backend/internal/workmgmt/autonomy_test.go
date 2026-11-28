package workmgmt

import "testing"

// TestParseAutonomyLabel covers the tier extraction (ADR-069 decision 4,
// #5316): an absent label resolves to the supplied default, each valid tier
// wins over the default, an unrecognised or empty tier resolves to the default,
// an empty or out-of-set default degrades to medium (never ""), and the first
// autonomy label still wins. It is the single-source-of-truth table for the
// parser the assembly path and the reconcile-on-read refresh both use (#2355).
func TestParseAutonomyLabel(t *testing.T) {
	cases := []struct {
		name        string
		labels      []string
		defaultTier string
		want        string
	}{
		{"nil labels resolve to default low", nil, "low", "low"},
		{"nil labels resolve to default high", nil, "high", "high"},
		{"no autonomy label resolves to default", []string{"area:server", "phase:1"}, "low", "low"},
		{"low beats default", []string{"autonomy:low"}, "high", "low"},
		{"medium beats default", []string{"autonomy:medium"}, "low", "medium"},
		{"high beats default", []string{"autonomy:high"}, "low", "high"},
		{"unrecognised tier resolves to default", []string{"autonomy:critical"}, "low", "low"},
		{"empty tier resolves to default", []string{"autonomy:"}, "low", "low"},
		{"empty default degrades to medium", nil, "", "medium"},
		{"unrecognised tier with empty default degrades to medium", []string{"autonomy:critical"}, "", "medium"},
		{"out-of-set default degrades to medium", nil, "critical", "medium"},
		{"non-autonomy label alongside", []string{"type:feature", "autonomy:high"}, "low", "high"},
		{"first autonomy label wins", []string{"autonomy:low", "autonomy:high"}, "medium", "low"},
		{"unrecognised first label resolves to default, not the second", []string{"autonomy:critical", "autonomy:high"}, "low", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseAutonomyLabel(tc.labels, tc.defaultTier); got != tc.want {
				t.Errorf("ParseAutonomyLabel(%v, %q) = %q, want %q", tc.labels, tc.defaultTier, got, tc.want)
			}
		})
	}
}

// TestDeclaredAutonomyTier pins the declared-tier predicate the reconcile
// refresh uses to apply an explicit label without a conventions read (#5316).
func TestDeclaredAutonomyTier(t *testing.T) {
	cases := []struct {
		name     string
		labels   []string
		wantTier string
		wantOK   bool
	}{
		{"nil labels", nil, "", false},
		{"no autonomy label", []string{"type:bug"}, "", false},
		{"declared low", []string{"type:bug", "autonomy:low"}, "low", true},
		{"declared high", []string{"autonomy:high"}, "high", true},
		{"unrecognised tier is not declared", []string{"autonomy:critical"}, "", false},
		{"empty tier is not declared", []string{"autonomy:"}, "", false},
		{"first autonomy label wins", []string{"autonomy:medium", "autonomy:low"}, "medium", true},
		{"unrecognised first label is not declared", []string{"autonomy:critical", "autonomy:low"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, ok := DeclaredAutonomyTier(tc.labels)
			if tier != tc.wantTier || ok != tc.wantOK {
				t.Errorf("DeclaredAutonomyTier(%v) = (%q, %v), want (%q, %v)", tc.labels, tier, ok, tc.wantTier, tc.wantOK)
			}
		})
	}
}

// TestNormalizeAutonomyTier pins the backstop: an in-set tier passes through,
// and "" or an out-of-set tier degrades to BuiltinDefaultAutonomyTier — never "".
func TestNormalizeAutonomyTier(t *testing.T) {
	cases := map[string]string{
		"low":          "low",
		"medium":       "medium",
		"high":         "high",
		"":             "medium",
		"critical":     "medium",
		"LOW":          "medium",
		"autonomy:low": "medium",
	}
	for in, want := range cases {
		if got := NormalizeAutonomyTier(in); got != want {
			t.Errorf("NormalizeAutonomyTier(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestConventionsDefaultAutonomyTier pins the repo-default derivation: the
// least permissive in-set label_defaults.autonomy across item types, falling
// back to medium, with malformed values ignored (#5316). The disagreeing-types
// fixture declares high, low and medium, so the result is low regardless of map
// iteration order.
func TestConventionsDefaultAutonomyTier(t *testing.T) {
	typ := func(autonomy string) ItemType {
		if autonomy == "" {
			return ItemType{}
		}
		return ItemType{LabelDefaults: map[string]string{"autonomy": autonomy}}
	}
	cases := []struct {
		name  string
		types map[string]ItemType
		want  string
	}{
		{"single type declaring low", map[string]ItemType{"feature": typ("autonomy:low")}, "low"},
		{"single type declaring high", map[string]ItemType{"feature": typ("autonomy:high")}, "high"},
		{"least permissive across disagreeing types", map[string]ItemType{
			"feature": typ("autonomy:high"),
			"bug":     typ("autonomy:low"),
			"chore":   typ("autonomy:medium"),
		}, "low"},
		{"one specialty type declaring low among medium types", map[string]ItemType{
			"feature":  typ("autonomy:medium"),
			"bug":      typ("autonomy:medium"),
			"security": typ("autonomy:low"),
			"epic":     typ(""),
		}, "low"},
		{"no type declaring one", map[string]ItemType{"feature": typ(""), "epic": typ("")}, "medium"},
		{"no types at all", nil, "medium"},
		{"malformed tier ignored", map[string]ItemType{
			"feature": typ("autonomy:critical"),
			"bug":     typ("autonomy:high"),
		}, "high"},
		{"namespace-less value ignored", map[string]ItemType{
			"feature": typ("low"),
			"bug":     typ("autonomy:high"),
		}, "high"},
		{"only malformed values fall back to medium", map[string]ItemType{
			"feature": typ("autonomy:critical"),
			"bug":     typ("low"),
		}, "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Conventions{Types: tc.types}
			if got := c.DefaultAutonomyTier(); got != tc.want {
				t.Errorf("DefaultAutonomyTier() = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("built-in Default conventions resolve to medium", func(t *testing.T) {
		if got := Default().DefaultAutonomyTier(); got != "medium" {
			t.Errorf("Default().DefaultAutonomyTier() = %q, want medium", got)
		}
	})
}

// TestDefaultConventionsAutonomyDefaultMatchesBuiltin is the lockstep check:
// the shipped built-in conventions' derived default must equal the constant
// ADR-069 decision 4 names, so the two cannot drift silently.
func TestDefaultConventionsAutonomyDefaultMatchesBuiltin(t *testing.T) {
	if got := Default().DefaultAutonomyTier(); got != BuiltinDefaultAutonomyTier {
		t.Errorf("Default().DefaultAutonomyTier() = %q, BuiltinDefaultAutonomyTier = %q; keep the shipped conventions' label_defaults.autonomy and the constant in lockstep", got, BuiltinDefaultAutonomyTier)
	}
}
