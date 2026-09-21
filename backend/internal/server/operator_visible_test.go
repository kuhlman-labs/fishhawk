package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// operatorVisibleSeam is the minimal Server + JSON log buffer + notifier
// recorder the notifyOperatorVisible runtime-defence tests drive (built like
// newCancelDropSeam). withNotifier=false leaves s.issueNotifier nil.
type operatorVisibleSeam struct {
	s      *Server
	rec    *pageClassRecorder
	logBuf *bytes.Buffer
	runID  uuid.UUID
}

func newOperatorVisibleSeam(t *testing.T, withNotifier bool) *operatorVisibleSeam {
	t.Helper()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newOrchestratorRepo(), AuditRepo: newAuditFake(), APITokenRepo: stubToken("write:runs")})
	seam := &operatorVisibleSeam{s: s, logBuf: &bytes.Buffer{}, runID: uuid.New()}
	if withNotifier {
		seam.rec = &pageClassRecorder{}
		s.issueNotifier = seam.rec
	}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(seam.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return seam
}

// errorLines returns the decoded level=ERROR JSON log lines.
func (o *operatorVisibleSeam) errorLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(o.logBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if m["level"] == "ERROR" {
			out = append(out, m)
		}
	}
	return out
}

// TestNotifyOperatorVisible_UnrenderableCategoryLogsErrorAndStillRefreshes
// pins the runtime defence: a category absent from
// issuecomment.activityCategories logs at ERROR naming the category and the
// run, and the status refresh STILL fires (the helper delegates
// unconditionally, so the refresh behaviour is unchanged from
// notifyStatusUpdate).
func TestNotifyOperatorVisible_UnrenderableCategoryLogsErrorAndStillRefreshes(t *testing.T) {
	seam := newOperatorVisibleSeam(t, true)
	const category = "not_a_rendered_category"
	seam.s.notifyOperatorVisible(context.Background(), seam.runID, category)

	errs := seam.errorLines(t)
	if len(errs) != 1 {
		t.Fatalf("ERROR log lines = %d, want exactly 1:\n%s", len(errs), seam.logBuf.String())
	}
	if errs[0]["category"] != category {
		t.Errorf("ERROR line category = %v, want %q", errs[0]["category"], category)
	}
	if errs[0]["run_id"] != seam.runID.String() {
		t.Errorf("ERROR line run_id = %v, want %s", errs[0]["run_id"], seam.runID)
	}
	if msg, _ := errs[0]["msg"].(string); !strings.Contains(msg, "activityCategories") {
		t.Errorf("ERROR line msg = %q, want it to name issuecomment.activityCategories as the file to edit", msg)
	}
	if got := len(seam.rec.status); got != 1 {
		t.Errorf("NotifyStatusUpdateForRun calls = %d, want exactly 1 (the refresh must still fire)", got)
	}
}

// TestNotifyOperatorVisible_RenderableCategoryIsSilent pins the happy path:
// a registered category logs nothing at ERROR and refreshes exactly once.
func TestNotifyOperatorVisible_RenderableCategoryIsSilent(t *testing.T) {
	seam := newOperatorVisibleSeam(t, true)
	seam.s.notifyOperatorVisible(context.Background(), seam.runID, "fixup_pushed")

	if errs := seam.errorLines(t); len(errs) != 0 {
		t.Errorf("ERROR log lines = %d, want 0 for a renderable category:\n%s", len(errs), seam.logBuf.String())
	}
	if got := len(seam.rec.status); got != 1 {
		t.Errorf("NotifyStatusUpdateForRun calls = %d, want exactly 1", got)
	}
}

// TestNotifyOperatorVisible_NilNotifierStillLogsError pins that the intent
// check is independent of notifier presence: with s.issueNotifier nil (no
// refresh possible) the unrenderable category is still an ERROR, because the
// writer/renderer mismatch is a defect whether or not a notifier is
// configured.
func TestNotifyOperatorVisible_NilNotifierStillLogsError(t *testing.T) {
	seam := newOperatorVisibleSeam(t, false)
	const category = "not_a_rendered_category"
	seam.s.notifyOperatorVisible(context.Background(), seam.runID, category)

	errs := seam.errorLines(t)
	if len(errs) != 1 {
		t.Fatalf("ERROR log lines = %d, want exactly 1 with a nil notifier:\n%s", len(errs), seam.logBuf.String())
	}
	if errs[0]["category"] != category {
		t.Errorf("ERROR line category = %v, want %q", errs[0]["category"], category)
	}
}
