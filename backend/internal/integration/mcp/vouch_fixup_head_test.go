package mcpe2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// vouchHeadForge is the httptest GitHub the vouch probe compares against:
// compares maps "base...head" to the commit list; an unstubbed compare 404s.
// prHead is the run's live pull-request head the probe's live-head rung reads.
type vouchHeadForge struct {
	mu       sync.Mutex
	compares map[string][]string
	prHead   string
}

type vouchHeadTokens struct{}

func (vouchHeadTokens) Token(context.Context, int64) (string, error) { return "ghs_vouch", nil }

func (g *vouchHeadForge) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		commits, ok := g.compares[r.PathValue("basehead")]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		type c struct {
			SHA string `json:"sha"`
		}
		body := struct {
			Commits []c `json:"commits"`
		}{Commits: []c{}}
		for _, sha := range commits {
			body.Commits = append(body.Commits, c{SHA: sha})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		head := g.prHead
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"node_id":"PR_vouch","state":"open","head":{"sha":%q,"ref":"run/branch"},"base":{"ref":"main"}}`, head)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  vouchHeadTokens{},
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// TestE2E_VouchCommit_AdvancesFixupExpectedHead pins the #4856 seam end to
// end: the real fishhawk-mcp binary calls fishhawk_vouch_commit, the real
// handler probes the forge and writes the Postgres audit row, the real
// fishhawk_fixup_stage re-opens the stage, and the prompt-render wire walks the
// chain. A verified fast-forward vouch of V over the recorded head H (the run
// 3a390a2e shape: H is an acceptance_scenarios_pushed head) makes
// fixup_expected_head_sha V; when the forge reports V is NOT a fast-forward of
// H the vouch is recorded membership-only and the expectation stays H. This
// crosses MCP request, HTTP handler, forge probe, audit persistence, the walk
// and the prompt wire.
func TestE2E_VouchCommit_AdvancesFixupExpectedHead(t *testing.T) {
	t.Parallel()
	const (
		recordedHead = "8888888888888888888888888888888888888888"
		vouchedTip   = "9999999999999999999999999999999999999999"
		foreign      = "aaaaaaaa00000000000000000000000000000000"
	)
	tests := []struct {
		name         string
		behind       []string
		wantAdvanced bool
		wantHead     string
	}{
		{"fast-forward advances to the vouched tip", []string{}, true, vouchedTip},
		{"not a fast-forward keeps the recorded head", []string{foreign}, false, recordedHead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			forge := &vouchHeadForge{compares: map[string][]string{
				vouchedTip + "..." + recordedHead: tt.behind,
				recordedHead + "..." + vouchedTip: {vouchedTip},
			}, prHead: vouchedTip}
			auditRepo := audit.NewPostgresRepository(fx.pool)
			srv := server.New(server.Config{
				Addr:         "127.0.0.1:0",
				RunRepo:      fx.runRepo,
				AuditRepo:    auditRepo,
				SigningRepo:  signing.NewPostgresRepository(fx.pool),
				APITokenRepo: fx.apitokenRepo,
				GitHub:       forge.client(t),
			})
			httpSrv := httptest.NewServer(srv.Handler())
			t.Cleanup(httpSrv.Close)

			inst := int64(4856)
			requiresCharter := false
			runRow, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
				Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
				TriggerSource: runpkg.TriggerCLI, InstallationID: &inst, RequiresCharter: &requiresCharter,
			})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			// The probe advances only the run's live pull-request head (#4856).
			if _, err := fx.runRepo.SetRunPullRequestURL(ctx, runRow.ID, "https://github.com/kuhlman-labs/fishhawk/pull/4856"); err != nil {
				t.Fatalf("SetRunPullRequestURL: %v", err)
			}
			stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
				RunID: runRow.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
				ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1", RequiresApproval: true,
			})
			if err != nil {
				t.Fatalf("CreateStage: %v", err)
			}
			parkAtGate(t, ctx, fx.runRepo, stage.ID)
			seedImplementReview(t, ctx, auditRepo, runRow.ID, stage.ID,
				planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "address the drift"})
			seedReportedHead(t, ctx, auditRepo, runRow.ID, stage.ID, "acceptance_scenarios_pushed", recordedHead, time.Now().Add(-1*time.Hour).UTC())

			session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_vouch_commit", Arguments: map[string]any{
				"run_id": runRow.ID.String(), "sha": vouchedTip, "reason": "operator remediation commit",
			}})
			if err != nil {
				t.Fatalf("CallTool fishhawk_vouch_commit: %v", err)
			}
			if res.IsError {
				t.Fatalf("vouch returned error: %s", toolContentString(t, res))
			}
			var vouchOut struct {
				Result struct {
					RecordedHeadAdvanced    bool   `json:"recorded_head_advanced"`
					RecordedHeadAdvanceNote string `json:"recorded_head_advance_note"`
				} `json:"result"`
			}
			raw, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(raw, &vouchOut); err != nil {
				t.Fatalf("decode vouch result %s: %v", raw, err)
			}
			if vouchOut.Result.RecordedHeadAdvanced != tt.wantAdvanced {
				t.Errorf("recorded_head_advanced = %v, want %v (note %q)", vouchOut.Result.RecordedHeadAdvanced, tt.wantAdvanced, vouchOut.Result.RecordedHeadAdvanceNote)
			}

			res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_fixup_stage", Arguments: map[string]any{
				"stage_id": stage.ID.String(), "concerns": []int{0},
			}})
			if err != nil {
				t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
			}
			if res.IsError {
				t.Fatalf("fix-up returned error: %s", toolContentString(t, res))
			}
			var promptOut struct {
				Fixup                bool   `json:"fixup"`
				FixupExpectedHeadSHA string `json:"fixup_expected_head_sha"`
			}
			getPromptRenderJSON(t, ctx, httpSrv.URL, stage.ID, &promptOut)
			if !promptOut.Fixup {
				t.Fatal("precondition: prompt fixup = false, want true after the fix-up trigger")
			}
			if promptOut.FixupExpectedHeadSHA != tt.wantHead {
				t.Errorf("prompt fixup_expected_head_sha = %q, want %q", promptOut.FixupExpectedHeadSHA, tt.wantHead)
			}
		})
	}
}
