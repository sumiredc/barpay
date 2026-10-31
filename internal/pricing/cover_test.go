package pricing

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

// The examples below check in at t0, with 10-minute blocks, no pause and no cap unless a test
// says otherwise, and $12.00/h, so a block is $2.00.
func minute(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func rates(points ...any) []Point {
	var out []Point
	for i := 0; i < len(points); i += 2 {
		out = append(out, Point{Rate: usd(int64(points[i+1].(int))), PublishedAt: points[i].(time.Time)})
	}
	return out
}

func plain() settings.Settings {
	s := usdSettings()
	s.PauseMinutes, s.CapPerVisit = 0, usd(0)
	return s
}

func changes(cs ...settings.Change) []settings.Change { return cs }

func at(t time.Time, mod func(*settings.Settings)) settings.Change {
	s := plain()
	if mod != nil {
		mod(&s)
	}
	return settings.Change{Settings: s, At: t}
}

var (
	flat      = rates(t0.Add(-time.Hour), 1200)
	unchanged = changes(at(t0.Add(-time.Hour), nil))
)

func charge(t *testing.T, v Visit, r []Point, c []settings.Change, when time.Time) CoverCharge {
	t.Helper()
	cc, err := Charge(v, r, c, when)
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func amounts(cc CoverCharge) []int64 {
	var out []int64
	for _, b := range cc.Blocks {
		out = append(out, b.Amount.Value())
	}
	return out
}

func visit(orders ...int) Visit {
	v := Visit{CheckedInAt: t0}
	for _, o := range orders {
		v.Orders = append(v.Orders, minute(o))
	}
	return v
}

// Pricing model, Blocks: rate × block length, rounded down to the cent, at the rate published
// when the block ends.
func TestBlockAmount(t *testing.T) {
	cc := charge(t, visit(), rates(t0.Add(-time.Hour), 1205, minute(15), 1330), unchanged, minute(30))
	// 1205 × 10/60 = 200.83 → 200; the 2nd block ends at 20 after 1330 is published: 221.67 → 221.
	if got := amounts(cc); !slices.Equal(got, []int64{200, 221, 221}) || cc.Amount != usd(642) {
		t.Errorf("blocks %v, total %v; want [200 221 221] = 642", got, cc.Amount)
	}
}

// Check-in costs nothing, and a block not ended at the instant is not counted.
func TestCheckInCostsNothing(t *testing.T) {
	for _, when := range []time.Time{t0, minute(9), minute(10).Add(-1)} {
		if cc := charge(t, visit(), flat, unchanged, when); cc.Amount != usd(0) || len(cc.Blocks) != 0 || cc.Status != Running {
			t.Errorf("at %v: %+v; want 0, running", when.Sub(t0), cc)
		}
	}
}

// An ended block keeps its amount whatever rates and settings come later.
func TestEndedBlocksDoNotChange(t *testing.T) {
	before := charge(t, visit(), flat, unchanged, minute(35))
	r := append(slices.Clone(flat), Point{Rate: usd(2400), PublishedAt: minute(36)})
	c := append(slices.Clone(unchanged), at(minute(36), func(s *settings.Settings) {
		s.BlockMinutes, s.PauseMinutes, s.CapPerVisit = 5, 30, usd(100)
	}))
	after := charge(t, visit(), r, c, minute(60))
	if !slices.Equal(after.Blocks[:3], before.Blocks) {
		t.Errorf("blocks ended by 35 changed: %v, then %v", before.Blocks, after.Blocks[:3])
	}
}

// When the total is fixed, a block not yet ended is not charged, and nothing accrues later.
func TestFixedTotalStopsTheCharge(t *testing.T) {
	v := visit()
	v.TotalFixedAt = minute(25)
	cc := charge(t, v, flat, unchanged, minute(90))
	if got := amounts(cc); !slices.Equal(got, []int64{200, 200}) || cc.Status != Stopped {
		t.Errorf("blocks %v, status %s; want [200 200], stopped", got, cc.Status)
	}
}

// Pause for additional orders: the first order does not pause; each later one pauses from the
// order; a new order restarts the pause instead of adding to it.
func TestPauseForAdditionalOrders(t *testing.T) {
	c := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.PauseMinutes = 30 }))
	cases := []struct {
		name   string
		orders []int
		want   []int64
	}{
		{"the first order does not pause", []int{5}, []int64{200, 200, 200, 200, 200, 200, 200, 200}},
		// 2nd order at 15 pauses until 45: blocks ending 20, 30, 40 are free.
		{"the second order pauses", []int{5, 15}, []int64{200, 0, 0, 0, 200, 200, 200, 200}},
		// 3rd order at 40 restarts the pause until 70 (adding would run to 75 and free the block ending at 70).
		{"a new order restarts the pause", []int{5, 15, 40}, []int64{200, 0, 0, 0, 0, 0, 200, 200}},
	}
	for _, tc := range cases {
		cc := charge(t, visit(tc.orders...), flat, c, minute(80))
		if got := amounts(cc); !slices.Equal(got, tc.want) {
			t.Errorf("%s: blocks %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := amounts(charge(t, visit(5, 15), flat, unchanged, minute(30))); !slices.Equal(got, []int64{200, 200, 200}) {
		t.Errorf("pause 0: blocks %v, want none free", got)
	}
}

// Cap per visit: the block that reaches the cap is charged up to it and later blocks cost 0;
// orders do not count toward it (they are not an input); 0 means no cap.
func TestCapPerVisit(t *testing.T) {
	c := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.CapPerVisit = usd(500) }))
	cc := charge(t, visit(1, 2, 3), flat, c, minute(50))
	if got := amounts(cc); !slices.Equal(got, []int64{200, 200, 100, 0, 0}) || cc.Amount != usd(500) || cc.Status != Capped {
		t.Errorf("cap 500: blocks %v, total %v, status %s; want [200 200 100 0 0] = 500, capped", got, cc.Amount, cc.Status)
	}
	exact := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.CapPerVisit = usd(400) }))
	if cc := charge(t, visit(), flat, exact, minute(20)); cc.Amount != usd(400) || cc.Status != Capped {
		t.Errorf("a block reaching the cap exactly: %v, %s; want 400, capped", cc.Amount, cc.Status)
	}
	if cc := charge(t, visit(), flat, exact, minute(20).Add(-1)); cc.Status != Running {
		t.Errorf("just before the block that reaches the cap: %s, want running", cc.Status)
	}
	if cc := charge(t, visit(), flat, unchanged, minute(600)); cc.Amount != usd(12000) || cc.Status != Running {
		t.Errorf("cap 0: %v, %s; want 12000 (no cap), running", cc.Amount, cc.Status)
	}
}

// Settings changes apply to an active session from its next block.
func TestSettingsChangesApplyFromTheNextBlock(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*settings.Settings)
		want []int64
	}{
		// Changed at 15, during the 2nd block: the 3rd block (from 20) is 20 minutes, ending at 40.
		{"block length", func(s *settings.Settings) { s.BlockMinutes = 20 }, []int64{200, 200, 400, 400}},
		// The order at 12 pauses nothing (pause 0 then); the 3rd block uses the 30-minute pause
		// counted from that order, so the block ending at 30 is free and the one ending at 50 is not.
		{"pause", func(s *settings.Settings) { s.PauseMinutes = 30 }, []int64{200, 200, 0, 0, 200, 200, 200, 200}},
		// Cap 300 from the 3rd block: 400 accrued is already over it, so nothing more, and nothing taken back.
		{"cap lowered below the accrued", func(s *settings.Settings) { s.CapPerVisit = usd(300) }, []int64{200, 200, 0, 0, 0, 0, 0, 0}},
		{"cap raised", func(s *settings.Settings) { s.CapPerVisit = usd(700) }, []int64{200, 200, 200, 100, 0, 0, 0, 0}},
	}
	for _, tc := range cases {
		c := append(slices.Clone(unchanged), at(minute(15), tc.mod))
		cc := charge(t, visit(1, 12), flat, c, minute(80))
		if got := amounts(cc); !slices.Equal(got[:len(tc.want)], tc.want) {
			t.Errorf("%s: blocks %v, want %v…", tc.name, got, tc.want)
		}
	}
}

// Each boundary instant: just before, exactly at and just after.
func TestBoundaries(t *testing.T) {
	const ns = time.Nanosecond
	t.Run("a rate published when a block ends is that block's rate", func(t *testing.T) {
		for d, want := range map[time.Duration]int64{-ns: 400, 0: 400, ns: 200} {
			r := append(slices.Clone(flat), Point{Rate: usd(2400), PublishedAt: minute(10).Add(d)})
			if got := amounts(charge(t, visit(), r, unchanged, minute(10).Add(ns))); got[0] != want {
				t.Errorf("published at end%+v: block %d, want %d", d, got[0], want)
			}
		}
	})
	t.Run("a block ending when the pause ends is charged", func(t *testing.T) {
		c := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.PauseMinutes = 5 }))
		// The 2nd order at 5 + d pauses until 10 + d; the 1st block ends at 10.
		for d, want := range map[time.Duration]int64{-ns: 200, 0: 200, ns: 0} {
			v := Visit{CheckedInAt: t0, Orders: []time.Time{t0, minute(5).Add(d)}}
			if got := amounts(charge(t, v, flat, c, minute(10))); got[0] != want {
				t.Errorf("pause ends at end%+v: block %d, want %d", d, got[0], want)
			}
		}
	})
	t.Run("a block ending when an order is placed is free", func(t *testing.T) {
		c := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.PauseMinutes = 30 }))
		for d, want := range map[time.Duration]int64{-ns: 0, 0: 0, ns: 200} {
			v := Visit{CheckedInAt: t0, Orders: []time.Time{t0, minute(10).Add(d)}}
			if got := amounts(charge(t, v, flat, c, minute(10).Add(ns))); got[0] != want {
				t.Errorf("order at end%+v: block %d, want %d", d, got[0], want)
			}
		}
	})
	t.Run("a block ending when the total is fixed is charged", func(t *testing.T) {
		for d, want := range map[time.Duration]int{-ns: 0, 0: 1, ns: 1} {
			v := visit()
			v.TotalFixedAt = minute(10).Add(d)
			if got := charge(t, v, flat, unchanged, minute(60)); len(got.Blocks) != want {
				t.Errorf("fixed at end%+v: %d blocks, want %d", d, len(got.Blocks), want)
			}
		}
	})
	t.Run("a block ended at the instant of a lookup counts", func(t *testing.T) {
		for d, want := range map[time.Duration]int{-ns: 0, 0: 1, ns: 1} {
			if got := charge(t, visit(), flat, unchanged, minute(10).Add(d)); len(got.Blocks) != want {
				t.Errorf("lookup at end%+v: %d blocks, want %d", d, len(got.Blocks), want)
			}
		}
	})
	t.Run("a change made when a block ends applies to the next block", func(t *testing.T) {
		// Block length 10 → 20 and cap 0 → 100: the block ending at the change keeps the old
		// settings (200, no cap); the block starting at the change or later uses the new ones.
		mod := func(s *settings.Settings) { s.BlockMinutes, s.CapPerVisit = 20, usd(100) }
		for d, wantEnds := range map[time.Duration][]time.Time{
			-ns: {minute(10), minute(30)},
			0:   {minute(10), minute(30)},
			ns:  {minute(10), minute(20), minute(40)},
		} {
			c := append(slices.Clone(unchanged), at(minute(10).Add(d), mod))
			cc := charge(t, visit(), flat, c, minute(40))
			var ends []time.Time
			for _, b := range cc.Blocks {
				ends = append(ends, b.End)
			}
			if !slices.Equal(ends, wantEnds) || cc.Blocks[0].Amount != usd(200) {
				t.Errorf("change at end%+v: blocks end %v (first %v), want %v (first 200)", d, ends, cc.Blocks[0].Amount, wantEnds)
			}
		}
	})
}

// UC3's postcondition: the cover charge is the sum of the ended blocks, paused and capped ones
// counted as 0, and the status says which applies now.
func TestStatus(t *testing.T) {
	pause := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.PauseMinutes = 30; s.CapPerVisit = usd(600) }))
	v := visit(1, 12) // the 2nd order pauses 12–42
	fixed := v
	fixed.TotalFixedAt = minute(100)
	cases := []struct {
		name string
		v    Visit
		when time.Time
		want Status
	}{
		{"before the pause", v, minute(11), Running},
		{"at the order", v, minute(12), Paused},
		{"just before the pause ends", v, minute(42).Add(-1), Paused},
		{"when the pause ends", v, minute(42), Running},
		{"cap reached", v, minute(80), Capped},
		{"total fixed", fixed, minute(100), Stopped},
		{"total fixed while paused", Visit{CheckedInAt: t0, Orders: []time.Time{minute(1), minute(95)}, TotalFixedAt: minute(100)}, minute(100), Stopped},
		{"total fixed, later", fixed, minute(200), Stopped},
	}
	// Paused and capped at once: capped wins. The 2nd order at 25 pauses 25–55; the cap of 400 is
	// reached by the block ending at 20.
	both := changes(at(t0.Add(-time.Hour), func(s *settings.Settings) { s.PauseMinutes = 30; s.CapPerVisit = usd(400) }))
	if cc := charge(t, visit(1, 25), flat, both, minute(30)); cc.Status != Capped {
		t.Errorf("paused and capped: status %s, want capped", cc.Status)
	}
	for _, c := range cases {
		cc := charge(t, c.v, flat, pause, c.when)
		var sum int64
		for _, b := range cc.Blocks {
			sum += b.Amount.Value()
		}
		if cc.Status != c.want || cc.Amount.Value() != sum {
			t.Errorf("%s: status %s, amount %v (blocks sum %d); want %s and the sum", c.name, cc.Status, cc.Amount, sum, c.want)
		}
	}
}

// Computed from the store, the cover charge is the same after a restart.
func TestChargeAtSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)
	propose(t, st, 1500, minute(15))
	s := usdSettings()
	s.BlockMinutes = 20
	if _, err := UpdateSettings(ctx, st, "staff-1", s, minute(25)); err != nil {
		t.Fatal(err)
	}
	v := visit(3, 7)
	before, err := ChargeAt(ctx, st.Read(), money.USD, v, minute(70))
	if err != nil {
		t.Fatal(err)
	}
	st = storetest.Reopen(t, st)
	after, err := ChargeAt(ctx, st.Read(), money.USD, v, minute(70))
	if err != nil {
		t.Fatal(err)
	}
	// Blocks end at 10, 20 and 30 (paused from 7 to 37), then 20-minute ones from 30 (the change at
	// 25 applies from the next block): 50 and 70.
	if !slices.Equal(before.Blocks, after.Blocks) || before.Amount != after.Amount || len(after.Blocks) != 5 {
		t.Errorf("before restart %+v\nafter %+v", before, after)
	}
}
