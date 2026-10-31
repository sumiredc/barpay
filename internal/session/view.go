package session

import (
	"context"
	"errors"
	"net/http"
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
// so the rate, the cover charge and the total it shows agree. A session whose total is fixed is
// shown as at that instant, so what it shows no longer changes with time: the amount checkout
// charges or staff collect is the amount the panel showed.
func view(ctx context.Context, q store.Querier, s Session, now time.Time) (View, error) {
	times, ordersTotal, err := orders(ctx, q, s.ID)
	if err != nil {
		return View{}, err
	}
	visit := pricing.Visit{CheckedInAt: s.CheckedInAt, Orders: times, TotalFixedAt: s.TotalFixedAt}
	var rate pricing.Point
	if s.TotalFixedAt.IsZero() {
		rate, err = pricing.Current(ctx, q, s.Currency)
	} else {
		now = s.TotalFixedAt
		// TODO(#19): an ended session from before a currency change finds no rate in its currency; show it from its stored receipt.
		rate, err = pricing.At(ctx, q, s.Currency, now)
	}
	if err != nil {
		return View{}, err
	}
	cc, err := pricing.ChargeAt(ctx, q, s.Currency, visit, now)
	if err != nil {
		return View{}, err
	}
	ot := money.New(ordersTotal, s.Currency)
	total, err := ot.Add(cc.Amount)
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
		OrdersTotal:       httpapi.ToMoney(ot),
		OrderCount:        len(times),
		CoverCharge:       httpapi.ToMoney(cc.Amount),
		Total:             httpapi.ToMoney(total),
	}
	if s.MemberID != "" {
		v.MemberID = &s.MemberID
	}
	if !s.TotalFixedAt.IsZero() {
		v.TotalFixedAt = httpapi.FormatTime(s.TotalFixedAt)
	}
	// TODO(#19): unpaid_amount and the receipt, once checkout and its results are stored.
	return v, nil
}

// lookup shows the session with id as at the clock's time, from one state of the store.
// That state may predate a rate or an order committed at the same moment near a block end;
// it decides only what the panel shows, never an amount that is charged.
func (a API) lookup(ctx context.Context, id string) (View, error) {
	var v View
	err := a.Store.View(ctx, func(q store.Querier) error {
		s, err := get(ctx, q, id)
		if err != nil {
			return err
		}
		v, err = view(ctx, q, s, a.Clock.Now())
		return err
	})
	return v, err
}

// getSession is the lookup of the PRD's Responsibilities, Lookup and Panel, and Guarantees,
// Transparency.
func (a API) getSession(w http.ResponseWriter, req *http.Request) {
	v, err := a.lookup(req.Context(), req.PathValue("session_id"))
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		httpapi.WriteJSON(w, http.StatusOK, v)
	}
}

func writeNotFound(w http.ResponseWriter) {
	httpapi.WriteError(w, http.StatusNotFound, codeSessionNotFound, "No session has this ID.", "")
}
