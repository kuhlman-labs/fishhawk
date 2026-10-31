package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// labelConcernStub answers POST /v0/concerns/{concern_id}/label with
// status/body, recording each request line and body.
type labelConcernStub struct {
	requests []string
	bodies   []string
	status   int
	body     string
}

func (b *labelConcernStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

// TestLabelConcern_JudgementBody: a judgement label POSTs the concern-scoped
// route with the judgement and cost verbatim, OMITS the blank duplicate_of
// key, always sends backfill, and decodes the backend's 200 body.
func TestLabelConcern_JudgementBody(t *testing.T) {
	id := uuid.New()
	runID, stageID := uuid.NewString(), uuid.NewString()
	stub := &labelConcernStub{body: `{"id":"` + id.String() + `","run_id":"` + runID + `","stage_id":"` + stageID +
		`","stage_kind":"implement","state":"waived","outcome":"waived","judgement":"tradeoff",` +
		`"judgement_actor_kind":"user","judgement_backfill":true,"tradeoff_cost":"slower path"}`}
	r := stub.resolver(t)

	_, out, err := r.labelConcern(context.Background(), nil, LabelConcernInput{
		ConcernID: id.String(), Judgement: "tradeoff", TradeoffCost: "slower path", Backfill: true,
	})
	if err != nil {
		t.Fatalf("labelConcern: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /v0/concerns/"+id.String()+"/label" {
		t.Fatalf("requests = %v, want one POST to the label route", stub.requests)
	}
	// Exact bytes: the omitempty keys absent, backfill present.
	if want := `{"judgement":"tradeoff","tradeoff_cost":"slower path","backfill":true}`; stub.bodies[0] != want {
		t.Errorf("sent body = %s, want %s", stub.bodies[0], want)
	}
	want := LabelledConcern{
		ID: id.String(), RunID: runID, StageID: stageID, StageKind: "implement", State: "waived",
		Outcome: "waived", Judgement: "tradeoff", JudgementActorKind: "user", JudgementBackfill: true, TradeoffCost: "slower path",
	}
	if out.Concern != want {
		t.Errorf("decoded concern = %+v, want %+v", out.Concern, want)
	}
}

// TestLabelConcern_DuplicateBody: a duplicate label sends ONLY duplicate_of
// (plus backfill:false), and the inherited judgement fields decode.
func TestLabelConcern_DuplicateBody(t *testing.T) {
	id, target := uuid.New(), uuid.NewString()
	stub := &labelConcernStub{body: `{"id":"` + id.String() + `","state":"raised","outcome":"duplicate_of:` + target +
		`","judgement":"defect","judgement_inherited_from":"` + target + `","duplicate_of":"` + target + `","judgement_actor_kind":"user"}`}
	r := stub.resolver(t)

	_, out, err := r.labelConcern(context.Background(), nil, LabelConcernInput{ConcernID: id.String(), DuplicateOf: target})
	if err != nil {
		t.Fatalf("labelConcern: %v", err)
	}
	if want := `{"duplicate_of":"` + target + `","backfill":false}`; len(stub.bodies) != 1 || stub.bodies[0] != want {
		t.Errorf("sent bodies = %v, want exactly %s", stub.bodies, want)
	}
	c := out.Concern
	if c.Judgement != "defect" || c.JudgementInheritedFrom != target || c.DuplicateOf != target || c.Outcome != "duplicate_of:"+target {
		t.Errorf("decoded concern = %+v, want the inherited defect from %s", c, target)
	}
}

// TestLabelConcern_NoLocalRulePreCheck: the tool refuses none of the
// judgement or exclusivity rules itself. A request carrying BOTH judgement and
// duplicate_of reaches the backend with both keys, and the backend's 400 is
// what the caller sees — the backend is the single refusal authority.
func TestLabelConcern_NoLocalRulePreCheck(t *testing.T) {
	stub := &labelConcernStub{
		status: http.StatusBadRequest,
		body:   `{"error":{"code":"validation_failed","message":"duplicate_of and judgement are exclusive","details":{"field":"duplicate_of","rule":"duplicate_of_excludes_judgement"}}}`,
	}
	r := stub.resolver(t)
	target := uuid.NewString()
	_, _, err := r.labelConcern(context.Background(), nil, LabelConcernInput{
		ConcernID: uuid.NewString(), Judgement: "defect", DuplicateOf: target,
	})
	if err == nil || !strings.Contains(err.Error(), "validation_failed") || !strings.Contains(err.Error(), "duplicate_of_excludes_judgement") {
		t.Errorf("err = %v, want the backend's validation_failed rule surfaced", err)
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("backend requests = %d, want 1 (no local pre-check)", len(stub.bodies))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(stub.bodies[0]), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["judgement"] != "defect" || sent["duplicate_of"] != target {
		t.Errorf("sent body = %v, want both judgement and duplicate_of passed through", sent)
	}
}

// TestLabelConcern_LocalValidation: a malformed concern_id or duplicate_of
// fails before the HTTP hop — the backend sees zero requests.
func TestLabelConcern_LocalValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   LabelConcernInput
		want string
	}{
		{"bad concern id", LabelConcernInput{ConcernID: "nope", Judgement: "defect"}, `concern_id "nope"`},
		{"empty concern id", LabelConcernInput{Judgement: "defect"}, `concern_id ""`},
		{"bad duplicate_of", LabelConcernInput{ConcernID: uuid.NewString(), DuplicateOf: "not-a-uuid"}, `duplicate_of "not-a-uuid"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &labelConcernStub{body: `{}`}
			r := stub.resolver(t)
			_, _, err := r.labelConcern(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(stub.requests) != 0 {
				t.Errorf("backend requests = %v, want 0 (local validation short-circuits)", stub.requests)
			}
		})
	}
}

// TestLabelConcern_BackendRefusalsSurfaced: each backend refusal surfaces as a
// tool error carrying its code.
func TestLabelConcern_BackendRefusalsSurfaced(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "validation_failed"},
		{http.StatusForbidden, "label_requires_human"},
		{http.StatusNotFound, "concern_not_found"},
		{http.StatusConflict, "concern_label_conflict"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			stub := &labelConcernStub{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
			r := stub.resolver(t)
			_, _, err := r.labelConcern(context.Background(), nil, LabelConcernInput{ConcernID: uuid.NewString(), Judgement: "defect"})
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Errorf("err = %v, want the backend refusal code %q surfaced", err, tc.code)
			}
		})
	}
}
