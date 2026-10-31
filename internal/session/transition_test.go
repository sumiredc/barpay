package session

import (
	"context"
	"errors"
	"testing"

	"github.com/sumiredc/barpay/internal/store"
)

// move updates only from the state it was given: a second move from the same stale read is
// refused and changes nothing, even though the judge allows the edge from that state.
func TestMoveIsConditionalOnTheStateRead(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	ctx := context.Background()
	s, err := get(ctx, f.st.Read(), v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	moveOnce := func() error {
		return f.st.Tx(ctx, func(tx store.Querier) error {
			_, err := move(ctx, tx, s, registerRequested, f.clk.Now())
			return err
		})
	}
	if err := moveOnce(); err != nil {
		t.Fatalf("first move: %v", err)
	}
	if err := moveOnce(); !errors.Is(err, errRefused) {
		t.Errorf("second move from the same read: %v, want errRefused", err)
	}
	if got := f.state(t, v.SessionID); got != AtRegister {
		t.Errorf("state %s, want at_register", got)
	}
}
