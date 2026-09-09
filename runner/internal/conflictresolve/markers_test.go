package conflictresolve

import (
	"reflect"
	"testing"
)

func TestClassifyMarkerLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want MarkerKind
	}{
		{"ours bare", "<<<<<<<", MarkerOurs},
		{"ours labelled", "<<<<<<< HEAD", MarkerOurs},
		{"separator bare", "=======", MarkerSeparator},
		{"separator labelled", "======= label", MarkerSeparator},
		{"theirs labelled", ">>>>>>> origin/main", MarkerTheirs},
		{"theirs bare", ">>>>>>>", MarkerTheirs},
		{"diff3 base bare", "|||||||", MarkerBase},
		{"diff3 base labelled", "||||||| merged common ancestors", MarkerBase},
		{"crlf terminated marker", "<<<<<<< HEAD\r", MarkerOurs},
		{"crlf terminated bare marker", "=======\r", MarkerSeparator},
		{"eight character run", "========", MarkerNone},
		{"eight character run with label", "======== label", MarkerNone},
		{"eight character ours run", "<<<<<<<< HEAD", MarkerNone},
		{"six character run", "<<<<<< HEAD", MarkerNone},
		{"indented run", " =======", MarkerNone},
		{"tab indented run", "\t<<<<<<< HEAD", MarkerNone},
		{"mixed run", "<<<<<<= HEAD", MarkerNone},
		{"mixed run trailing", "======<= x", MarkerNone},
		{"label without space separator", "=======label", MarkerNone},
		{"empty line", "", MarkerNone},
		{"ordinary content", "func main() {", MarkerNone},
		{"short line", "<<<", MarkerNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyMarkerLine([]byte(tc.line)); got != tc.want {
				t.Errorf("ClassifyMarkerLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestContainsMarkerLine(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"clean file", "alpha\nbeta\n", false},
		{"opening marker", "alpha\n<<<<<<< HEAD\n", true},
		{"marker on unterminated final line", "alpha\n=======", true},
		{"eight character run only", "alpha\n========\n", false},
		{"empty", "", false},
		{"marker mid file", "a\n>>>>>>> base\nb\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainsMarkerLine([]byte(tc.body)); got != tc.want {
				t.Errorf("ContainsMarkerLine(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestEachLine(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []lineSpan
	}{
		{"empty", "", nil},
		{"single terminated line", "ab\n", []lineSpan{{0, 2, 3}}},
		{"unterminated final line", "ab\ncd", []lineSpan{{0, 2, 3}, {3, 5, 5}}},
		{"blank line in the middle", "a\n\nb\n", []lineSpan{{0, 1, 2}, {2, 2, 3}, {3, 4, 5}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []lineSpan
			eachLine([]byte(tc.body), func(sp lineSpan) bool {
				got = append(got, sp)
				return true
			})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("eachLine(%q) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestEachLineStopsEarly(t *testing.T) {
	n := 0
	eachLine([]byte("a\nb\nc\n"), func(lineSpan) bool {
		n++
		return n < 2
	})
	if n != 2 {
		t.Errorf("eachLine visited %d lines, want 2 (early stop)", n)
	}
}

func TestMarkerLineSpansCoverTheTerminatingNewline(t *testing.T) {
	// The span deliberately runs to Next, not End: a replacement region that
	// supplies a marker line's newline must still intersect it.
	body := []byte("a\n=======\nb\n")
	got := markerLineSpans(body)
	want := []lineSpan{{2, 9, 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("markerLineSpans = %+v, want %+v", got, want)
	}
}
