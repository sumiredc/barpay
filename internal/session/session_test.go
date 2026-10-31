package session

import (
	"context"
	"testing"
)

// UC1 postcondition: a session that checked in is counted as active; each state counts its own.
func TestCountByState(t *testing.T) {
	f := newFixture(t)
	counts := func() map[State]int {
		t.Helper()
		c, err := CountByState(context.Background(), f.st.Read())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, s := range States {
		if n, ok := counts()[s]; !ok || n != 0 {
			t.Errorf("with no session: %s = %d, %v; want 0", s, n, ok)
		}
	}
	a := f.checkedIn(t, "m_1")
	f.checkedIn(t, "")
	if n := counts()[Active]; n != 2 {
		t.Errorf("active = %d after two check-ins, want 2", n)
	}
	f.setState(t, a.SessionID, AtRegister)
	if c := counts(); c[Active] != 1 || c[AtRegister] != 1 {
		t.Errorf("counts = %v; want 1 active and 1 at the register", c)
	}
}

// A session is open in every state but closed and closed_unpaid.
func TestAnyOpen(t *testing.T) {
	f := newFixture(t)
	anyOpen := func() bool {
		t.Helper()
		open, err := OpenSessions{}.AnyOpen(context.Background(), f.st.Read())
		if err != nil {
			t.Fatal(err)
		}
		return open
	}
	if anyOpen() {
		t.Error("open with no session")
	}
	v := f.checkedIn(t, "")
	for _, s := range States {
		f.setState(t, v.SessionID, s)
		if got := anyOpen(); got == ended[s] {
			t.Errorf("in %s: open = %v", s, got)
		}
	}
}
