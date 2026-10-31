// Package pricing decides and publishes the rate within the guardrails, and computes the cover
// charge from the published rates and the settings (the PRD's Pricing model).
package pricing

import (
	"errors"
	"fmt"

	"github.com/sumiredc/barpay/internal/money"
)

// Trigger is why a rate is guarded. Decision logs use the spec's names for the last two.
type Trigger string

const (
	// TriggerFirst: no rate is published yet in the currency (the PRD's Pricing model, First rate).
	TriggerFirst Trigger = "first"
	// TriggerEvaluation: the AI proposed a rate.
	TriggerEvaluation Trigger = "evaluation"
	// TriggerRangeChange: the owner moved the floor or ceiling.
	TriggerRangeChange Trigger = "range_change"
)

// Limits are the guardrails: the settings' floor and ceiling, and the config file's max step.
type Limits struct {
	Floor   money.Amount
	Ceiling money.Amount
	MaxStep money.Amount
}

// Guarded is a rate that passed the guardrails. Only this package's guard makes one, so code
// that publishes a rate cannot be handed a raw proposal.
type Guarded struct {
	trigger Trigger
	rate    money.Amount
	change  money.Amount
}

func (g Guarded) Trigger() Trigger     { return g.trigger }
func (g Guarded) Rate() money.Amount   { return g.rate }
func (g Guarded) Change() money.Amount { return g.change } // from the current rate; 0 for TriggerFirst

// ErrLimits is returned when the limits or the current rate break the conditions the config file
// and settings.Validate enforce; a guard with them could not keep its promises.
var ErrLimits = errors.New("pricing: limits are not on the rate step or not in order")

// Guard decides the rate to publish for an AI proposal, which may be anything, including
// negative or far above the ceiling.
func Guard(current, proposal money.Amount, l Limits) (Guarded, error) {
	return guard(TriggerEvaluation, current, proposal, l)
}

// FitRange moves the current rate into a new floor and ceiling, beyond the max step if needed.
// Change is 0 when the rate is already inside.
func FitRange(current money.Amount, l Limits) (Guarded, error) {
	return guard(TriggerRangeChange, current, current, l)
}

// First decides the first rate in a currency from the starting rate. There is no rate to step from.
func First(starting money.Amount, l Limits) (Guarded, error) {
	return guard(TriggerFirst, money.Amount{}, starting, l)
}

// guard is the PRD's Pricing model, Guardrails, in this order: clamp the proposal into the
// floor and ceiling; for an evaluation, also into the max step around the current rate, where
// the range wins if the two do not meet; then round to the nearest multiple of the rate step,
// a tie going toward the current rate. The floor, ceiling, max step and current rate are
// multiples of the step, so rounding never leaves the window the clamping made.
func guard(t Trigger, current, proposal money.Amount, l Limits) (Guarded, error) {
	cur := l.Floor.Currency()
	step, ok := money.RateStep(cur)
	if !ok {
		return Guarded{}, fmt.Errorf("%w: no rate step for %q", ErrLimits, cur)
	}
	same := []money.Amount{l.Ceiling, proposal}
	if t != TriggerFirst {
		same = append(same, current)
	}
	if t == TriggerEvaluation {
		same = append(same, l.MaxStep)
	}
	for _, a := range same {
		if a.Currency() != cur {
			return Guarded{}, fmt.Errorf("%w: %v and %v", money.ErrCurrencyMismatch, a, l.Floor)
		}
	}
	floor, ceiling := l.Floor.Value(), l.Ceiling.Value()
	if floor <= 0 || floor >= ceiling || !l.Floor.IsMultipleOf(step) || !l.Ceiling.IsMultipleOf(step) {
		return Guarded{}, fmt.Errorf("%w: floor %v, ceiling %v", ErrLimits, l.Floor, l.Ceiling)
	}
	if t != TriggerFirst && !current.IsMultipleOf(step) {
		return Guarded{}, fmt.Errorf("%w: current rate %v", ErrLimits, current)
	}

	lo, hi := floor, ceiling
	if t == TriggerEvaluation {
		if l.MaxStep.Value() < step.Value() || !l.MaxStep.IsMultipleOf(step) {
			return Guarded{}, fmt.Errorf("%w: max step %v", ErrLimits, l.MaxStep)
		}
		c, m := current.Value(), l.MaxStep.Value()
		lo, hi = max(lo, c-m), min(hi, c+m)
		if lo > hi { // the current rate is further outside the range than one step
			if c < floor {
				lo, hi = floor, floor
			} else {
				lo, hi = ceiling, ceiling
			}
		}
	}
	x := min(max(proposal.Value(), lo), hi)
	toward := x
	if t != TriggerFirst {
		toward = current.Value()
	}
	rate := money.New(roundToStep(x, step.Value(), toward), cur)

	change := money.New(0, cur)
	if t != TriggerFirst {
		change = money.New(rate.Value()-current.Value(), cur)
	}
	return Guarded{trigger: t, rate: rate, change: change}, nil
}

func roundToStep(x, step, toward int64) int64 {
	lower := x / step * step
	if lower > x {
		lower -= step
	}
	if lower == x {
		return x
	}
	upper := lower + step
	switch dl, du := x-lower, upper-x; {
	case dl < du:
		return lower
	case dl > du:
		return upper
	}
	if abs(toward-upper) < abs(toward-lower) {
		return upper
	}
	return lower
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
