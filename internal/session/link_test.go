package session

import (
	"context"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

func (f *fixture) link(t *testing.T, id string) bool {
	t.Helper()
	var linked bool
	err := f.st.Tx(context.Background(), func(tx store.Querier) (err error) {
		linked, err = Link(context.Background(), tx, id, f.clk.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return linked
}

// UC1 alternative flow, UC2: an active session is linked once; from then on it pays by PayPal,
// and session.paypal_linked carries it as linked, fitting the spec.
func TestLinkAnActiveSession(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.clk.Advance(time.Minute)
	if !f.link(t, v.SessionID) {
		t.Fatal("the active session was not linked")
	}
	if got := f.looked(t, v.SessionID); got.PaymentMethod != "paypal" {
		t.Errorf("payment method %s, want paypal", got.PaymentMethod)
	}
	evs := f.events(t, v.SessionID, "session.paypal_linked")
	if len(evs) != 1 {
		t.Fatalf("%d session.paypal_linked events, want 1", len(evs))
	}
	spectest.Valid(t, "SessionPaypalLinkedEvent", evs[0])
	if f.link(t, v.SessionID) {
		t.Error("an already linked session was linked again")
	}
	if n := len(f.events(t, v.SessionID, "session.paypal_linked")); n != 1 {
		t.Errorf("%d session.paypal_linked events after linking again, want 1", n)
	}
	o, ok, err := OpenOf(context.Background(), f.st.Read(), "m_1")
	if err != nil || !ok || o.ID != v.SessionID || !o.Linked || o.State != Active {
		t.Errorf("OpenOf = %+v, %v, %v", o, ok, err)
	}
}

// UC1 and UC2 exceptions: a session that has left active, or a guest's, is not linked and
// nothing is sent.
func TestLinkOnlyAnActiveMembersSession(t *testing.T) {
	for _, state := range []State{CheckingOut, AtRegister, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.checkedIn(t, "m_1")
			f.setState(t, v.SessionID, state)
			if f.link(t, v.SessionID) || len(f.events(t, v.SessionID, "session.paypal_linked")) != 0 {
				t.Error("a session that is not active was linked")
			}
		})
	}
	f := newFixture(t)
	guest := f.checkedIn(t, "")
	if f.link(t, guest.SessionID) {
		t.Error("a guest's session was linked")
	}
}

// The sessions a saved PayPal could still be linked to are the active, unlinked ones of members.
func TestActiveUnlinked(t *testing.T) {
	f := newFixture(t)
	a := f.checkedIn(t, "m_a")
	f.checkedIn(t, "")
	b := f.checkedIn(t, "m_b")
	c := f.checkedIn(t, "m_c")
	f.link(t, b.SessionID)
	f.setState(t, c.SessionID, AtRegister)
	got, err := ActiveUnlinked(context.Background(), f.st.Read())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Unlinked{ID: a.SessionID, MemberID: "m_a"}) {
		t.Errorf("ActiveUnlinked = %+v, want only %s", got, a.SessionID)
	}
}

// A linked session that requests the register pays at the register, in its response and lookup.
func TestLinkedSessionAtTheRegisterPaysThere(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.link(t, v.SessionID)
	if got := decodeView(t, f.requestRegister(v.SessionID).Body.Bytes()); got.PaymentMethod != "register" {
		t.Errorf("register requested: payment method %s, want register", got.PaymentMethod)
	}
	o, _, _ := OpenOf(context.Background(), f.st.Read(), "m_1")
	if !o.Linked || o.State != AtRegister {
		t.Errorf("OpenOf = %+v; a session at the register still uses the saved PayPal", o)
	}
}
