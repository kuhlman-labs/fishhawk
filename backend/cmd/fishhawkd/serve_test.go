package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewresolver"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// resolveMaxParallelChildren mirrors runServe's --max-parallel-children
// flag wiring (E24.6 / #1146) so the resolution precedence — explicit flag
// arg > FISHHAWKD_MAX_PARALLEL_CHILDREN env > the built-in 0 default — is
// unit-testable without booting the whole server. It is the same shape as
// the live `fs.Int("max-parallel-children", envOrInt(...), ...)` call.
func resolveMaxParallelChildren(t *testing.T, args []string) int {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := fs.Int("max-parallel-children",
		envOrInt("FISHHAWKD_MAX_PARALLEL_CHILDREN", 0),
		"test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *v
}

// resolveImplementModelConfig mirrors runServe's --implement-model-default and
// --implement-allowed-models flag wiring (#1013) so the env > flag resolution
// and the ParseAllowedModels handoff are unit-testable without booting the
// server. Same shape as the live fs.String(... envOr(...) ...) calls.
func resolveImplementModelConfig(t *testing.T, args []string) (string, server.AllowedModels) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	deflt := fs.String("implement-model-default",
		envOr("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", ""), "test")
	allowed := fs.String("implement-allowed-models",
		envOr("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", ""), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *deflt, server.ParseAllowedModels(*allowed)
}

// resolveBudgetTierConfig mirrors runServe's --budget-limit-override-usd /
// --budget-ack-multiple / --budget-page-multiple flag wiring (#1371) and
// the handoff into server.Config (serve.go line ~553), so the env > flag
// resolution AND the Config wiring are unit-testable without booting the
// server. It returns a server.Config carrying only the three #1371 fields,
// built exactly as runServe builds them.
func resolveBudgetTierConfig(t *testing.T, args []string) server.Config {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limitOverride := fs.Float64("budget-limit-override-usd",
		envOrFloat("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", 0), "test")
	ackMultiple := fs.Float64("budget-ack-multiple",
		envOrFloat("FISHHAWKD_BUDGET_ACK_MULTIPLE", 2.0), "test")
	pageMultiple := fs.Float64("budget-page-multiple",
		envOrFloat("FISHHAWKD_BUDGET_PAGE_MULTIPLE", 3.0), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return server.Config{
		BudgetLimitOverrideUSD: *limitOverride,
		BudgetAckMultiple:      *ackMultiple,
		BudgetPageMultiple:     *pageMultiple,
	}
}

// TestResolveBudgetTierConfig is binding condition (1): the three #1371
// budget env vars parse and wire into server.Config — both the defaults
// (0 / 2.0 / 3.0) and an explicit override.
func TestResolveBudgetTierConfig(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", "")
		t.Setenv("FISHHAWKD_BUDGET_ACK_MULTIPLE", "")
		t.Setenv("FISHHAWKD_BUDGET_PAGE_MULTIPLE", "")
		cfg := resolveBudgetTierConfig(t, nil)
		if cfg.BudgetLimitOverrideUSD != 0 {
			t.Errorf("BudgetLimitOverrideUSD = %g, want 0 (spec limit)", cfg.BudgetLimitOverrideUSD)
		}
		if cfg.BudgetAckMultiple != 2.0 {
			t.Errorf("BudgetAckMultiple = %g, want 2.0", cfg.BudgetAckMultiple)
		}
		if cfg.BudgetPageMultiple != 3.0 {
			t.Errorf("BudgetPageMultiple = %g, want 3.0", cfg.BudgetPageMultiple)
		}
	})
	t.Run("explicit overrides via flags wire into Config", func(t *testing.T) {
		cfg := resolveBudgetTierConfig(t, []string{
			"--budget-limit-override-usd", "250",
			"--budget-ack-multiple", "1.5",
			"--budget-page-multiple", "2.5",
		})
		if cfg.BudgetLimitOverrideUSD != 250 {
			t.Errorf("BudgetLimitOverrideUSD = %g, want 250", cfg.BudgetLimitOverrideUSD)
		}
		if cfg.BudgetAckMultiple != 1.5 {
			t.Errorf("BudgetAckMultiple = %g, want 1.5", cfg.BudgetAckMultiple)
		}
		if cfg.BudgetPageMultiple != 2.5 {
			t.Errorf("BudgetPageMultiple = %g, want 2.5", cfg.BudgetPageMultiple)
		}
	})
	t.Run("env override wins over default", func(t *testing.T) {
		t.Setenv("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", "500")
		t.Setenv("FISHHAWKD_BUDGET_ACK_MULTIPLE", "2.25")
		t.Setenv("FISHHAWKD_BUDGET_PAGE_MULTIPLE", "4")
		cfg := resolveBudgetTierConfig(t, nil)
		if cfg.BudgetLimitOverrideUSD != 500 || cfg.BudgetAckMultiple != 2.25 || cfg.BudgetPageMultiple != 4 {
			t.Errorf("env values not wired: %+v", cfg)
		}
	})
}

// resolveReviewResolution mirrors runServe's --review-resolution flag wiring
// (ADR-031 Phase 2) so the env > flag resolution and the reviewresolver.Select
// handoff are unit-testable without booting the server. Same shape as the live
// fs.String("review-resolution", envOr("FISHHAWKD_REVIEW_RESOLUTION",
// reviewresolver.DefaultResolution), ...) call followed by reviewresolver.Select.
func resolveReviewResolution(t *testing.T, args []string) (reviewresolver.Resolver, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := fs.String("review-resolution",
		envOr("FISHHAWKD_REVIEW_RESOLUTION", reviewresolver.DefaultResolution), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return reviewresolver.Select(*v)
}

// noopReviewResolver is a named reviewresolver.Resolver for the serve-wiring
// test: it records nothing and resolves to nil, standing in for a registered
// provider so Select can return it.
type noopReviewResolver struct{ name string }

func (n noopReviewResolver) Name() string { return n.name }

func (n noopReviewResolver) ResolveReviewFromPollState(context.Context, uuid.UUID, bool, string) error {
	return nil
}

// TestResolveReviewResolution covers the --review-resolution /
// FISHHAWKD_REVIEW_RESOLUTION wiring (ADR-031 Phase 2 binding condition): the
// flag default resolves to github_merge, an explicit value is parsed and
// selected, and an UNKNOWN value fails closed (reviewresolver.Select returns
// UnknownResolverError — runServe fails startup, not silently defaulting).
func TestResolveReviewResolution(t *testing.T) {
	// Register the github_merge provider (as runServe does after srv is built)
	// plus a second named provider so the explicit-value branch resolves to a
	// real registration. The registry is global per-process; registering here
	// mirrors the startup wiring without booting a server.
	reviewresolver.Register(noopReviewResolver{name: reviewresolver.DefaultResolution})
	reviewresolver.Register(noopReviewResolver{name: "alt_merge"})

	t.Run("flag default resolves to github_merge", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		got, err := resolveReviewResolution(t, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != reviewresolver.DefaultResolution {
			t.Errorf("resolved provider = %q, want github_merge", got.Name())
		}
	})

	t.Run("explicit value is parsed and selected", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		got, err := resolveReviewResolution(t, []string{"--review-resolution", "alt_merge"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != "alt_merge" {
			t.Errorf("resolved provider = %q, want alt_merge (explicit flag value selected)", got.Name())
		}
	})

	t.Run("env value is parsed and selected", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "alt_merge")
		got, err := resolveReviewResolution(t, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != "alt_merge" {
			t.Errorf("resolved provider = %q, want alt_merge (env value selected)", got.Name())
		}
	})

	t.Run("unknown value fails closed", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		_, err := resolveReviewResolution(t, []string{"--review-resolution", "nonexistent"})
		var unknown *reviewresolver.UnknownResolverError
		if !errors.As(err, &unknown) {
			t.Fatalf("error = %v, want *UnknownResolverError (runServe would fail startup, not default)", err)
		}
		if unknown.ID != "nonexistent" {
			t.Errorf("UnknownResolverError.ID = %q, want nonexistent", unknown.ID)
		}
	})
}

// TestResolveImplementModelConfig covers the implement-model deployment config
// resolution (#1013): the default model env/flag and the per-adapter
// allowed-model policy parse, plus the empty/fail-open default.
func TestResolveImplementModelConfig(t *testing.T) {
	t.Run("unset yields empty default and fail-open policy", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "")
		t.Setenv("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", "")
		deflt, policy := resolveImplementModelConfig(t, nil)
		if deflt != "" {
			t.Errorf("default = %q, want empty", deflt)
		}
		if !policy.IsAllowed("claudecode", "anything") {
			t.Error("empty policy must fail open")
		}
	})
	t.Run("env values parse into default and policy", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "claude-sonnet-4-6")
		t.Setenv("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", "claudecode=claude-opus-4-8;codex=gpt-5.5")
		deflt, policy := resolveImplementModelConfig(t, nil)
		if deflt != "claude-sonnet-4-6" {
			t.Errorf("default = %q, want claude-sonnet-4-6", deflt)
		}
		if !policy.IsAllowed("claudecode", "claude-opus-4-8") {
			t.Error("claudecode opus should be allowed")
		}
		if policy.IsAllowed("claudecode", "gpt-5.5") {
			t.Error("claudecode should reject a codex-only model")
		}
		if !policy.IsAllowed("codex", "gpt-5.5") {
			t.Error("codex gpt-5.5 should be allowed")
		}
	})
	t.Run("flag arg wins over env for the default", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "claude-sonnet-4-6")
		deflt, _ := resolveImplementModelConfig(t, []string{"--implement-model-default", "claude-opus-4-8"})
		if deflt != "claude-opus-4-8" {
			t.Errorf("default = %q, want claude-opus-4-8 (flag wins)", deflt)
		}
	})
}

// TestPlanReviewerSet_CodexReasoningEffort covers the #1493 per-reviewer
// reasoning-effort ladder applied deployment-side: the spec value wins over the
// FISHHAWKD_CODEX_REASONING_EFFORT default; an empty spec value falls back to
// the env default; both empty carries no override; and For("codex", ...) routes
// the resolved value into a constructed codex reviewer.
func TestPlanReviewerSet_CodexReasoningEffort(t *testing.T) {
	t.Run("spec value overrides the deployment default", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: "low"}}
		if got := set.resolveCodexEffort("high"); got != "high" {
			t.Errorf("resolveCodexEffort = %q, want high (spec overrides env default)", got)
		}
	})
	t.Run("empty spec value falls back to the deployment default", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: "medium"}}
		if got := set.resolveCodexEffort(""); got != "medium" {
			t.Errorf("resolveCodexEffort = %q, want medium (env default is the lowest rung)", got)
		}
	})
	t.Run("both empty carries no override", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: ""}}
		if got := set.resolveCodexEffort(""); got != "" {
			t.Errorf("resolveCodexEffort = %q, want empty (host config inherited)", got)
		}
	})
	t.Run("For codex constructs a reviewer with the resolved effort", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{
			enableCodexReviewer: true,
			codexModel:          "gpt-5.5",
			codexEffort:         "low",
		}}
		reviewer, err := set.For("codex", "", "high")
		if err != nil {
			t.Fatalf("For(codex): %v", err)
		}
		if reviewer == nil {
			t.Fatal("For(codex) returned a nil reviewer")
		}
	})
}

// TestResolveMaxParallelChildren covers the FISHHAWKD_MAX_PARALLEL_CHILDREN
// resolution branches: the default applies when unset, the env value wins
// over the default, an explicit env 0 is honored as the unlimited semantic
// (not coerced), and a flag arg wins over the env.
func TestResolveMaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"

	t.Run("unset resolves to default 0 (unlimited)", func(t *testing.T) {
		t.Setenv(key, "")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (default unlimited)", got)
		}
	})

	t.Run("env value wins over default", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, nil); got != 4 {
			t.Errorf("got %d, want 4 (env over default)", got)
		}
	})

	t.Run("explicit env 0 is honored as unlimited", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 must reach the cap as unlimited, not be coerced)", got)
		}
	})

	t.Run("flag arg wins over env", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, []string{"--max-parallel-children", "7"}); got != 7 {
			t.Errorf("got %d, want 7 (explicit flag over env)", got)
		}
	})
}

// TestEnvOrInt_MaxParallelChildren pins the FISHHAWKD_MAX_PARALLEL_CHILDREN
// env name the flag default resolves through envOrInt, so the env name can't
// silently drift from the flag wiring in runServe. Mirrors the explicit-0
// discipline of the plan-review-max-retries test: an env "0" must reach the
// setter as 0 (unlimited), not be treated as empty.
func TestEnvOrInt_MaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"
	t.Run("unset returns default 0", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("explicit 0 resolves to 0", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 is the unlimited sentinel, not empty)", got)
		}
	})
	t.Run("positive value resolves verbatim", func(t *testing.T) {
		t.Setenv(key, "5")
		if got := envOrInt(key, 0); got != 5 {
			t.Errorf("got %d, want 5", got)
		}
	})
}

// TestNewStageOrchestrator_WiresDriveEngine pins the construction-site
// wiring (E24.3 / #1143): the orchestrator runServe builds must carry a
// non-nil Drive engine, so the RuleChildrenDispatch run_auto_advanced
// trail for concurrent decomposed-child dispatch can't be silently
// dropped behind the orchestrator-fake behavioral tests.
func TestNewStageOrchestrator_WiresDriveEngine(t *testing.T) {
	o := newStageOrchestrator(server.Config{}, slog.Default())
	if o == nil {
		t.Fatal("newStageOrchestrator returned nil")
	}
	if o.Drive == nil {
		t.Error("orchestrator Drive engine is nil; the RuleChildrenDispatch trail would be dropped")
	}
}

// TestNewChildCompletionSweeper_WiresDispatchBackstop pins the
// construction-site wiring (E24.3 / #1143): the sweeper runServe builds
// must carry a non-nil Dispatch backstop (the childCompletionAdvancer
// adapter), so the fail-closed concurrent-dispatch top-up can't be
// silently omitted. Advance + Integrate are asserted alongside so the
// extraction can't regress the pre-existing wiring either.
func TestNewChildCompletionSweeper_WiresDispatchBackstop(t *testing.T) {
	sw := newChildCompletionSweeper(server.Config{}, slog.Default(), time.Minute)
	if sw == nil {
		t.Fatal("newChildCompletionSweeper returned nil")
	}
	if sw.Dispatch == nil {
		t.Error("sweeper Dispatch backstop is nil; the fail-closed dispatch top-up would be omitted")
	}
	if sw.Advance == nil {
		t.Error("sweeper Advance adapter is nil")
	}
	if sw.Integrate == nil {
		t.Error("sweeper Integrate adapter is nil")
	}
}

// TestCampaignDriverStartDecision is the binding-condition serve-level test
// (E25.5 / #1444): the fail-closed switch does NOT construct/start the ticker
// when the flag is false, and skips with a reason when a required dependency
// is unwired. Each branch of campaignDriverStartDecision is asserted (mode a:
// DISABLED in serve.go).
func TestCampaignDriverStartDecision(t *testing.T) {
	wired := server.Config{
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}

	// (a) DISABLED: the flag is false → the ticker is NOT started and there is
	// no warn reason (a flag-off skip is silent, not a misconfiguration).
	if start, reason := campaignDriverStartDecision(false, wired); start || reason != "" {
		t.Fatalf("flag-off: start=%v reason=%q; want false + empty (ticker must NOT be constructed/started)", start, reason)
	}

	// Enabled + fully wired → started, no reason.
	if start, reason := campaignDriverStartDecision(true, wired); !start || reason != "" {
		t.Fatalf("wired: start=%v reason=%q; want true + empty", start, reason)
	}

	// Enabled but a required repo missing → fail-closed skip with a reason.
	missingRepo := wired
	missingRepo.CampaignRepo = nil
	if start, reason := campaignDriverStartDecision(true, missingRepo); start || reason == "" {
		t.Fatalf("missing campaign repo: start=%v reason=%q; want false + a reason", start, reason)
	}

	// Enabled but the GitHub client is missing → fail-closed skip (the
	// run-starter needs it to resolve the workflow spec the campaign lacks).
	missingGitHub := wired
	missingGitHub.GitHub = nil
	if start, reason := campaignDriverStartDecision(true, missingGitHub); start || reason == "" {
		t.Fatalf("missing github: start=%v reason=%q; want false + a reason", start, reason)
	}
}

// TestNewCampaignDriver_WiresDependencies asserts the constructor binds every
// required dependency (a nil Starter/Audit would make Run() refuse to start).
func TestNewCampaignDriver_WiresDependencies(t *testing.T) {
	cfg := server.Config{
		Addr:         "127.0.0.1:0",
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}
	srv := server.New(cfg)
	notifier := issuecomment.New(issuecomment.Deps{
		GitHub:      cfg.GitHub,
		Runs:        cfg.RunRepo,
		Audit:       cfg.AuditRepo,
		ExternalURL: "https://app.fishhawk.test",
	})
	tk := newCampaignDriver(cfg, srv, slog.Default(), notifier, time.Minute, "feature_change", "")
	if tk == nil {
		t.Fatal("newCampaignDriver returned nil")
	}
	if tk.Campaigns == nil || tk.Runs == nil || tk.Starter == nil || tk.Audit == nil {
		t.Fatalf("ticker has a nil required dependency: %+v", tk)
	}
	if tk.Interval != time.Minute {
		t.Errorf("interval = %v, want 1m", tk.Interval)
	}
	// E25.6: with the GitHub client wired the constructor binds a live
	// GateActor so the driver auto-acts on each run gate.
	if tk.GateActor == nil {
		t.Error("ticker GateActor is nil despite a configured GitHub client; auto-drive would never run")
	}
	// E25.7: a concrete notifier is bound as the page seam so the Paged branch
	// fires the human page.
	if tk.Notifier == nil {
		t.Error("ticker Notifier is nil despite a configured notifier; the Paged branch would never page")
	}
}

// TestNewCampaignDriver_NilNotifier_ObserveOnly guards the typed-nil trap: a nil
// *issuecomment.Notifier (the unconfigured-deps case) must leave the seam a true
// nil interface so the driver's Paged branch takes the observe-only path rather
// than calling a nil pointer.
func TestNewCampaignDriver_NilNotifier_ObserveOnly(t *testing.T) {
	cfg := server.Config{
		Addr:         "127.0.0.1:0",
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}
	srv := server.New(cfg)
	tk := newCampaignDriver(cfg, srv, slog.Default(), nil, time.Minute, "feature_change", "")
	if tk.Notifier != nil {
		t.Errorf("ticker Notifier = %#v, want a true nil interface (observe-only)", tk.Notifier)
	}
}

// TestCampaignOperatorIdentity asserts the in-process actor identity carries
// the operator-agent attribution, the gate-action write scopes, and a non-empty
// TokenID (scope-acceptance parity — the handler scope check must apply rather
// than the cookie-session bypass). E25.6 / ADR-047 slice 3.
func TestCampaignOperatorIdentity(t *testing.T) {
	id := campaignOperatorIdentity()
	if id.Subject != operatorrole.CampaignActorSubject {
		t.Errorf("Subject = %q, want %q", id.Subject, operatorrole.CampaignActorSubject)
	}
	if id.TokenID == "" {
		t.Error("TokenID is empty; the handler scope check would be bypassed (cookie-session path) instead of enforced")
	}
	want := operatorrole.CampaignActorScopes()
	if len(id.Scopes) != len(want) {
		t.Fatalf("Scopes = %v, want %v", id.Scopes, want)
	}
	have := map[string]bool{}
	for _, s := range id.Scopes {
		have[s] = true
	}
	for _, s := range want {
		if !have[s] {
			t.Errorf("Scopes missing %q (have %v)", s, id.Scopes)
		}
	}
}

// TestNewCampaignGateActor covers the serve-wiring construction AND the
// fail-closed observe-only path (E25.6 / ADR-047 slice 3): a configured GitHub
// client yields a live actor binding the campaign identity + GitHub merger; an
// unconfigured client returns nil so the driver runs observe-only.
func TestNewCampaignGateActor(t *testing.T) {
	srv := server.New(server.Config{Addr: "127.0.0.1:0"})

	// Configured: GitHub client present → a non-nil actor binding the campaign
	// identity and a GitHubMerger.
	actor := newCampaignGateActor(server.Config{GitHub: &githubclient.Client{}}, srv, slog.Default())
	if actor == nil {
		t.Fatal("newCampaignGateActor returned nil with a configured GitHub client; auto-drive would never run")
	}
	cga, ok := actor.(campaignGateActor)
	if !ok {
		t.Fatalf("actor concrete = %T, want campaignGateActor", actor)
	}
	if cga.id.Subject != operatorrole.CampaignActorSubject {
		t.Errorf("actor identity subject = %q, want %q", cga.id.Subject, operatorrole.CampaignActorSubject)
	}
	if cga.merger == nil {
		t.Error("actor GitHubMerger is nil; a delegated may_merge could not be honoured")
	}

	// Fail-closed: no GitHub client → nil actor (the driver runs observe-only).
	if got := newCampaignGateActor(server.Config{GitHub: nil}, srv, slog.Default()); got != nil {
		t.Errorf("newCampaignGateActor(nil GitHub) = %T, want nil (observe-only fail-closed)", got)
	}
}

// TestGithubAutoMerger_FailsClosed asserts the merger refuses — before any
// HTTP call — a run that lacks the installation id or PR url the merge needs,
// or whose PR url is unparseable. These are the defensive guards the merge seam
// adds; each returns an error the actor surfaces rather than a silent no-op.
func TestGithubAutoMerger_FailsClosed(t *testing.T) {
	m := githubAutoMerger{gh: &githubclient.Client{}}
	ctx := context.Background()
	inst := int64(42)
	prURL := "https://github.com/x/y/pull/7"
	bad := "not-a-url"

	cases := []struct {
		name string
		run  *runpkg.Run
	}{
		{"no installation id", &runpkg.Run{ID: uuid.New(), PullRequestURL: &prURL}},
		{"no pull request url", &runpkg.Run{ID: uuid.New(), InstallationID: &inst}},
		{"unparseable pull request url", &runpkg.Run{ID: uuid.New(), InstallationID: &inst, PullRequestURL: &bad}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.MergePullRequest(ctx, tc.run); err == nil {
				t.Fatalf("MergePullRequest(%s) = nil, want an error (fail-closed before HTTP)", tc.name)
			}
		})
	}
}

// TestParseCampaignPRURL covers the PR-url parser's accept + reject branches.
func TestParseCampaignPRURL(t *testing.T) {
	repo, n, err := parseCampaignPRURL("https://github.com/owner/name/pull/123")
	if err != nil {
		t.Fatalf("valid url: unexpected error %v", err)
	}
	if repo.Owner != "owner" || repo.Name != "name" || n != 123 {
		t.Errorf("parsed = %+v #%d, want owner/name #123", repo, n)
	}
	for _, bad := range []string{"", "https://github.com/owner/name", "https://github.com/owner/name/issues/1", "https://github.com/owner/name/pull/abc"} {
		if _, _, err := parseCampaignPRURL(bad); err == nil {
			t.Errorf("parseCampaignPRURL(%q) = nil error, want a reject", bad)
		}
	}
}

// TestBuildModelProviders covers the live ModelOracle provider-registration
// wiring (#1341, binding condition 1 — the previously-untested serve.go
// activation seam). A provider is registered ONLY when its API key is present;
// an absent key leaves it UNREGISTERED so its Snapshot reports ok=false (the
// fail-open / never-a-boot-blocker invariant). Keyed under the existing
// "claudecode"/"codex" strings the allow-list and #1339 already use.
func TestBuildModelProviders(t *testing.T) {
	t.Run("both keys present registers both providers", func(t *testing.T) {
		p := buildModelProviders("anthropic-key", "openai-key")
		if _, ok := p["claudecode"]; !ok {
			t.Error("claudecode not registered with an anthropic key present")
		}
		if _, ok := p["codex"]; !ok {
			t.Error("codex not registered with an openai key present")
		}
		if len(p) != 2 {
			t.Errorf("len(providers) = %d, want 2", len(p))
		}
	})

	t.Run("anthropic key only registers claudecode and leaves codex unregistered", func(t *testing.T) {
		p := buildModelProviders("anthropic-key", "")
		if _, ok := p["claudecode"]; !ok {
			t.Error("claudecode not registered with an anthropic key present")
		}
		if _, ok := p["codex"]; ok {
			t.Error("codex registered despite an absent openai key")
		}
	})

	t.Run("openai key only registers codex and leaves claudecode unregistered", func(t *testing.T) {
		p := buildModelProviders("", "openai-key")
		if _, ok := p["codex"]; !ok {
			t.Error("codex not registered with an openai key present")
		}
		if _, ok := p["claudecode"]; ok {
			t.Error("claudecode registered despite an absent anthropic key")
		}
	})

	t.Run("no keys registers nothing and Snapshot fails open", func(t *testing.T) {
		p := buildModelProviders("", "")
		if len(p) != 0 {
			t.Fatalf("len(providers) = %d, want 0 with no keys", len(p))
		}
		// An unregistered provider must report ok=false so #1339 fails open
		// (never a boot blocker, never a false rejection).
		o := modeloracle.NewCached(p, 24*time.Hour, slog.Default())
		if _, _, ok := o.Snapshot(context.Background(), "claudecode"); ok {
			t.Error("Snapshot ok=true for an unregistered provider, want false (fail-open)")
		}
	})
}

// resolveModelsFlags mirrors runServe's --models-refresh-interval /
// --models-staleness-threshold flag wiring (#1341) so the duration defaults
// (12h refresh / 24h staleness) are unit-testable without booting the server —
// binding condition 1's "assert the two duration-flag defaults parse" clause.
func resolveModelsFlags(t *testing.T, args []string) (refresh, staleness time.Duration) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r := fs.Duration("models-refresh-interval",
		envOrDuration("FISHHAWKD_MODELS_REFRESH_INTERVAL", 12*time.Hour), "test")
	s := fs.Duration("models-staleness-threshold",
		envOrDuration("FISHHAWKD_MODELS_STALENESS_THRESHOLD", 24*time.Hour), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *r, *s
}

// TestResolveModelsFlags asserts the model-oracle duration defaults parse to 12h
// / 24h, an env override is honored, and an explicit flag wins — the done-means
// defaults from #1341's plan (binding condition 1).
func TestResolveModelsFlags(t *testing.T) {
	t.Run("defaults parse to 12h / 24h", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "")
		t.Setenv("FISHHAWKD_MODELS_STALENESS_THRESHOLD", "")
		refresh, staleness := resolveModelsFlags(t, nil)
		if refresh != 12*time.Hour {
			t.Errorf("refresh default = %s, want 12h", refresh)
		}
		if staleness != 24*time.Hour {
			t.Errorf("staleness default = %s, want 24h", staleness)
		}
	})

	t.Run("env override honored", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "6h")
		t.Setenv("FISHHAWKD_MODELS_STALENESS_THRESHOLD", "48h")
		refresh, staleness := resolveModelsFlags(t, nil)
		if refresh != 6*time.Hour {
			t.Errorf("refresh = %s, want 6h from env", refresh)
		}
		if staleness != 48*time.Hour {
			t.Errorf("staleness = %s, want 48h from env", staleness)
		}
	})

	t.Run("explicit flag wins over env", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "6h")
		refresh, _ := resolveModelsFlags(t, []string{"--models-refresh-interval", "1h"})
		if refresh != time.Hour {
			t.Errorf("refresh = %s, want 1h from the explicit flag", refresh)
		}
	})
}

// TestResolveRefinementDrafter asserts the agent-backed drafter is wired only
// when the local claude adapter is configured — the seam serve.go reads to
// populate Config.RefinementDrafter.
func TestResolveRefinementDrafter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("wired when local claude enabled", func(t *testing.T) {
		d := resolveRefinementDrafter(planReviewerOptions{
			enableLocalClaudeReviewer: true,
			localClaudeBinary:         "claude",
			localClaudeModel:          "claude-opus-4-8",
		}, logger)
		if d == nil {
			t.Fatal("resolveRefinementDrafter returned nil with the local claude adapter enabled")
		}
	})

	t.Run("nil when unconfigured", func(t *testing.T) {
		if d := resolveRefinementDrafter(planReviewerOptions{}, logger); d != nil {
			t.Fatalf("resolveRefinementDrafter = %v, want nil (literal) when unconfigured", d)
		}
	})
}

// TestServeWiresRefinementConfig locks the operator binding condition: the
// production serve path populates BOTH Config.RefinementRepo (always-on
// Postgres adapter, the DB-block call) and Config.RefinementDrafter (agent-
// backed, when the local claude adapter is configured) non-nil. The route-level
// unconfigured-503 tests in the server package cover the nil branches; this
// asserts the wired branch the serve path takes.
func TestServeWiresRefinementConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var cfg server.Config
	// Drive the SAME production helpers the serve DB block calls, rather than
	// re-constructing the repo inline — a hand-rolled refinement.NewPostgresRepository
	// here would stay green even if the DB block stopped populating the field. A
	// nil pool is fine: the constructor stores it without dialing, and we only
	// assert the field is populated (non-nil), exactly as production wires.
	cfg.RefinementRepo = resolveRefinementRepo(nil)
	cfg.RefinementDrafter = resolveRefinementDrafter(planReviewerOptions{
		enableLocalClaudeReviewer: true,
		localClaudeBinary:         "claude",
		localClaudeModel:          "claude-opus-4-8",
	}, logger)

	if cfg.RefinementRepo == nil {
		t.Error("serve path left Config.RefinementRepo nil")
	}
	if cfg.RefinementDrafter == nil {
		t.Error("serve path left Config.RefinementDrafter nil")
	}
}
