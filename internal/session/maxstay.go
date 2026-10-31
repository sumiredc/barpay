package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/store"
)

// maxStayReached tells whether, at now, a session checked in at checkedIn has reached the
// maximum stay (the PRD's UC3 exception). The PRD leaves the exact instant open; it counts as
// reached.
func (a API) maxStayReached(checkedIn, now time.Time) bool {
	return !now.Before(checkedIn.Add(a.MaxStay))
}

// MaxStayDue handles every active session that reached the maximum stay at the clock's time, and
// returns how many it handled: a linked one is checked out as in UC5 without the gate, with its
// total fixed then; for one that is not linked, max_stay_reached is raised once and the session
// stays active. Only active sessions count: the cover charge of any other has stopped. It decides
// from the stored check-in times alone, so after a restart it handles those that reached it while
// BarPay was stopped.
func (a API) MaxStayDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	rows, err := a.Store.Read().QueryContext(ctx, `SELECT id FROM sessions s WHERE state = 'active' AND checked_in_at <= ?
		AND NOT (paypal_linked_at IS NULL AND EXISTS (SELECT 1 FROM alerts WHERE kind = 'max_stay_reached' AND session_id = s.id))
		ORDER BY seq`, now.Add(-a.MaxStay).UnixNano())
	if err != nil {
		return 0, fmt.Errorf("session: maximum stay: %w", err)
	}
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("session: maximum stay: %w", err)
		}
		due = append(due, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("session: maximum stay: %w", err)
	}
	n := 0
	for _, id := range due {
		handled, err := a.maxStay(ctx, id)
		if err != nil {
			return n, err
		}
		if handled {
			n++
		}
	}
	return n, nil
}

// maxStay handles the session with id if it is active and reached the maximum stay at the time
// read in the transaction, and tells whether it did.
func (a API) maxStay(ctx context.Context, id string) (bool, error) {
	var c *charge
	handled, closed := false, false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, id)
		if err != nil || s.State != Active || !a.maxStayReached(s.CheckedInAt, now) {
			return err
		}
		if s.LinkedAt.IsZero() {
			var raised bool
			err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE kind = 'max_stay_reached' AND session_id = ?)`, id).Scan(&raised)
			if err != nil || raised {
				return err
			}
			handled = true
			_, err = alert.Raise(ctx, tx, alert.Alert{
				Kind: alert.MaxStayReached, RaisedAt: now, SessionID: id,
				Message: "The session reached the maximum stay without PayPal linked, so it stays open. Speak to the customer.",
			})
			return err
		}
		// The total is fixed at the maximum stay itself, however late BarPay reaches it.
		_, c, err = a.begin(ctx, tx, s, checkout, s.CheckedInAt.Add(a.MaxStay), now)
		if errors.Is(err, errNoSavedPaypal) {
			return nil
		}
		handled, closed = err == nil, err == nil && c == nil
		return err
	})
	if err != nil {
		return false, err
	}
	if closed {
		a.Notify.Wake()
	}
	if c != nil {
		_, err = a.send(ctx, *c, nil)
	}
	return handled, err
}
