package planreview

import (
	"testing"
	"time"
)

func TestReviewBudget_Budget(t *testing.T) {
	// A representative policy with round numbers so the arithmetic is obvious.
	b := ReviewBudget{
		Floor: 300 * time.Second,
		PerKB: 10 * time.Second,
		Cap:   1200 * time.Second,
	}

	tests := []struct {
		name      string
		budget    ReviewBudget
		promptLen int
		want      time.Duration
	}{
		{
			name:      "empty prompt returns floor",
			budget:    b,
			promptLen: 0,
			want:      300 * time.Second,
		},
		{
			name:      "sub-KB prompt rounds up to one KB allowance",
			budget:    b,
			promptLen: 1,
			want:      310 * time.Second,
		},
		{
			name:      "exact KB boundary does not over-round",
			budget:    b,
			promptLen: 1024,
			want:      310 * time.Second,
		},
		{
			name:      "one byte over a KB rounds up to the next KB",
			budget:    b,
			promptLen: 1025,
			want:      320 * time.Second,
		},
		{
			name:      "mid-size prompt scales linearly",
			budget:    b,
			promptLen: 25 * 1024,
			want:      300*time.Second + 25*10*time.Second, // 550s
		},
		{
			name:      "oversized prompt clamps to cap",
			budget:    b,
			promptLen: 10 * 1024 * 1024, // far past the cap
			want:      1200 * time.Second,
		},
		{
			name:      "exactly at the cap boundary",
			budget:    b,
			promptLen: 90 * 1024, // 300 + 900 = 1200 == cap
			want:      1200 * time.Second,
		},
		{
			name: "zero PerKB degrades to a flat floor",
			budget: ReviewBudget{
				Floor: 300 * time.Second,
				PerKB: 0,
				Cap:   1200 * time.Second,
			},
			promptLen: 500 * 1024,
			want:      300 * time.Second,
		},
		{
			name: "non-positive cap disables the ceiling",
			budget: ReviewBudget{
				Floor: 300 * time.Second,
				PerKB: 10 * time.Second,
				Cap:   0,
			},
			promptLen: 200 * 1024,
			want:      300*time.Second + 200*10*time.Second,
		},
		{
			name: "floor wins when cap is misconfigured below it",
			budget: ReviewBudget{
				Floor: 300 * time.Second,
				PerKB: 10 * time.Second,
				Cap:   100 * time.Second,
			},
			promptLen: 50 * 1024,
			want:      300 * time.Second,
		},
		{
			name:      "negative length is treated as empty",
			budget:    b,
			promptLen: -1,
			want:      300 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.budget.Budget(tc.promptLen); got != tc.want {
				t.Errorf("Budget(%d) = %v, want %v", tc.promptLen, got, tc.want)
			}
		})
	}
}

// TestDefaultReviewBudget pins the documented defaults so a change to the
// policy constants is a deliberate, reviewable edit.
func TestDefaultReviewBudget(t *testing.T) {
	if DefaultReviewBudget.Floor != 300*time.Second {
		t.Errorf("Floor = %v, want 300s (#606 floor)", DefaultReviewBudget.Floor)
	}
	if DefaultReviewBudget.PerKB != 10*time.Second {
		t.Errorf("PerKB = %v, want 10s", DefaultReviewBudget.PerKB)
	}
	if DefaultReviewBudget.Cap != 1200*time.Second {
		t.Errorf("Cap = %v, want 1200s", DefaultReviewBudget.Cap)
	}
}
