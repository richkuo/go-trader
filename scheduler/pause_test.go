package main

import (
	"testing"
)

func TestPausedBlocksSignal(t *testing.T) {
	cases := []struct {
		name          string
		signal        int
		closeFraction float64
		posQty        float64
		posSide       string
		allowsLong    bool
		allowsShort   bool
		want          bool
	}{
		{"hold flat", 0, 0, 0, "", true, false, false},
		{"hold open", 0, 0, 1, "long", true, true, false},

		{"flat buy", 1, 0, 0, "", true, false, true},
		{"flat sell short-capable", -1, 0, 0, "", true, true, true},
		{"flat stale close action", -1, 1.0, 0, "", true, false, true},

		{"open long partial close", -1, 0.5, 2, "long", true, true, false},
		{"open long full close", -1, 1.0, 2, "long", true, true, false},
		{"open short partial close", 1, 0.25, 3, "short", true, true, false},

		{"long-only sell exit", -1, 0, 2, "long", true, false, false},
		{"short-only buy exit", 1, 0, 2, "short", false, true, false},
		{"spot sell", -1, 0, 1.5, "long", true, false, false},

		{"open long buy add", 1, 0, 2, "long", true, false, true},
		{"open long buy add both", 1, 0, 2, "long", true, true, true},
		{"open short sell add", -1, 0, 2, "short", true, true, true},
		{"spot buy while long", 1, 0, 1.5, "long", true, false, true},

		{"long flip to short", -1, 0, 2, "long", true, true, true},
		{"short flip to long", 1, 0, 2, "short", true, true, true},

		{"legacy buy on short under long", 1, 0, 2, "short", true, false, true},

		{"futures long sell flip", -1, 0, 3, "long", true, true, true},
		{"futures short buy flip", 1, 0, 3, "short", true, true, true},
		{"futures long partial registry close", -1, 0.5, 3, "long", true, true, false},
		{"futures long full registry close", -1, 1.0, 3, "long", true, true, false},
		{"futures flat sell fresh short", -1, 0, 0, "", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pausedBlocksSignal(tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort)
			if got != tc.want {
				t.Fatalf("pausedBlocksSignal(signal=%d cf=%.2f qty=%.1f side=%q long=%t short=%t) = %t, want %t",
					tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort, got, tc.want)
			}
		})
	}
}
