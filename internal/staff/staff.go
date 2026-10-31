// Package staff records which staff member made each change through the admin API (the PRD's UC7 postcondition).
package staff

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

// Header carries the staff ID on admin requests (the spec's StaffId parameter).
const Header = "BarPay-Staff-Id"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ID is the store app's ID of the staff member acting. BarPay trusts it as sent.
type ID string

// ErrInvalidID is returned for an ID that does not fit the spec's StaffId.
var ErrInvalidID = errors.New("staff: malformed staff ID")

// ParseID checks s against the spec's StaffId.
func ParseID(s string) (ID, error) {
	if !idPattern.MatchString(s) {
		return "", ErrInvalidID
	}
	return ID(s), nil
}

// Change describes one change an admin request makes.
type Change struct {
	Staff ID
	// Operation is the spec's operationId, such as "updateSettings".
	Operation string
	// Subject is what changed: a session ID, or a name such as "settings".
	Subject string
	At      time.Time
}

// Action is a Change as stored.
type Action struct {
	Change
	Seq int64 // order of recording
}

// Tx runs fn in one transaction with c recorded in it, so the staff member is stored exactly
// when the change is. Every admin request that changes something makes its change through Tx.
func Tx(ctx context.Context, st *store.Store, c Change, fn func(tx store.Querier) error) error {
	if !idPattern.MatchString(string(c.Staff)) {
		return ErrInvalidID
	}
	if c.Operation == "" || c.Subject == "" || c.At.IsZero() {
		return errors.New("staff: change needs an operation, a subject and a time")
	}
	return st.Tx(ctx, func(tx store.Querier) error {
		if err := fn(tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO staff_actions (staff_id, operation, subject, at) VALUES (?, ?, ?, ?)`,
			string(c.Staff), c.Operation, c.Subject, c.At.UnixNano())
		if err != nil {
			return fmt.Errorf("staff: record: %w", err)
		}
		return nil
	})
}

// Actions returns the recorded changes to subject, oldest first.
func Actions(ctx context.Context, q store.Querier, subject string) ([]Action, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq, staff_id, operation, subject, at FROM staff_actions
		WHERE subject = ? ORDER BY seq`, subject)
	if err != nil {
		return nil, fmt.Errorf("staff: actions: %w", err)
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var id string
		var at int64
		if err := rows.Scan(&a.Seq, &id, &a.Operation, &a.Subject, &at); err != nil {
			return nil, fmt.Errorf("staff: actions: %w", err)
		}
		a.Staff, a.At = ID(id), time.Unix(0, at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
