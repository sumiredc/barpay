package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// closeSession is staff checking out a linked customer who left through another door (the PRD's
// UC7 alternative flow): checkout as in UC5, without the gate.
func (a API) closeSession(w http.ResponseWriter, req *http.Request) {
	a.charge(w, req, "closeSession", checkout)
}

// retryCharge is staff charging a PaymentFailed session again as a new attempt (the PRD's UC7
// exception). It keeps the session's invoice_id, so it cannot succeed if an earlier attempt did.
func (a API) retryCharge(w http.ResponseWriter, req *http.Request) {
	a.charge(w, req, "retryCharge", retried)
}

// charge runs closeSession or retryCharge. The move to checking out, the attempt with its
// PayPal-Request-Id, the staff member and the response for the Idempotency-Key are stored in one
// transaction before PayPal is called, so a concurrent request is refused and a repeat of this one
// never charges again. The first send is made within the request; the response is the session
// after it (closed or payment_failed), or in checking_out while its outcome is unknown, and the
// response kept for the key becomes the one returned. A request refused before anything changed
// keeps nothing.
func (a API) charge(w http.ResponseWriter, req *http.Request, operation string, t trigger) {
	claim := idempotency.FromRequest(req)
	if claim == nil {
		httpapi.WriteInternal(w)
		return
	}
	ctx, id := req.Context(), req.PathValue("session_id")
	if t == retried {
		if err := a.retryWindowOver(ctx, id); err != nil {
			a.writeChargeError(w, err, t)
			return
		}
	}
	var v View
	var c *charge
	err := staff.TxNow(ctx, a.Store, a.Clock, staff.Change{Staff: httpapi.StaffID(req), Operation: operation, Subject: id},
		func(tx store.Querier, now time.Time) error {
			s, err := get(ctx, tx, id)
			if err != nil {
				return err
			}
			if v, c, err = a.begin(ctx, tx, s, t, now); err != nil {
				return err
			}
			return keep(ctx, tx, claim, now, v)
		})
	if err != nil {
		a.writeChargeError(w, err, t)
		return
	}
	if c == nil {
		a.Notify.Wake()
		writeKept(w, v)
		return
	}
	v, err = a.send(ctx, *c, func(tx store.Querier, _ time.Time, v View) error {
		out, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return claim.Replace(ctx, tx, http.StatusOK, out)
	})
	if err != nil {
		a.Log.Error("charge outcome not stored", "operation", operation, "error", err)
		httpapi.WriteInternal(w)
		return
	}
	writeKept(w, v)
}

func keep(ctx context.Context, tx store.Querier, claim *idempotency.Claim, now time.Time, v View) error {
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return claim.Keep(ctx, tx, now, http.StatusOK, out)
}

// writeKept writes v as it was kept for the Idempotency-Key, so a repeat gets the same bytes.
func writeKept(w http.ResponseWriter, v View) {
	out, err := json.Marshal(v)
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	idempotency.Write(w, http.StatusOK, out)
}

// retryWindowOver closes the session with id as unpaid if it reached its 3-day limit, and
// returns errPastLimit for a session whose charge failed and that has reached it, closed by
// now or not (the PRD's UC7 exception), so a retry from that instant gets the same answer
// whether or not the limit's close ran first.
func (a API) retryWindowOver(ctx context.Context, id string) error {
	if err := a.closeIfDue(ctx, id); err != nil {
		return err
	}
	var over bool
	err := a.Store.Read().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions s JOIN charges c ON c.session_id = s.id
		WHERE s.id = ? AND s.state = 'closed_unpaid')`, id).Scan(&over)
	if err != nil {
		return err
	}
	if over {
		return errPastLimit
	}
	return nil
}

func (a API) writeChargeError(w http.ResponseWriter, err error, t trigger) {
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errNotLinked):
		httpapi.WriteError(w, http.StatusConflict, codeSessionNotLinked,
			"The session has no PayPal linked, so nothing was charged; record a register payment instead.", "")
	case errors.Is(err, errPastLimit):
		httpapi.WriteError(w, http.StatusConflict, codeRetryWindowOver,
			"It is 3 days or more since checkout began, so the charge was not retried.", "")
	case errors.Is(err, errNoSavedPaypal):
		httpapi.WriteError(w, http.StatusConflict, codeSavedPaypalNotFound,
			"The member no longer has a saved PayPal, so nothing was charged; settle outside BarPay instead.", "")
	case errors.Is(err, errRefused) && t == retried:
		httpapi.WriteError(w, http.StatusConflict, codeNotPaymentFailed,
			"The session is not PaymentFailed, so the charge was not retried.", "")
	case errors.Is(err, errRefused):
		writeNotActive(w, "so it was not closed")
	default:
		httpapi.WriteInternal(w)
	}
}

// settleOutside is staff closing a PaymentFailed session whose payment they took outside BarPay
// (the PRD's UC7 exception), with the amount settled. The close, its receipt, its
// session.settled_outside and the staff member are stored in one transaction. Nothing is charged.
func (a API) settleOutside(w http.ResponseWriter, req *http.Request) {
	amount, field, err := decodeAmount(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	ctx, id := req.Context(), req.PathValue("session_id")
	if err := a.closeIfDue(ctx, id); err != nil {
		httpapi.WriteInternal(w)
		return
	}
	var v View
	var cur money.Currency
	err = staff.TxNow(ctx, a.Store, a.Clock, staff.Change{Staff: httpapi.StaffID(req), Operation: "settleOutside", Subject: id},
		func(tx store.Querier, now time.Time) error {
			s, err := get(ctx, tx, id)
			if err != nil {
				return err
			}
			if cur = s.Currency; amount.Currency() != cur {
				return errCurrencyMismatch
			}
			if v, err = end(ctx, tx, s, outsideSettled, now, settledOutside, proceeds{recorded: &amount}); err != nil {
				return err
			}
			_, err = notify.Enqueue(ctx, tx, now, notify.Event{
				Type: notify.SessionSettledOutside, SubjectID: s.ID, Data: sessionData{Session: v},
			})
			return err
		})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errCurrencyMismatch):
		httpapi.WriteError(w, http.StatusUnprocessableEntity, codeCurrencyMismatch,
			"amount_recorded must be in the store's currency ("+string(cur)+").", "")
	case errors.Is(err, errRefused):
		httpapi.WriteError(w, http.StatusConflict, codeNotPaymentFailed,
			"The session is not PaymentFailed, so nothing was recorded.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		a.Notify.Wake()
		httpapi.WriteJSON(w, http.StatusOK, v)
	}
}
