package workmgmt

import "testing"

// TestNextChildNumber table-drives the pure {n} allocation half of server-side
// child-number discovery (#1958): max+1 over the matched child titles, with
// epic-literal anchoring, malformed-title skipping, and gap tolerance. It also
// pins the #2101 fail-closed contract: a NON-EMPTY child set with zero numbered
// matches returns (0, false) rather than silently allocating 1.
func TestNextChildNumber(t *testing.T) {
	const defaultFormat = "[E{epic}.{n}] {summary}"

	child := func(title string) EpicChild { return EpicChild{Title: title} }

	cases := []struct {
		name   string
		format string
		epic   string
		kids   []EpicChild
		want   int
		wantOK bool
	}{
		{
			name:   "max plus one over mixed open and closed children",
			format: defaultFormat,
			epic:   "7",
			// Titles carry no open/closed marker — EpicChildren enumerates both
			// via sub-issue links, so this list stands in for the merged set.
			kids:   []EpicChild{child("[E7.1] first"), child("[E7.2] second"), child("[E7.3] third")},
			want:   4,
			wantOK: true,
		},
		{
			name:   "non-empty children with zero numbered matches fails closed",
			format: defaultFormat,
			epic:   "7",
			kids:   []EpicChild{child("[E9.1] a different epic's child"), child("plain title")},
			want:   0,
			wantOK: false,
		},
		{
			name:   "empty children yields one",
			format: defaultFormat,
			epic:   "7",
			kids:   nil,
			want:   1,
			wantOK: true,
		},
		{
			// The #389 corpus: 100 sub-issues all carrying the placeholder
			// literal [E22.X] (non-numeric), zero integer matches. Allocating 1
			// would collide, so this must fail closed rather than yield [E22.1].
			name:   "placeholder-literal corpus (issue #389 shape) fails closed",
			format: defaultFormat,
			epic:   "22",
			kids:   []EpicChild{child("[E22.X] placeholder a"), child("[E22.X] placeholder b"), child("[E22.X] placeholder c")},
			want:   0,
			wantOK: false,
		},
		{
			// A digit run too long for strconv.Atoi matches the regexp but must
			// NOT count as a numbered match — otherwise it could yield a
			// spurious (1, true) with max==0. It still counts toward "children
			// exist", so a corpus of only overflow titles fails closed (0,
			// false) rather than allocating a colliding 1.
			name:   "overflow-length digit run does not count as a match",
			format: defaultFormat,
			epic:   "7",
			kids:   []EpicChild{child("[E7.99999999999999999999999999999999] overflow")},
			want:   0,
			wantOK: false,
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
			want:   5,
			wantOK: true,
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
			want:   3,
			wantOK: true,
		},
		{
			name:   "gaps are tolerated: max plus one, not a count",
			format: defaultFormat,
			epic:   "7",
			kids:   []EpicChild{child("[E7.1] one"), child("[E7.5] five")},
			want:   6,
			wantOK: true,
		},
		{
			name:   "non-default title format still derives correctly",
			format: "E{epic}-{n}: {summary}",
			epic:   "12",
			kids:   []EpicChild{child("E12-3: alpha"), child("E12-8: beta"), child("E120-9: not a child of 12")},
			want:   9,
			wantOK: true,
		},
		{
			name:   "format without an {n} placeholder yields one",
			format: "[E{epic}] {summary}",
			epic:   "7",
			kids:   []EpicChild{child("[E7] whatever")},
			want:   1,
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NextChildNumber(tc.format, tc.epic, tc.kids)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("NextChildNumber(%q, %q, %d children) = (%d, %t), want (%d, %t)",
					tc.format, tc.epic, len(tc.kids), got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
