package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/store"
)

// settlement is the spec's ReceiptBase.settlement.
type settlement string

const (
	settledRegister settlement = "register"
	settledUnpaid   settlement = "unpaid"
)

// receipt is how a session ended, as stored, with the values the session showed then.
type receipt struct {
	settlement     settlement
	paymentMethod  string
	ratePerHour    int64
	orderCount     int
	ordersTotal    int64
	coverCharge    int64
	total          int64
	amountRecorded sql.NullInt64 // register and outside only
	unpaidAmount   sql.NullInt64 // unpaid only
	endedAt        time.Time
}

// receiptJSON is the spec's Receipt.
type receiptJSON struct {
	Settlement     settlement     `json:"settlement"`
	OrdersTotal    httpapi.Money  `json:"orders_total"`
	CoverCharge    httpapi.Money  `json:"cover_charge"`
	Total          httpapi.Money  `json:"total"`
	EndedAt        string         `json:"ended_at"`
	AmountRecorded *httpapi.Money `json:"amount_recorded,omitempty"`
	UnpaidAmount   *httpapi.Money `json:"unpaid_amount,omitempty"`
}

// ended tells whether a session in state s has ended.
func (s State) ended() bool { return s == Closed || s == ClosedUnpaid }

// end moves s along t at now to an ended state and stores its receipt, settled as set, from what
// the session shows at now with its total fixed. recorded is the amount staff recorded, for a
// settlement that has one. It returns the session as ended.
func end(ctx context.Context, tx store.Querier, s Session, t trigger, now time.Time, set settlement, recorded *money.Amount) (View, error) {
	ended, err := move(ctx, tx, s, t, now)
	if err != nil {
		return View{}, err
	}
	if !ended.State.ended() {
		return View{}, fmt.Errorf("session: %s does not end a session", t)
	}
	before := ended
	before.State = s.State
	shown, err := compute(ctx, tx, before, now)
	if err != nil {
		return View{}, err
	}
	r := receipt{
		settlement: set, paymentMethod: shown.PaymentMethod, ratePerHour: shown.RatePerHour.Value,
		orderCount: shown.OrderCount, ordersTotal: shown.OrdersTotal.Value, coverCharge: shown.CoverCharge.Value,
		total: shown.Total.Value, endedAt: now,
	}
	if set == settledRegister {
		// Paid at the register, linked or not (the spec's PaymentMethod).
		r.paymentMethod = "register"
	}
	if recorded != nil {
		r.amountRecorded = sql.NullInt64{Int64: recorded.Value(), Valid: true}
	}
	if set == settledUnpaid {
		r.unpaidAmount = sql.NullInt64{Int64: r.total, Valid: true}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipts (session_id, settlement, payment_method, rate_per_hour,
		order_count, orders_total, cover_charge, total, amount_recorded, unpaid_amount, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, string(r.settlement), r.paymentMethod, r.ratePerHour, r.orderCount, r.ordersTotal, r.coverCharge,
		r.total, r.amountRecorded, r.unpaidAmount, r.endedAt.UnixNano())
	if err != nil {
		return View{}, fmt.Errorf("session: store receipt: %w", err)
	}
	return fromReceipt(ended, r), nil
}

func getReceipt(ctx context.Context, q store.Querier, id string) (receipt, error) {
	var r receipt
	var set string
	var at int64
	err := q.QueryRowContext(ctx, `SELECT settlement, payment_method, rate_per_hour, order_count, orders_total,
		cover_charge, total, amount_recorded, unpaid_amount, ended_at FROM receipts WHERE session_id = ?`, id).
		Scan(&set, &r.paymentMethod, &r.ratePerHour, &r.orderCount, &r.ordersTotal, &r.coverCharge, &r.total,
			&r.amountRecorded, &r.unpaidAmount, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt{}, fmt.Errorf("session: %s has ended without a receipt", id)
	}
	if err != nil {
		return receipt{}, fmt.Errorf("session: get receipt: %w", err)
	}
	r.settlement, r.endedAt = settlement(set), time.Unix(0, at).UTC()
	return r, nil
}

// fromReceipt shows the ended session s from its receipt r.
func fromReceipt(s Session, r receipt) View {
	m := func(v int64) httpapi.Money { return httpapi.Money{Value: v, Currency: s.Currency} }
	opt := func(v sql.NullInt64) *httpapi.Money {
		if !v.Valid {
			return nil
		}
		x := m(v.Int64)
		return &x
	}
	v := View{
		SessionID:         s.ID,
		State:             s.State,
		PaymentMethod:     r.paymentMethod,
		CheckedInAt:       httpapi.FormatTime(s.CheckedInAt),
		TotalFixedAt:      httpapi.FormatTime(s.TotalFixedAt),
		RatePerHour:       m(r.ratePerHour),
		CoverChargeStatus: pricing.Stopped,
		OrdersTotal:       m(r.ordersTotal),
		CoverCharge:       m(r.coverCharge),
		Total:             m(r.total),
		OrderCount:        r.orderCount,
		UnpaidAmount:      opt(r.unpaidAmount),
		Receipt: &receiptJSON{
			Settlement: r.settlement, OrdersTotal: m(r.ordersTotal), CoverCharge: m(r.coverCharge), Total: m(r.total),
			EndedAt: httpapi.FormatTime(r.endedAt), AmountRecorded: opt(r.amountRecorded), UnpaidAmount: opt(r.unpaidAmount),
		},
	}
	if s.MemberID != "" {
		v.MemberID = &s.MemberID
	}
	return v
}
