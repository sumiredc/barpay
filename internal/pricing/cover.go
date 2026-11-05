package pricing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// Visit is what a session's cover charge is computed from, as stored.
type Visit struct {
	CheckedInAt time.Time
	// Orders are the times of the visit's orders, in the order they were recorded.
	Orders []time.Time
	// TotalFixedAt is when checkout began or the register was requested; zero while active.
	TotalFixedAt time.Time
}

// Status is the spec's CoverChargeStatus.
type Status string

const (
	Running Status = "running"
	Paused  Status = "paused"
	Capped  Status = "capped"
	Stopped Status = "stopped"
)

// Block is one ended block and its amount, which never changes once the block has ended.
type Block struct {
	Start, End time.Time
	Amount     money.Amount
}

// CoverCharge is a session's cover charge at one instant.
type CoverCharge struct {
	Amount money.Amount // the sum of Blocks
	Status Status
	Blocks []Block // ended blocks, oldest first
	// ResumesAt is, while Status is Paused, the instant the pause ends and blocks are charged
	// again; zero otherwise. It is computed with the pause of the block in progress, so it moves
	// when a later block starts with a changed pause.
	ResumesAt time.Time
}

// ErrNoSettings is returned when no settings or no rate were in effect at an instant the
// computation needs, which a session checked in after start never meets.
var ErrNoSettings = errors.New("pricing: no settings or rate in effect")

// ChargeAt is Charge with the rates and settings changes read from the store. Every lookup and
// every checkout computes the cover charge through it, so the panel and the bill always agree.
func ChargeAt(ctx context.Context, q store.Querier, cur money.Currency, v Visit, at time.Time) (CoverCharge, error) {
	rates, err := History(ctx, q, cur, v.CheckedInAt, -1)
	if err != nil {
		return CoverCharge{}, err
	}
	changes, err := settings.History(ctx, q, v.CheckedInAt)
	if err != nil {
		return CoverCharge{}, err
	}
	return Charge(v, rates, changes, at)
}

// Charge computes the cover charge of v at the instant at (the PRD's Pricing model: Blocks,
// Pause for additional orders, Cap per visit, Settings changes). rates and changes are oldest
// first and include the ones in effect at check-in. It reads nothing but its arguments, so the
// same stored state and instant give the same amount, across a restart too.
//
// Boundaries, each decided by one comparison:
//   - a block has ended at t when its end is at or before t, so a block ending exactly when the
//     total is fixed is charged;
//   - a block is charged at the rate in effect at its end: the last published at or before it;
//   - a block uses the settings in effect at its start, so a change applies from the next block,
//     and a change made exactly when a block ends applies to the block starting then;
//   - a pause runs from the order (included) to the order plus the pause (excluded), so a block
//     ending exactly at an order is free and one ending exactly when a pause ends is charged.
func Charge(v Visit, rates []Point, changes []settings.Change, at time.Time) (CoverCharge, error) {
	limit, stopped := at, false
	if !v.TotalFixedAt.IsZero() && !at.Before(v.TotalFixedAt) {
		limit, stopped = v.TotalFixedAt, true
	}
	s, ok := settingsAt(changes, v.CheckedInAt)
	if !ok {
		return CoverCharge{}, ErrNoSettings
	}
	cur := s.Currency
	cc := CoverCharge{Amount: money.New(0, cur)}
	accrued := int64(0)
	start := v.CheckedInAt
	for {
		s, ok = settingsAt(changes, start)
		if !ok || s.Currency != cur {
			return CoverCharge{}, fmt.Errorf("%w: settings at %v", ErrNoSettings, start)
		}
		minutes := int64(s.BlockMinutes)
		end := start.Add(time.Duration(minutes) * time.Minute)
		if !ended(end, limit) {
			break
		}
		rate, ok := rateAt(rates, end)
		if !ok || rate.Currency() != cur {
			return CoverCharge{}, fmt.Errorf("%w: rate at %v", ErrNoSettings, end)
		}
		amount := rate.Value() * minutes / 60 // rounded down to the smallest unit
		if paused(v.Orders, s.PauseMinutes, end) {
			amount = 0
		}
		if c := s.CapPerVisit.Value(); c > 0 {
			amount = max(0, min(amount, c-accrued))
		}
		accrued += amount
		cc.Blocks = append(cc.Blocks, Block{Start: start, End: end, Amount: money.New(amount, cur)})
		start = end
	}
	cc.Amount = money.New(accrued, cur)

	// The block in progress uses s, the settings at its start.
	resumesAt, inPause := pauseEnd(v.Orders, s.PauseMinutes, at)
	switch c := s.CapPerVisit.Value(); {
	case stopped:
		cc.Status = Stopped
	case c > 0 && accrued >= c:
		cc.Status = Capped
	case inPause:
		cc.Status, cc.ResumesAt = Paused, resumesAt
	default:
		cc.Status = Running
	}
	return cc, nil
}

// ended tells whether something ending at end has ended at t.
func ended(end, t time.Time) bool { return !end.After(t) }

// paused tells whether t is in a pause: from an order after the first, for pauseMinutes.
func paused(orders []time.Time, pauseMinutes int, t time.Time) bool {
	_, in := pauseEnd(orders, pauseMinutes, t)
	return in
}

// pauseEnd is the end of the pause t is in, and whether t is in one. Only the latest order after
// the first at or before t matters, since a new order restarts the pause.
func pauseEnd(orders []time.Time, pauseMinutes int, t time.Time) (time.Time, bool) {
	if pauseMinutes == 0 || len(orders) < 2 {
		return time.Time{}, false
	}
	var latest time.Time
	found := false
	for _, o := range orders[1:] { // the first order of a visit does not count
		if ended(o, t) && (!found || o.After(latest)) {
			latest, found = o, true
		}
	}
	if !found {
		return time.Time{}, false
	}
	end := latest.Add(time.Duration(pauseMinutes) * time.Minute)
	return end, t.Before(end)
}

func rateAt(rates []Point, t time.Time) (money.Amount, bool) {
	var r money.Amount
	ok := false
	for _, p := range rates {
		if p.PublishedAt.After(t) {
			break
		}
		r, ok = p.Rate, true
	}
	return r, ok
}

func settingsAt(changes []settings.Change, t time.Time) (settings.Settings, bool) {
	var s settings.Settings
	ok := false
	for _, c := range changes {
		if c.At.After(t) {
			break
		}
		s, ok = c.Settings, true
	}
	return s, ok
}
