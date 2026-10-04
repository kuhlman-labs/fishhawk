package server

// Cross-boundary decode of the #3774 work-item payloads: the REAL handler's
// bytes, carried across the server -> mcpserver boundary through the MCP
// package's LOCAL decode-only mirrors (ADR-064 keeps workmgmt/intakegroom out
// of mcpserver, so the mirrors agree with the server only by convention).
// DisallowUnknownFields fails on a key the mirror lacks or names under a
// different tag; the round-trip equality below additionally fails on a value
// the mirror re-encodes differently (an omitempty the server does not carry,
// e.g. dropping an explicit in_window:false).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// assertMirrorRoundTrips decodes raw into out with DisallowUnknownFields,
// re-encodes out, and requires the re-encoding to equal raw as a JSON value.
// A key the mirror lacks (or names under a different tag) fails the decode; a
// value it re-encodes differently fails the equality.
func assertMirrorRoundTrips(t *testing.T, raw []byte, out any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("the handler's bytes do not decode through the MCP mirror: %v\nbody=%s", err, raw)
	}
	again, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("re-encode mirror: %v", err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode handler bytes: %v", err)
	}
	if err := json.Unmarshal(again, &got); err != nil {
		t.Fatalf("decode re-encoded mirror: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("the MCP mirror does not round-trip the handler payload:\n--- handler\n%s\n--- mirror\n%s", raw, again)
	}
}

// TestWorkItemPreview_MCPDecodesRealPreviewPayload (a): the preview handler's
// real 200 decodes through mcpserver.WorkItemPreview with no field lost —
// title, body, intake.duplicates and intake.derives_from included. The adr
// subcase makes the number key present, so its mirror tag is exercised too.
func TestWorkItemPreview_MCPDecodesRealPreviewPayload(t *testing.T) {
	cases := map[string]struct {
		items      []workmgmt.WorkItemRecord
		draft      workItemRequest
		wantNumber int
	}{
		"source refs": {
			items: []workmgmt.WorkItemRecord{
				{Number: 1234, Title: "[E22.4] Add the widget endpoint", Body: "the source item's own body", URL: "https://example.test/1234", Labels: []string{"type:chore", "area:backend"}},
				{Number: 1240, Title: "Widget endpoint pagination", URL: "https://example.test/1240", Labels: []string{"type:chore"}},
				{Number: 22, Title: "[E22] Widget platform", URL: "https://example.test/22", Labels: []string{"epic", "area:backend"}},
			},
			draft: func() workItemRequest {
				d := pvStandardDraft()
				d.Body = "A draft written from #1234."
				d.SourceRefs = []string{"#1234", "#4242"}
				d.Relations = &workItemRelations{DependsOn: []string{"#41"}}
				return d
			}(),
		},
		"numbered adr": {
			items:      pvHealthyItems(),
			draft:      workItemRequest{Repo: "kuhlman-labs/fishhawk", Type: "adr", Summary: "Add the widget endpoint"},
			wantNumber: 80,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := pvRegisterRead(t, tc.items)
			p.discovered = []int{79}
			igInstallCharterConventions(t)
			s := New(igCharterConfig(igCharterDoc, false))

			rec := previewWorkItemRec(t, s, tc.draft, pvOperator())
			if rec.Code != http.StatusOK {
				t.Fatalf("preview status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			var pv mcpserver.WorkItemPreview
			assertMirrorRoundTrips(t, rec.Body.Bytes(), &pv)

			srv := decodeWorkItemPreview(t, rec)
			if pv.Title == "" || pv.Title != srv.Title {
				t.Errorf("MCP-decoded title = %q, handler title = %q", pv.Title, srv.Title)
			}
			if pv.Body == "" || pv.Body != srv.Body {
				t.Errorf("MCP-decoded body differs from the handler's")
			}
			if pv.Number != tc.wantNumber {
				t.Errorf("MCP-decoded number = %d, want %d", pv.Number, tc.wantNumber)
			}
			if len(pv.Intake.Duplicates) == 0 {
				t.Fatalf("MCP-decoded intake.duplicates is empty; the fixture must produce one (body=%s)", rec.Body.String())
			}
			if name != "source refs" {
				return
			}
			if pv.Intake.Duplicates[0].Number != 1240 || pv.Intake.Duplicates[0].Confidence == "" {
				t.Errorf("MCP-decoded duplicates = %+v, want #1240 with a confidence band", pv.Intake.Duplicates)
			}
			want := []mcpserver.IntakeSourceItem{
				{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "https://example.test/1234", InWindow: true},
				{Number: 4242},
			}
			if !reflect.DeepEqual(pv.Intake.DerivesFrom, want) {
				t.Errorf("MCP-decoded derives_from = %+v, want %+v", pv.Intake.DerivesFrom, want)
			}
			if pv.Relations == nil || len(pv.Relations.DependsOn) != 1 || pv.Relations.DependsOn[0] != "#41" {
				t.Errorf("MCP-decoded relations = %+v, want depends_on [#41]", pv.Relations)
			}
		})
	}
}

// TestFileWorkItem_MCPDecodesSourceRefsIntake (b): a filing carrying
// source_refs through the REAL handler produces a 201 whose intake (with
// derives_from) decodes through mcpserver.FiledWorkItem. The source is #9, so
// the standard window's #1234 stays the top duplicate the shared
// assertMCPDecodesIntake helper requires; #4242 is outside the window.
func TestFileWorkItem_MCPDecodesSourceRefsIntake(t *testing.T) {
	p := pvRegisterRead(t, pvHealthyItems())
	p.guard.fileOK.Store(true)
	igInstallCharterConventions(t)
	s := New(igCharterConfig(igCharterDoc, false))

	draft := pvStandardDraft()
	draft.SourceRefs = []string{"#9", "4242"}
	rec := fileWorkItem(t, s, draft, "github:operator")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	assertMCPDecodesIntake(t, rec.Body.Bytes())

	var filed mcpserver.FiledWorkItem
	assertMirrorRoundTrips(t, rec.Body.Bytes(), &filed)
	if filed.Intake == nil {
		t.Fatal("MCP decode dropped the intake object")
	}
	want := []mcpserver.IntakeSourceItem{
		{Number: 9, Title: "Something entirely unrelated about invoices", URL: "https://example.test/9", InWindow: true},
		{Number: 4242},
	}
	if !reflect.DeepEqual(filed.Intake.DerivesFrom, want) {
		t.Errorf("MCP-decoded derives_from = %+v, want %+v", filed.Intake.DerivesFrom, want)
	}
	for _, d := range filed.Intake.Duplicates {
		if d.Number == 9 {
			t.Errorf("the declared source #9 was reported as a duplicate: %+v", filed.Intake.Duplicates)
		}
	}
}
