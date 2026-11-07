package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
)

// stillOpen fails the test unless the session is active, has max_stay_reached raised alerts
// times, and nothing was sent to PayPal to charge it.
func (f *fixture) stillOpen(t *testing.T, id string, alerts int) {
	t.Helper()
	if s, n := f.state(t, id), f.alerts(t, id, "max_stay_reached"); s != Active || n != alerts {
		t.Errorf("state %s with %d max_stay_reached; want active with %d", s, n, alerts)
	}
	if n, stored := len(f.charges()), f.count(t, "SELECT count(*) FROM charges WHERE session_id = ?", id); n != 0 || stored != 0 {
		t.Errorf("%d charges sent, %d stored; want none", n, stored)
	}
}

// UC3 exception: just before the maximum stay, a linked session is left alone.
func TestMaxStayDoesNothingJustBeforeIt(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.clk.Set(t0.Add(maxStay - time.Nanosecond))
	if n := f.maxStayDue(t); n != 0 {
		t.Errorf("handled %d, want 0", n)
	}
	f.stillOpen(t, v.SessionID, 0)
}

// UC3 exception: from exactly the maximum stay, a linked session stays active with
// max_stay_reached raised once, however often BarPay looks, and PayPal is not asked to charge.
// The exact instant counts as reached.
func TestMaxStayKeepsALinkedSessionOpen(t *testing.T) {
	for name, at := range map[string]time.Duration{"exactly at": maxStay, "just after": maxStay + time.Nanosecond} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			v := f.ordered(t, "m_1")
			f.clk.Set(t0.Add(at))
			if n := f.maxStayDue(t); n != 1 {
				t.Errorf("handled %d, want 1", n)
			}
			f.clk.Advance(time.Hour)
			if n := f.maxStayDue(t); n != 0 {
				t.Errorf("handled %d again, want 0", n)
			}
			f.stillOpen(t, v.SessionID, 1)
		})
	}
}

// UC3 exception: an unlinked session at the maximum stay stays active with max_stay_reached
// raised once, however often BarPay looks.
func TestMaxStayAlertsOnceForAnUnlinkedSession(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(maxStay))
	if n := f.maxStayDue(t); n != 1 {
		t.Errorf("handled %d, want 1", n)
	}
	f.clk.Advance(time.Hour)
	f.maxStayDue(t)
	f.stillOpen(t, v.SessionID, 1)
}

// UC3 exception: a linked session whose member has no saved PayPal any more is handled like any
// other at the maximum stay: it stays active with max_stay_reached raised once.
func TestMaxStayAlertsOnceWithNoSavedPaypal(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.saved.mu.Lock()
	delete(f.saved.vaults, "m_1")
	f.saved.mu.Unlock()
	f.clk.Set(t0.Add(maxStay))
	if n := f.maxStayDue(t); n != 1 {
		t.Errorf("handled %d, want 1", n)
	}
	f.maxStayDue(t)
	f.stillOpen(t, v.SessionID, 1)
}

// UC3 exception: a session linked after its max_stay_reached was raised is not checked out, and
// the alert is not raised again.
func TestMaxStayIgnoresALinkAfterTheAlert(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.clk.Set(t0.Add(maxStay))
	f.maxStayDue(t)
	f.saved.mu.Lock()
	f.saved.vaults["m_1"] = f.pp.Vault(payer("m_1"))
	f.saved.mu.Unlock()
	f.clk.Advance(time.Minute)
	if !f.link(t, v.SessionID) {
		t.Fatal("not linked")
	}
	if n := f.maxStayDue(t); n != 0 {
		t.Errorf("handled %d after the link, want 0", n)
	}
	f.stillOpen(t, v.SessionID, 1)
}

// UC3 exception: a checkout after the maximum stay, at the exit gate or by staff, charges the
// total at that moment, not the total at the maximum stay.
func TestCheckoutAfterTheMaximumStayChargesTheTotalThen(t *testing.T) {
	checkouts := map[string]func(t *testing.T, f *fixture, id string){
		"exit gate":   func(t *testing.T, f *fixture, id string) { f.scan(t, f.token(t, id).Token) },
		"staff close": func(t *testing.T, f *fixture, id string) { f.closed(t, id) },
	}
	for name, checkout := range checkouts {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			v := f.ordered(t, "m_1")
			f.api.MaxStay = time.Hour
			f.clk.Set(t0.Add(time.Hour))
			f.maxStayDue(t)
			f.clk.Set(t0.Add(2 * time.Hour))
			checkout(t, f, v.SessionID)
			// $13.00 of orders; six blocks of $2.00 by t0+1h ($25.00), twelve by t0+2h ($37.00).
			if caps := f.pp.Captures(); len(caps) != 1 || caps[0].Amount != usd(3700) {
				t.Errorf("captures %+v, want one of 3700 USD", caps)
			}
			if s := f.state(t, v.SessionID); s != Closed {
				t.Errorf("state %s, want closed", s)
			}
		})
	}
}

// UC3 exception: only an active session reaches the maximum stay; the cover charge of any other
// has stopped, so none gets max_stay_reached.
func TestMaxStayIgnoresSessionsNotActive(t *testing.T) {
	for _, state := range []State{AtRegister, CheckingOut, PaymentFailed, Closed, ClosedUnpaid} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			v := f.ordered(t, "m_1")
			f.setState(t, v.SessionID, state)
			f.clk.Set(t0.Add(maxStay))
			if n := f.maxStayDue(t); n != 0 || f.alerts(t, v.SessionID, "max_stay_reached") != 0 {
				t.Errorf("handled %d, want 0 and no alert", n)
			}
		})
	}
}

// Guarantees → Durability: whether max_stay_reached was raised is stored, so a restart after it
// does not raise it again.
func TestMaxStayAlertsOnceAcrossARestart(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.clk.Set(t0.Add(maxStay))
	f.maxStayDue(t)
	f.restart(t)
	f.clk.Advance(time.Hour)
	if n := f.maxStayDue(t); n != 0 {
		t.Errorf("handled %d after the restart, want 0", n)
	}
	f.stillOpen(t, v.SessionID, 1)
}

// Guarantees → Durability: the maximum stay is computed from the stored check-in, so a session
// that reached it while BarPay was stopped gets max_stay_reached once at the next start, and
// stays open.
func TestMaxStayReachedWhileStoppedAlertsAtTheStart(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.restart(t)
	f.clk.Set(t0.Add(maxStay + 3*time.Hour))
	if n := f.maxStayDue(t); n != 1 {
		t.Errorf("handled %d, want 1", n)
	}
	f.stillOpen(t, v.SessionID, 1)
}

func (f *fixture) unlockGate(body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/gate/unlock", auth: adminAuth, body: body})
}

// waitingEdge is an edge device waiting for an unlock by hand; got receives its answer.
func (f *fixture) waitingEdge(t *testing.T) (got chan *httptest.ResponseRecorder, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	got = make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/edge/unlocks/next", nil)
		req.Header.Set("Authorization", edgeAuth)
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		got <- w
	}()
	return got, cancel
}

// UC7 alternative flow: staff open the gate by hand. With no edge device connected it is refused
// as gate_unavailable and nothing is recorded; with one waiting, it gets one unlock, recorded
// with the staff member and the session.
func TestUnlockGateByHand(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	assertError(t, f.unlockGate(`{"session_id":"`+v.SessionID+`"}`), http.StatusServiceUnavailable, "gate_unavailable")
	if f.count(t, "SELECT count(*) FROM gate_unlocks")+f.count(t, "SELECT count(*) FROM staff_actions") != 0 {
		t.Fatal("an unlock nobody received was recorded")
	}
	got, stop := f.waitingEdge(t)
	defer stop()
	var w *httptest.ResponseRecorder
	for give := time.Now().Add(5 * time.Second); ; {
		if w = f.unlockGate(`{"session_id":"` + v.SessionID + `"}`); w.Code != http.StatusServiceUnavailable || time.Now().After(give) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("unlockGate: %d %s", w.Code, w.Body)
	}
	edge := <-got
	if edge.Code != http.StatusOK {
		t.Fatalf("edge device: %d %s", edge.Code, edge.Body)
	}
	spectest.ValidEdge(t, "Unlock", edge.Body.Bytes())
	var u struct {
		UnlockID string `json:"unlock_id"`
	}
	_ = json.Unmarshal(edge.Body.Bytes(), &u)
	if n := f.count(t, "SELECT count(*) FROM gate_unlocks WHERE id = ? AND session_id = ? AND source = 'staff'", u.UnlockID, v.SessionID); n != 1 {
		t.Errorf("unlock %q not recorded for the session", u.UnlockID)
	}
	if a := f.staffActions(t, v.SessionID); len(a) != 1 || a[0].Operation != "unlockGate" || a[0].Staff != "staff_1" {
		t.Errorf("staff actions %+v", a)
	}
	if w := f.reportUnlock(u.UnlockID, false); w.Code != http.StatusNoContent || f.alerts(t, v.SessionID, "unlock_failed") != 1 ||
		len(f.events(t, v.SessionID, "gate.unlock_failed")) != 0 {
		t.Error("a failed unlock by hand: want the alert only")
	}
	assertError(t, f.unlockGate(`{"session_id":"ses_`+newID()[4:]+`"}`), http.StatusNotFound, "session_not_found")
	assertError(t, f.unlockGate(`{"session_id":"nope"}`), http.StatusBadRequest, "invalid_request")
}

// The edge device's wait ends with 204 when no unlock comes.
func TestWaitForUnlockEndsEmpty(t *testing.T) {
	f := newFixture(t)
	got, stop := f.waitingEdge(t)
	stop()
	if w := <-got; w.Code != http.StatusNoContent {
		t.Errorf("%d, want 204", w.Code)
	}
}

// Guarantees → Single payment: any two of the scan, closeSession, retryCharge and settleOutside
// arriving together charge once at most and end the session with one kind of notification; with
// requestRegister or recordRegisterPayment, only one goes through, and PayPal is called only if
// the checkout did. The maximum stay arriving with a scan or closeSession adds no charge and
// raises max_stay_reached once at most.
func TestOperationsOnOneSessionAtOnce(t *testing.T) {
	type op func(f *fixture, v View, tok string)
	scan := func(f *fixture, _ View, tok string) { f.scanRaw(tok) }
	closeOp := func(f *fixture, v View, _ string) { f.closeSession(v.SessionID, "c-"+newID()) }
	register := func(f *fixture, v View, _ string) { f.requestRegister(v.SessionID) }
	record := func(f *fixture, v View, _ string) { f.recordPayment(v.SessionID, amountBody(1700, "USD")) }
	retry := func(f *fixture, v View, _ string) { f.retry(v.SessionID, "r-"+newID()) }
	settle := func(f *fixture, v View, _ string) { f.settleOutside(v.SessionID, amountBody(1700, "USD")) }
	maxStayOp := func(f *fixture, _ View, _ string) { _, _ = f.api.MaxStayDue(context.Background()) }
	pairs := map[string][2]op{
		"scan+close": {scan, closeOp}, "scan+register": {scan, register}, "close+register": {closeOp, register},
		"scan+record": {scan, record}, "close+record": {closeOp, record},
		"retry+settle": {retry, settle}, "scan+max stay": {scan, maxStayOp}, "close+max stay": {closeOp, maxStayOp},
	}
	atMaxStay := map[string]bool{"scan+max stay": true, "close+max stay": true}
	for name, pair := range pairs {
		t.Run(name, func(t *testing.T) {
			for range 10 {
				f := newFixture(t)
				var v View
				if name == "retry+settle" {
					v = f.failed(t, "m_1")
					f.pp.Broke(payer("m_1"), false)
				} else {
					v = f.ordered(t, "m_1")
				}
				if atMaxStay[name] {
					f.clk.Set(t0.Add(maxStay))
				}
				tok := ""
				if w := f.exitToken(v.SessionID); w.Code == http.StatusOK {
					var et exitTokenJSON
					_ = json.Unmarshal(w.Body.Bytes(), &et)
					tok = et.Token
				}
				var wg sync.WaitGroup
				for _, o := range pair {
					wg.Go(func() { o(f, v, tok) })
				}
				wg.Wait()
				if n := len(f.pp.Captures()); n > 1 {
					t.Fatalf("%d captures", n)
				}
				if n := f.alerts(t, v.SessionID, "max_stay_reached"); n > 1 {
					t.Fatalf("%d max_stay_reached", n)
				}
				if f.endEvents(t, v.SessionID) > 1 {
					t.Fatalf("ended with %d notifications", f.endEvents(t, v.SessionID))
				}
				registered := f.count(t, `SELECT count(*) FROM receipts WHERE session_id = ? AND settlement IN ('register', 'outside')`, v.SessionID)
				atRegister := f.state(t, v.SessionID) == AtRegister
				if (registered == 1 || atRegister) && len(f.pp.Captures()) != 0 {
					t.Fatal("charged and settled at the register")
				}
			}
		})
	}
}
