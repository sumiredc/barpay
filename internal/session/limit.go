package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

// unpaidAfter is the PRD's 3-day limit in Use cases (UC6 and UC7 exceptions): how long after its
// total was fixed a session in one of limitedStates is closed as unpaid.
const unpaidAfter = 3 * 24 * time.Hour

// limitedStates are the states the 3-day limit applies to: the session is closed as unpaid
// when it is reached in one of them (the PRD's state diagram in Use cases). It does not apply
// while checking out: a charge whose outcome is not known yet may still succeed (UC7 exception).
var limitedStates = []State{AtRegister, PaymentFailed}

// unpaidLimitReached tells whether, at now, the 3-day limit of a session whose total was fixed at
// fixedAt has been reached. It is reached at exactly 3 days: from that instant the session is
// closed as unpaid, and until just before it staff can still act on it.
func unpaidLimitReached(fixedAt, now time.Time) bool { return !fixedAt.Add(unpaidAfter).After(now) }

// pastLimit tells whether s, as read, has reached its 3-day limit at now.
func pastLimit(s Session, now time.Time) bool {
	return slices.Contains(limitedStates, s.State) && unpaidLimitReached(s.TotalFixedAt, now)
}

// poll is how often Run looks for what fell due. It only wakes the loop: what is due is decided
// from the stored times.
const poll = time.Second

// Run, when it starts and every second until ctx ends, closes the sessions that reached their
// 3-day limit, sends again the charges whose outcome is unknown, and checks out or raises an
// alert for the sessions at the maximum stay.
func (a API) Run(ctx context.Context, log *slog.Logger) {
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if _, err := a.CloseDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("sessions past their limit not closed", "error", err)
		}
		if _, err := a.ChargeDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("charges not sent again", "error", err)
		}
		if _, err := a.MaxStayDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("sessions at the maximum stay not handled", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// CloseDue closes as unpaid every session that reached its 3-day limit at the clock's time, and
// returns how many it closed. It decides from the stored times alone, so after a restart it
// closes those that reached it while BarPay was stopped.
func (a API) CloseDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	states := make([]any, len(limitedStates))
	for i, s := range limitedStates {
		states[i] = string(s)
	}
	rows, err := a.Store.Read().QueryContext(ctx, `SELECT id, state, total_fixed_at FROM sessions
		WHERE state IN (?`+strings.Repeat(", ?", len(states)-1)+`)`, states...)
	if err != nil {
		return 0, fmt.Errorf("session: due: %w", err)
	}
	var due []string
	for rows.Next() {
		var s Session
		var state string
		var fixed int64
		if err := rows.Scan(&s.ID, &state, &fixed); err != nil {
			rows.Close()
			return 0, fmt.Errorf("session: due: %w", err)
		}
		s.State, s.TotalFixedAt = State(state), time.Unix(0, fixed)
		if pastLimit(s, now) {
			due = append(due, s.ID)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("session: due: %w", err)
	}
	n := 0
	defer func() {
		if n > 0 {
			a.Notify.Wake()
		}
	}()
	for _, id := range due {
		closed, err := a.closeDue(ctx, id)
		if err != nil {
			return n, err
		}
		if closed {
			n++
		}
	}
	return n, nil
}

// closeIfDue closes the session with id as unpaid if it reached its 3-day limit, before an
// operation on it or a lookup, so from that instant the session is shown and treated as ended
// even when Run has not reached it yet.
func (a API) closeIfDue(ctx context.Context, id string) error {
	closed, err := a.closeDue(ctx, id)
	if closed {
		a.Notify.Wake()
	}
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// closeDue closes the session with id as unpaid, with its receipt and session.closed_unpaid, if
// it reached its limit at the time read in the transaction. It tells whether it closed it; the
// caller wakes the dispatcher.
func (a API) closeDue(ctx context.Context, id string) (bool, error) {
	closed := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		v, err := end(ctx, tx, s, unpaidLimit, now, settledUnpaid, proceeds{})
		if errors.Is(err, errRefused) {
			return nil
		}
		if err != nil {
			return err
		}
		closed = true
		return enqueueUnpaid(ctx, tx, now, v)
	})
	return closed, err
}
