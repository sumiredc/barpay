package money

import (
	"errors"
	"testing"
)

func TestSameCurrencyArithmetic(t *testing.T) {
	a, b := New(1250, USD), New(250, USD)
	if got, err := a.Add(b); err != nil || got != New(1500, USD) {
		t.Errorf("Add = %v, %v; want 1500 USD", got, err)
	}
	if got, err := a.Sub(b); err != nil || got != New(1000, USD) {
		t.Errorf("Sub = %v, %v; want 1000 USD", got, err)
	}
	for _, c := range []struct {
		x, y Amount
		want int
	}{{b, a, -1}, {a, a, 0}, {a, b, 1}} {
		if got, err := c.x.Cmp(c.y); err != nil || got != c.want {
			t.Errorf("%v.Cmp(%v) = %d, %v; want %d", c.x, c.y, got, err, c.want)
		}
	}
}

func TestDifferentCurrenciesAreRejected(t *testing.T) {
	usd, jpy := New(100, USD), New(100, JPY)
	ops := map[string]func() error{
		"Add": func() error { _, err := usd.Add(jpy); return err },
		"Sub": func() error { _, err := usd.Sub(jpy); return err },
		"Cmp": func() error { _, err := usd.Cmp(jpy); return err },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("%s across USD and JPY: err = %v, want ErrCurrencyMismatch", name, err)
		}
	}
}

func TestZeroAmountMatchesNothing(t *testing.T) {
	if _, err := (Amount{}).Cmp(Amount{}); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp of two zero Amounts: err = %v, want ErrCurrencyMismatch", err)
	}
}

func TestRateStep(t *testing.T) {
	for c, want := range map[Currency]Amount{USD: New(5, USD), JPY: New(10, JPY)} {
		if got, ok := RateStep(c); !ok || got != want {
			t.Errorf("RateStep(%s) = %v, %v; want %v", c, got, ok, want)
		}
	}
	if _, ok := RateStep("EUR"); ok {
		t.Error("RateStep(EUR) is defined")
	}
}

func TestIsMultipleOf(t *testing.T) {
	step := New(5, USD)
	for v, want := range map[int64]bool{0: true, 5: true, 600: true, -5: true, 4: false, 6: false, 1199: false} {
		if got := New(v, USD).IsMultipleOf(step); got != want {
			t.Errorf("%d.IsMultipleOf(5) = %v, want %v", v, got, want)
		}
	}
	if New(10, JPY).IsMultipleOf(step) {
		t.Error("an amount in another currency is a multiple")
	}
}
