package diffcov

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrParse is the sentinel every malformed-report failure wraps. Callers
// distinguish "the report could not be parsed" (a measurement FAILURE the
// backend treats as a violation) from "the report says nothing is covered"
// (a legitimate zero measurement) by testing for it — a silently empty map
// would collapse those two into the same, wrong, answer.
var ErrParse = errors.New("diffcov: malformed lcov report")

// FileCoverage maps a 1-based source line number to its execution count.
type FileCoverage map[int]int

// Coverage maps a source path — VERBATIM as the report's `SF:` record
// spells it, un-normalized — to its per-line hit counts. Normalization into
// repo-relative form happens in Measure, which is the only place that knows
// the repo root.
type Coverage map[string]FileCoverage

// ParseLCOV parses the LCOV tracefile subset that per-line coverage needs:
//
//	SF:<path>          begins a record for one source file
//	DA:<line>,<hits>   one line's execution count
//	end_of_record      closes the record
//
// Every other record type (TN, FN/FNDA/FNF/FNH, BRDA/BRF/BRH, LF/LH) is
// IGNORED rather than rejected — real producers emit them and none carry
// per-line data this measurement needs. The grammar is the stable subset
// documented in the lcov/geninfo tracefile reference.
//
// Malformed input returns an error wrapping ErrParse rather than a
// partially-filled map: a truncated record, a non-numeric line number or
// hit count, and a DA line outside any SF record are each a report the
// producer did not finish writing, and treating one as "nothing covered"
// would fail an opted-in run with a false RED.
//
// An empty report (no SF records at all) is likewise an error: a coverage
// tool that wrote a zero-record file did not measure anything, which is a
// measurement failure, not a measurement of zero.
//
// Repeated DA lines for the same line SUM, matching lcov's own semantics
// for a file measured across several test binaries.
func ParseLCOV(r io.Reader) (Coverage, error) {
	out := Coverage{}
	sc := bufio.NewScanner(r)
	// Coverage reports carry no long lines in the records we read, but a
	// producer may emit a long SF path; raise the cap off the 64KB default
	// so a legitimate report is never reported as malformed.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	current := ""
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "SF:"):
			path := strings.TrimSpace(strings.TrimPrefix(line, "SF:"))
			if path == "" {
				return nil, fmt.Errorf("%w: empty SF path at line %d", ErrParse, lineNo)
			}
			current = path
			if _, ok := out[path]; !ok {
				out[path] = FileCoverage{}
			}
		case line == "end_of_record":
			current = ""
		case strings.HasPrefix(line, "DA:"):
			if current == "" {
				return nil, fmt.Errorf("%w: DA record outside any SF record at line %d", ErrParse, lineNo)
			}
			ln, hits, err := parseDA(strings.TrimPrefix(line, "DA:"))
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: %s", ErrParse, lineNo, err.Error())
			}
			out[current][ln] += hits
		default:
			// Ignored record type (TN/FN/BRDA/LF/LH/...). Not an error.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: read: %s", ErrParse, err.Error())
	}
	if current != "" {
		return nil, fmt.Errorf("%w: record for %q is truncated (no end_of_record)", ErrParse, current)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: report contains no SF records", ErrParse)
	}
	return out, nil
}

// parseDA parses the "<line>,<hits>" body of a DA record. LCOV permits a
// trailing ",<checksum>" field, which is accepted and ignored.
func parseDA(body string) (line, hits int, err error) {
	parts := strings.Split(body, ",")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("DA record %q needs <line>,<hits>", body)
	}
	line, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("DA line number %q is not an integer", parts[0])
	}
	if line <= 0 {
		return 0, 0, fmt.Errorf("DA line number %d is not positive", line)
	}
	// Hit counts are integers; some producers emit a large count that
	// still fits int64 on a 64-bit platform. A non-numeric count is
	// malformed.
	hits, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("DA hit count %q is not an integer", parts[1])
	}
	if hits < 0 {
		return 0, 0, fmt.Errorf("DA hit count %d is negative", hits)
	}
	return line, hits, nil
}
