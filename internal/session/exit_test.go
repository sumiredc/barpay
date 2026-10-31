package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// UC5 main flow and postcondition, Guarantees → Accuracy: the scan fixes the total at that
// instant and stops the cover charge, charges it once with the saved PayPal, and unlocks only
// after the charge; session.closed carries the receipt with the total and the capture ID; the
// saved PayPal stays.
func TestScanChargesTheTotalOnceThenUnlocks(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	tok := f.token(t, v.SessionID)
	r := f.scan(t, tok.Token)
	if !r.Unlock || r.UnlockID == "" || r.ReaderMessage != readThankYou {
		t.Fatalf("scan = %+v, want an unlock", r)
	}
	calls, caps := f.charges(), f.pp.Captures()
	if len(calls) != 1 || len(caps) != 1 {
		t.Fatalf("%d charges sent, %d captured; want 1", len(calls), len(caps))
	}
	if caps[0].Amount != usd(1700) || caps[0].InvoiceID != "barpay-"+v.SessionID {
		t.Errorf("charged %v with invoice_id %q; want 1700 USD and the session's invoice_id", caps[0].Amount, caps[0].InvoiceID)
	}
	if calls[0].RequestID != "barpay-"+v.SessionID+"-charge-1" {
		t.Errorf("PayPal-Request-Id %q", calls[0].RequestID)
	}
	// Later blocks do not add to the closed session, and the receipt equals what was charged.
	f.clk.Set(t0.Add(3 * time.Hour))
	w := f.getSession(v.SessionID)
	spectest.Valid(t, "Session", w.Body.Bytes())
	got := decodePaid(t, w.Body.Bytes())
	if s := f.state(t, v.SessionID); s != Closed || got.Receipt == nil || got.Receipt.Settlement != "paypal" ||
		got.Receipt.Total.Value != 1700 || got.Receipt.PaypalCaptureID == nil || *got.Receipt.PaypalCaptureID != caps[0].ID {
		t.Errorf("state %s, receipt %s", s, w.Body)
	}
	evs := f.events(t, v.SessionID, "session.closed")
	if len(evs) != 1 {
		t.Fatalf("%d session.closed, want 1", len(evs))
	}
	spectest.Valid(t, "SessionClosedEvent", evs[0])
	if !strings.Contains(string(evs[0]), caps[0].ID) {
		t.Errorf("session.closed without the capture ID: %s", evs[0])
	}
	if _, ok := f.saved.vaults["m_1"]; !ok || len(f.pp.Vaults()) != 1 {
		t.Error("the saved PayPal was not kept")
	}
}

// A block ending exactly as the QR is scanned has ended and is charged; one that has not ended is
// not (the PRD's Pricing model, Blocks).
func TestScanChargesOnlyEndedBlocks(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.clk.Set(t0.Add(30 * time.Minute))
	f.scan(t, f.token(t, v.SessionID).Token)
	if caps := f.pp.Captures(); len(caps) != 1 || caps[0].Amount != usd(1900) {
		t.Errorf("captures %+v, want one of 1900 USD (three blocks)", caps)
	}
	g := newFixture(t)
	v = g.ordered(t, "m_1")
	g.clk.Set(t0.Add(30*time.Minute - time.Nanosecond))
	g.scan(t, g.token(t, v.SessionID).Token)
	if caps := g.pp.Captures(); len(caps) != 1 || caps[0].Amount != usd(1700) {
		t.Errorf("captures %+v, want one of 1700 USD (two blocks)", caps)
	}
}

// Responsibilities → Notifications, Guarantees → Unlock only on payment: a total of 0 charges
// nothing, unlocks the gate and ends with session.closed without a capture.
func TestScanOfAZeroTotal(t *testing.T) {
	f := newFixture(t)
	v := f.linkedIn(t, "m_1")
	f.clk.Advance(5 * time.Minute)
	w0 := f.wake.n.Load()
	if r := f.scan(t, f.token(t, v.SessionID).Token); !r.Unlock {
		t.Fatalf("scan = %+v, want an unlock", r)
	}
	if n := len(f.charges()); n != 0 {
		t.Errorf("%d charges sent for a total of 0", n)
	}
	if f.wake.n.Load() == w0 {
		t.Error("the dispatcher was not woken")
	}
	got := decodePaid(t, f.getSession(v.SessionID).Body.Bytes())
	if got.Receipt == nil || got.Receipt.Settlement != "paypal" || got.Receipt.PaypalCaptureID != nil {
		t.Errorf("receipt %+v, want paypal without a capture", got.Receipt)
	}
	evs := f.events(t, v.SessionID, "session.closed")
	if len(evs) != 1 {
		t.Fatalf("%d session.closed, want 1", len(evs))
	}
	spectest.Valid(t, "SessionClosedEvent", evs[0])
}

// UC5 exception: a token that is unknown, malformed or made up is refused; nothing is charged
// and the gate stays locked.
func TestScanOfAnUnknownToken(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	real := f.token(t, v.SessionID).Token
	forged := real[:len(real)-1] + map[bool]string{true: "B", false: "A"}[strings.HasSuffix(real, "A")]
	for _, tok := range []string{"x", strings.Repeat("A", 60), "ses_" + newID()[4:] + strings.Repeat("A", 32), forged} {
		if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readNewQR {
			t.Errorf("scan of %q = %+v, want refused", tok, r)
		}
	}
	if len(f.charges()) != 0 || f.state(t, v.SessionID) != Active {
		t.Error("an unknown token charged or moved the session")
	}
}

// Responsibilities → Exit token, UC5: the token changes at the refresh interval and an old one is
// refused. The boundary instant is left open by the PRD, so only both sides are checked.
func TestExitTokenChangesAtTheRefreshInterval(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	start := t0.Add(25 * time.Minute) // the 50th period of 30 seconds starts here
	first := f.token(t, v.SessionID)
	if first.ExpiresAt != httpapi.FormatTime(start.Add(refresh)) {
		t.Errorf("expires_at %s, want %s", first.ExpiresAt, httpapi.FormatTime(start.Add(refresh)))
	}
	f.clk.Set(start.Add(refresh - time.Second))
	if tok := f.token(t, v.SessionID); tok.Token != first.Token {
		t.Error("the token changed before the refresh interval")
	}
	f.clk.Set(start.Add(refresh + time.Second))
	next := f.token(t, v.SessionID)
	if next.Token == first.Token {
		t.Fatal("the token did not change after the refresh interval")
	}
	if r := f.scan(t, first.Token); r.Unlock || r.ReaderMessage != readNewQR {
		t.Errorf("old token: %+v, want refused", r)
	}
	if len(f.charges()) != 0 || f.state(t, v.SessionID) != Active {
		t.Fatal("an old token charged")
	}
	if r := f.scan(t, next.Token); !r.Unlock {
		t.Errorf("current token: %+v, want an unlock", r)
	}
}

// UC5 alternative flow, Guarantees → Unlock only on payment: once closed, the session's token no
// longer changes and opens the gate again within the re-unlock window without charging; after
// it, it is neither issued nor accepted. Both sides of the window's end are checked.
func TestReunlockWithinTheWindow(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	tok := f.token(t, v.SessionID).Token
	f.scan(t, tok)
	closedAt := f.clk.Now()
	f.clk.Set(closedAt.Add(reunlock - time.Second))
	again := f.token(t, v.SessionID)
	if again.Token != tok || again.ExpiresAt != httpapi.FormatTime(closedAt.Add(reunlock)) {
		t.Errorf("token after closing: %+v, want the scanned one until the window ends", again)
	}
	if r := f.scan(t, tok); !r.Unlock || r.ReaderMessage != readThankYou {
		t.Errorf("within the window: %+v, want an unlock", r)
	}
	f.clk.Set(closedAt.Add(reunlock + time.Second))
	assertError(t, f.exitToken(v.SessionID), http.StatusConflict, "exit_token_unavailable")
	if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readSeeStaff {
		t.Errorf("after the window: %+v, want refused", r)
	}
	if n := len(f.charges()); n != 1 {
		t.Errorf("%d charges, want 1", n)
	}
}

// Responsibilities → Exit token, Guarantees → Unlock only on payment: a session whose payment
// staff recorded at the register, or settled outside BarPay, gets a token that opens the gate
// within the re-unlock window; one closed as unpaid gets none.
func TestTokenAfterAPaymentStaffRecorded(t *testing.T) {
	f := newFixture(t)
	reg := f.atRegister(t)
	if w := f.recordPayment(reg.SessionID, amountBody(1700, "USD")); w.Code != http.StatusOK {
		t.Fatalf("recordRegisterPayment: %d %s", w.Code, w.Body)
	}
	if r := f.scan(t, f.token(t, reg.SessionID).Token); !r.Unlock {
		t.Errorf("register: %+v, want an unlock", r)
	}
	out := f.failed(t, "m_2")
	if w := f.settleOutside(out.SessionID, amountBody(1700, "USD")); w.Code != http.StatusOK {
		t.Fatalf("settleOutside: %d %s", w.Code, w.Body)
	}
	if r := f.scan(t, f.token(t, out.SessionID).Token); !r.Unlock {
		t.Errorf("outside: %+v, want an unlock", r)
	}
	unpaid := f.atRegister(t)
	if w := f.markUnpaid(unpaid.SessionID); w.Code != http.StatusOK {
		t.Fatalf("markUnpaid: %d %s", w.Code, w.Body)
	}
	assertError(t, f.exitToken(unpaid.SessionID), http.StatusConflict, "exit_token_unavailable")
	if n := len(f.charges()); n != 1 {
		t.Errorf("%d charges, want only the refused one", n)
	}
}

// UC5 exceptions: no token for a session that is not linked, at the register, checking out,
// payment failed, or closed past the window, and the scan of a token it had is refused without
// charging; an unlinked or at-the-register customer is sent to the register.
func TestNoExitTokenUnlessLinkedAndActiveOrJustClosed(t *testing.T) {
	f := newFixture(t)
	key, err := f.api.tokenKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	guest := f.checkedIn(t, "")
	assertError(t, f.exitToken(guest.SessionID), http.StatusConflict, "exit_token_unavailable")
	s, _ := get(context.Background(), f.st.Read(), guest.SessionID)
	made, _ := f.api.activeToken(key, s, f.clk.Now())
	if r := f.scan(t, made); r.Unlock || r.ReaderMessage != readPayAtRegister {
		t.Errorf("not linked: %+v, want the register", r)
	}

	reg := f.linkedIn(t, "m_1")
	tok := f.token(t, reg.SessionID).Token
	f.requested(t, reg.SessionID)
	assertError(t, f.exitToken(reg.SessionID), http.StatusConflict, "exit_token_unavailable")
	if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readPayAtRegister {
		t.Errorf("at the register: %+v, want the register", r)
	}

	pending := f.ordered(t, "m_2")
	f.pp.Next(paypaltest.Charge, paypaltest.NoAnswer)
	tok = f.token(t, pending.SessionID).Token
	f.scan(t, tok)
	assertError(t, f.exitToken(pending.SessionID), http.StatusConflict, "exit_token_unavailable")
	if r := f.scan(t, tok); r.Unlock || r.ReaderMessage != readSeeStaff {
		t.Errorf("checking out: %+v, want staff", r)
	}

	failed := f.failed(t, "m_3")
	assertError(t, f.exitToken(failed.SessionID), http.StatusConflict, "exit_token_unavailable")

	if n := len(f.charges()); n != 2 {
		t.Errorf("%d charges, want only the two first scans", n)
	}
	assertError(t, f.exitToken("ses_"+newID()[4:]), http.StatusNotFound, "session_not_found")
}

// UC5 exception: a refused charge leaves the gate locked and sends the customer to staff; the
// session is PaymentFailed with the unpaid amount, an alert is raised and session.payment_failed
// (refused) is sent. The saved PayPal stays for the staff retry.
func TestRefusedCharge(t *testing.T) {
	f := newFixture(t)
	v := f.failed(t, "m_1")
	if v.UnpaidAmount == nil || v.UnpaidAmount.Value != 1700 {
		t.Errorf("unpaid_amount %+v, want 1700", v.UnpaidAmount)
	}
	if f.alerts(t, v.SessionID, "payment_failed") != 1 || f.count(t, "SELECT count(*) FROM gate_unlocks") != 0 {
		t.Error("no payment_failed alert, or an unlock")
	}
	evs := f.events(t, v.SessionID, "session.payment_failed")
	if len(evs) != 1 {
		t.Fatalf("%d session.payment_failed, want 1", len(evs))
	}
	spectest.Valid(t, "SessionPaymentFailedEvent", evs[0])
	if state, reason := eventData(t, evs[0]); state != PaymentFailed || reason != "refused" {
		t.Errorf("event %s, %s", state, reason)
	}
	if _, ok := f.saved.vaults["m_1"]; !ok || len(f.saved.gone) != 0 {
		t.Error("the saved PayPal was not kept")
	}
}

// UC5 exception: PayPal answering that it no longer has the saved PayPal (permission denied)
// fails the charge as refused, and BarPay deletes the saved PayPal; staff then settle outside
// BarPay, since a retry has nothing to charge.
func TestChargeWithASavedPaypalPayPalNoLongerHas(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.pp.Revoke(f.saved.vaults["m_1"])
	if r := f.scan(t, f.token(t, v.SessionID).Token); r.Unlock || r.ReaderMessage != readSeeStaff {
		t.Fatalf("scan = %+v, want staff", r)
	}
	if s := f.state(t, v.SessionID); s != PaymentFailed || len(f.saved.gone) != 1 {
		t.Fatalf("state %s, saved PayPals forgotten %v", s, f.saved.gone)
	}
	evs := f.events(t, v.SessionID, "session.payment_failed")
	if _, reason := eventData(t, evs[0]); reason != "refused" {
		t.Errorf("reason %s, want refused", reason)
	}
	assertError(t, f.retry(v.SessionID, "r-1"), http.StatusConflict, "saved_paypal_not_found")
	if w := f.settleOutside(v.SessionID, amountBody(1700, "USD")); w.Code != http.StatusOK {
		t.Errorf("settleOutside: %d %s", w.Code, w.Body)
	}
}

func (f *fixture) reportUnlock(id string, unlocked bool) *httptest.ResponseRecorder {
	body := `{"unlocked":false}`
	if unlocked {
		body = `{"unlocked":true}`
	}
	return f.do(call{method: http.MethodPost, path: "/v1/edge/unlocks/" + id + "/result", auth: edgeAuth, body: body})
}

// UC5 exception: the charge succeeded but the gate did not unlock: the payment stands, an alert
// is raised and gate.unlock_failed sent, once however often it is reported.
func TestUnlockFailedAfterACharge(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	r := f.scan(t, f.token(t, v.SessionID).Token)
	for range 2 {
		if w := f.reportUnlock(r.UnlockID, false); w.Code != http.StatusNoContent {
			t.Fatalf("report: %d %s", w.Code, w.Body)
		}
	}
	if f.state(t, v.SessionID) != Closed || f.alerts(t, v.SessionID, "unlock_failed") != 1 {
		t.Errorf("state %s, %d alerts", f.state(t, v.SessionID), f.alerts(t, v.SessionID, "unlock_failed"))
	}
	evs := f.events(t, v.SessionID, "gate.unlock_failed")
	if len(evs) != 1 {
		t.Fatalf("%d gate.unlock_failed, want 1", len(evs))
	}
	spectest.Valid(t, "GateUnlockFailedEvent", evs[0])

	again := f.scan(t, f.token(t, v.SessionID).Token)
	if w := f.reportUnlock(again.UnlockID, true); w.Code != http.StatusNoContent || f.alerts(t, v.SessionID, "unlock_failed") != 1 {
		t.Errorf("a working unlock: %d, %d alerts", w.Code, f.alerts(t, v.SessionID, "unlock_failed"))
	}
	assertError(t, f.reportUnlock("glk_"+newID()[4:], false), http.StatusNotFound, "unlock_not_found")
	assertError(t, f.reportUnlock("nope", false), http.StatusBadRequest, "invalid_request")
}

// UC5 exception, Guarantees → Single payment: two scans of the same QR at the same time run one
// checkout, and both get the same answer, for a success and for an unknown outcome.
func TestTwoScansAtOnce(t *testing.T) {
	for name, fault := range map[string]paypaltest.Fault{"charged": paypaltest.NoFault, "unknown": paypaltest.LostAnswer} {
		t.Run(name, func(t *testing.T) {
			for range 20 {
				f := newFixture(t)
				v := f.ordered(t, "m_1")
				f.pp.Next(paypaltest.Charge, fault)
				f.pp.During = func(paypaltest.Call) { time.Sleep(5 * time.Millisecond) }
				tok := f.token(t, v.SessionID).Token
				var wg sync.WaitGroup
				got := make([]scanResultJSON, 2)
				for i := range got {
					wg.Go(func() { got[i] = f.scan(t, tok) })
				}
				wg.Wait()
				if got[0].Unlock != got[1].Unlock || got[0].ReaderMessage != got[1].ReaderMessage {
					t.Fatalf("different answers %+v and %+v", got[0], got[1])
				}
				if n := len(f.charges()); n != 1 {
					t.Fatalf("%d charges sent, want 1", n)
				}
			}
		})
	}
}

// Guarantees → Access: an exit token is never written to logs, and its response is not cached.
func TestExitTokenIsNotLoggedOrCached(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	w := f.exitToken(v.SessionID)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	tok := f.token(t, v.SessionID).Token
	f.pp.Next(paypaltest.Charge, paypaltest.NoAnswer)
	f.scan(t, tok)
	f.scan(t, tok+"x")
	if strings.Contains(f.log.String(), tok) {
		t.Error("the exit token is in the log")
	}
}

// Guarantees → Access: each operation needs its own key and no other.
func TestExitOperationsNeedTheirKey(t *testing.T) {
	f := newFixture(t)
	v := f.linkedIn(t, "m_1")
	for _, c := range []struct {
		method, path, body, pass string
	}{
		{http.MethodGet, "/v1/sessions/" + v.SessionID + "/exit-token", "", storeAuth},
		{http.MethodPost, "/v1/admin/sessions/" + v.SessionID + "/close", "", adminAuth},
		{http.MethodPost, "/v1/admin/sessions/" + v.SessionID + "/retry", "", adminAuth},
		{http.MethodPost, "/v1/admin/sessions/" + v.SessionID + "/settle-outside", amountBody(0, "USD"), adminAuth},
		{http.MethodPost, "/v1/admin/gate/unlock", `{}`, adminAuth},
		{http.MethodPost, "/v1/edge/exit-scans", `{"token":"x"}`, edgeAuth},
		{http.MethodGet, "/v1/edge/unlocks/next", "", edgeAuth},
		{http.MethodPost, "/v1/edge/unlocks/glk_AAAAAAAAAAAAAAAAAAAAAAAAAA/result", `{"unlocked":true}`, edgeAuth},
	} {
		for _, auth := range []string{noAuth, storeAuth, adminAuth, edgeAuth} {
			if auth == c.pass {
				continue
			}
			w := f.do(call{method: c.method, path: c.path, auth: auth, key: "k-1", body: c.body})
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: %d, want 401", c.method, c.path, auth, w.Code)
			}
		}
	}
	if len(f.charges()) != 0 || f.state(t, v.SessionID) != Active || f.count(t, "SELECT count(*) FROM staff_actions") != 0 {
		t.Error("a call without its key changed something")
	}
}

// Guarantees → Charges only for this visit: nothing but checkout and a staff retry charges a
// saved PayPal: not check-in, orders, lookups, exit tokens, the register or recording a payment.
func TestOnlyCheckoutCharges(t *testing.T) {
	f := newFixture(t)
	v := f.ordered(t, "m_1")
	f.looked(t, v.SessionID)
	f.token(t, v.SessionID)
	f.requested(t, v.SessionID)
	f.recordPayment(v.SessionID, amountBody(1700, "USD"))
	f.clk.Set(t0.Add(maxStay + time.Hour))
	f.maxStayDue(t)
	f.chargeDue(t)
	if n := len(f.charges()); n != 0 {
		t.Errorf("%d charges", n)
	}
	if got := f.looked(t, v.SessionID); got.Receipt == nil || got.PaymentMethod != "register" {
		t.Errorf("session %+v", got)
	}
}
