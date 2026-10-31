package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/store"
)

// Open is a member's session that has not ended, as the member's saved PayPal needs to know it.
type Open struct {
	ID    string
	State State
	// Linked is whether it is linked to the member's saved PayPal. A linked session uses the saved
	// PayPal until it ends, also while at the register (the PRD's UC9 exception).
	Linked bool
}

// OpenOf returns memberID's session that has not ended, reading through q, and false when there
// is none. A member has at most one.
func OpenOf(ctx context.Context, q store.Querier, memberID string) (Open, bool, error) {
	var o Open
	var state string
	var linked sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, state, paypal_linked_at FROM sessions
		WHERE member_id = ? AND state NOT IN `+endedStates, memberID).Scan(&o.ID, &state, &linked)
	if errors.Is(err, sql.ErrNoRows) {
		return Open{}, false, nil
	}
	if err != nil {
		return Open{}, false, fmt.Errorf("session: open of member: %w", err)
	}
	o.State, o.Linked = State(state), linked.Valid
	return o, true, nil
}

// Unlinked is an active member's session that is not linked.
type Unlinked struct {
	ID       string
	MemberID string
}

// ActiveUnlinked returns every active session of a member that is not linked, in check-in order,
// reading through q: the sessions a saved PayPal could still be linked to (the PRD's UC1).
func ActiveUnlinked(ctx context.Context, q store.Querier) ([]Unlinked, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, member_id FROM sessions
		WHERE state = 'active' AND member_id IS NOT NULL AND paypal_linked_at IS NULL ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("session: active unlinked: %w", err)
	}
	defer rows.Close()
	var out []Unlinked
	for rows.Next() {
		var u Unlinked
		if err := rows.Scan(&u.ID, &u.MemberID); err != nil {
			return nil, fmt.Errorf("session: active unlinked: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Link links the session with id to its member's saved PayPal at now, inside tx, and stores its
// session.paypal_linked. Only an active session that is not linked yet is linked: one that has
// left active is not, so a session that chose the register never shows as paying by PayPal
// (the PRD's UC1 and UC2 exceptions). It tells whether it linked the session; the caller, who
// has checked that the member's saved PayPal is there, wakes the dispatcher after the commit.
func Link(ctx context.Context, tx store.Querier, id string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET paypal_linked_at = ?
		WHERE id = ? AND state = 'active' AND member_id IS NOT NULL AND paypal_linked_at IS NULL`, now.UnixNano(), id)
	if err != nil {
		return false, fmt.Errorf("session: link: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("session: link: %w", err)
	} else if n == 0 {
		return false, nil
	}
	s, err := get(ctx, tx, id)
	if err != nil {
		return false, err
	}
	v, err := view(ctx, tx, s, now)
	if err != nil {
		return false, err
	}
	_, err = notify.Enqueue(ctx, tx, now, notify.Event{Type: notify.SessionPaypalLinked, SubjectID: id, Data: sessionData{Session: v}})
	return err == nil, err
}
