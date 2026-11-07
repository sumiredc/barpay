package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

// trigger is what moves a session along an edge of the PRD's state diagram in Use cases: an
// operation, or a time limit reached.
type trigger string

const (
	registerRequested       trigger = "the register is requested"
	registerPaymentRecorded trigger = "staff record a register payment"
	markedUnpaid            trigger = "staff mark it unpaid"
	unpaidLimit             trigger = "the 3-day limit is reached"
	checkout                trigger = "the exit QR is scanned or staff close it"
	charged                 trigger = "the full total is charged"
	chargeFailed            trigger = "the charge failed or could not be confirmed"
	retried                 trigger = "staff retry the charge"
	outsideSettled          trigger = "staff settle it outside BarPay"
)

// edge is the states a trigger moves a session from, and the state it moves it to.
type edge struct {
	from []State
	to   State
}

// edges are the PRD's state diagram, by trigger. Every change of a session's state is one of
// them, made by move.
var edges = map[trigger]edge{
	registerRequested:       {from: []State{Active}, to: AtRegister},
	registerPaymentRecorded: {from: []State{Active, AtRegister}, to: Closed},
	markedUnpaid:            {from: []State{AtRegister}, to: ClosedUnpaid},
	unpaidLimit:             {from: limitedStates, to: ClosedUnpaid},
	checkout:                {from: []State{Active}, to: CheckingOut},
	charged:                 {from: []State{CheckingOut}, to: Closed},
	chargeFailed:            {from: []State{CheckingOut}, to: PaymentFailed},
	retried:                 {from: []State{PaymentFailed}, to: CheckingOut},
	outsideSettled:          {from: []State{PaymentFailed}, to: Closed},
}

// needsLink are the triggers that charge the session's saved PayPal from active: only a session
// linked to it is checked out (UC5 precondition).
var needsLink = []trigger{checkout}

// errRefused is returned by move when the trigger does not apply to the session at that instant.
var errRefused = errors.New("session: the session's state does not allow this")

// errPastLimit is the errRefused of a trigger whose edge leaves the session's state, refused
// because the session reached its 3-day limit: from then on only the limit moves it, even before
// that close is stored.
var errPastLimit = fmt.Errorf("%w: the session reached its 3-day limit", errRefused)

// errNotLinked is the errRefused of a trigger in needsLink for a session that is not linked.
var errNotLinked = fmt.Errorf("%w: the session has no PayPal linked", errRefused)

// judge tells whether t moves s at now: nil if it does, errPastLimit, errNotLinked or errRefused
// if not.
func judge(s Session, t trigger, now time.Time) error {
	if !slices.Contains(edges[t].from, s.State) {
		return errRefused
	}
	if slices.Contains(needsLink, t) && s.LinkedAt.IsZero() {
		return errNotLinked
	}
	switch past := pastLimit(s, now); {
	case past && t != unpaidLimit:
		return errPastLimit
	case !past && t == unpaidLimit:
		return errRefused
	}
	return nil
}

// move is the one place that changes a session's state: it moves s, as read in tx, along t's edge
// at now, and returns it as moved. Leaving active fixes the total at now. When t does not apply,
// it returns judge's error, or errRefused when the state changed since s was read, and changes
// nothing. The update is conditional on the state s was read in, so of two requests that read
// the same state only one moves it, even if the store ever lets both read before either writes.
func move(ctx context.Context, tx store.Querier, s Session, t trigger, now time.Time) (Session, error) {
	if err := judge(s, t, now); err != nil {
		return s, err
	}
	fixed := s.TotalFixedAt
	if fixed.IsZero() {
		fixed = now
	}
	to := edges[t].to
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, total_fixed_at = ? WHERE id = ? AND state = ?`,
		string(to), fixed.UnixNano(), s.ID, string(s.State))
	if err != nil {
		return s, fmt.Errorf("session: move: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return s, fmt.Errorf("session: move: %w", err)
	} else if n == 0 {
		return s, errRefused
	}
	s.State, s.TotalFixedAt = to, fixed
	return s, nil
}
