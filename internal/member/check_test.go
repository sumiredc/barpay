package member

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// savedMember gives member a saved PayPal with payer's account and no open session.
func (f *fixture) savedMember(t *testing.T, member, payer string) {
	t.Helper()
	f.linked(t, member, payer)
}

// UC1 alternative flow: a member with a saved PayPal checks in and gets the session at once,
// unlinked; once PayPal confirms it still has it, the session is linked and
// session.paypal_linked, fitting the spec, is stored.
func TestCheckinLinksAfterTheCheck(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s, body := f.checkin(t, "m_1")
	spectest.Valid(t, "Session", body)
	if s.PaymentMethod != "register" || len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 0 {
		t.Fatalf("the check-in waited for PayPal or came back linked: %s", body)
	}
	if n := f.checkDue(t); n != 1 {
		t.Fatalf("%d sessions linked, want 1", n)
	}
	if got := f.lookup(t, s.SessionID); got.PaymentMethod != "paypal" {
		t.Errorf("payment method %s, want paypal", got.PaymentMethod)
	}
	evs := f.events(t, s.SessionID, "session.paypal_linked")
	if len(evs) != 1 {
		t.Fatalf("%d session.paypal_linked events, want 1", len(evs))
	}
	spectest.Valid(t, "SessionPaypalLinkedEvent", evs[0])
	if f.checkDue(t); len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 1 {
		t.Error("a linked session was checked again")
	}
	if v := f.getMember(t, "m_1"); v.SavedPaypal.InUseBySessionID == nil || *v.SavedPaypal.InUseBySessionID != s.SessionID {
		t.Errorf("in_use_by_session_id = %v, want %s", v.SavedPaypal.InUseBySessionID, s.SessionID)
	}
}

// UC1 exception: a check that times out keeps the saved PayPal and is made again 1 minute after
// it ended, also across a restart, and the session is linked once PayPal answers.
func TestCheckThatTimesOutIsMadeAgain(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s, _ := f.checkin(t, "m_1")
	f.pp.Next(paypaltest.CheckPaymentToken, paypaltest.NoAnswer)
	if n := f.checkDue(t); n != 0 {
		t.Fatal("linked without an answer")
	}
	if f.getMember(t, "m_1").SavedPaypal == nil || len(f.pp.Vaults()) != 1 || len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 ||
		len(f.events(t, "m_1", "member.saved_paypal_deleted")) != 0 {
		t.Fatal("a check without an answer deleted something")
	}
	f.clk.Set(t0.Add(time.Minute - time.Nanosecond))
	f.checkDue(t)
	if n := len(f.pp.Calls(paypaltest.CheckPaymentToken)); n != 1 {
		t.Fatalf("checked %d times before 1 minute had passed, want 1", n)
	}
	f.restart(t)
	f.clk.Set(t0.Add(time.Minute))
	if n := f.checkDue(t); n != 1 {
		t.Fatalf("after the restart, %d sessions linked; want 1", n)
	}
	if f.lookup(t, s.SessionID).PaymentMethod != "paypal" {
		t.Error("the session was not linked")
	}
}

// UC1 exception: a session that is no longer active when the check ends is not linked, and
// session.paypal_linked is not sent, whether it left active before a check or during one.
func TestCheckDoesNotLinkASessionNoLongerActive(t *testing.T) {
	t.Run("before the check", func(t *testing.T) {
		f := newFixture(t)
		f.savedMember(t, "m_1", "payer-1")
		s, _ := f.checkin(t, "m_1")
		f.pp.Next(paypaltest.CheckPaymentToken, paypaltest.NoAnswer)
		f.checkDue(t)
		f.do(call{method: http.MethodPost, path: "/v1/sessions/" + s.SessionID + "/register-request"})
		f.clk.Set(t0.Add(time.Hour))
		f.checkDue(t)
		if len(f.events(t, s.SessionID, "session.paypal_linked")) != 0 || f.lookup(t, s.SessionID).PaymentMethod != "register" {
			t.Error("a session at the register was linked")
		}
	})
	t.Run("during the check", func(t *testing.T) {
		f := newFixture(t)
		f.savedMember(t, "m_1", "payer-1")
		s, _ := f.checkin(t, "m_1")
		f.pp.During = func(c paypaltest.Call) {
			if c.Op == paypaltest.CheckPaymentToken {
				f.do(call{method: http.MethodPost, path: "/v1/sessions/" + s.SessionID + "/register-request"})
			}
		}
		if n := f.checkDue(t); n != 0 {
			t.Errorf("%d sessions linked, want 0", n)
		}
		if len(f.events(t, s.SessionID, "session.paypal_linked")) != 0 || f.lookup(t, s.SessionID).PaymentMethod != "register" {
			t.Error("a session that left active during the check was linked")
		}
	})
}

// UC1 exception: PayPal no longer has the saved PayPal (the customer cancelled it in PayPal) → the
// session stays unlinked, BarPay deletes its record without calling PayPal, and
// member.saved_paypal_deleted (no_longer_valid) is sent; the member can link again.
func TestCheckFindsTheSavedPayPalGone(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	for v := range f.pp.Vaults() {
		f.pp.Revoke(v)
	}
	s, _ := f.checkin(t, "m_1")
	if n := f.checkDue(t); n != 0 {
		t.Fatal("linked a saved PayPal PayPal no longer has")
	}
	if f.lookup(t, s.SessionID).PaymentMethod != "register" || f.getMember(t, "m_1").SavedPaypal != nil {
		t.Error("the session was linked or the saved PayPal kept")
	}
	evs := f.events(t, "m_1", "member.saved_paypal_deleted")
	if len(evs) != 1 {
		t.Fatalf("%d member.saved_paypal_deleted events, want 1", len(evs))
	}
	spectest.Valid(t, "MemberSavedPaypalDeletedEvent", evs[0])
	if !strings.Contains(string(evs[0]), `"reason":"no_longer_valid"`) {
		t.Errorf("event %s", evs[0])
	}
	if len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 {
		t.Error("PayPal was asked to delete what it no longer has")
	}
	if w := f.createLink("m_1", linkBody("consent-v1")); w.Code != http.StatusCreated {
		t.Errorf("linking again: %d %s", w.Code, w.Body)
	}
	if f.checkDue(t); len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 1 {
		t.Error("checked again after the saved PayPal was gone")
	}
}

// A deletion asked for while a check was out wins: the check's answer links nothing.
func TestCheckAnsweredAfterADeletion(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s, _ := f.checkin(t, "m_1")
	f.pp.During = func(c paypaltest.Call) {
		if c.Op == paypaltest.CheckPaymentToken {
			f.pp.During = nil
			if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
				t.Errorf("delete during the check: %d %s", w.Code, w.Body)
			}
		}
	}
	f.checkDue(t)
	if f.lookup(t, s.SessionID).PaymentMethod != "register" || len(f.events(t, s.SessionID, "session.paypal_linked")) != 0 {
		t.Error("the session was linked to a deleted saved PayPal")
	}
}
