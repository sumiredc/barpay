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
	registerRequested trigger = "the register is requested"
)

// edge is the states a trigger moves a session from, and the state it moves it to.
type edge struct {
	from []State
	to   State
}

// edges are the PRD's state diagram, by trigger. Every change of a session's state is one of
// them, made by move.
var edges = map[trigger]edge{
	registerRequested: {from: []State{Active}, to: AtRegister},
}

// errRefused is returned by move when the trigger does not apply to the session at that instant.
var errRefused = errors.New("session: the session's state does not allow this")

// allowed tells whether t moves s at now.
func allowed(s Session, t trigger, _ time.Time) bool {
	return slices.Contains(edges[t].from, s.State)
}

// move is the one place that changes a session's state: it moves s, as read in tx, along t's edge
// at now, and returns it as moved. Leaving active fixes the total at now. When t does not apply,
// it returns errRefused and changes nothing. The update is conditional on the state s was read
// in, so of two requests that read the same state only one moves it, even if the store ever lets
// both read before either writes.
func move(ctx context.Context, tx store.Querier, s Session, t trigger, now time.Time) (Session, error) {
	if !allowed(s, t, now) {
		return s, errRefused
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
