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

// MaxStayDue raises max_stay_reached once for every active session that reached the maximum stay
// at the clock's time, linked or not, and returns how many it raised it for. The session stays
// active: BarPay never checks it out at the maximum stay, so nothing is charged that the customer
// did not choose. Only active sessions count: the cover charge of any other has stopped. It
// decides from the stored check-in times and alerts alone, so after a restart it raises the
// alert for those that reached it while BarPay was stopped, and never twice.
func (a API) MaxStayDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	rows, err := a.Store.Read().QueryContext(ctx, `SELECT id FROM sessions s WHERE state = 'active' AND checked_in_at <= ?
		AND NOT EXISTS (SELECT 1 FROM alerts WHERE kind = 'max_stay_reached' AND session_id = s.id)
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
		raised, err := a.maxStay(ctx, id)
		if err != nil {
			return n, err
		}
		if raised {
			n++
		}
	}
	return n, nil
}

// maxStay raises max_stay_reached for the session with id if it is active, reached the maximum
// stay at the time read in the transaction and has not had it raised, and tells whether it did.
func (a API) maxStay(ctx context.Context, id string) (bool, error) {
	raised := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, id)
		if err != nil || s.State != Active || !a.maxStayReached(s.CheckedInAt, now) {
			return err
		}
		var before bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE kind = 'max_stay_reached' AND session_id = ?)`, id).Scan(&before)
		if err != nil || before {
			return err
		}
		raised = true
		_, err = alert.Raise(ctx, tx, alert.Alert{
			Kind: alert.MaxStayReached, RaisedAt: now, SessionID: id,
			Message: "The session reached the maximum stay and stays open. Speak to the customer.",
		})
		return err
	})
	return raised, err
}
