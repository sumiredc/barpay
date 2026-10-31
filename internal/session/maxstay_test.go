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

// UC3 exception: a linked session at the maximum stay is checked out as in UC5 without the gate;
// before it, nothing happens. The boundary instant is left open by the PRD.
func TestMaxStayChecksOutALinkedSession(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.clk.Set(t0.Add(maxStay - time.Second))
	if n := f.maxStayDue(t); n != 0 || f.state(t, v.SessionID) != Active {
		t.Fatalf("handled %d before the maximum stay", n)
	}
	f.clk.Set(t0.Add(maxStay + time.Second))
	if n := f.maxStayDue(t); n != 1 {
		t.Fatalf("handled %d at the maximum stay, want 1", n)
	}
	if f.state(t, v.SessionID) != Closed || len(f.pp.Captures()) != 1 || f.count(t, "SELECT count(*) FROM gate_unlocks") != 0 {
		t.Errorf("state %s, %d captures", f.state(t, v.SessionID), len(f.pp.Captures()))
	}
	evs := f.events(t, v.SessionID, "session.closed")
	if len(evs) != 1 {
		t.Fatalf("%d session.closed", len(evs))
	}
	spectest.Valid(t, "SessionClosedEvent", evs[0])
	if n := f.maxStayDue(t); n != 0 || len(f.charges()) != 1 {
		t.Error("handled again")
	}
	// The customer can check in again.
	f.checkedIn(t, "m_1")
}

// UC3 exception: an unlinked session at the maximum stay stays active with max_stay_reached raised
// once, however often and across restarts BarPay looks. A session at the register is left alone.
func TestMaxStayAlertsOnceForAnUnlinkedSession(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	reg := f.linkedIn(t, "m_1")
	f.requested(t, reg.SessionID)
	f.clk.Set(t0.Add(maxStay - time.Second))
	if n := f.maxStayDue(t); n != 0 {
		t.Fatalf("handled %d before the maximum stay", n)
	}
	f.clk.Set(t0.Add(maxStay + time.Second))
	if n := f.maxStayDue(t); n != 1 {
		t.Fatalf("handled %d, want 1", n)
	}
	f.maxStayDue(t)
	f.restart(t)
	f.clk.Advance(time.Hour)
	f.maxStayDue(t)
	if f.state(t, v.SessionID) != Active || f.alerts(t, v.SessionID, "max_stay_reached") != 1 {
		t.Errorf("state %s, %d alerts; want active with one alert", f.state(t, v.SessionID), f.alerts(t, v.SessionID, "max_stay_reached"))
	}
	if f.state(t, reg.SessionID) != AtRegister || f.alerts(t, reg.SessionID, "max_stay_reached") != 0 || len(f.charges()) != 0 {
		t.Error("the session at the register was touched")
	}
}

// Guarantees → Durability: the maximum stay is computed from the stored check-in, so a session
// that reached it while BarPay was stopped is checked out at the next start.
func TestMaxStayAcrossARestart(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.restart(t)
	f.clk.Set(t0.Add(maxStay + 3*time.Hour))
	if n := f.maxStayDue(t); n != 1 || f.state(t, v.SessionID) != Closed {
		t.Errorf("handled %d, state %s", n, f.state(t, v.SessionID))
	}
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

// Guarantees → Single payment: any two of the scan, closeSession, retryCharge, settleOutside and
// the maximum stay arriving together charge once at most and end the session with one kind of
// notification; with requestRegister or recordRegisterPayment, only one goes through, and PayPal
// is called only if the checkout did.
func TestOperationsOnOneSessionAtOnce(t *testing.T) {
	type op func(f *fixture, v View, tok string)
	scan := func(f *fixture, _ View, tok string) { f.scanRaw(tok) }
	closeOp := func(f *fixture, v View, _ string) { f.closeSession(v.SessionID, "c-"+newID()) }
	maxStayOp := func(f *fixture, _ View, _ string) { _, _ = f.api.MaxStayDue(context.Background()) }
	register := func(f *fixture, v View, _ string) { f.requestRegister(v.SessionID) }
	record := func(f *fixture, v View, _ string) { f.recordPayment(v.SessionID, amountBody(1700, "USD")) }
	retry := func(f *fixture, v View, _ string) { f.retry(v.SessionID, "r-"+newID()) }
	settle := func(f *fixture, v View, _ string) { f.settleOutside(v.SessionID, amountBody(1700, "USD")) }
	pairs := map[string][2]op{
		"scan+close": {scan, closeOp}, "scan+max stay": {scan, maxStayOp}, "close+max stay": {closeOp, maxStayOp},
		"scan+register": {scan, register}, "close+register": {closeOp, register}, "max stay+register": {maxStayOp, register},
		"scan+record": {scan, record}, "close+record": {closeOp, record}, "max stay+record": {maxStayOp, record},
		"retry+settle": {retry, settle},
	}
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
				// At the maximum stay; the token stays current, its period beginning then.
				f.clk.Set(t0.Add(maxStay))
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
