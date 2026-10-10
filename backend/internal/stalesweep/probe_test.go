package stalesweep

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestParseRunnerProcesses pins the scripts/dev _parse_live_runs /
// runner lockholder.go identity rules, including C2: the single-token
// `--run-id=<id>` form is UNATTRIBUTED (neither parser recognises it), so
// --apply fails closed on it.
func TestParseRunnerProcesses(t *testing.T) {
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for name, tc := range map[string]struct {
		line         string
		attributed   []uuid.UUID
		unattributed []string
	}{
		"adjacent --run-id pair":           {fmt.Sprintf("101 fishhawk-runner --run-id %s --stage-id x", a), []uuid.UUID{a}, nil},
		"single-token --run-id= form (C2)": {fmt.Sprintf("102 fishhawk-runner --run-id=%s", b), nil, []string{"102"}},
		"absolute path argv0":              {fmt.Sprintf("103 /Users/x/fishhawk/bin/fishhawk-runner run --run-id %s", c), []uuid.UUID{c}, nil},
		"extension stripped from basename": {fmt.Sprintf("104 C:/bin/fishhawk-runner.exe --run-id %s", d), []uuid.UUID{d}, nil},
		"mentions the binary only later":   {fmt.Sprintf("105 grep fishhawk-runner --run-id %s", a), nil, nil},
		"sleep impersonation":              {fmt.Sprintf("106 /bin/sleep fishhawk-runner --run-id %s", a), nil, nil},
		"missing run id":                   {"107 fishhawk-runner --stage-id x", nil, []string{"107"}},
		"--run-id with no value":           {"108 fishhawk-runner --run-id", nil, []string{"108"}},
		"non-UUID run id":                  {"109 fishhawk-runner --run-id abc", nil, []string{"109"}},
		"first --run-id token wins":        {fmt.Sprintf("110 fishhawk-runner --run-id nope --run-id %s", a), nil, []string{"110"}},
		"different binary prefix":          {fmt.Sprintf("111 fishhawk-runner-old --run-id %s", a), nil, nil},
		"non-numeric pid":                  {fmt.Sprintf("pid fishhawk-runner --run-id %s", a), nil, nil},
		"pid only":                         {"112", nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			rep := ParseRunnerProcesses(tc.line + "\n")
			var got, want []string
			for id := range rep.RunIDs {
				got = append(got, id.String())
			}
			for _, id := range tc.attributed {
				want = append(want, id.String())
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("RunIDs = %v, want %v", got, want)
			}
			if strings.Join(rep.UnattributedPIDs, ",") != strings.Join(tc.unattributed, ",") {
				t.Errorf("UnattributedPIDs = %v, want %v", rep.UnattributedPIDs, tc.unattributed)
			}
		})
	}
}

// TestParseRunnerProcesses_MultiLineDedup: a whole ps capture, a repeated pid
// counted once, blank lines and leading whitespace tolerated.
func TestParseRunnerProcesses_MultiLineDedup(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	raw := strings.Join([]string{
		"  1 /sbin/launchd",
		fmt.Sprintf("  201 fishhawk-runner --run-id %s", a),
		fmt.Sprintf("  201 fishhawk-runner --run-id %s", b),
		"",
		fmt.Sprintf("  202 /opt/bin/fishhawk-runner --run-id %s", b),
		"  203 fishhawk-runner",
		"  203 fishhawk-runner",
	}, "\n")
	rep := ParseRunnerProcesses(raw)
	if len(rep.RunIDs) != 2 || !rep.RunIDs[a] || !rep.RunIDs[b] {
		t.Errorf("RunIDs = %v, want {%s, %s}", rep.RunIDs, a, b)
	}
	sort.Strings(rep.UnattributedPIDs)
	if fmt.Sprint(rep.UnattributedPIDs) != "[203]" {
		t.Errorf("UnattributedPIDs = %v, want [203]", rep.UnattributedPIDs)
	}
}

func TestPSProbe_ExecError(t *testing.T) {
	p := PSProbe{Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(fmt.Sprintf("1 fishhawk-runner --run-id %s\n", uuid.New())), errors.New("exec: \"ps\": executable file not found in $PATH")
	}}
	rep, err := p.LiveRunners(context.Background())
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("err = %v, want the exec error", err)
	}
	if len(rep.RunIDs) != 0 || len(rep.UnattributedPIDs) != 0 {
		t.Errorf("report = %+v, want empty alongside the error", rep)
	}
}

func TestPSProbe_RunsPSAndParses(t *testing.T) {
	id := uuid.New()
	var gotName string
	var gotArgs []string
	p := PSProbe{Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(fmt.Sprintf("42 fishhawk-runner --run-id %s\n", id)), nil
	}}
	rep, err := p.LiveRunners(context.Background())
	if err != nil || !rep.RunIDs[id] {
		t.Fatalf("report = %+v, err %v", rep, err)
	}
	if gotName != "ps" || strings.Join(gotArgs, " ") != "-axww -o pid=,args=" {
		t.Errorf("exec = %s %v, want the scripts/dev ps invocation", gotName, gotArgs)
	}
}

// TestPSProbe_DefaultExecMissingBinary drives the real exec seam with a binary
// that cannot exist: the error surfaces, never an empty clean scan.
func TestPSProbe_DefaultExecMissingBinary(t *testing.T) {
	if _, err := execOutput(context.Background(), "fishhawk-stalesweep-no-such-binary"); err == nil {
		t.Fatal("execOutput on a missing binary returned nil error")
	}
}
