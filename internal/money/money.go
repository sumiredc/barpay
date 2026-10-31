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
