package pricing

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/sumiredc/barpay/internal/money"
)

func usd(v int64) money.Amount { return money.New(v, money.USD) }

// $6.00–$24.00 per hour, at most $3.00 per evaluation.
var limits = Limits{Floor: usd(600), Ceiling: usd(2400), MaxStep: usd(300)}

func TestGuardClampsAndRounds(t *testing.T) {
	cases := []struct {
		name              string
		current, proposal int64
		want              int64
	}{
		{"inside, on the step", 1200, 1350, 1350},
		{"unchanged", 1200, 1200, 1200},
		{"beyond the max step up", 1200, 2000, 1500},
		{"beyond the max step down", 1200, 100, 900},
		{"exactly the max step", 1200, 1500, 1500},
		{"above the ceiling", 2300, 99_999_999, 2400},
		{"below the floor", 700, 1, 600},
		{"negative", 700, -500, 600},
		{"most negative", 700, math.MinInt64, 600},
		{"largest", 2300, math.MaxInt64, 2400},
		{"zero", 650, 0, 600},
		{"rounds down to the nearest step", 1200, 1302, 1300},
		{"rounds up to the nearest step", 1200, 1303, 1305},
		{"rounds inside the max step", 1200, 1499, 1500},
		{"rounds inside the range", 2350, 2399, 2400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := Guard(usd(c.current), usd(c.proposal), limits)
			if err != nil {
				t.Fatal(err)
			}
			if g.Rate() != usd(c.want) || g.Change() != usd(c.want-c.current) || g.Trigger() != TriggerEvaluation {
				t.Errorf("Guard(%d, %d) = %v (change %v), want %d", c.current, c.proposal, g.Rate(), g.Change(), c.want)
			}
		})
	}
}

// With a step of 5 a half-way proposal is not an integer, so the tie rule is tested with JPY's step of 10.
func TestGuardTieGoesTowardCurrentRate(t *testing.T) {
	jpy := func(v int64) money.Amount { return money.New(v, money.JPY) }
	l := Limits{Floor: jpy(600), Ceiling: jpy(2400), MaxStep: jpy(300)}
	for _, c := range []struct{ current, proposal, want int64 }{
		{1000, 1105, 1100}, // rising: 1100 is nearer the current rate
		{1200, 1105, 1110}, // falling: 1110 is nearer
	} {
		g, err := Guard(jpy(c.current), jpy(c.proposal), l)
		if err != nil || g.Rate() != jpy(c.want) {
			t.Errorf("Guard(%d, %d) = %v, %v; want %d", c.current, c.proposal, g.Rate(), err, c.want)
		}
	}
}

// When the current rate is further outside the range than the max step (which the range change
// prevents), the range still wins.
func TestGuardRangeWinsOverMaxStep(t *testing.T) {
	for _, c := range []struct{ current, want int64 }{{100, 600}, {9000, 2400}} {
		g, err := Guard(usd(c.current), usd(c.current), limits)
		if err != nil || g.Rate() != usd(c.want) {
			t.Errorf("Guard from %d = %v, %v; want %d", c.current, g.Rate(), err, c.want)
		}
	}
}

// Any proposal — the property the PRD's Guarantees, Price bounds states.
func TestGuardPropertyAnyProposal(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 20000 {
		floor := 5 * (1 + r.Int64N(1000))
		ceiling := floor + 5*(1+r.Int64N(1000))
		maxStep := 5 * (1 + r.Int64N(100))
		current := floor + 5*r.Int64N((ceiling-floor)/5+1)
		var proposal int64
		switch i % 4 {
		case 0:
			proposal = r.Int64() - math.MaxInt64/2
		case 1:
			proposal = current + r.Int64N(4*maxStep+1) - 2*maxStep
		case 2:
			proposal = -r.Int64N(1 << 40)
		default:
			proposal = floor + r.Int64N(ceiling-floor+1)
		}
		l := Limits{Floor: usd(floor), Ceiling: usd(ceiling), MaxStep: usd(maxStep)}
		g, err := Guard(usd(current), usd(proposal), l)
		if err != nil {
			t.Fatal(err)
		}
		v := g.Rate().Value()
		if v < floor || v > ceiling || v%5 != 0 || abs(v-current) > maxStep || g.Change().Value() != v-current {
			t.Fatalf("Guard(current %d, proposal %d, floor %d, ceiling %d, step %d) = %d", current, proposal, floor, ceiling, maxStep, v)
		}
		if again, _ := Guard(usd(current), usd(proposal), l); again != g {
			t.Fatalf("Guard is not deterministic: %v then %v", g, again)
		}
	}
}

func TestFitRange(t *testing.T) {
	cases := []struct {
		name           string
		current        int64
		floor, ceiling int64
		want           int64
		wantUnchanged  bool
	}{
		{"floor raised above the rate, beyond the max step", 700, 1500, 2400, 1500, false},
		{"ceiling lowered below the rate, beyond the max step", 2400, 600, 1000, 1000, false},
		{"rate stays inside", 1200, 1000, 1500, 1200, true},
		{"rate on the new floor", 1000, 1000, 1500, 1000, true},
		{"rate on the new ceiling", 1500, 1000, 1500, 1500, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, err := FitRange(usd(c.current), Limits{Floor: usd(c.floor), Ceiling: usd(c.ceiling), MaxStep: usd(5)})
			if err != nil {
				t.Fatal(err)
			}
			if g.Rate() != usd(c.want) || (g.Change().Value() == 0) != c.wantUnchanged || g.Trigger() != TriggerRangeChange {
				t.Errorf("FitRange(%d into %d–%d) = %v (change %v), want %d", c.current, c.floor, c.ceiling, g.Rate(), g.Change(), c.want)
			}
		})
	}
}

func TestFirst(t *testing.T) {
	for _, c := range []struct{ starting, want int64 }{{1200, 1200}, {100, 600}, {5000, 2400}} {
		g, err := First(usd(c.starting), limits)
		if err != nil || g.Rate() != usd(c.want) || g.Change() != usd(0) || g.Trigger() != TriggerFirst {
			t.Errorf("First(%d) = %v (change %v), %v; want %d with change 0", c.starting, g.Rate(), g.Change(), err, c.want)
		}
	}
}

func TestGuardRefusesBrokenLimits(t *testing.T) {
	cases := map[string]struct {
		current, proposal money.Amount
		l                 Limits
		want              error
	}{
		"floor off the step":   {usd(1200), usd(1200), Limits{usd(601), usd(2400), usd(300)}, ErrLimits},
		"ceiling off the step": {usd(1200), usd(1200), Limits{usd(600), usd(2401), usd(300)}, ErrLimits},
		"floor not below":      {usd(1200), usd(1200), Limits{usd(2400), usd(2400), usd(300)}, ErrLimits},
		"floor 0":              {usd(5), usd(5), Limits{usd(0), usd(2400), usd(300)}, ErrLimits},
		"max step 0":           {usd(1200), usd(1200), Limits{usd(600), usd(2400), usd(0)}, ErrLimits},
		"max step off":         {usd(1200), usd(1200), Limits{usd(600), usd(2400), usd(301)}, ErrLimits},
		"current off":          {usd(1201), usd(1200), limits, ErrLimits},
		"proposal in JPY":      {usd(1200), money.New(1200, money.JPY), limits, money.ErrCurrencyMismatch},
		"current in JPY":       {money.New(1200, money.JPY), usd(1200), limits, money.ErrCurrencyMismatch},
	}
	for name, c := range cases {
		if _, err := Guard(c.current, c.proposal, c.l); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}
