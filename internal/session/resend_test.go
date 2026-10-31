package session

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

// Fixed constraints → Idempotency: the charge's PayPal-Request-Id and invoice_id are stored
// before PayPal is called, with when the attempt is first sent.
func TestChargeIsStoredBeforeItIsSent(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	seen := false
	f.pp.During = func(c paypaltest.Call) {
		seen = f.count(t, `SELECT count(*) FROM charges WHERE session_id = ? AND request_id = ? AND invoice_id = ?
			AND first_sent_at IS NOT NULL AND outcome IS NULL`, v.SessionID, c.RequestID, c.InvoiceID) == 1
	}
	f.scan(t, f.token(t, v.SessionID).Token)
	if !seen {
		t.Error("the charge was not stored when PayPal received it")
	}
}

// UC5 exception, Fixed constraints → Idempotency: PayPal charged but its answer was lost: the
// gate stays locked and the customer is sent to staff at once; 1 minute after, BarPay sends the
// same charge with the same PayPal-Request-Id and invoice_id, gets the same capture back and
// closes the session with it, without unlocking. PayPal charged once. The store app then gets the
// closed session's token, which opens the gate within the re-unlock window.
func TestLostAnswerIsSentAgainWithTheSameKeys(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.pp.Next(paypaltest.Charge, paypaltest.LostAnswer)
	if r := f.scan(t, f.token(t, v.SessionID).Token); r.Unlock || r.ReaderMessage != readSeeStaff {
		t.Fatalf("scan = %+v, want locked and staff", r)
	}
	if s := f.state(t, v.SessionID); s != CheckingOut {
		t.Fatalf("state %s, want checking_out", s)
	}
	f.clk.Advance(resendAfter - time.Second)
	if n := f.chargeDue(t); n != 0 {
		t.Fatalf("sent again %d before a minute had passed", n)
	}
	f.clk.Advance(2 * time.Second)
	if n := f.chargeDue(t); n != 1 {
		t.Fatalf("sent again %d, want 1", n)
	}
	calls := f.charges()
	if len(calls) != 2 || calls[1].RequestID != calls[0].RequestID || calls[1].InvoiceID != calls[0].InvoiceID {
		t.Fatalf("calls %+v, want the same charge twice", calls)
	}
	if caps := f.pp.Captures(); len(caps) != 1 {
		t.Fatalf("%d captures, want 1", len(caps))
	}
	got := decodePaid(t, f.getSession(v.SessionID).Body.Bytes())
	if f.state(t, v.SessionID) != Closed || got.Receipt.PaypalCaptureID == nil || *got.Receipt.PaypalCaptureID != f.pp.Captures()[0].ID {
		t.Errorf("state %s, receipt %+v", f.state(t, v.SessionID), got.Receipt)
	}
	if n := f.count(t, "SELECT count(*) FROM gate_unlocks"); n != 0 {
		t.Errorf("%d unlocks after a late success, want 0", n)
	}
	if r := f.scan(t, f.token(t, v.SessionID).Token); !r.Unlock {
		t.Errorf("scan of the closed session's token: %+v, want an unlock", r)
	}
	if n := f.chargeDue(t); n != 0 || len(f.charges()) != 2 {
		t.Errorf("sent again after the outcome was stored")
	}
}

// Guarantees → Durability, UC5 exception: after a restart with the outcome unknown, BarPay sends
// the same charge again and the session ends, charged once.
func TestUnknownChargeAcrossARestart(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.pp.Next(paypaltest.Charge, paypaltest.LostAnswer)
	f.scan(t, f.token(t, v.SessionID).Token)
	f.restart(t)
	f.clk.Advance(10 * time.Minute)
	if n := f.chargeDue(t); n != 1 {
		t.Fatalf("sent again %d after the restart, want 1", n)
	}
	calls := f.charges()
	if len(calls) != 2 || calls[1].RequestID != calls[0].RequestID || len(f.pp.Captures()) != 1 || f.state(t, v.SessionID) != Closed {
		t.Errorf("calls %+v, %d captures, state %s", calls, len(f.pp.Captures()), f.state(t, v.SessionID))
	}
}

// A charge stored but cut off by a restart before it was sent is sent after the restart.
func TestChargeNeverSentIsSentAfterARestart(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	s, err := get(context.Background(), f.st.Read(), v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		_, _, err := f.api.begin(context.Background(), tx, s, checkout, f.clk.Now(), f.clk.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.clk.Advance(resendAfter)
	if n := f.chargeDue(t); n != 1 || len(f.pp.Captures()) != 1 || f.state(t, v.SessionID) != Closed {
		t.Errorf("sent %d, %d captures, state %s", n, len(f.pp.Captures()), f.state(t, v.SessionID))
	}
}

// Fixed constraints → Idempotency, UC5 exception: with no answer, the charge is sent again each
// minute only within 30 minutes of its first send; from then on it is not, and the session is
// PaymentFailed with a charge_unconfirmed alert and session.payment_failed (unconfirmed).
func TestNoAnswerWithinTheWindowIsGivenUp(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	first := f.clk.Now()
	f.pp.Next(paypaltest.Charge, repeat(paypaltest.NoAnswer, 40)...)
	f.scan(t, f.token(t, v.SessionID).Token)
	for f.clk.Now().Before(first.Add(resendWindow - time.Second)) {
		f.clk.Advance(time.Second)
		f.chargeDue(t)
	}
	sent := len(f.charges())
	if sent < 29 || sent > 31 || f.state(t, v.SessionID) != CheckingOut {
		t.Fatalf("%d sends and state %s just before the window ends; want about 30, checking_out", sent, f.state(t, v.SessionID))
	}
	f.clk.Set(first.Add(resendWindow))
	f.chargeDue(t)
	if len(f.charges()) != sent {
		t.Error("sent again at the end of the window")
	}
	if s := f.state(t, v.SessionID); s != PaymentFailed || f.alerts(t, v.SessionID, "charge_unconfirmed") != 1 {
		t.Fatalf("state %s, %d alerts; want payment_failed with charge_unconfirmed", s, f.alerts(t, v.SessionID, "charge_unconfirmed"))
	}
	evs := f.events(t, v.SessionID, "session.payment_failed")
	if len(evs) != 1 {
		t.Fatalf("%d session.payment_failed", len(evs))
	}
	spectest.Valid(t, "SessionPaymentFailedEvent", evs[0])
	if _, reason := eventData(t, evs[0]); reason != "unconfirmed" {
		t.Errorf("reason %s, want unconfirmed", reason)
	}
	f.clk.Advance(time.Hour)
	if f.chargeDue(t); len(f.charges()) != sent {
		t.Error("sent again after giving up")
	}
}

func repeat(fault paypaltest.Fault, n int) []paypaltest.Fault {
	out := make([]paypaltest.Fault, n)
	for i := range out {
		out[i] = fault
	}
	return out
}

// UC7 exception: a charge given up as unconfirmed that had in fact gone through cannot be charged
// again: the staff retry, a new attempt with the same invoice_id, gets DUPLICATE_INVOICE_ID, and
// the session stays PaymentFailed with another charge_unconfirmed alert; staff settle outside.
func TestRetryOfAChargeThatWentThrough(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	first := f.clk.Now()
	f.pp.Next(paypaltest.Charge, paypaltest.LostAnswer)
	f.pp.Next(paypaltest.Charge, repeat(paypaltest.NoAnswer, 40)...)
	f.scan(t, f.token(t, v.SessionID).Token)
	f.clk.Set(first.Add(resendWindow))
	f.chargeDue(t)
	if f.state(t, v.SessionID) != PaymentFailed {
		t.Fatalf("state %s", f.state(t, v.SessionID))
	}
	f.pp.Clear(paypaltest.Charge)
	w := f.retry(v.SessionID, "r-1")
	if w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	calls := f.charges()
	last := calls[len(calls)-1]
	if last.RequestID != "barpay-"+v.SessionID+"-charge-2" || last.InvoiceID != calls[0].InvoiceID {
		t.Errorf("retry sent %+v, want attempt 2 with the same invoice_id", last)
	}
	if len(f.pp.Captures()) != 1 || f.state(t, v.SessionID) != PaymentFailed || f.alerts(t, v.SessionID, "charge_unconfirmed") != 2 {
		t.Errorf("%d captures, state %s, %d alerts", len(f.pp.Captures()), f.state(t, v.SessionID), f.alerts(t, v.SessionID, "charge_unconfirmed"))
	}
}
