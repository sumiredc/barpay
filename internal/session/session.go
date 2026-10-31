// Package session keeps visits (the PRD's Use cases): it starts them at check-in, records their
// orders and shows them by lookup, and serves those operations of the API spec.
package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/store"
)

// State is the spec's SessionState: a state of the PRD's state diagram in Use cases.
type State string

const (
	Active        State = "active"
	CheckingOut   State = "checking_out"
	AtRegister    State = "at_register"
	PaymentFailed State = "payment_failed"
	Closed        State = "closed"
	ClosedUnpaid  State = "closed_unpaid"
)

// States is the spec's enum. The sessions table's CHECK constraint lists the same values.
var States = []State{Active, CheckingOut, AtRegister, PaymentFailed, Closed, ClosedUnpaid}

// endedStates are the states of a session that has ended. Every other state is open: a member may
// have only one open session, and a currency change waits until none is open. The sessions
// table's unique index on open sessions lists the same.
const endedStates = `('closed', 'closed_unpaid')`

// errNotFound is returned for a session ID that names no session.
var errNotFound = errors.New("session: not found")

// Session is a visit as stored.
type Session struct {
	ID          string
	MemberID    string // empty for a guest
	State       State
	Currency    money.Currency
	CheckedInAt time.Time
	// TotalFixedAt is when checkout began or the register was requested; zero while active.
	TotalFixedAt time.Time
}

// newID returns "ses_" and 26 random base32 characters (130 bits), which fit the spec's SessionId.
func newID() string { return "ses_" + rand.Text() }

// errMemberHasOpenSession is returned by insert when the member already has an open session.
var errMemberHasOpenSession = errors.New("session: the member has an open session")

// insert stores s, unless s is a member's and that member has an open session.
func insert(ctx context.Context, tx store.Querier, s Session) error {
	var member any
	if s.MemberID != "" {
		member = s.MemberID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, member_id, state, currency, checked_in_at)
		SELECT ?1, ?2, ?3, ?4, ?5
		WHERE ?2 IS NULL OR NOT EXISTS (SELECT 1 FROM sessions WHERE member_id = ?2 AND state NOT IN `+endedStates+`)`,
		s.ID, member, string(s.State), string(s.Currency), s.CheckedInAt.UnixNano())
	if err != nil {
		return fmt.Errorf("session: insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("session: insert: %w", err)
	}
	if n == 0 {
		return errMemberHasOpenSession
	}
	return nil
}

// get returns the session with id, or errNotFound.
func get(ctx context.Context, q store.Querier, id string) (Session, error) {
	var s Session
	var member sql.NullString
	var state, cur string
	var checkedIn int64
	var fixed sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, member_id, state, currency, checked_in_at, total_fixed_at
		FROM sessions WHERE id = ?`, id).Scan(&s.ID, &member, &state, &cur, &checkedIn, &fixed)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, errNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("session: get: %w", err)
	}
	s.MemberID, s.State, s.Currency = member.String, State(state), money.Currency(cur)
	s.CheckedInAt = time.Unix(0, checkedIn).UTC()
	if fixed.Valid {
		s.TotalFixedAt = time.Unix(0, fixed.Int64).UTC()
	}
	return s, nil
}
