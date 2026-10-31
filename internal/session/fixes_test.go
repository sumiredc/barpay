package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/store"
)

// A linked session whose member has no saved PayPal any more is not checked out by a scan or the
// maximum stay: it stays active, with no attempt stored, and nothing is charged.
func TestNoSavedPaypalChangesNothing(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	tok := f.token(t, v.SessionID).Token
	f.saved.mu.Lock()
	delete(f.saved.vaults, "m_1")
	f.saved.mu.Unlock()
	if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readSeeStaff {
		t.Errorf("scan = %+v, want locked and staff", r)
	}
	f.clk.Set(t0.Add(maxStay + time.Second))
	if n := f.maxStayDue(t); n != 0 {
		t.Errorf("maximum stay handled %d, want 0", n)
	}
	if s := f.state(t, v.SessionID); s != Active || len(f.charges()) != 0 || f.count(t, "SELECT count(*) FROM charges") != 0 {
		t.Errorf("state %s, %d charges sent; want active and nothing charged", s, len(f.charges()))
	}
}

// Within the re-unlock window, only the closed session's own token opens the gate: a token of an
// earlier period, or the session's prefix with anything after it, is refused.
func TestOnlyTheClosedTokenReunlocks(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	key, err := f.api.tokenKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.scan(t, f.token(t, v.SessionID).Token)
	s, err := get(context.Background(), f.st.Read(), v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	earlier := makeToken(key, s.ID, f.api.period(s.CheckedInAt, s.TotalFixedAt)-1)
	made := strings.TrimPrefix(s.ID, "ses_") + strings.Repeat("A", tokenMACLen)
	f.clk.Advance(time.Second)
	for _, tok := range []string{earlier, made} {
		if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readNewQR {
			t.Errorf("scan of %q = %+v, want refused", tok, r)
		}
	}
	if n := f.count(t, "SELECT count(*) FROM gate_unlocks"); n != 1 {
		t.Errorf("%d unlocks, want only the checkout's", n)
	}
}

// UC3 exception, decided in the PRD: the total is fixed at the maximum stay itself, so a restart
// long after it charges only what had accrued by then.
func TestMaxStayAfterALongStopChargesUpToTheMaximumStay(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.restart(t)
	f.api.MaxStay = time.Hour
	f.clk.Set(t0.Add(6 * time.Hour))
	if n := f.maxStayDue(t); n != 1 {
		t.Fatalf("handled %d, want 1", n)
	}
	// $13.00 of orders and six blocks of $2.00 by t0+1h; by t0+6h the cover charge would be $30.00.
	if caps := f.pp.Captures(); len(caps) != 1 || caps[0].Amount != usd(2500) {
		t.Errorf("captures %+v, want one of 2500 USD", caps)
	}
	s, err := get(context.Background(), f.st.Read(), v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !s.TotalFixedAt.Equal(t0.Add(time.Hour)) || s.State != Closed {
		t.Errorf("total fixed at %s, state %s", s.TotalFixedAt, s.State)
	}
}

// What came of an attempt is stored once: settling it again changes nothing and returns the
// session as it is.
func TestSettlingAnAttemptTwiceChangesNothing(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	s, err := get(context.Background(), f.st.Read(), v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var c *charge
	err = f.st.Tx(context.Background(), func(tx store.Querier) (err error) {
		_, c, err = f.api.begin(context.Background(), tx, s, checkout, f.clk.Now(), f.clk.Now())
		return err
	})
	if err != nil || c == nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := f.api.settle(context.Background(), *c, paypal.Charged{Outcome: paypal.Succeeded, CaptureID: "CAP-1"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := f.api.settle(context.Background(), *c, paypal.Charged{Outcome: paypal.Failed, Refusal: paypal.Refused}, nil)
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if got.State != Closed || f.state(t, v.SessionID) != Closed || f.alerts(t, v.SessionID, "payment_failed") != 0 ||
		len(f.events(t, v.SessionID, "session.payment_failed")) != 0 || len(f.events(t, v.SessionID, "session.closed")) != 1 {
		t.Errorf("second settle: view %s, state %s", got.State, f.state(t, v.SessionID))
	}
}
