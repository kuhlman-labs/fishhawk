package diffcov

import (
	"errors"
	"strings"
	"testing"
)

// goldenLCOV is a two-record tracefile in the shape real producers emit,
// including the record types this parser deliberately IGNORES (TN, FN,
// FNDA, BRDA, LF, LH) so a producer's full output is exercised, not just
// the subset the parser reads.
const goldenLCOV = `TN:
SF:src/app.go
FN:10,main
FNDA:1,main
DA:10,1
DA:11,0
DA:14,3
LF:3
LH:2
BRDA:11,0,0,-
end_of_record
TN:
SF:src/util.go
DA:2,7
DA:3,7
LF:2
LH:2
end_of_record
`

func TestParseLCOVGolden(t *testing.T) {
	cov, err := ParseLCOV(strings.NewReader(goldenLCOV))
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}
	if len(cov) != 2 {
		t.Fatalf("records = %d, want 2 (%v)", len(cov), cov)
	}
	app := cov["src/app.go"]
	for line, want := range map[int]int{10: 1, 11: 0, 14: 3} {
		if got := app[line]; got != want {
			t.Errorf("src/app.go line %d hits = %d, want %d", line, got, want)
		}
	}
	if _, present := app[12]; present {
		t.Errorf("src/app.go line 12 has no DA record and must be absent, got present")
	}
	if got := cov["src/util.go"][2]; got != 7 {
		t.Errorf("src/util.go line 2 hits = %d, want 7", got)
	}
}

func TestParseLCOVRepeatedDASums(t *testing.T) {
	// One file measured by two test binaries: lcov's own semantics sum the
	// hit counts rather than last-write-wins.
	const in = "SF:a.go\nDA:1,2\nend_of_record\nSF:a.go\nDA:1,3\nend_of_record\n"
	cov, err := ParseLCOV(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}
	if got := cov["a.go"][1]; got != 5 {
		t.Errorf("summed hits = %d, want 5", got)
	}
}

// TestParseLCOVMalformed covers every branch that returns ErrParse. Each
// must be an ERROR, never a silently-empty map: an empty map reads as
// "nothing is covered" and would fail an opted-in run with a false RED.
func TestParseLCOVMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"truncated record", "SF:a.go\nDA:1,1\n", "truncated"},
		{"non-numeric line number", "SF:a.go\nDA:abc,1\nend_of_record\n", "not an integer"},
		{"non-numeric hit count", "SF:a.go\nDA:1,xyz\nend_of_record\n", "not an integer"},
		{"DA missing hit field", "SF:a.go\nDA:1\nend_of_record\n", "needs <line>,<hits>"},
		{"non-positive line number", "SF:a.go\nDA:0,1\nend_of_record\n", "not positive"},
		{"negative hit count", "SF:a.go\nDA:1,-4\nend_of_record\n", "negative"},
		{"DA outside any SF", "DA:1,1\n", "outside any SF"},
		// A NESTED SF: truncates the record it interrupts exactly as a
		// run-off-the-end does; it must fail closed for the same reason,
		// rather than letting a partial first record be evaluated as a
		// successful measurement.
		{"nested SF without end_of_record", "SF:a.go\nDA:1,1\nSF:b.go\nDA:2,1\nend_of_record\n", "still open"},
		// The nested-SF guard must not misfire on a record RE-opened after a
		// proper close (TestParseLCOVRepeatedDASums pins the happy shape);
		// here the second SF is nested inside the third, un-closed record.
		{"nested SF after a closed record", "SF:a.go\nDA:1,1\nend_of_record\nSF:b.go\nSF:c.go\nend_of_record\n", "still open"},
		{"empty SF path", "SF:\nend_of_record\n", "empty SF path"},
		{"empty file", "", "no SF records"},
		{"records but no SF", "TN:\nLF:0\n", "no SF records"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cov, err := ParseLCOV(strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("ParseLCOV succeeded with %v, want an error", cov)
			}
			if !errors.Is(err, ErrParse) {
				t.Errorf("error %v does not wrap ErrParse", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
			if cov != nil {
				t.Errorf("coverage = %v, want nil on a parse error", cov)
			}
		})
	}
}

func TestParseLCOVIgnoresTrailingChecksumAndCR(t *testing.T) {
	// A DA record may carry a third checksum field, and a report written on
	// Windows carries CRLF line endings. Neither is malformed.
	const in = "SF:a.go\r\nDA:1,4,abc123\r\nend_of_record\r\n"
	cov, err := ParseLCOV(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}
	if got := cov["a.go"][1]; got != 4 {
		t.Errorf("hits = %d, want 4", got)
	}
}
