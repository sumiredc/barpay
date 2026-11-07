package member

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// attachedGuest checks a guest in and attaches member to the session (the PRD's UC10), failing
// the test unless it is attached, and returns the session as the response showed it.
func (f *fixture) attachedGuest(t *testing.T, member string) sessionView {
	t.Helper()
	w := f.do(call{method: http.MethodPost, path: "/v1/checkins", key: "key-" + strconv.FormatInt(keyN.Add(1), 10), body: `{}`})
	if w.Code != http.StatusCreated {
		t.Fatalf("check-in: %d %s", w.Code, w.Body)
	}
	g := decodeSession(t, w.Body.Bytes())
	w = f.do(call{method: http.MethodPost, path: "/v1/sessions/" + g.SessionID + "/member",
		key: "key-" + strconv.FormatInt(keyN.Add(1), 10), body: `{"member_id":"` + member + `"}`})
	if w.Code != http.StatusOK {
		t.Fatalf("attachMember: %d %s", w.Code, w.Body)
	}
	return decodeSession(t, w.Body.Bytes())
}

func sequenceOf(t *testing.T, body []byte) int64 {
	t.Helper()
	var ev struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Sequence
}

// UC10 alternative flow: a member with a saved PayPal is attached at once, unlinked and without
// waiting for PayPal; once PayPal confirms it still has the saved PayPal, the session is linked
// and session.paypal_linked follows session.member_attached.
func TestAttachedMemberWithASavedPayPalIsLinkedAfterTheCheck(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s := f.attachedGuest(t, "m_1")
	if s.PaymentMethod != "register" || len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 0 {
		t.Fatalf("the attach waited for PayPal or came back linked: %+v", s)
	}
	if n := f.checkDue(t); n != 1 {
		t.Fatalf("%d sessions linked, want 1", n)
	}
	if got := f.lookup(t, s.SessionID); got.PaymentMethod != "paypal" {
		t.Errorf("payment method %s, want paypal", got.PaymentMethod)
	}
	attached, linked := f.events(t, s.SessionID, "session.member_attached"), f.events(t, s.SessionID, "session.paypal_linked")
	if len(attached) != 1 || len(linked) != 1 {
		t.Fatalf("%d session.member_attached and %d session.paypal_linked events, want 1 each", len(attached), len(linked))
	}
	spectest.Valid(t, "SessionPaypalLinkedEvent", linked[0])
	if sequenceOf(t, linked[0]) <= sequenceOf(t, attached[0]) {
		t.Error("session.paypal_linked does not follow session.member_attached")
	}
}

// UC10 alternative flow, UC1 exception: a check that times out is made again 1 minute after it
// ended, and the session is linked once PayPal answers.
func TestCheckForAnAttachedMemberThatTimesOutIsMadeAgain(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s := f.attachedGuest(t, "m_1")
	f.pp.Next(paypaltest.CheckPaymentToken, paypaltest.NoAnswer)
	if n := f.checkDue(t); n != 0 {
		t.Fatal("linked without an answer")
	}
	f.clk.Set(t0.Add(time.Minute - time.Nanosecond))
	if f.checkDue(t); len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 1 {
		t.Fatal("checked again before 1 minute had passed")
	}
	f.clk.Set(t0.Add(time.Minute))
	if n := f.checkDue(t); n != 1 || f.lookup(t, s.SessionID).PaymentMethod != "paypal" {
		t.Errorf("after 1 minute, %d sessions linked; want the attached one", n)
	}
}

// UC10 alternative flow, UC1 exception: PayPal no longer has the saved PayPal → the session stays
// unlinked, and BarPay deletes its record and sends member.saved_paypal_deleted.
func TestCheckForAnAttachedMemberFindsTheSavedPayPalGone(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	for v := range f.pp.Vaults() {
		f.pp.Revoke(v)
	}
	s := f.attachedGuest(t, "m_1")
	if n := f.checkDue(t); n != 0 {
		t.Fatal("linked a saved PayPal PayPal no longer has")
	}
	if f.lookup(t, s.SessionID).PaymentMethod != "register" || f.getMember(t, "m_1").SavedPaypal != nil {
		t.Error("the session was linked or the saved PayPal kept")
	}
	if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 1 {
		t.Errorf("%d member.saved_paypal_deleted events, want 1", n)
	}
}

// UC10 alternative flow, UC1 exception: a session that left active before the check answered is
// not linked.
func TestCheckDoesNotLinkAnAttachedSessionNoLongerActive(t *testing.T) {
	f := newFixture(t)
	f.savedMember(t, "m_1", "payer-1")
	s := f.attachedGuest(t, "m_1")
	f.pp.During = func(c paypaltest.Call) {
		if c.Op == paypaltest.CheckPaymentToken {
			f.do(call{method: http.MethodPost, path: "/v1/sessions/" + s.SessionID + "/register-request"})
		}
	}
	if n := f.checkDue(t); n != 0 || f.lookup(t, s.SessionID).PaymentMethod != "register" {
		t.Error("a session that left active during the check was linked")
	}
}

// UC10 alternative flow, UC9: a saved PayPal whose deletion is pending does not link the session,
// and is not checked.
func TestAttachedMemberWithADeletionPendingIsNotLinked(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	f.noAnswers(paypaltest.DeletePaymentToken, sendAttempts)
	f.deleteSaved("m_1")
	s := f.attachedGuest(t, "m_1")
	f.checkDue(t)
	if f.lookup(t, s.SessionID).PaymentMethod != "register" || len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 0 {
		t.Error("the session was checked or linked during the deletion")
	}
}

// UC10 main flow, UC2: a member without a saved PayPal stays unlinked, with no check; linking
// PayPal later links the session at once.
func TestAttachedMemberWithoutASavedPayPalLinksLater(t *testing.T) {
	f := newFixture(t)
	s := f.attachedGuest(t, "m_1")
	if n := f.checkDue(t); n != 0 || len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 0 {
		t.Fatal("a member without a saved PayPal was checked or linked")
	}
	if got := f.lookup(t, s.SessionID); got.PaymentMethod != "register" {
		t.Fatalf("payment method %s, want register", got.PaymentMethod)
	}
	v := f.linked(t, "m_1", "payer-1")
	if v.SavedPaypal.InUseBySessionID == nil || *v.SavedPaypal.InUseBySessionID != s.SessionID {
		t.Errorf("in_use_by_session_id = %v, want %s", v.SavedPaypal.InUseBySessionID, s.SessionID)
	}
	if got := f.lookup(t, s.SessionID); got.PaymentMethod != "paypal" {
		t.Errorf("after linking: payment method %s, want paypal", got.PaymentMethod)
	}
	if n := len(f.events(t, s.SessionID, "session.paypal_linked")); n != 1 {
		t.Errorf("%d session.paypal_linked events, want 1", n)
	}
}
