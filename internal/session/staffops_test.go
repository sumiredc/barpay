package session

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// UC7 alternative flow: staff close a linked session, which is checked out as in UC5 without the
// gate: charged once, closed with the receipt, and recorded with the staff member.
func TestCloseSessionChargesWithoutTheGate(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	got := f.closed(t, v.SessionID)
	if got.State != Closed || len(f.pp.Captures()) != 1 || f.pp.Captures()[0].Amount != usd(1700) {
		t.Errorf("state %s, captures %+v", got.State, f.pp.Captures())
	}
	if n := f.count(t, "SELECT count(*) FROM gate_unlocks"); n != 0 {
		t.Errorf("%d unlocks, want none", n)
	}
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Operation != "closeSession" || a[0].Staff != "staff_1" {
		t.Errorf("staff actions %+v", a)
	}
	evs := f.events(t, v.SessionID, "session.closed")
	if len(evs) != 1 {
		t.Fatalf("%d session.closed", len(evs))
	}
	spectest.Valid(t, "SessionClosedEvent", evs[0])
}

// closeSession refuses a session without PayPal linked and one that is not active; nothing is
// charged and nothing is kept for the key, so it can be sent again.
func TestCloseSessionRefusals(t *testing.T) {
	f := newFixture(t)
	guest := f.checkedIn(t, "")
	assertError(t, f.closeSession(guest.SessionID, "k-1"), http.StatusConflict, "session_not_linked")
	reg := f.linkedIn(t, "m_1")
	f.requested(t, reg.SessionID)
	assertError(t, f.closeSession(reg.SessionID, "k-2"), http.StatusConflict, "session_not_active")
	assertError(t, f.closeSession("ses_"+newID()[4:], "k-3"), http.StatusNotFound, "session_not_found")
	// With something to charge, the state is still what refuses: not a missing saved PayPal.
	f.clk.Set(t0.Add(25 * time.Minute))
	assertError(t, f.closeSession(guest.SessionID, "k-4"), http.StatusConflict, "session_not_linked")
	if len(f.charges()) != 0 || f.count(t, "SELECT count(*) FROM idempotency_keys WHERE scope = 'admin'") != 0 || f.count(t, "SELECT count(*) FROM staff_actions") != 0 {
		t.Error("a refused close charged or kept something")
	}
}

// A closed session whose member no longer has a saved PayPal is refused for its state by close
// and retry, not as missing a saved PayPal.
func TestClosedSessionWithoutASavedPaypalIsRefusedForItsState(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.closed(t, v.SessionID)
	f.saved.mu.Lock()
	delete(f.saved.vaults, "m_1")
	f.saved.mu.Unlock()
	assertError(t, f.closeSession(v.SessionID, "k-1"), http.StatusConflict, "session_not_active")
	assertError(t, f.retry(v.SessionID, "k-2"), http.StatusConflict, "session_not_payment_failed")
	if len(f.charges()) != 1 {
		t.Errorf("%d charges, want only the close", len(f.charges()))
	}
}

// UC7 postcondition: an admin operation without BarPay-Staff-Id is refused and changes nothing.
func TestStaffOperationsNeedTheStaffMember(t *testing.T) {
	f := newFixture(t)
	v := f.failed(t, "m_1")
	w := f.linkedIn(t, "m_2")
	for _, c := range []call{
		{method: http.MethodPost, path: "/v1/admin/sessions/" + w.SessionID + "/close", key: "k-1"},
		{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/retry", key: "k-2"},
		{method: http.MethodPost, path: "/v1/admin/sessions/" + v.SessionID + "/settle-outside", body: amountBody(1700, "USD")},
		{method: http.MethodPost, path: "/v1/admin/gate/unlock", body: `{}`},
	} {
		c.auth, c.staff = adminAuth, noAuth
		assertError(t, f.do(c), http.StatusBadRequest, "invalid_request")
	}
	if f.state(t, v.SessionID) != PaymentFailed || f.state(t, w.SessionID) != Active || len(f.charges()) != 1 {
		t.Error("an operation without a staff member changed something")
	}
}

// UC7 exception: staff retry a PaymentFailed session as a new attempt, with the same invoice_id;
// it is closed once the charge goes through. A session that is not PaymentFailed is refused.
func TestRetryCharge(t *testing.T) {
	f := newFixture(t)
	v := f.failed(t, "m_1")
	f.pp.Broke(payer("m_1"), false)
	f.clk.Advance(time.Hour)
	w := f.retry(v.SessionID, "r-1")
	if w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	calls := f.charges()
	if len(calls) != 2 || calls[1].RequestID != "barpay-"+v.SessionID+"-charge-2" || calls[1].InvoiceID != calls[0].InvoiceID {
		t.Fatalf("calls %+v", calls)
	}
	got := decodePaid(t, w.Body.Bytes())
	if f.state(t, v.SessionID) != Closed || got.Receipt == nil || got.Receipt.Total.Value != 1700 || len(f.pp.Captures()) != 1 {
		t.Errorf("state %s, receipt %+v", f.state(t, v.SessionID), got.Receipt)
	}
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Operation != "retryCharge" {
		t.Errorf("staff actions %+v", a)
	}
	assertError(t, f.retry(v.SessionID, "r-2"), http.StatusConflict, "session_not_payment_failed")
	active := f.linkedIn(t, "m_2")
	assertError(t, f.retry(active.SessionID, "r-3"), http.StatusConflict, "session_not_payment_failed")
}

// UC7 exception, Guarantees → Charges only for this visit: staff can retry until just before 3
// days after checkout began; from exactly then, the session is closed as unpaid with the unpaid
// amount and session.closed_unpaid, and a retry is refused as retry_window_over, whether or not
// BarPay closed it first. Across a restart too.
func TestThreeDaysAfterCheckoutBegan(t *testing.T) {
	began := t0.Add(25 * time.Minute)
	f := newFixture(t)
	v := f.failed(t, "m_1")
	f.restart(t)
	f.clk.Set(began.Add(unpaidAfter - time.Nanosecond))
	if n := f.closeDue(t); n != 0 {
		t.Fatalf("closed %d before the limit", n)
	}
	w := f.retry(v.SessionID, "r-1")
	if w.Code != http.StatusOK || f.state(t, v.SessionID) != PaymentFailed {
		t.Fatalf("retry just before the limit: %d %s", w.Code, w.Body)
	}
	f.clk.Set(began.Add(unpaidAfter))
	if n := f.closeDue(t); n != 1 {
		t.Fatalf("closed %d at the limit, want 1", n)
	}
	got := decodePaid(t, f.getSession(v.SessionID).Body.Bytes())
	if f.state(t, v.SessionID) != ClosedUnpaid || got.UnpaidAmount == nil || got.UnpaidAmount.Value != 1700 {
		t.Errorf("state %s, unpaid %+v", f.state(t, v.SessionID), got.UnpaidAmount)
	}
	evs := f.events(t, v.SessionID, "session.closed_unpaid")
	if len(evs) != 1 {
		t.Fatalf("%d session.closed_unpaid", len(evs))
	}
	spectest.Valid(t, "SessionClosedUnpaidEvent", evs[0])
	assertError(t, f.retry(v.SessionID, "r-2"), http.StatusConflict, "retry_window_over")

	g := newFixture(t)
	u := g.failed(t, "m_1")
	g.clk.Set(began.Add(unpaidAfter))
	assertError(t, g.retry(u.SessionID, "r-1"), http.StatusConflict, "retry_window_over")
	assertError(t, g.settleOutside(u.SessionID, amountBody(1700, "USD")), http.StatusConflict, "session_not_payment_failed")
	if g.state(t, u.SessionID) != ClosedUnpaid || len(g.charges()) != 1 {
		t.Errorf("state %s, %d charges", g.state(t, u.SessionID), len(g.charges()))
	}
}

// UC7 exception decided in the PRD: the 3-day limit does not close a session while its retry is
// still being sent again; once that charge fails past the limit, the session is closed as unpaid.
func TestThreeDaysWaitForARetryStillSending(t *testing.T) {
	began := t0.Add(25 * time.Minute)
	f := newFixture(t)
	v := f.failed(t, "m_1")
	f.clk.Set(began.Add(unpaidAfter - time.Minute))
	f.pp.Next(paypaltest.Charge, paypaltest.NoAnswer)
	if w := f.retry(v.SessionID, "r-1"); w.Code != http.StatusOK || decodeView(t, w.Body.Bytes()).State != CheckingOut {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	f.clk.Set(began.Add(unpaidAfter + time.Second))
	if n := f.closeDue(t); n != 0 || f.state(t, v.SessionID) != CheckingOut {
		t.Fatalf("closed %d while checking out", n)
	}
	f.chargeDue(t) // the refusal comes back on the resend
	if f.state(t, v.SessionID) != PaymentFailed {
		t.Fatalf("state %s", f.state(t, v.SessionID))
	}
	if n := f.closeDue(t); n != 1 || f.state(t, v.SessionID) != ClosedUnpaid {
		t.Errorf("closed %d, state %s; want closed_unpaid", n, f.state(t, v.SessionID))
	}
}

// UC7 exception: staff settle a PaymentFailed session outside BarPay with the amount settled:
// it is closed with an outside receipt and session.settled_outside, nothing is charged, and the
// staff member is recorded. Another state or another currency is refused, changing nothing.
func TestSettleOutside(t *testing.T) {
	f := newFixture(t)
	v := f.failed(t, "m_1")
	assertError(t, f.settleOutside(v.SessionID, amountBody(1700, "JPY")), http.StatusUnprocessableEntity, "currency_mismatch")
	if f.state(t, v.SessionID) != PaymentFailed {
		t.Fatal("a refused settlement changed the session")
	}
	w := f.settleOutside(v.SessionID, amountBody(1500, "USD"))
	if w.Code != http.StatusOK {
		t.Fatalf("settleOutside: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	r := decodeReceipt(t, w.Body.Bytes())
	if r.Receipt.Settlement != "outside" || r.Receipt.AmountRecorded == nil || r.Receipt.AmountRecorded.Value != 1500 || r.Receipt.Total.Value != 1700 {
		t.Errorf("receipt %+v", r.Receipt)
	}
	evs := f.events(t, v.SessionID, "session.settled_outside")
	if len(evs) != 1 {
		t.Fatalf("%d session.settled_outside", len(evs))
	}
	spectest.Valid(t, "SessionSettledOutsideEvent", evs[0])
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Operation != "settleOutside" {
		t.Errorf("staff actions %+v", a)
	}
	assertError(t, f.settleOutside(v.SessionID, amountBody(1500, "USD")), http.StatusConflict, "session_not_payment_failed")
	active := f.linkedIn(t, "m_2")
	assertError(t, f.settleOutside(active.SessionID, amountBody(0, "USD")), http.StatusConflict, "session_not_payment_failed")
	if len(f.charges()) != 1 || f.endEvents(t, v.SessionID) != 1 {
		t.Error("settling charged, or ended twice")
	}
}

// The spec's IdempotencyKey for closeSession and retryCharge: a repeat returns the first response
// byte for byte without charging, including after a refused charge and while the outcome is
// unknown (checking_out); a different request with the key is refused.
func TestChargeOperationsKeepTheirResponse(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault paypaltest.Fault
		state State
	}{
		{"charged", paypaltest.NoFault, Closed},
		{"refused", paypaltest.Refuse, PaymentFailed},
		{"unknown", paypaltest.NoAnswer, CheckingOut},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			v := f.ordered(t, "m_1")
			f.pp.Next(paypaltest.Charge, c.fault)
			first := f.closeSession(v.SessionID, "k-1")
			if first.Code != http.StatusOK || decodeView(t, first.Body.Bytes()).State != c.state {
				t.Fatalf("close: %d %s", first.Code, first.Body)
			}
			f.chargeDue(t)
			f.clk.Advance(5 * time.Minute)
			f.chargeDue(t) // a late outcome does not change what the key returns
			again := f.closeSession(v.SessionID, "k-1")
			if again.Code != first.Code || !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) {
				t.Errorf("repeat: %d %s, want the first response", again.Code, again.Body)
			}
			if n := len(f.charges()); n > 1+map[State]int{CheckingOut: 1}[c.state] {
				t.Errorf("%d charges", n)
			}
			assertError(t, f.retry(v.SessionID, "k-1"), http.StatusConflict, "idempotency_key_reused")
		})
	}
	f := newFixture(t)
	v := f.failed(t, "m_1")
	first := f.retry(v.SessionID, "k-1")
	again := f.retry(v.SessionID, "k-1")
	if !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) || len(f.charges()) != 2 {
		t.Errorf("retry repeat: %s, %d charges", again.Body, len(f.charges()))
	}
}

// The spec's IdempotencyKey: a repeat while the first is still running is refused as in progress
// and charges nothing; once the first ends, the key returns its response.
func TestChargeOperationKeyInProgress(t *testing.T) {
	for _, op := range []string{"close", "retry"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t)
			var v View
			if op == "close" {
				v = f.ordered(t, "m_1")
			} else {
				v = f.failed(t, "m_1")
				f.pp.Broke(payer("m_1"), false)
			}
			send := map[string]func(string, string) *httptest.ResponseRecorder{"close": f.closeSession, "retry": f.retry}[op]
			hold, arrived := make(chan struct{}), make(chan struct{})
			f.pp.During = func(paypaltest.Call) { close(arrived); <-hold }
			var first *httptest.ResponseRecorder
			done := make(chan struct{})
			go func() { first = send(v.SessionID, "k-1"); close(done) }()
			<-arrived
			assertError(t, send(v.SessionID, "k-1"), http.StatusConflict, "idempotency_key_in_progress")
			close(hold)
			<-done
			f.pp.During = nil
			again := send(v.SessionID, "k-1")
			if !bytes.Equal(again.Body.Bytes(), first.Body.Bytes()) {
				t.Errorf("after it ended: %s, want %s", again.Body, first.Body)
			}
			if len(f.pp.Captures()) != 1 {
				t.Errorf("%d captures, want 1", len(f.pp.Captures()))
			}
		})
	}
}

// Guarantees → Single payment: two requests with different keys at once: one moves the session
// to checking out and charges; the other is refused, and PayPal charges once.
func TestTwoChargeRequestsAtOnce(t *testing.T) {
	for _, op := range []string{"close", "retry"} {
		t.Run(op, func(t *testing.T) {
			for range 20 {
				f := newFixture(t)
				var v View
				refusal := "session_not_active"
				if op == "close" {
					v = f.ordered(t, "m_1")
				} else {
					v = f.failed(t, "m_1")
					f.pp.Broke(payer("m_1"), false)
					refusal = "session_not_payment_failed"
				}
				send := map[string]func(string, string) *httptest.ResponseRecorder{"close": f.closeSession, "retry": f.retry}[op]
				before := len(f.charges())
				var wg sync.WaitGroup
				got := make([]*httptest.ResponseRecorder, 2)
				for i := range got {
					wg.Go(func() { got[i] = send(v.SessionID, "k-"+string(rune('a'+i))) })
				}
				wg.Wait()
				ok, refused := 0, 0
				for _, w := range got {
					switch {
					case w.Code == http.StatusOK:
						ok++
					case w.Code == http.StatusConflict && errorCode(w) == refusal:
						refused++
					}
				}
				if ok != 1 || refused != 1 || len(f.charges())-before != 1 || len(f.pp.Captures()) != 1 {
					t.Fatalf("%d ok, %d refused, %d charges", ok, refused, len(f.charges())-before)
				}
			}
		})
	}
}

// errorCode is the error code in w's body, or "" when it has none.
func errorCode(w *httptest.ResponseRecorder) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error.Code
}
