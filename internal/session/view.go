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
// charges or staff collect is the amount the panel showed. An ended session is shown from its
// receipt, which holds what it showed when it ended.
func view(ctx context.Context, q store.Querier, s Session, now time.Time) (View, error) {
	if s.State.ended() {
		r, err := getReceipt(ctx, q, s.ID)
		if err != nil {
			return View{}, err
		}
		return fromReceipt(s, r), nil
	}
	return compute(ctx, q, s, now)
}

// compute is view of a session that has not ended, from its orders, the rates and the settings.
// A session that has not ended never spans a currency change, which waits until none is open.
func compute(ctx context.Context, q store.Querier, s Session, now time.Time) (View, error) {
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
		SessionID:         s.ID,
		State:             s.State,
		PaymentMethod:     paymentMethod(!s.LinkedAt.IsZero(), s.State),
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
	if s.State == PaymentFailed {
		// What BarPay could not confirm as paid: the total it tried to charge.
		unpaid := v.Total
		v.UnpaidAmount = &unpaid
	}
	return v, nil
}

// paymentMethod is the spec's PaymentMethod of a session in state that is linked or not: once the
// register is requested it is the register, linked or not (UC6 precondition and exception).
func paymentMethod(linked bool, state State) string {
	if linked && state != AtRegister {
		return "paypal"
	}
	return "register"
}

// lookup shows the session with id as at the clock's time, from one state of the store.
// That state may predate a rate or an order committed at the same moment near a block end;
// it decides only what the panel shows, never an amount that is charged. A session that reached
// its 3-day limit is closed first, so it is shown as ended from that instant.
func (a API) lookup(ctx context.Context, id string) (View, error) {
	v, due, err := a.read(ctx, id)
	if err == nil && due {
		if err = a.closeIfDue(ctx, id); err == nil {
			v, _, err = a.read(ctx, id)
		}
	}
	return v, err
}

// read is lookup without closing: it also tells whether the session reached its 3-day limit.
func (a API) read(ctx context.Context, id string) (v View, due bool, err error) {
	err = a.Store.View(ctx, func(q store.Querier) error {
		s, err := get(ctx, q, id)
		if err != nil {
			return err
		}
		now := a.Clock.Now()
		due = pastLimit(s, now)
		v, err = view(ctx, q, s, now)
		return err
	})
	return v, due, err
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
