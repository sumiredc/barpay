package member

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// noAnswers makes the next n calls of op give no answer.
func (f *fixture) noAnswers(op paypaltest.Op, n int) {
	for range n {
		f.pp.Next(op, paypaltest.NoAnswer)
	}
}

// UC9 main flow: PayPal confirms within the request → 204; the member has no saved PayPal, PayPal
// no longer holds it, and member.saved_paypal_deleted (member_request) fits the spec.
func TestDeleteConfirmedAtOnce(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	w := f.deleteSaved("m_1")
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if f.getMember(t, "m_1").SavedPaypal != nil || len(f.pp.Vaults()) != 0 {
		t.Error("the saved PayPal is still there")
	}
	evs := f.events(t, "m_1", "member.saved_paypal_deleted")
	if len(evs) != 1 {
		t.Fatalf("%d member.saved_paypal_deleted events, want 1", len(evs))
	}
	spectest.Valid(t, "MemberSavedPaypalDeletedEvent", evs[0])
	if !contains(evs[0], `"reason":"member_request"`) || !contains(evs[0], `"saved_paypal":null`) {
		t.Errorf("event %s", evs[0])
	}
	// UC9 postcondition: the next check-in starts unlinked, and linking again goes through PayPal.
	s, _ := f.checkin(t, "m_1")
	f.checkDue(t)
	if f.lookup(t, s.SessionID).PaymentMethod != "register" {
		t.Error("the check-in after the deletion was linked")
	}
	if w := f.createLink("m_1", linkBody("consent-v2")); w.Code != http.StatusCreated {
		t.Errorf("linking again: %d %s", w.Code, w.Body)
	}
}

func contains(b []byte, s string) bool { return strings.Contains(string(b), s) }

// UC9 alternative flow: PayPal does not confirm within the request → 202 and deletion_pending; it
// is tried again 1 minute after the previous try ended, until confirmed, also across a restart,
// and member.saved_paypal_deleted is sent once, after the confirmation.
func TestDeleteRetriedUntilConfirmed(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	f.noAnswers(paypaltest.DeletePaymentToken, sendAttempts)
	w := f.deleteSaved("m_1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Member", w.Body.Bytes())
	if v := decodeMember(t, w.Body.Bytes()); v.SavedPaypal == nil || v.SavedPaypal.Status != "deletion_pending" {
		t.Errorf("response %s; want deletion_pending", w.Body)
	}
	if v := f.getMember(t, "m_1"); v.SavedPaypal == nil || v.SavedPaypal.Status != "deletion_pending" {
		t.Errorf("getMember %+v; want deletion_pending", v.SavedPaypal)
	}
	if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 0 {
		t.Errorf("%d events before PayPal confirmed", n)
	}
	// A repeat while pending starts nothing new.
	before := len(f.pp.Calls(paypaltest.DeletePaymentToken))
	if w := f.deleteSaved("m_1"); w.Code != http.StatusAccepted || len(f.pp.Calls(paypaltest.DeletePaymentToken)) != before {
		t.Errorf("repeat while pending: %d, %d more calls", w.Code, len(f.pp.Calls(paypaltest.DeletePaymentToken))-before)
	}

	f.clk.Set(t0.Add(time.Minute - time.Nanosecond))
	if f.deleteDue(t); len(f.pp.Calls(paypaltest.DeletePaymentToken)) != before {
		t.Error("tried again before 1 minute had passed")
	}
	f.restart(t)
	f.noAnswers(paypaltest.DeletePaymentToken, sendAttempts)
	f.clk.Set(t0.Add(time.Minute))
	if n := f.deleteDue(t); n != 0 {
		t.Fatalf("%d confirmed, want 0", n)
	}
	f.clk.Set(t0.Add(2*time.Minute - time.Nanosecond))
	f.deleteDue(t)
	f.clk.Set(t0.Add(2 * time.Minute))
	if n := f.deleteDue(t); n != 1 {
		t.Fatalf("%d confirmed, want 1", n)
	}
	if f.getMember(t, "m_1").SavedPaypal != nil || len(f.pp.Vaults()) != 0 {
		t.Error("the saved PayPal is still there")
	}
	if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 1 {
		t.Errorf("%d member.saved_paypal_deleted events, want 1", n)
	}
	f.clk.Set(t0.Add(time.Hour))
	f.deleteDue(t)
	if n := len(f.pp.Calls(paypaltest.DeletePaymentToken)); n != 2*sendAttempts+1 {
		t.Errorf("%d deletion calls, want %d: nothing after the confirmation", n, 2*sendAttempts+1)
	}
}

// UC9 alternative flow: while the deletion is pending the member cannot link again and a
// check-in starts unlinked, without a check.
func TestDeletionPendingBlocksLinkingAndCheckin(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	f.noAnswers(paypaltest.DeletePaymentToken, sendAttempts)
	f.deleteSaved("m_1")
	assertError(t, f.createLink("m_1", linkBody("consent-v1")), http.StatusConflict, "saved_paypal_exists")
	s, _ := f.checkin(t, "m_1")
	f.checkDue(t)
	if f.lookup(t, s.SessionID).PaymentMethod != "register" || len(f.pp.Calls(paypaltest.CheckPaymentToken)) != 0 {
		t.Error("a check-in during the deletion was checked or linked")
	}
}

// UC9 alternative flow, Guarantees, Observability: a deletion PayPal has not confirmed 1 hour after
// the member asked raises deletion_failing, once. A refusal makes the next try a new attempt.
func TestDeletionThatKeepsFailingRaisesAnAlert(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	for range 70 {
		f.pp.Next(paypaltest.DeletePaymentToken, paypaltest.Refuse)
	}
	f.deleteSaved("m_1")
	alerts := func() int {
		return f.count(t, "SELECT count(*) FROM alerts WHERE kind = 'deletion_failing' AND member_id = 'm_1'")
	}
	for m := 1; m < 60; m++ {
		f.clk.Set(t0.Add(time.Duration(m) * time.Minute))
		f.deleteDue(t)
	}
	if n := alerts(); n != 0 {
		t.Fatalf("%d alerts before an hour had passed", n)
	}
	f.clk.Set(t0.Add(time.Hour))
	f.deleteDue(t)
	if n := alerts(); n != 1 {
		t.Errorf("at 1 hour: %d deletion_failing alerts, want 1", n)
	}
	f.clk.Set(t0.Add(61 * time.Minute))
	f.deleteDue(t)
	if n := alerts(); n != 1 {
		t.Errorf("after 1 hour: %d deletion_failing alerts, want 1", n)
	}
	calls := f.pp.Calls(paypaltest.DeletePaymentToken)
	if len(calls) != 62 || calls[0].RequestID == calls[1].RequestID {
		t.Errorf("%d calls; a refused try was sent again with %s", len(calls), calls[1].RequestID)
	}
}

// A deletion confirmed by two tries at once — the request's and Run's — is finished and
// notified once.
func TestDeletionConfirmedTwiceIsNotifiedOnce(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	f.pp.During = func(c paypaltest.Call) {
		if c.Op == paypaltest.DeletePaymentToken {
			f.pp.During = nil
			f.clk.Advance(deleteRetryAfter)
			f.deleteDue(t)
		}
	}
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if n := len(f.pp.Calls(paypaltest.DeletePaymentToken)); n != 2 {
		t.Fatalf("%d deletion calls, want 2", n)
	}
	if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 1 {
		t.Errorf("%d member.saved_paypal_deleted events, want 1", n)
	}
}

// The return carries the store app's own query, but never a code of its own on a failed return.
func TestReturnURLWithACodeOfItsOwn(t *testing.T) {
	f := newFixture(t)
	body := `{"state":"` + state + `","consent_version":"v1","return_url":"` + returnURL + `&code=plc_OLD"}`
	w := f.createLink("m_1", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("createPaypalLink: %d %s", w.Code, w.Body)
	}
	var out struct {
		LinkURL string `json:"link_url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	token := f.toPayPal(t, out.LinkURL)
	if q := returned(t, f.browse(t, f.pp.URLs(token).Return)); q.Get("paypal_link") != "failed" || q.Has("code") {
		t.Errorf("not approved: %v; want failed without a code", q)
	}
}

// UC9 exception, Guarantees → Charges only for this visit: a saved PayPal that a session checking
// out or whose payment failed uses is not deleted; once staff settle that session outside
// BarPay, it is.
func TestDeleteRefusedWhileCheckoutUsesIt(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_1")
	f.linked(t, "m_1", "payer-1")
	f.clk.Advance(25 * time.Minute) // two blocks: a total of $4.00
	f.pp.Next(paypaltest.Charge, paypaltest.NoAnswer, paypaltest.Refuse)
	w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/close", auth: adminAuth, key: "k-1"})
	if w.Code != http.StatusOK || decodeSession(t, w.Body.Bytes()).State != "checking_out" {
		t.Fatalf("closeSession: %d %s", w.Code, w.Body)
	}
	assertError(t, f.deleteSaved("m_1"), http.StatusConflict, "saved_paypal_in_use")
	f.clk.Advance(time.Minute)
	if _, err := f.sessions.ChargeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.lookup(t, s.SessionID); got.State != "payment_failed" {
		t.Fatalf("state %s, want payment_failed", got.State)
	}
	assertError(t, f.deleteSaved("m_1"), http.StatusConflict, "saved_paypal_in_use")
	if len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 || f.getMember(t, "m_1").SavedPaypal.Status != "saved" {
		t.Error("something was deleted")
	}
	w = f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/settle-outside", auth: adminAuth,
		body: `{"amount_recorded":{"value":400,"currency":"USD"}}`})
	if w.Code != http.StatusOK {
		t.Fatalf("settleOutside: %d %s", w.Code, w.Body)
	}
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Errorf("after the session closed: %d %s", w.Code, w.Body)
	}
}

// UC5 exception: a charge PayPal refuses because it no longer has the saved PayPal (permission
// denied) deletes BarPay's record of it, as at check-in, with member.saved_paypal_deleted
// (no_longer_valid); the member can link again.
func TestChargeWithASavedPaypalPayPalNoLongerHasDeletesIt(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_1")
	f.linked(t, "m_1", "payer-1")
	for v := range f.pp.Vaults() {
		f.pp.Revoke(v)
	}
	f.clk.Advance(25 * time.Minute)
	w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/close", auth: adminAuth, key: "k-1"})
	if w.Code != http.StatusOK || decodeSession(t, w.Body.Bytes()).State != "payment_failed" {
		t.Fatalf("closeSession: %d %s", w.Code, w.Body)
	}
	if got := f.getMember(t, "m_1"); got.SavedPaypal != nil {
		t.Errorf("saved PayPal %+v, want deleted", got.SavedPaypal)
	}
	evs := f.events(t, "m_1", "member.saved_paypal_deleted")
	if len(evs) != 1 || !strings.Contains(string(evs[0]), `"no_longer_valid"`) {
		t.Fatalf("events %s", evs)
	}
	spectest.Valid(t, "MemberSavedPaypalDeletedEvent", evs[0])
	w = f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/retry", auth: adminAuth, key: "k-2"})
	assertError(t, w, http.StatusConflict, "saved_paypal_not_found")
	f.linked(t, "m_1", "payer-1")
}

// UC9 exception: a saved PayPal that a session not yet ended uses — linked and active, or linked
// and waiting at the register — is not deleted; once that session ends, it is.
func TestDeleteRefusedWhileASessionUsesIt(t *testing.T) {
	f := newFixture(t)
	s, _ := f.checkin(t, "m_1")
	f.linked(t, "m_1", "payer-1")
	w := f.deleteSaved("m_1")
	assertError(t, w, http.StatusConflict, "saved_paypal_in_use")
	spectest.Valid(t, "Error", w.Body.Bytes())
	if _, _, id := errorOf(t, w); id != s.SessionID {
		t.Errorf("details.session_id = %q, want %s", id, s.SessionID)
	}
	f.do(call{method: http.MethodPost, path: "/v1/sessions/" + s.SessionID + "/register-request"})
	assertError(t, f.deleteSaved("m_1"), http.StatusConflict, "saved_paypal_in_use")
	if len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 || f.getMember(t, "m_1").SavedPaypal.Status != "saved" {
		t.Error("something was deleted")
	}
	w = f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/register-payment", auth: adminAuth,
		body: `{"amount_recorded":{"value":0,"currency":"USD"}}`})
	if w.Code != http.StatusOK {
		t.Fatalf("register payment: %d %s", w.Code, w.Body)
	}
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Errorf("after the session closed: %d %s", w.Code, w.Body)
	}
}

// deleteSavedPaypal: a member without a saved PayPal is refused, and a malformed member ID too.
func TestDeleteWithoutASavedPayPal(t *testing.T) {
	f := newFixture(t)
	assertError(t, f.deleteSaved("m_1"), http.StatusNotFound, "saved_paypal_not_found")
	assertError(t, f.deleteSaved("m!1"), http.StatusBadRequest, "invalid_request")
}
