package signals

import (
	"context"
	"fmt"
	"testing"

	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// addSessions stores n guest sessions in state, standing in for check-in and the later use cases.
func (f *fixture) addSessions(t *testing.T, state session.State, n int) {
	t.Helper()
	ctx := context.Background()
	err := f.st.Tx(ctx, func(tx store.Querier) error {
		for range n {
			var fixed any
			if state != session.Active {
				fixed = t0.UnixNano()
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, state, currency, checked_in_at, total_fixed_at)
				VALUES ('ses_' || hex(randomblob(13)), ?, 'USD', ?, ?)`, string(state), t0.UnixNano(), fixed)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) occupancy(t *testing.T, capacity int) float64 {
	t.Helper()
	o, err := Occupancy(context.Background(), f.st.Read(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOccupancyIsActiveSessionsOverCapacityAtMostOne(t *testing.T) {
	f := newFixture(t)
	if got := f.occupancy(t, 40); got != 0 {
		t.Errorf("empty room: %v", got)
	}
	f.addSessions(t, session.Active, 10)
	if got := f.occupancy(t, 40); got != 0.25 {
		t.Errorf("10 of 40: %v", got)
	}
	f.addSessions(t, session.Active, 30)
	if got := f.occupancy(t, 40); got != 1 {
		t.Errorf("40 of 40: %v", got)
	}
	f.addSessions(t, session.Active, 5)
	if got := f.occupancy(t, 40); got != 1 {
		t.Errorf("45 of 40: %v, want at most 1", got)
	}
}

// Each of the other five states, one session each, leaves occupancy as it was.
func TestOccupancyCountsOnlyActiveSessions(t *testing.T) {
	f := newFixture(t)
	f.addSessions(t, session.Active, 2)
	for _, s := range session.States {
		if s == session.Active {
			continue
		}
		t.Run(fmt.Sprint(s), func(t *testing.T) {
			f.addSessions(t, s, 1)
			if got := f.occupancy(t, 8); got != 0.25 {
				t.Errorf("with a %s session: %v, want 2/8", s, got)
			}
		})
	}
}
