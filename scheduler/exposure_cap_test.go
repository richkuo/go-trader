package main

import (
	"strings"
	"testing"
)

func TestExposureCapBlocksSignal(t *testing.T) {
	bothCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, ShortUSD: 500, LongBlocked: true, ShortBlocked: true}
	longCapped := ExposureCapStatus{Configured: true, CapUSD: 100, LongUSD: 500, LongBlocked: true}
	shortCapped := ExposureCapStatus{Configured: true, CapUSD: 100, ShortUSD: 500, ShortBlocked: true}
	longBook := ExposureCapStatus{Configured: true, CapUSD: 15000, LongUSD: 19000, LongBlocked: true}
	cases := []struct {
		name          string
		st            ExposureCapStatus
		signal        int
		closeFraction float64
		posQty        float64
		posSide       string
		allowsLong    bool
		allowsShort   bool
		wantBlocked   bool
		wantReason    []string
	}{
		{"manage_only_signal0_passes", bothCapped, 0, 0, 1, "long", true, true, false, nil},
		{"close_action_passes", bothCapped, -1, 1.0, 1, "long", true, true, false, nil},
		{"pure_close_sell_on_long_passes", bothCapped, -1, 0, 1, "long", true, false, false, nil},
		{"pure_close_buy_on_short_passes", bothCapped, 1, 0, 1, "short", false, true, false, nil},
		{"scale_in_add_on_long_blocked_while_longs_capped", longCapped, 1, 0, 1, "long", true, true, true, nil},
		{"long_to_short_flip_passes_while_only_longs_capped", longCapped, -1, 0, 1, "long", true, true, false, nil},
		{"long_to_short_flip_held_while_shorts_capped", shortCapped, -1, 0, 1, "long", true, true, true, nil},
		{"fresh_short_open_blocked_while_shorts_capped", shortCapped, -1, 0, 0, "", true, true, true, nil},
		{"fresh_short_open_passes_while_only_longs_capped", longCapped, -1, 0, 0, "", true, true, false, nil},
		{"fresh_long_open_blocked_with_amounts_in_reason", longBook, 1, 0, 0, "", true, true, true, []string{"new long opens blocked", "$19000.00", "$15000.00"}},
		{"fresh_short_open_passes_on_long_capped_book", longBook, -1, 0, 0, "", true, true, false, nil},
		{"disabled_cap_never_blocks", ExposureCapStatus{}, 1, 0, 0, "", true, true, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, why := exposureCapBlocksSignal(tc.st, "BTC", tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort)
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v (why=%q)", blocked, tc.wantBlocked, why)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(why, want) {
					t.Errorf("reason %q missing %q", why, want)
				}
			}
		})
	}
}
