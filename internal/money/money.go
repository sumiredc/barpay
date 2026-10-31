// Package money holds amounts as integers in a currency's smallest unit.
package money

import (
	"errors"
	"fmt"
)

// Currency is a currency code from the API spec's Currency.
type Currency string

const (
	JPY Currency = "JPY"
	USD Currency = "USD"
)

// MaxValue is the spec's Money maximum, in the smallest unit.
const MaxValue = 100_000_000

// MaxTotal is the spec's Total maximum, for sums such as a session's total: the largest
// integer a JSON reader keeps exactly.
const MaxTotal = 1<<53 - 1

// Known lists every currency in the API spec, whether or not BarPay can start with it.
var Known = []Currency{JPY, USD}

// ErrCurrencyMismatch is returned when two amounts in different currencies are compared or combined.
var ErrCurrencyMismatch = errors.New("money: amounts are in different currencies")

// Amount is a value in the smallest unit of its currency: yen for JPY, cents for USD.
// The zero Amount has no currency and mismatches every amount, itself included.
type Amount struct {
	value    int64
	currency Currency
}

// New returns value smallest units of c.
func New(value int64, c Currency) Amount {
	return Amount{value: value, currency: c}
}

func (a Amount) Value() int64       { return a.value }
func (a Amount) Currency() Currency { return a.currency }

func (a Amount) String() string {
	return fmt.Sprintf("%d %s", a.value, a.currency)
}

func (a Amount) sameCurrency(b Amount) error {
	if a.currency == "" || a.currency != b.currency {
		return fmt.Errorf("%w: %q and %q", ErrCurrencyMismatch, a.currency, b.currency)
	}
	return nil
}

// Add returns a + b, or ErrCurrencyMismatch.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	return Amount{value: a.value + b.value, currency: a.currency}, nil
}

// Sub returns a - b, or ErrCurrencyMismatch.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	return Amount{value: a.value - b.value, currency: a.currency}, nil
}

// Cmp returns -1, 0 or +1 as a is less than, equal to or greater than b, or ErrCurrencyMismatch.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := a.sameCurrency(b); err != nil {
		return 0, err
	}
	switch {
	case a.value < b.value:
		return -1, nil
	case a.value > b.value:
		return 1, nil
	}
	return 0, nil
}

// rateSteps are the PRD's Pricing model, Guardrails: every published rate, floor and ceiling is a
// multiple of its currency's step.
var rateSteps = map[Currency]int64{JPY: 10, USD: 5}

// RateStep returns the rate step of c, or ok == false for a currency the spec does not list.
func RateStep(c Currency) (step Amount, ok bool) {
	v, ok := rateSteps[c]
	return Amount{value: v, currency: c}, ok
}

// IsMultipleOf tells whether a is a whole number of steps, in step's currency.
func (a Amount) IsMultipleOf(step Amount) bool {
	return a.sameCurrency(step) == nil && step.value > 0 && a.value%step.value == 0
}
