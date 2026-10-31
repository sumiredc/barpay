package paypal

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sumiredc/barpay/internal/money"
)

// decimals is how many decimal places PayPal writes an amount of each currency with.
var decimals = map[money.Currency]int{money.JPY: 0, money.USD: 2}

// value writes a in PayPal's form: a decimal string in the currency's unit, such as "12.34".
func value(a money.Amount) string {
	d := decimals[a.Currency()]
	v := strconv.FormatInt(a.Value(), 10)
	if d == 0 {
		return v
	}
	v = strings.Repeat("0", max(0, d+1-len(v))) + v
	return v[:len(v)-d] + "." + v[len(v)-d:]
}

type amountJSON struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

type purchaseUnit struct {
	InvoiceID string     `json:"invoice_id"`
	Amount    amountJSON `json:"amount"`
}

type orderRequest struct {
	Intent        string         `json:"intent"`
	PurchaseUnits []purchaseUnit `json:"purchase_units"`
	PaymentSource struct {
		PayPal struct {
			VaultID string `json:"vault_id"`
		} `json:"paypal"`
	} `json:"payment_source"`
}

type orderResponse struct {
	Status        string `json:"status"`
	PurchaseUnits []struct {
		Payments struct {
			Captures []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"captures"`
		} `json:"payments"`
	} `json:"purchase_units"`
}

// Charge is POST /v2/checkout/orders with intent CAPTURE, the saved PayPal's vault_id and the
// invoice_id (the PRD's System overview). Only an order COMPLETED with a COMPLETED capture is
// Succeeded, and a DECLINED or FAILED capture is Refused. Any other refusal is a 4xx other than
// 408 and 429, told apart by its issue. Anything else, including a 5xx and a capture in another
// state such as PENDING, does not say whether the total was charged, and is Unknown.
func (c *HTTP) Charge(ctx context.Context, ch Charge) Charged {
	in := orderRequest{Intent: "CAPTURE", PurchaseUnits: []purchaseUnit{{
		InvoiceID: ch.InvoiceID,
		Amount:    amountJSON{CurrencyCode: string(ch.Amount.Currency()), Value: value(ch.Amount)},
	}}}
	in.PaymentSource.PayPal.VaultID = ch.VaultID
	a := c.call(ctx, http.MethodPost, "/v2/checkout/orders", ch.RequestID, in)
	var out orderResponse
	var captureID, captureStatus string
	if !a.noAnswer() && json.Unmarshal(a.body, &out) == nil && len(out.PurchaseUnits) == 1 &&
		len(out.PurchaseUnits[0].Payments.Captures) == 1 {
		cp := out.PurchaseUnits[0].Payments.Captures[0]
		captureID, captureStatus = cp.ID, cp.Status
	}
	answered2xx := !a.noAnswer() && a.status >= 200 && a.status < 300
	switch {
	case answered2xx && out.Status == "COMPLETED" && captureStatus == "COMPLETED" && captureID != "":
		return Charged{Outcome: Succeeded, CaptureID: captureID}
	case answered2xx && (captureStatus == "DECLINED" || captureStatus == "FAILED"):
		c.opts.Log.Warn("PayPal refused", "call", "charge", "status", a.status, "capture_status", captureStatus)
		return Charged{Outcome: Failed, Refusal: Refused}
	}
	if c.outcome("charge", a, false) != Failed {
		return Charged{Outcome: Unknown}
	}
	is := issues(a.body)
	switch {
	case a.status == http.StatusForbidden && slices.Contains(is, "PERMISSION_DENIED"):
		return Charged{Outcome: Failed, Refusal: NotSaved}
	case a.status == http.StatusUnprocessableEntity && slices.Contains(is, "DUPLICATE_INVOICE_ID"):
		return Charged{Outcome: Failed, Refusal: DuplicateInvoice}
	}
	return Charged{Outcome: Failed, Refusal: Refused}
}

// issues are the error's name and every issue in its details, as PayPal names them.
func issues(body []byte) []string {
	var e struct {
		Name    string `json:"name"`
		Details []struct {
			Issue string `json:"issue"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil {
		return nil
	}
	out := []string{e.Name}
	for _, d := range e.Details {
		out = append(out, d.Issue)
	}
	return out
}
