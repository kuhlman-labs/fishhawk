package agent

import (
	"errors"
	"strings"
	"testing"
)

// scriptReader serves data across Read calls, then returns tail (which may be
// io.EOF or a genuine error) once the data is exhausted. It lets a test drive a
// non-EOF read error at a chosen point in the stream.
type scriptReader struct {
	data []byte
	off  int
	tail error
}

func (s *scriptReader) Read(p []byte) (int, error) {
	if s.off < len(s.data) {
		n := copy(p, s.data[s.off:])
		s.off += n
		return n, nil
	}
	return 0, s.tail
}

// collectLine is one logical line the reader surfaced.
type collectLine struct {
	bytes     string
	truncated bool
	original  int
	retained  int
}

// drain runs Scan to completion, returning the lines and the terminal Err().
func drain(r *TraceLineReader) ([]collectLine, error) {
	var out []collectLine
	for r.Scan() {
		out = append(out, collectLine{
			bytes:     string(r.Bytes()),
			truncated: r.Truncated(),
			original:  r.OriginalBytes(),
			retained:  r.RetainedBytes(),
		})
	}
	return out, r.Err()
}

func TestTraceLineReader_Normal(t *testing.T) {
	r := NewTraceLineReader(strings.NewReader("line1\nline22\nline333\n"), 0)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []collectLine{
		{"line1", false, 5, 5},
		{"line22", false, 6, 6},
		{"line333", false, 7, 7},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
}

func TestTraceLineReader_ExactlyMaxNotTruncated(t *testing.T) {
	const max = 8
	r := NewTraceLineReader(strings.NewReader(strings.Repeat("a", max)+"\n"), max)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if l.truncated {
		t.Errorf("exactly-max line marked truncated: %+v", l)
	}
	if l.original != max || l.retained != max || len(l.bytes) != max {
		t.Errorf("exactly-max accounting = %+v, want original=retained=len=%d", l, max)
	}
}

func TestTraceLineReader_MaxPlusOneTruncated(t *testing.T) {
	const max = 8
	r := NewTraceLineReader(strings.NewReader(strings.Repeat("a", max+1)+"\n"), max)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if !l.truncated {
		t.Errorf("max+1 line not marked truncated: %+v", l)
	}
	if l.retained != max || l.original != max+1 {
		t.Errorf("max+1 accounting = %+v, want retained=%d original=%d", l, max, max+1)
	}
}

func TestTraceLineReader_CRLFWithinFragment(t *testing.T) {
	// '\r\n' wholly within one fragment: the '\r' is a terminator, excluded
	// from Bytes() and from OriginalBytes.
	r := NewTraceLineReader(strings.NewReader("abc\r\n"), 0)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 1 || lines[0].bytes != "abc" || lines[0].original != 3 {
		t.Fatalf("CRLF-within = %+v, want bytes=abc original=3", lines)
	}
}

func TestTraceLineReader_CRLFAtFragmentBoundary(t *testing.T) {
	// bufSize 16, content 15 chars + '\r' fills the first fragment exactly, so
	// ReadSlice returns an ErrBufferFull fragment ending in '\r' and the '\n'
	// opens the next fragment. Must equal the within-fragment CRLF result: the
	// '\r' excluded from content AND from OriginalBytes.
	content := strings.Repeat("a", 15)
	r := newTraceLineReader(strings.NewReader(content+"\r\n"), 0, 16)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if l.bytes != content || l.original != 15 || l.truncated {
		t.Fatalf("CRLF-at-boundary = %+v, want bytes=%q original=15 truncated=false", l, content)
	}
}

func TestTraceLineReader_CarriageReturnAtBoundaryIsContent(t *testing.T) {
	// A '\r' at a fragment boundary NOT followed by '\n' is genuine content,
	// retained and counted — the pending-CR rule must not eat real CRs.
	content := strings.Repeat("a", 15) // fills the 16-byte fragment as 15a + '\r'
	r := newTraceLineReader(strings.NewReader(content+"\rXYZ\n"), 0, 16)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := content + "\rXYZ"
	if len(lines) != 1 || lines[0].bytes != want || lines[0].original != len(want) {
		t.Fatalf("CR-as-content = %+v, want bytes=%q original=%d", lines, want, len(want))
	}
}

func TestTraceLineReader_TruncatedLineResyncsOnNextLine(t *testing.T) {
	const max = 8
	r := NewTraceLineReader(strings.NewReader(strings.Repeat("x", 40)+"\nshort\n"), max)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	if !lines[0].truncated || lines[0].retained != max || lines[0].original != 40 {
		t.Errorf("over-cap line = %+v, want truncated retained=%d original=40", lines[0], max)
	}
	if lines[1].truncated || lines[1].bytes != "short" {
		t.Errorf("resync line = %+v, want bytes=short not truncated", lines[1])
	}
}

func TestTraceLineReader_ConsecutiveOverLongLines(t *testing.T) {
	const max = 8
	r := NewTraceLineReader(strings.NewReader(strings.Repeat("x", 40)+"\n"+strings.Repeat("y", 30)+"\n"), max)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(lines), lines)
	}
	for i, want := range []int{40, 30} {
		if !lines[i].truncated || lines[i].retained != max || lines[i].original != want {
			t.Errorf("line %d = %+v, want truncated retained=%d original=%d", i, lines[i], max, want)
		}
	}
}

func TestTraceLineReader_FinalLineWithoutTerminator(t *testing.T) {
	r := NewTraceLineReader(strings.NewReader("a\nbc"), 0)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []collectLine{{"a", false, 1, 1}, {"bc", false, 2, 2}}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("unterminated final = %+v, want %+v", lines, want)
	}
}

func TestTraceLineReader_EmptyLines(t *testing.T) {
	// Blank lines and a stream that is a single '\n' each yield an empty line.
	r := NewTraceLineReader(strings.NewReader("\n\n"), 0)
	lines, err := drain(r)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(lines) != 2 || lines[0].bytes != "" || lines[1].bytes != "" {
		t.Fatalf("blank lines = %+v, want two empty", lines)
	}
	r2 := NewTraceLineReader(strings.NewReader("\n"), 0)
	lines2, _ := drain(r2)
	if len(lines2) != 1 || lines2[0].bytes != "" || lines2[0].original != 0 {
		t.Fatalf("single newline = %+v, want one empty line", lines2)
	}
}

func TestTraceLineReader_NonEOFErrorSurfaces(t *testing.T) {
	boom := errors.New("injected non-EOF read error")
	r := NewTraceLineReader(&scriptReader{data: []byte("good\n"), tail: boom}, 0)
	lines, err := drain(r)
	if !errors.Is(err, boom) {
		t.Fatalf("Err = %v, want %v", err, boom)
	}
	if len(lines) != 1 || lines[0].bytes != "good" || lines[0].truncated {
		t.Fatalf("pre-error line = %+v, want one clean line 'good'", lines)
	}
}

func TestTraceLineReader_NonEOFErrorMidPartialLine(t *testing.T) {
	boom := errors.New("injected mid-partial error")
	r := NewTraceLineReader(&scriptReader{data: []byte("partial"), tail: boom}, 0)
	lines, err := drain(r)
	if !errors.Is(err, boom) {
		t.Fatalf("Err = %v, want %v", err, boom)
	}
	// The partial line is not surfaced as a complete line; the error wins.
	if len(lines) != 0 {
		t.Fatalf("mid-partial lines = %+v, want none", lines)
	}
}

func TestTraceLineReader_RetainedMemoryBounded(t *testing.T) {
	const max = 1024
	huge := strings.Repeat("z", 16<<20) // 16 MiB single line, no terminator
	r := NewTraceLineReader(strings.NewReader(huge), max)
	if !r.Scan() {
		t.Fatal("Scan returned false on a huge line")
	}
	if r.RetainedBytes() != max || !r.Truncated() || r.OriginalBytes() != len(huge) {
		t.Fatalf("bounded = retained %d truncated %v original %d, want retained=%d original=%d",
			r.RetainedBytes(), r.Truncated(), r.OriginalBytes(), max, len(huge))
	}
	if r.Scan() {
		t.Fatal("Scan returned true after the single huge line")
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err after EOF = %v, want nil", err)
	}
}
