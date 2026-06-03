package budget

import "testing"

func TestEvaluateRun(t *testing.T) {
	tests := []struct {
		name      string
		costUSD   float64
		tokens    int64
		maxUSD    float64
		maxTokens int64
		wantOver  bool
		wantDim   string
	}{
		{
			name:    "both ceilings disabled (default 0)",
			costUSD: 9999, tokens: 9_000_000,
			maxUSD: 0, maxTokens: 0,
			wantOver: false, wantDim: "",
		},
		{
			name:    "usd under ceiling",
			costUSD: 4.99, maxUSD: 5.00,
			wantOver: false, wantDim: "",
		},
		{
			name:    "usd exactly at ceiling is over",
			costUSD: 5.00, maxUSD: 5.00,
			wantOver: true, wantDim: DimensionUSD,
		},
		{
			name:    "usd over ceiling",
			costUSD: 5.01, maxUSD: 5.00,
			wantOver: true, wantDim: DimensionUSD,
		},
		{
			name:   "tokens under ceiling",
			tokens: 999, maxTokens: 1000,
			wantOver: false, wantDim: "",
		},
		{
			name:   "tokens exactly at ceiling is over",
			tokens: 1000, maxTokens: 1000,
			wantOver: true, wantDim: DimensionTokens,
		},
		{
			name:   "tokens over ceiling",
			tokens: 1001, maxTokens: 1000,
			wantOver: true, wantDim: DimensionTokens,
		},
		{
			name:    "only tokens configured, usd unset",
			costUSD: 100, tokens: 2000,
			maxUSD: 0, maxTokens: 1000,
			wantOver: true, wantDim: DimensionTokens,
		},
		{
			name:    "both over reports usd (usd wins)",
			costUSD: 6.00, tokens: 2000,
			maxUSD: 5.00, maxTokens: 1000,
			wantOver: true, wantDim: DimensionUSD,
		},
		{
			name:    "negative ceiling treated as disabled",
			costUSD: 100, tokens: 100,
			maxUSD: -1, maxTokens: -1,
			wantOver: false, wantDim: "",
		},
		{
			name:    "usd under but tokens over",
			costUSD: 1.00, tokens: 5000,
			maxUSD: 5.00, maxTokens: 1000,
			wantOver: true, wantDim: DimensionTokens,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := EvaluateRun(tc.costUSD, tc.tokens, tc.maxUSD, tc.maxTokens)
			if d.Over != tc.wantOver {
				t.Errorf("Over = %v, want %v", d.Over, tc.wantOver)
			}
			if d.Dimension != tc.wantDim {
				t.Errorf("Dimension = %q, want %q", d.Dimension, tc.wantDim)
			}
			// Figures are always echoed regardless of the verdict.
			if d.CostUSD != tc.costUSD || d.MaxUSD != tc.maxUSD ||
				d.Tokens != tc.tokens || d.MaxTokens != tc.maxTokens {
				t.Errorf("echoed figures mismatch: %+v", d)
			}
		})
	}
}
