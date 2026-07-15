package workmgmt

import "testing"

// TestNextChildNumber table-drives the pure {n} allocation half of server-side
// child-number discovery (#1958): max+1 over the matched child titles, with
// epic-literal anchoring, malformed-title skipping, and gap tolerance.
func TestNextChildNumber(t *testing.T) {
	const defaultFormat = "[E{epic}.{n}] {summary}"

	child := func(title string) EpicChild { return EpicChild{Title: title} }

	cases := []struct {
		name   string
		format string
		epic   string
		kids   []EpicChild
		want   int
	}{
		{
			name:   "max plus one over mixed open and closed children",
			format: defaultFormat,
			epic:   "7",
			// Titles carry no open/closed marker — EpicChildren enumerates both
			// via sub-issue links, so this list stands in for the merged set.
			kids: []EpicChild{child("[E7.1] first"), child("[E7.2] second"), child("[E7.3] third")},
			want: 4,
		},
		{
			name:   "no matching children yields one",
			format: defaultFormat,
			epic:   "7",
			kids:   []EpicChild{child("[E9.1] a different epic's child"), child("plain title")},
			want:   1,
		},
		{
			name:   "empty children yields one",
			format: defaultFormat,
			epic:   "7",
			kids:   nil,
			want:   1,
		},
		{
			name:   "epic literal is anchored: 7 never matches 17, 70, or bare [E7]",
			format: defaultFormat,
			epic:   "7",
			kids: []EpicChild{
				child("[E17.5] child of epic 17"),
				child("[E70.2] child of epic 70"),
				child("[E7] the epic issue's own title"),
				child("[E7.4] the only real child of epic 7"),
			},
			want: 5,
		},
		{
			name:   "malformed and non-conforming titles are skipped",
			format: defaultFormat,
			epic:   "7",
			kids: []EpicChild{
				child("[E7.2] good"),
				child("E7.99 missing brackets"),
				child("[E7.] no number"),
				child("[E7.abc] non-numeric"),
				child(""),
			},
			want: 3,
		},
		{
			name:   "gaps are tolerated: max plus one, not a count",
			format: defaultFormat,
			epic:   "7",
			kids:   []EpicChild{child("[E7.1] one"), child("[E7.5] five")},
			want:   6,
		},
		{
			name:   "non-default title format still derives correctly",
			format: "E{epic}-{n}: {summary}",
			epic:   "12",
			kids:   []EpicChild{child("E12-3: alpha"), child("E12-8: beta"), child("E120-9: not a child of 12")},
			want:   9,
		},
		{
			name:   "format without an {n} placeholder yields one",
			format: "[E{epic}] {summary}",
			epic:   "7",
			kids:   []EpicChild{child("[E7] whatever")},
			want:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextChildNumber(tc.format, tc.epic, tc.kids)
			if got != tc.want {
				t.Errorf("NextChildNumber(%q, %q, %d children) = %d, want %d",
					tc.format, tc.epic, len(tc.kids), got, tc.want)
			}
		})
	}
}
