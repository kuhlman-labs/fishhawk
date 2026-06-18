package budget

import "testing"

func TestParallelDecision(t *testing.T) {
	tests := []struct {
		name        string
		requested   int
		cap         int
		wantAllowed int
		wantCapped  bool
	}{
		{
			// cap <= 0 → unlimited: no throttle regardless of requested.
			name:      "cap zero is unlimited",
			requested: 5, cap: 0,
			wantAllowed: 5, wantCapped: false,
		},
		{
			name:      "negative cap is unlimited",
			requested: 5, cap: -1,
			wantAllowed: 5, wantCapped: false,
		},
		{
			// cap >= requested → cap not binding: dispatch all requested.
			name:      "cap above requested does not throttle",
			requested: 3, cap: 10,
			wantAllowed: 3, wantCapped: false,
		},
		{
			name:      "cap equal to requested does not throttle",
			requested: 4, cap: 4,
			wantAllowed: 4, wantCapped: false,
		},
		{
			// cap < requested → throttle: Allowed clamps to cap, Capped set.
			name:      "cap below requested throttles",
			requested: 8, cap: 3,
			wantAllowed: 3, wantCapped: true,
		},
		{
			name:      "cap one with many requested",
			requested: 12, cap: 1,
			wantAllowed: 1, wantCapped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParallelDecision(tt.requested, tt.cap)
			if got.Requested != tt.requested {
				t.Errorf("Requested = %d, want %d", got.Requested, tt.requested)
			}
			if got.Cap != tt.cap {
				t.Errorf("Cap = %d, want %d", got.Cap, tt.cap)
			}
			if got.Allowed != tt.wantAllowed {
				t.Errorf("Allowed = %d, want %d", got.Allowed, tt.wantAllowed)
			}
			if got.Capped != tt.wantCapped {
				t.Errorf("Capped = %v, want %v", got.Capped, tt.wantCapped)
			}
		})
	}
}
