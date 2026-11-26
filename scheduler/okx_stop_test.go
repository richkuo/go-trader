package main

import "testing"

func TestOKXStopGeometryMatchesDecimalCases(t *testing.T) {
	cases := []struct {
		name string
		in   okxStopGeometryInput
		want okxStopGeometryResult
		err  string
	}{
		{
			name: "long percent inside cross bound",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerPct, Entry: "50000", Mark: "50000", Pct: "2", Tick: "0.1", Equity: "1000", Contracts: "10", CtVal: "0.01", MMR: "0.005", Fee: "0.0005"},
			want: okxStopGeometryResult{Trigger: "49000.0", Moved: true, Clamped: false},
		},
		{
			name: "short percent",
			in:   okxStopGeometryInput{Side: "short", Owner: okxStopOwnerPct, Entry: "50000", Mark: "50000", Pct: "2", Tick: "0.1", Equity: "1000", Contracts: "10", CtVal: "0.01", MMR: "0.005", Fee: "0.0005"},
			want: okxStopGeometryResult{Trigger: "51000.0", Moved: true},
		},
		{
			name: "long atr rounds up to the tick",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerATR, Entry: "50000", Mark: "50000", ATRMult: "1.5", EntryATR: "200", Tick: "0.5"},
			want: okxStopGeometryResult{Trigger: "49700.0", Moved: true},
		},
		{
			name: "trail moves after the rounded minimum",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerTrail, Entry: "50000", Mark: "52000", Pct: "2", Tick: "0.1", PreviousTrigger: "49000.0", MinMovePct: "0.5", HighWater: "50000"},
			want: okxStopGeometryResult{Trigger: "50960.0", HighWater: "52000.0", Moved: true},
		},
		{
			name: "trail holds below the rounded minimum",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerTrail, Entry: "50000", Mark: "50100", Pct: "2", Tick: "0.1", PreviousTrigger: "49000.0", MinMovePct: "0.5", HighWater: "50000"},
			want: okxStopGeometryResult{Trigger: "49000.0", HighWater: "50100.0", Moved: false},
		},
		{
			name: "long ceil",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerPct, Entry: "100.01", Mark: "100", Pct: "2", Tick: "0.1"},
			want: okxStopGeometryResult{Trigger: "98.1", Moved: true},
		},
		{
			name: "short floor",
			in:   okxStopGeometryInput{Side: "short", Owner: okxStopOwnerPct, Entry: "100.01", Mark: "100", Pct: "1", Tick: "0.1"},
			want: okxStopGeometryResult{Trigger: "101.0", Moved: true},
		},
		{
			name: "long clamp then re-round",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerPct, Entry: "100", Mark: "100", Pct: "20", Tick: "0.1", VenueLiq: "95", VenueLiqKnown: true, RequireVenueLiq: true},
			want: okxStopGeometryResult{Trigger: "95.5", Moved: true, Clamped: true, LiqBound: "95"},
		},
		{
			name: "short clamp then re-round",
			in:   okxStopGeometryInput{Side: "short", Owner: okxStopOwnerPct, Entry: "100", Mark: "100", Pct: "20", Tick: "0.1", VenueLiq: "110", VenueLiqKnown: true, RequireVenueLiq: true},
			want: okxStopGeometryResult{Trigger: "109.4", Moved: true, Clamped: true, LiqBound: "110"},
		},
		{
			name: "missing venue evidence",
			in:   okxStopGeometryInput{Side: "long", Owner: okxStopOwnerPct, Entry: "100", Mark: "100", Pct: "2", Tick: "0.1", RequireVenueLiq: true},
			err:  "position liquidation evidence is missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := okxFinalizeStop(tc.in)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("err %v, want %s", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Trigger != tc.want.Trigger || got.Moved != tc.want.Moved || got.Clamped != tc.want.Clamped || got.HighWater != tc.want.HighWater {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if tc.want.LiqBound != "" && got.LiqBound != tc.want.LiqBound {
				t.Fatalf("liq %q want %q", got.LiqBound, tc.want.LiqBound)
			}
		})
	}
}
