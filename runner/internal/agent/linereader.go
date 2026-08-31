package agent

import (
	"bufio"
	"io"
)

// MaxTraceLineBytes is the default retained-content cap for a single trace
// line (4 MiB, ~4x the prior bufio.Scanner limit). A physical line whose
// CONTENT exceeds this is truncated to the first MaxTraceLineBytes bytes and
// the rest discarded, rather than aborting the whole read the way
// bufio.Scanner's ErrTooLong did (#3020). Truncation is bounded: the retained
// buffer never exceeds this, whatever the physical line length.
const MaxTraceLineBytes = 4 << 20

// defaultWorkBufBytes is the working buffer the exported constructor hands to
// bufio.Reader. It bounds the ReadSlice fragment size, NOT the retained line:
// a physical line longer than this is reassembled across fragments up to the
// cap. Tests use newTraceLineReader to choose a small size and place fragment
// boundaries deterministically.
const defaultWorkBufBytes = 64 << 10

// minWorkBufBytes mirrors bufio's own minimum working-buffer floor (16), so a
// test asking for a small fragment size gets the size it can actually rely on
// rather than bufio silently clamping under it.
const minWorkBufBytes = 16

// TraceLineReader reads a child agent's newline-delimited trace stream one
// logical line at a time, replacing bufio.Scanner in both adapters (#3020).
//
// It differs from bufio.Scanner in the one way that matters: a line whose
// CONTENT exceeds the cap is TRUNCATED — the first max bytes are retained,
// the rest of the physical line is discarded, and reading CONTINUES on the
// next line — where Scanner would return bufio.ErrTooLong and stop the whole
// stream. An over-long line therefore costs one log line, not the pass.
//
// BYTE ACCOUNTING is content-only: neither the terminating '\n' nor a '\r'
// that pairs with it (a CRLF terminator) is counted as content or retained.
// OriginalBytes is the full content length of the physical line INCLUDING
// bytes discarded past the cap; RetainedBytes == len(Bytes()) == min(Original,
// max). This holds whether a CRLF terminator lands within one ReadSlice
// fragment or is split across a fragment boundary (the pending-CR rule below).
//
// It is NOT safe for concurrent use; each adapter drives one reader on one
// goroutine.
type TraceLineReader struct {
	br  *bufio.Reader
	max int

	buf       []byte // retained content of the current line, capped at max
	original  int    // full content length of the current line (uncapped)
	truncated bool   // current line exceeded max
	err       error  // genuine non-EOF read error, surfaced via Err()
	done      bool   // stream exhausted or a terminal error was reached
}

// NewTraceLineReader returns a reader over r with the given retained-content
// cap. A max <= 0 selects MaxTraceLineBytes, so an adapter that leaves its cap
// seam at the zero value transparently gets the production cap. The working
// buffer is a fixed 64 KiB; only tests need a different fragment size, via the
// unexported newTraceLineReader.
func NewTraceLineReader(r io.Reader, max int) *TraceLineReader {
	return newTraceLineReader(r, max, defaultWorkBufBytes)
}

// newTraceLineReader backs the exported constructor and lets in-package tests
// choose the bufio working-buffer size, which is what places ReadSlice
// fragment boundaries at chosen offsets (the pending-CR boundary cases).
func newTraceLineReader(r io.Reader, max, bufSize int) *TraceLineReader {
	if max <= 0 {
		max = MaxTraceLineBytes
	}
	if bufSize < minWorkBufBytes {
		bufSize = minWorkBufBytes
	}
	return &TraceLineReader{br: bufio.NewReaderSize(r, bufSize), max: max}
}

// Scan advances to the next line, returning false at end of stream or on a
// genuine non-EOF read error (distinguish the two with Err). After a true
// return, Bytes/Truncated/OriginalBytes/RetainedBytes describe the line.
func (r *TraceLineReader) Scan() bool {
	if r.done {
		return false
	}
	r.buf = r.buf[:0]
	r.original = 0
	r.truncated = false

	// pendingCR holds a '\r' seen at the end of a non-terminating fragment: it
	// is a CRLF terminator iff the very next byte is '\n', which we cannot know
	// until the next fragment arrives. Line-local: a line always terminates
	// within one Scan call, so it never carries across calls.
	pendingCR := false
	read := false // did this Scan observe any fragment at all?

	for {
		frag, err := r.br.ReadSlice('\n')
		if len(frag) > 0 {
			read = true
		}

		// Resolve a '\r' held from the previous fragment before touching frag.
		// It terminates the line iff frag begins with '\n' (a delimiter frag is
		// exactly "\n" when its pre-'\n' content is empty); otherwise it was
		// genuine content. When it IS a terminator, the '\r' is simply dropped
		// from both content and OriginalBytes.
		if pendingCR {
			terminator := len(frag) > 0 && frag[0] == '\n'
			if !terminator {
				r.appendContent(crByte)
			}
			pendingCR = false
		}

		switch err {
		case nil:
			// Delimiter found. Drop the '\n'; then drop a trailing '\r' that
			// pairs with it (CRLF terminator wholly within this fragment).
			content := frag[:len(frag)-1]
			if len(content) > 0 && content[len(content)-1] == '\r' {
				content = content[:len(content)-1]
			}
			r.appendContent(content)
			return true

		case bufio.ErrBufferFull:
			// No delimiter yet; more of this physical line follows. Hold a
			// trailing '\r' at the boundary rather than counting it — its role
			// is decided by the next fragment.
			if len(frag) > 0 && frag[len(frag)-1] == '\r' {
				pendingCR = true
				frag = frag[:len(frag)-1]
			}
			r.appendContent(frag)
			continue

		case io.EOF:
			// Final unterminated line. A trailing '\r' at true EOF is content
			// (no '\n' follows it), so frag is appended verbatim.
			r.appendContent(frag)
			r.done = true
			// Nothing at all this Scan -> end of stream.
			if !read && r.original == 0 {
				return false
			}
			return true

		default:
			// Genuine non-EOF read error. Surface it via Err(); any partial
			// content already appended is not returned as a line.
			r.appendContent(frag)
			r.err = err
			r.done = true
			return false
		}
	}
}

// crByte is a one-element slice reused for appending a resolved pending '\r'
// as content, so the append routes through the same cap accounting as any
// other byte.
var crByte = []byte{'\r'}

// appendContent adds b to the current line's content, always counting it into
// OriginalBytes and retaining as much as the cap allows. Once the cap is hit
// the line is marked truncated and further bytes are counted but discarded, so
// the retained buffer never exceeds max (bounded memory).
func (r *TraceLineReader) appendContent(b []byte) {
	if len(b) == 0 {
		return
	}
	r.original += len(b)
	if r.truncated {
		return
	}
	room := r.max - len(r.buf)
	if len(b) <= room {
		r.buf = append(r.buf, b...)
		return
	}
	r.buf = append(r.buf, b[:room]...)
	r.truncated = true
}

// Bytes returns the retained content of the current line (terminator excluded),
// valid until the next Scan. Truncated when Truncated() is true.
func (r *TraceLineReader) Bytes() []byte { return r.buf }

// Truncated reports whether the current line's content exceeded the cap and
// was truncated. When true, Bytes() is a PREFIX of the physical line and MUST
// NOT be handed to a parser — the caller records the truncation and skips the
// line (#3020).
func (r *TraceLineReader) Truncated() bool { return r.truncated }

// OriginalBytes is the full content length of the current physical line,
// including bytes discarded past the cap. Terminator bytes are excluded.
func (r *TraceLineReader) OriginalBytes() int { return r.original }

// RetainedBytes is len(Bytes()) — min(OriginalBytes, max).
func (r *TraceLineReader) RetainedBytes() int { return len(r.buf) }

// Err returns the genuine non-EOF read error that stopped the stream, or nil.
// A clean io.EOF terminates the loop with Err() == nil.
func (r *TraceLineReader) Err() error { return r.err }
