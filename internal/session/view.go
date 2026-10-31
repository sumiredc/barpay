package session

import (
	"context"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/store"
)

// View is the spec's Session: a session as lookup shows it at one instant.
type View struct {
	SessionID         string         `json:"session_id"`
	State             State          `json:"state"`
	MemberID          *string        `json:"member_id"`
	PaymentMethod     string         `json:"payment_method"`
	CheckedInAt       string         `json:"checked_in_at"`
	TotalFixedAt      string         `json:"total_fixed_at,omitempty"`
	RatePerHour       httpapi.Money  `json:"rate_per_hour"`
	CoverChargeStatus pricing.Status `json:"cover_charge_status"`
	OrdersTotal       httpapi.Money  `json:"orders_total"`
	CoverCharge       httpapi.Money  `json:"cover_charge"`
	Total             httpapi.Money  `json:"total"`
	OrderCount        int            `json:"order_count"`
	UnpaidAmount      *httpapi.Money `json:"unpaid_amount"`
	Receipt           any            `json:"receipt"`
}

// view shows s at now, reading through q. q must give every read the same state (a transaction),
// so the rate, the cover charge and the total it shows agree.
func view(ctx context.Context, q store.Querier, s Session, now time.Time) (View, error) {
	cc, err := pricing.ChargeAt(ctx, q, s.Currency, pricing.Visit{CheckedInAt: s.CheckedInAt}, now)
	if err != nil {
		return View{}, err
	}
	rate, err := pricing.Current(ctx, q, s.Currency)
	if err != nil {
		return View{}, err
	}
	orders := money.New(0, s.Currency)
	total, err := orders.Add(cc.Amount)
	if err != nil {
		return View{}, err
	}
	v := View{
		SessionID: s.ID,
		State:     s.State,
		// TODO(#18): paypal once the session is linked to the member's saved PayPal.
		PaymentMethod:     "register",
		CheckedInAt:       httpapi.FormatTime(s.CheckedInAt),
		RatePerHour:       httpapi.ToMoney(rate.Rate),
		CoverChargeStatus: cc.Status,
		OrdersTotal:       httpapi.ToMoney(orders),
		CoverCharge:       httpapi.ToMoney(cc.Amount),
		Total:             httpapi.ToMoney(total),
	}
	if s.MemberID != "" {
		v.MemberID = &s.MemberID
	}
	return v, nil
}
