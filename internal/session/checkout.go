package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/store"
)

// SavedPaypals is the members' saved PayPals as checkout needs them (package member).
type SavedPaypals interface {
	// VaultOf returns the vault ID of memberID's saved PayPal, reading through q, and false when
	// the member has none.
	VaultOf(ctx context.Context, q store.Querier, memberID string) (vaultID string, ok bool, err error)
	// Gone removes memberID's saved PayPal vaultID inside tx after PayPal answered a charge that
	// it no longer has it, and stores member.saved_paypal_deleted (the PRD's UC5 exception).
	Gone(ctx context.Context, tx store.Querier, memberID, vaultID string, now time.Time) error
}

// stepCharge is the step of the checkout charge, in its PayPal-Request-Id.
const stepCharge = "charge"

// chargeRequestID is the PRD's Fixed constraints, Idempotency: the PayPal-Request-Id of the
// session's charge attempt. A resend of the attempt reuses it; a staff retry is the next attempt.
func chargeRequestID(sessionID string, attempt int) string {
	return fmt.Sprintf("barpay-%s-%s-%d", sessionID, stepCharge, attempt)
}

// invoiceID is the session's invoice_id, the same for every attempt (the PRD's Fixed
// constraints, Idempotency). Session IDs are random, so it is unique across the store's PayPal
// account.
func invoiceID(sessionID string) string { return "barpay-" + sessionID }

// resendAfter is the PRD's UC5 exception: a charge whose outcome is unknown is sent again this
// long after the previous send ended.
const resendAfter = time.Minute

// resendWindow is the PRD's Fixed constraints, Idempotency: how long after its attempt was first
// sent a charge may be sent again.
const resendWindow = 30 * time.Minute

// mayResend tells whether, at t, a charge first sent at first may be sent again: until just
// before resendWindow has passed; from exactly then on, it is not.
func mayResend(first, t time.Time) bool { return t.Before(first.Add(resendWindow)) }

// nextSend is when a charge first sent at first, whose last send ended at ended with no
// readable answer, is sent again: resendAfter later, or at the end of its window, when it is
// given up instead, whichever comes first.
func nextSend(first, ended time.Time) time.Time {
	next := ended.Add(resendAfter)
	if last := first.Add(resendWindow); next.After(last) {
		next = last
	}
	if next.Before(ended) {
		next = ended
	}
	return next
}

// Reasons of the spec's SessionPaymentFailedEvent.
const (
	reasonRefused     = "refused"
	reasonUnconfirmed = "unconfirmed"
)

// paymentFailedData is the data of the spec's SessionPaymentFailedEvent.
type paymentFailedData struct {
	Session View   `json:"session"`
	Reason  string `json:"reason"`
}

// charge is an attempt to charge a session's total, as stored before it is sent.
type charge struct {
	sessionID, memberID, requestID, invoiceID, vaultID string
	attempt                                            int
	amount                                             money.Amount
	firstSentAt                                        time.Time
}

func (c charge) paypal() paypal.Charge {
	return paypal.Charge{RequestID: c.requestID, InvoiceID: c.invoiceID, VaultID: c.vaultID, Amount: c.amount}
}

// errNoSavedPaypal is returned by begin when the member no longer has a saved PayPal; nothing
// has changed then.
var errNoSavedPaypal = errors.New("session: the member has no saved PayPal")

// begin moves s along t (checkout from active, or a staff retry) inside tx and stores the attempt
// to charge its total with the member's saved PayPal, before PayPal is called (the PRD's
// Guarantees, Single payment). A session leaving active has its total fixed at fixAt, which is
// now except at the maximum stay; now is when the attempt is first sent. It returns the session
// as moved and the attempt to send. A total of 0 charges nothing: the session is closed at once
// with its session.closed, and there is no attempt to send. When the member has no saved PayPal
// to charge, it returns errNoSavedPaypal before changing anything.
func (a API) begin(ctx context.Context, tx store.Querier, s Session, t trigger, fixAt, now time.Time) (View, *charge, error) {
	// Refused moves are refused as such, before the saved PayPal is looked for.
	if err := judge(s, t, fixAt); err != nil {
		return View{}, nil, err
	}
	// The total as the move will fix it, to find whether there is anything to charge first.
	fixed := s
	if fixed.TotalFixedAt.IsZero() {
		fixed.TotalFixedAt = fixAt
	}
	v, err := compute(ctx, tx, fixed, fixAt)
	if err != nil {
		return View{}, nil, err
	}
	var vault string
	if v.Total.Value > 0 {
		var ok bool
		if vault, ok, err = a.Saved.VaultOf(ctx, tx, s.MemberID); err != nil {
			return View{}, nil, err
		} else if !ok {
			return View{}, nil, errNoSavedPaypal
		}
	}
	moved, err := move(ctx, tx, s, t, fixAt)
	if err != nil {
		return View{}, nil, err
	}
	if v, err = compute(ctx, tx, moved, now); err != nil {
		return View{}, nil, err
	}
	if v.Total.Value == 0 {
		if v, err = end(ctx, tx, moved, charged, now, settledPaypal, proceeds{}); err != nil {
			return View{}, nil, err
		}
		return v, nil, enqueueClosed(ctx, tx, now, v)
	}
	var last int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(attempt), 0) FROM charges WHERE session_id = ?`, s.ID).Scan(&last); err != nil {
		return View{}, nil, fmt.Errorf("session: last attempt: %w", err)
	}
	c := charge{
		sessionID: s.ID, memberID: s.MemberID, attempt: last + 1, invoiceID: invoiceID(s.ID), vaultID: vault,
		amount: money.New(v.Total.Value, s.Currency), firstSentAt: now,
	}
	c.requestID = chargeRequestID(s.ID, c.attempt)
	// next_at is the resend should this send never store its outcome, such as after a restart.
	_, err = tx.ExecContext(ctx, `INSERT INTO charges (session_id, attempt, request_id, invoice_id, vault_id, amount, first_sent_at, next_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.sessionID, c.attempt, c.requestID, c.invoiceID, c.vaultID, c.amount.Value(),
		c.firstSentAt.UnixNano(), nextSend(c.firstSentAt, now).UnixNano())
	if err != nil {
		return View{}, nil, fmt.Errorf("session: store charge: %w", err)
	}
	return v, &c, nil
}

// afterCharge runs in the transaction that stores what came of a send, with the session as it
// is then, so what the caller keeps (an Idempotency-Key response, an unlock) is stored with it.
type afterCharge func(tx store.Querier, now time.Time, v View) error

// send sends c to PayPal and stores what came of it, and returns the session as it is then. A
// send cut off by the caller still ends: its outcome is stored, not dropped.
func (a API) send(ctx context.Context, c charge, after afterCharge) (View, error) {
	ctx = context.WithoutCancel(ctx)
	return a.settle(ctx, c, a.PayPal.Charge(ctx, c.paypal()), after)
}

// settle stores what came of a send of c and what follows from it, in one transaction:
//   - charged: the session is closed with its receipt and capture ID, and session.closed;
//   - refused: PaymentFailed with its alert and session.payment_failed (refused); when PayPal no
//     longer has the saved PayPal, BarPay deletes it too (UC5 exception);
//   - unknown: sent again later, while the resend window lasts;
//   - unknown past the window, or DUPLICATE_INVOICE_ID with no capture recorded: PaymentFailed
//     with a charge_unconfirmed alert and session.payment_failed (unconfirmed). A capture would
//     have closed the session, so a session with this attempt still unknown has none.
//
// If another send of c stored its outcome first, nothing changes and the session is returned as
// it is.
func (a API) settle(ctx context.Context, c charge, got paypal.Charged, after afterCharge) (View, error) {
	var v View
	notified := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, c.sessionID)
		if err != nil {
			return err
		}
		var unknown bool
		err = tx.QueryRowContext(ctx, `SELECT outcome IS NULL FROM charges WHERE session_id = ? AND attempt = ?`,
			c.sessionID, c.attempt).Scan(&unknown)
		if err != nil {
			return fmt.Errorf("session: read charge: %w", err)
		}
		switch {
		case !unknown:
			v, err = view(ctx, tx, s, now)
		case got.Outcome == paypal.Succeeded:
			if err = storeOutcome(ctx, tx, c, "succeeded", got.CaptureID, now); err != nil {
				return err
			}
			if v, err = end(ctx, tx, s, charged, now, settledPaypal, proceeds{captureID: got.CaptureID}); err != nil {
				return err
			}
			notified, err = true, enqueueClosed(ctx, tx, now, v)
		case got.Outcome == paypal.Failed && got.Refusal == paypal.NotSaved:
			if err = storeOutcome(ctx, tx, c, "not_saved", "", now); err != nil {
				return err
			}
			if err = a.Saved.Gone(ctx, tx, c.memberID, c.vaultID, now); err != nil {
				return err
			}
			notified = true
			v, err = fail(ctx, tx, s, now, reasonRefused, alert.PaymentFailed,
				"PayPal no longer has the member's saved PayPal, so the charge failed and the gate stayed locked. "+
					"BarPay deleted the saved PayPal; settle the session outside BarPay.")
		case got.Outcome == paypal.Failed && got.Refusal != paypal.DuplicateInvoice:
			if err = storeOutcome(ctx, tx, c, "refused", "", now); err != nil {
				return err
			}
			notified = true
			v, err = fail(ctx, tx, s, now, reasonRefused, alert.PaymentFailed,
				"PayPal refused the charge, and the gate stayed locked. Retry the charge, or settle the session outside BarPay.")
		case got.Outcome == paypal.Unknown && mayResend(c.firstSentAt, now):
			_, err = tx.ExecContext(ctx, `UPDATE charges SET next_at = ? WHERE session_id = ? AND attempt = ?`,
				nextSend(c.firstSentAt, now).UnixNano(), c.sessionID, c.attempt)
			if err != nil {
				return fmt.Errorf("session: schedule resend: %w", err)
			}
			v, err = view(ctx, tx, s, now)
		default:
			if err = storeOutcome(ctx, tx, c, "unconfirmed", "", now); err != nil {
				return err
			}
			notified = true
			v, err = fail(ctx, tx, s, now, reasonUnconfirmed, alert.ChargeUnconfirmed,
				"BarPay could not confirm whether PayPal charged this session (invoice ID "+c.invoiceID+"). "+
					"Check the PayPal dashboard: if it was charged, settle outside BarPay with the amount charged; if not, retry the charge.")
		}
		if err != nil || after == nil {
			return err
		}
		return after(tx, now, v)
	})
	if err == nil && notified {
		a.Notify.Wake()
	}
	return v, err
}

// storeOutcome stores what c came to at now; it is sent no more.
func storeOutcome(ctx context.Context, tx store.Querier, c charge, outcome, captureID string, now time.Time) error {
	capture := sql.NullString{String: captureID, Valid: captureID != ""}
	_, err := tx.ExecContext(ctx, `UPDATE charges SET outcome = ?, capture_id = ?, next_at = NULL, ended_at = ?
		WHERE session_id = ? AND attempt = ?`, outcome, capture, now.UnixNano(), c.sessionID, c.attempt)
	if err != nil {
		return fmt.Errorf("session: store charge outcome: %w", err)
	}
	return nil
}

// fail moves s to PaymentFailed at now with the alert of kind and session.payment_failed for reason.
func fail(ctx context.Context, tx store.Querier, s Session, now time.Time, reason string, kind alert.Kind, message string) (View, error) {
	failed, err := move(ctx, tx, s, chargeFailed, now)
	if err != nil {
		return View{}, err
	}
	if _, err := alert.Raise(ctx, tx, alert.Alert{Kind: kind, RaisedAt: now, SessionID: s.ID, Message: message}); err != nil {
		return View{}, err
	}
	v, err := view(ctx, tx, failed, now)
	if err != nil {
		return View{}, err
	}
	_, err = notify.Enqueue(ctx, tx, now, notify.Event{
		Type: notify.SessionPaymentFailed, SubjectID: s.ID, Data: paymentFailedData{Session: v, Reason: reason},
	})
	return v, err
}

// enqueueClosed stores session.closed for v, the session as checked out through BarPay at now.
func enqueueClosed(ctx context.Context, tx store.Querier, now time.Time, v View) error {
	_, err := notify.Enqueue(ctx, tx, now, notify.Event{Type: notify.SessionClosed, SubjectID: v.SessionID, Data: sessionData{Session: v}})
	return err
}

// ChargeDue sends again every charge whose outcome is unknown and whose resend is due at the
// clock's time, and gives up those past their window, and returns how many it sent or gave up.
// It decides from the stored times alone, so after a restart it sends again those whose answer
// was lost when BarPay stopped (the PRD's UC5 exception).
func (a API) ChargeDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	rows, err := a.Store.Read().QueryContext(ctx, `SELECT c.session_id, s.member_id, s.currency, c.attempt, c.request_id,
		c.invoice_id, c.vault_id, c.amount, c.first_sent_at, c.next_at
		FROM charges c JOIN sessions s ON s.id = c.session_id WHERE c.outcome IS NULL AND c.next_at <= ? ORDER BY c.next_at`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("session: due charges: %w", err)
	}
	type due struct {
		charge
		nextAt int64
	}
	var all []due
	for rows.Next() {
		var d due
		var cur string
		var amount, first int64
		if err := rows.Scan(&d.sessionID, &d.memberID, &cur, &d.attempt, &d.requestID, &d.invoiceID, &d.vaultID,
			&amount, &first, &d.nextAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("session: due charges: %w", err)
		}
		d.amount, d.firstSentAt = money.New(amount, money.Currency(cur)), time.Unix(0, first).UTC()
		all = append(all, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("session: due charges: %w", err)
	}
	n := 0
	for _, d := range all {
		if !mayResend(d.firstSentAt, now) {
			if _, err := a.settle(ctx, d.charge, paypal.Charged{Outcome: paypal.Unknown}, nil); err != nil {
				return n, err
			}
			n++
			continue
		}
		// Claim the send by moving its next time, so a send already running elsewhere is not doubled.
		var claimed bool
		err := a.Store.Tx(ctx, func(tx store.Querier) error {
			res, err := tx.ExecContext(ctx, `UPDATE charges SET next_at = ? WHERE session_id = ? AND attempt = ? AND outcome IS NULL AND next_at = ?`,
				nextSend(d.firstSentAt, now).UnixNano(), d.sessionID, d.attempt, d.nextAt)
			if err != nil {
				return err
			}
			c, err := res.RowsAffected()
			claimed = c == 1
			return err
		})
		if err != nil {
			return n, fmt.Errorf("session: claim resend: %w", err)
		}
		if !claimed {
			continue
		}
		if _, err := a.send(ctx, d.charge, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
