package session

import (
	"context"
	"testing"
)

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
