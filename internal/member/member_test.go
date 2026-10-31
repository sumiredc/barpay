package member

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// Responsibilities, Lookup, getMember: null for a member BarPay keeps nothing for, saved once
// linked, deletion_pending while the deletion waits; 400 for a member ID that is not one.
func TestGetMember(t *testing.T) {
	f := newFixture(t)
	w := f.do(call{method: http.MethodGet, path: "/v1/members/m_1"})
	spectest.Valid(t, "Member", w.Body.Bytes())
	if w.Code != http.StatusOK || f.getMember(t, "m_1").SavedPaypal != nil {
		t.Errorf("unknown member: %d %s; want saved_paypal null", w.Code, w.Body)
	}
	f.linked(t, "m_1", "payer-1")
	w = f.do(call{method: http.MethodGet, path: "/v1/members/m_1"})
	spectest.Valid(t, "Member", w.Body.Bytes())
	if v := decodeMember(t, w.Body.Bytes()); v.SavedPaypal == nil || v.SavedPaypal.Status != "saved" {
		t.Errorf("linked: %s", w.Body)
	}
	f.noAnswers(paypaltest.DeletePaymentToken, sendAttempts)
	f.deleteSaved("m_1")
	w = f.do(call{method: http.MethodGet, path: "/v1/members/m_1"})
	spectest.Valid(t, "Member", w.Body.Bytes())
	if v := decodeMember(t, w.Body.Bytes()); v.SavedPaypal == nil || v.SavedPaypal.Status != "deletion_pending" {
		t.Errorf("deleting: %s", w.Body)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"vault_id", "token", "id"} {
		if _, ok := raw["saved_paypal"].(map[string]any)[k]; ok {
			t.Errorf("the member shows %s", k)
		}
	}
	assertError(t, f.do(call{method: http.MethodGet, path: "/v1/members/m!1"}), http.StatusBadRequest, "invalid_request")
}

// Guarantees, Access: only the store API key reaches the member operations; a missing, wrong,
// admin or edge key is refused and nothing changes.
func TestMemberOperationsNeedTheStoreAPIKey(t *testing.T) {
	f := newFixture(t)
	f.linked(t, "m_1", "payer-1")
	code := f.approvedCode(t, "m_2", "payer-2")
	ops := []call{
		{method: http.MethodGet, path: "/v1/members/m_1"},
		{method: http.MethodPost, path: "/v1/members/m_3/paypal-link", body: linkBody("consent-v1")},
		{method: http.MethodPost, path: "/v1/members/m_2/paypal-link/confirm", body: `{"code":"` + code + `"}`},
		{method: http.MethodDelete, path: "/v1/members/m_1/saved-paypal"},
	}
	for _, op := range ops {
		for _, auth := range []string{noAuth, "Bearer nope", adminAuth, edgeAuth} {
			op.auth = auth
			assertError(t, f.do(op), http.StatusUnauthorized, "unauthorized")
		}
	}
	if f.count(t, "SELECT count(*) FROM paypal_links WHERE member_id = 'm_3'") != 0 ||
		f.count(t, "SELECT count(*) FROM paypal_links WHERE code_used_at IS NOT NULL AND member_id = 'm_2'") != 0 ||
		f.getMember(t, "m_1").SavedPaypal.Status != "saved" || len(f.pp.Calls(paypaltest.DeletePaymentToken)) != 0 {
		t.Error("a refused request changed something")
	}
	// The code still works for the member it was made for, with the store API key.
	if w := f.confirm("m_2", code); w.Code != http.StatusCreated {
		t.Errorf("confirm after the refused ones: %d %s", w.Code, w.Body)
	}
}

// UC6 precondition and alternative flow, the spec's PaymentMethod, through linking: a linked
// active session that requests the register pays at the register in the response, the lookup and
// session.register_requested; one whose payment staff record at the register shows register in
// the lookup, the receipt and session.settled_outside.
func TestLinkedSessionPaidAtTheRegister(t *testing.T) {
	t.Run("register requested", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.checkin(t, "m_1")
		f.linked(t, "m_1", "payer-1")
		if got := f.lookup(t, s.SessionID); got.PaymentMethod != "paypal" {
			t.Fatalf("linked: payment method %s", got.PaymentMethod)
		}
		w := f.do(call{method: http.MethodPost, path: "/v1/sessions/" + s.SessionID + "/register-request"})
		if w.Code != http.StatusOK || decodeSession(t, w.Body.Bytes()).PaymentMethod != "register" {
			t.Errorf("response %d %s; want payment method register", w.Code, w.Body)
		}
		if got := f.lookup(t, s.SessionID); got.PaymentMethod != "register" {
			t.Errorf("lookup: payment method %s", got.PaymentMethod)
		}
		evs := f.events(t, s.SessionID, "session.register_requested")
		if len(evs) != 1 || eventSession(t, evs[0]).PaymentMethod != "register" {
			t.Errorf("session.register_requested %s", evs)
		}
	})
	t.Run("register payment recorded", func(t *testing.T) {
		f := newFixture(t)
		s, _ := f.checkin(t, "m_1")
		f.linked(t, "m_1", "payer-1")
		w := f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + s.SessionID + "/register-payment", auth: adminAuth,
			body: `{"amount_recorded":{"value":0,"currency":"USD"}}`})
		if w.Code != http.StatusOK {
			t.Fatalf("register payment: %d %s", w.Code, w.Body)
		}
		got := f.lookup(t, s.SessionID)
		if got.PaymentMethod != "register" || got.Receipt == nil || got.Receipt.Settlement != "register" {
			t.Errorf("lookup %+v; want payment method and settlement register", got)
		}
		evs := f.events(t, s.SessionID, "session.settled_outside")
		if len(evs) != 1 {
			t.Fatalf("%d session.settled_outside events", len(evs))
		}
		if v := eventSession(t, evs[0]); v.PaymentMethod != "register" || v.Receipt == nil || v.Receipt.Settlement != "register" {
			t.Errorf("session.settled_outside %s", evs[0])
		}
	})
}

func eventSession(t *testing.T, body []byte) sessionView {
	t.Helper()
	var ev struct {
		Data struct {
			Session json.RawMessage `json:"session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	return decodeSession(t, ev.Data.Session)
}
