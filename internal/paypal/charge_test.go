package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sumiredc/barpay/internal/money"
)

const completed = `{"id":"ORDER-1","status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP-1","status":"COMPLETED"}]}}]}`

// The charge is an order with intent CAPTURE paid by the saved PayPal, with the invoice_id and the
// stored PayPal-Request-Id, for the amount in the currency's unit.
func TestChargeRequest(t *testing.T) {
	s := newServer(t, reply(http.StatusCreated, completed))
	got := s.client.Charge(context.Background(), Charge{RequestID: "req-c", InvoiceID: "inv-1", VaultID: "VT-1", Amount: money.New(1205, money.USD)})
	if got != (Charged{Outcome: Succeeded, CaptureID: "CAP-1"}) {
		t.Fatalf("got %+v", got)
	}
	if s.lastReq.Method != http.MethodPost || s.lastReq.URL.Path != "/v2/checkout/orders" || s.lastReq.Header.Get("PayPal-Request-Id") != "req-c" {
		t.Errorf("request %s %s, PayPal-Request-Id %q", s.lastReq.Method, s.lastReq.URL.Path, s.lastReq.Header.Get("PayPal-Request-Id"))
	}
	var in orderRequest
	if err := json.Unmarshal(s.last, &in); err != nil {
		t.Fatal(err)
	}
	if in.Intent != "CAPTURE" || len(in.PurchaseUnits) != 1 || in.PaymentSource.PayPal.VaultID != "VT-1" ||
		in.PurchaseUnits[0] != (purchaseUnit{InvoiceID: "inv-1", Amount: amountJSON{CurrencyCode: "USD", Value: "12.05"}}) {
		t.Errorf("body %s", s.last)
	}
}

func TestValue(t *testing.T) {
	for a, want := range map[money.Amount]string{
		money.New(1205, money.USD): "12.05", money.New(5, money.USD): "0.05", money.New(100, money.USD): "1.00",
		money.New(1205, money.JPY): "1205",
	} {
		if got := value(a); got != want {
			t.Errorf("value(%v) = %q, want %q", a, got, want)
		}
	}
}

// What each answer to a charge comes to (the PRD's Fixed constraints, verified in the sandbox):
// only a completed capture is a success; a refusal tells whether PayPal no longer has the saved
// PayPal or the invoice_id was already charged; anything that does not say is unknown, as for
// the other calls.
func TestChargeOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   Charged
	}{
		{"completed", http.StatusCreated, completed, Charged{Outcome: Succeeded, CaptureID: "CAP-1"}},
		{"completed again", http.StatusOK, completed, Charged{Outcome: Succeeded, CaptureID: "CAP-1"}},
		{"capture pending", http.StatusCreated, `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP-1","status":"PENDING"}]}}]}`, Charged{Outcome: Unknown}},
		{"capture declined", http.StatusCreated, `{"status":"COMPLETED","purchase_units":[{"payments":{"captures":[{"id":"CAP-1","status":"DECLINED"}]}}]}`, Charged{Outcome: Failed, Refusal: Refused}},
		{"no capture", http.StatusCreated, `{"status":"COMPLETED","purchase_units":[{}]}`, Charged{Outcome: Unknown}},
		{"unreadable 2xx", http.StatusCreated, `<html>`, Charged{Outcome: Unknown}},
		{"refused", http.StatusUnprocessableEntity, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"TRANSACTION_REFUSED"}]}`, Charged{Outcome: Failed, Refusal: Refused}},
		{"declined", http.StatusUnprocessableEntity, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"INSTRUMENT_DECLINED"}]}`, Charged{Outcome: Failed, Refusal: Refused}},
		{"duplicate invoice", http.StatusUnprocessableEntity, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"DUPLICATE_INVOICE_ID"}]}`, Charged{Outcome: Failed, Refusal: DuplicateInvoice}},
		{"permission denied", http.StatusForbidden, `{"name":"PERMISSION_DENIED","details":[{"issue":"PERMISSION_DENIED"}]}`, Charged{Outcome: Failed, Refusal: NotSaved}},
		{"other 403", http.StatusForbidden, `{"name":"NOT_AUTHORIZED"}`, Charged{Outcome: Failed, Refusal: Refused}},
		{"server error", http.StatusInternalServerError, `{}`, Charged{Outcome: Unknown}},
		{"timeout status", http.StatusRequestTimeout, `{}`, Charged{Outcome: Unknown}},
		{"too many requests", http.StatusTooManyRequests, `{}`, Charged{Outcome: Unknown}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, reply(c.status, c.body))
			got := s.client.Charge(context.Background(), Charge{RequestID: "r", InvoiceID: "i", VaultID: "VT-1", Amount: money.New(100, money.USD)})
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
