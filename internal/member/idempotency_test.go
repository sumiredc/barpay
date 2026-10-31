package member

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
)

// storedBefore fails t unless, when each call of op arrives at PayPal, its PayPal-Request-Id is
// already stored, as query reads it, and derived from the link or deletion ID it names.
func (f *fixture) storedBefore(t *testing.T, op paypaltest.Op, query, prefix string) {
	t.Helper()
	f.pp.During = func(c paypaltest.Call) {
		if c.Op != op {
			return
		}
		var ids []string
		rows, err := f.st.Read().QueryContext(context.Background(), query)
		if err != nil {
			t.Error(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, owner string
			if err := rows.Scan(&id, &owner); err != nil {
				t.Error(err)
				return
			}
			if id == c.RequestID && strings.HasPrefix(id, "barpay-"+owner+"-"+prefix+"-") {
				return
			}
			ids = append(ids, id)
		}
		t.Errorf("%s was sent with %q; stored before it: %v", op, c.RequestID, ids)
	}
}

// Fixed constraints, Idempotency: the PayPal-Request-Id of each save and delete call is stored
// before the call, derived from the link's or the deletion's ID, the step and the attempt.
func TestRequestIDsAreStoredBeforeTheCall(t *testing.T) {
	f := newFixture(t)
	f.storedBefore(t, paypaltest.CreateSetupToken, "SELECT setup_request_id, id FROM paypal_links WHERE setup_request_id IS NOT NULL", "setup_token")
	token := f.toPayPal(t, f.linkURL(t, "m_1"))
	f.pp.Approve(token, "payer-1")
	code := returned(t, f.browse(t, f.pp.URLs(token).Return)).Get("code")
	f.storedBefore(t, paypaltest.CreatePaymentToken, "SELECT save_request_id, id FROM paypal_links WHERE save_request_id IS NOT NULL", "payment_token")
	if w := f.confirm("m_1", code); w.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	f.storedBefore(t, paypaltest.DeletePaymentToken, "SELECT request_id, id FROM paypal_deletions", "delete")
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	for _, op := range []paypaltest.Op{paypaltest.CreateSetupToken, paypaltest.CreatePaymentToken, paypaltest.DeletePaymentToken} {
		if calls := f.pp.Calls(op); len(calls) != 1 || !strings.HasSuffix(calls[0].RequestID, "-1") {
			t.Errorf("%s: %+v; want one first attempt", op, calls)
		}
	}
}

// Fixed constraints, Idempotency: a network retry sends the same PayPal-Request-Id.
func TestNetworkRetriesReuseTheRequestID(t *testing.T) {
	f := newFixture(t)
	f.noAnswers(paypaltest.CreateSetupToken, 2)
	f.noAnswers(paypaltest.CreatePaymentToken, 2)
	f.linked(t, "m_1", "payer-1")
	f.noAnswers(paypaltest.DeletePaymentToken, 2)
	if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	for _, op := range []paypaltest.Op{paypaltest.CreateSetupToken, paypaltest.CreatePaymentToken, paypaltest.DeletePaymentToken} {
		calls := f.pp.Calls(op)
		if len(calls) != 3 || calls[0].RequestID != calls[1].RequestID || calls[1].RequestID != calls[2].RequestID {
			t.Errorf("%s: %+v; want 3 sends with one PayPal-Request-Id", op, calls)
		}
	}
}

// Opening the link URL again after PayPal gave no answer sends the same setup token request.
func TestReopeningTheLinkURLResendsTheSameSetupTokenRequest(t *testing.T) {
	f := newFixture(t)
	u := f.linkURL(t, "m_1")
	f.noAnswers(paypaltest.CreateSetupToken, sendAttempts)
	if q := returned(t, f.browse(t, u)); q.Get("paypal_link") != "failed" {
		t.Fatalf("no answer: %v; want failed", q)
	}
	f.toPayPal(t, u)
	calls := f.pp.Calls(paypaltest.CreateSetupToken)
	if len(calls) != sendAttempts+1 || calls[0].RequestID != calls[sendAttempts].RequestID {
		t.Errorf("calls %+v; want one PayPal-Request-Id", calls)
	}
}

// Fixed constraints, Idempotency: PayPal saves but the answer is lost; BarPay sends the same
// PayPal-Request-Id again, gets the same saved PayPal, and no second one exists.
func TestLostSaveAnswerIsResent(t *testing.T) {
	f := newFixture(t)
	code := f.approvedCode(t, "m_1", "payer-1")
	f.pp.Next(paypaltest.CreatePaymentToken, paypaltest.LostAnswer)
	w := f.confirm("m_1", code)
	if w.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	calls := f.pp.Calls(paypaltest.CreatePaymentToken)
	if len(calls) != 2 || calls[0].RequestID != calls[1].RequestID {
		t.Errorf("calls %+v; want one resend with the same PayPal-Request-Id", calls)
	}
	vaults := f.pp.Vaults()
	var held string
	if err := f.st.Read().QueryRowContext(context.Background(), "SELECT vault_id FROM saved_paypals WHERE member_id = 'm_1'").Scan(&held); err != nil {
		t.Fatal(err)
	}
	if _, ok := vaults[held]; len(vaults) != 1 || !ok {
		t.Errorf("PayPal holds %v and BarPay %s; want the one saved PayPal", vaults, held)
	}
	if n := len(f.events(t, "m_1", "member.paypal_linked")); n != 1 {
		t.Errorf("%d member.paypal_linked events, want 1", n)
	}
}

// Fixed constraints, Idempotency: PayPal deletes but the answer is lost; BarPay sends the same
// PayPal-Request-Id again, and the confirmation and its notification happen once. The same holds
// when every answer of the request is lost and Run sends it.
func TestLostDeleteAnswerIsResent(t *testing.T) {
	t.Run("within the request", func(t *testing.T) {
		f := newFixture(t)
		f.linked(t, "m_1", "payer-1")
		f.pp.Next(paypaltest.DeletePaymentToken, paypaltest.LostAnswer)
		if w := f.deleteSaved("m_1"); w.Code != http.StatusNoContent {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		calls := f.pp.Calls(paypaltest.DeletePaymentToken)
		if len(calls) != 2 || calls[0].RequestID != calls[1].RequestID {
			t.Errorf("calls %+v; want one resend with the same PayPal-Request-Id", calls)
		}
		if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 1 {
			t.Errorf("%d member.saved_paypal_deleted events, want 1", n)
		}
		if n := f.count(t, "SELECT count(*) FROM paypal_deletions WHERE confirmed_at IS NOT NULL"); n != 1 {
			t.Errorf("%d deletions confirmed, want 1", n)
		}
	})
	t.Run("by Run", func(t *testing.T) {
		f := newFixture(t)
		f.linked(t, "m_1", "payer-1")
		f.pp.Next(paypaltest.DeletePaymentToken, paypaltest.LostAnswer, paypaltest.LostAnswer, paypaltest.LostAnswer)
		if w := f.deleteSaved("m_1"); w.Code != http.StatusAccepted {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		f.clk.Advance(deleteRetryAfter)
		if n := f.deleteDue(t); n != 1 {
			t.Fatalf("%d confirmed, want 1", n)
		}
		f.clk.Advance(deleteRetryAfter)
		f.deleteDue(t)
		calls := f.pp.Calls(paypaltest.DeletePaymentToken)
		for _, c := range calls {
			if c.RequestID != calls[0].RequestID {
				t.Errorf("calls %+v; want one PayPal-Request-Id", calls)
			}
		}
		if len(calls) != sendAttempts+1 || len(f.events(t, "m_1", "member.saved_paypal_deleted")) != 1 {
			t.Errorf("%d calls and %d events; want %d and 1", len(calls), len(f.events(t, "m_1", "member.saved_paypal_deleted")), sendAttempts+1)
		}
	})
}
