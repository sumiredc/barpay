// Package settings holds the owner's settings (the API spec's Settings) and the one check that
// applies both to a change through the admin API (UC7) and to the starting values in the config file (UC8).
package settings

import (
	"fmt"
	"strings"

	"github.com/sumiredc/barpay/internal/money"
)

// Limits from the API spec's Settings and Money schemas.
const (
	maxAmount       = money.MaxValue
	maxBlockMinutes = 240
	maxPauseMinutes = 240
	maxGoal         = 100
)

// Settings are in effect for the whole store. Currency is the store's currency, and every amount is in it.
type Settings struct {
	Currency     money.Currency
	Floor        money.Amount // rate per hour
	Ceiling      money.Amount // rate per hour
	BlockMinutes int
	PauseMinutes int          // 0 turns the pause off
	CapPerVisit  money.Amount // 0 means no cap
	Goal         int          // 0 "fill the room" … 100 "maximize revenue"
}

// FieldError names one setting that breaks a condition. Field is the spec's property name.
type FieldError struct {
	Field  string
	Reason string
}

func (e FieldError) Error() string { return e.Field + ": " + e.Reason }

// Errors lists every broken condition, in the order of the spec's properties.
type Errors []FieldError

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return "invalid settings: " + strings.Join(parts, "; ")
}

// Validate checks the conditions of the PRD's UC7 (floor and ceiling above 0, multiples of the
// rate step, floor below ceiling, block of at least 1 minute, pause and cap of 0 or more), the
// spec's ranges, and that every amount is in s.Currency. It returns nil or Errors.
func (s Settings) Validate() error {
	var es Errors
	add := func(field, format string, args ...any) {
		es = append(es, FieldError{Field: field, Reason: fmt.Sprintf(format, args...)})
	}
	amount := func(field string, a money.Amount, min int64) bool {
		switch {
		case a.Currency() != s.Currency:
			add(field, "must be in %s, not %q", s.Currency, a.Currency())
		case a.Value() < min:
			add(field, "must be at least %d", min)
		case a.Value() > maxAmount:
			add(field, "must be at most %d", maxAmount)
		default:
			return true
		}
		return false
	}

	// A floor and ceiling on the rate step leave a multiple of it between them, so the
	// guardrails can always round a rate without leaving the range.
	onStep := func(field string, a money.Amount) bool {
		step, ok := money.RateStep(s.Currency)
		if !ok || a.IsMultipleOf(step) {
			return true
		}
		add(field, "must be a multiple of the rate step (%d)", step.Value())
		return false
	}
	floorOK := amount("floor", s.Floor, 1) && onStep("floor", s.Floor)
	ceilingOK := amount("ceiling", s.Ceiling, 1) && onStep("ceiling", s.Ceiling)
	if floorOK && ceilingOK && s.Floor.Value() >= s.Ceiling.Value() {
		add("floor", "must be below the ceiling (%d)", s.Ceiling.Value())
	}
	if s.BlockMinutes < 1 || s.BlockMinutes > maxBlockMinutes {
		add("block_minutes", "must be from 1 to %d", maxBlockMinutes)
	}
	if s.PauseMinutes < 0 || s.PauseMinutes > maxPauseMinutes {
		add("pause_minutes", "must be from 0 to %d", maxPauseMinutes)
	}
	amount("cap_per_visit", s.CapPerVisit, 0)
	if s.Goal < 0 || s.Goal > maxGoal {
		add("goal", "must be from 0 to %d", maxGoal)
	}
	if es != nil {
		return es
	}
	return nil
}
