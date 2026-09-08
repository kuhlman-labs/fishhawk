package conflictresolve

import "testing"

func TestClassifyMarkerLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want MarkerKind
	}{
		{"ours bare", "<<<<<<<", MarkerOurs},
		{"ours labelled", "<<<<<<< HEAD", MarkerOurs},
		{"ours trailing space only", "<<<<<<< ", MarkerOurs},
		{"base bare", "|||||||", MarkerBase},
		{"base labelled", "||||||| merged common ancestors", MarkerBase},
		{"separator bare", "=======", MarkerSep},
		{"separator labelled", "======= label", MarkerSep},
		{"theirs bare", ">>>>>>>", MarkerTheirs},
		{"theirs labelled", ">>>>>>> origin/main", MarkerTheirs},
		{"ours with CR", "<<<<<<< HEAD\r", MarkerOurs},
		{"separator with CR", "=======\r", MarkerSep},

		{"eight ours is content", "<<<<<<<<", MarkerNone},
		{"eight separator is content", "========", MarkerNone},
		{"eight theirs is content", ">>>>>>>>", MarkerNone},
		{"six is content", "<<<<<<", MarkerNone},
		{"indented is content", " <<<<<<< HEAD", MarkerNone},
		{"tab indented is content", "\t=======", MarkerNone},
		{"label without space is content", "<<<<<<<HEAD", MarkerNone},
		{"mixed run is content", "<<<<<<= HEAD", MarkerNone},
		{"empty is content", "", MarkerNone},
		{"unrelated is content", "func main() {", MarkerNone},
		{"other punctuation run is content", "-------", MarkerNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyMarkerLine([]byte(tc.line)); got != tc.want {
				t.Fatalf("ClassifyMarkerLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestContainsMarkerLine(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"clean body", "alpha\nbeta\n", false},
		{"marker on first line", "<<<<<<< HEAD\nalpha\n", true},
		{"marker on last line without trailing newline", "alpha\n>>>>>>> theirs", true},
		{"marker mid body", "alpha\n=======\nbeta\n", true},
		{"diff3 base marker", "alpha\n||||||| base\nbeta\n", true},
		{"marker-looking prefix is content", "alpha\n========\nbeta\n", false},
		{"indented marker is content", "alpha\n  =======\nbeta\n", false},
		{"empty body", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainsMarkerLine([]byte(tc.body)); got != tc.want {
				t.Fatalf("ContainsMarkerLine(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
