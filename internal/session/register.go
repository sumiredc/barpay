package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// sessionData is the data of the session.* events that carry the session as of the event.
type sessionData struct {
	Session View `json:"session"`
}

// requestRegister is the customer choosing to pay at the register (the PRD's UC6 main flow). The
// move to at_register, its alert and its session.register_requested notification are stored in
// one transaction. A repeat for a session already at the register changes nothing and returns
// it as it is: the request cannot be withdrawn, and the store app is notified once. One that
// reached its 3-day limit has ended, and is refused.
func (a API) requestRegister(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if err := a.closeIfDue(ctx, req.PathValue("session_id")); err != nil {
		httpapi.WriteInternal(w)
		return
	}
	var v View
	moved := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		// Read inside the transaction, so no order stored before the total is fixed has a later time.
		now := a.Clock.Now()
		s, err := get(ctx, tx, req.PathValue("session_id"))
		if err != nil {
			return err
		}
		s, err = move(ctx, tx, s, registerRequested, now)
		if errors.Is(err, errRefused) && s.State == AtRegister && !pastLimit(s, now) {
			v, err = view(ctx, tx, s, now)
			return err
		}
		if err != nil {
			return err
		}
		moved = true
		if _, err := alert.Raise(ctx, tx, alert.Alert{
			Kind: alert.RegisterRequested, RaisedAt: now, SessionID: s.ID,
			Message: "The customer chose to pay at the register, and the total is fixed. Record the payment " +
				"taken, or mark the session unpaid if the customer leaves without paying.",
		}); err != nil {
			return err
		}
		if v, err = view(ctx, tx, s, now); err != nil {
			return err
		}
		_, err = notify.Enqueue(ctx, tx, now, notify.Event{
			Type: notify.SessionRegisterRequested, SubjectID: s.ID, Data: sessionData{Session: v},
		})
		return err
	})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errRefused):
		writeNotActive(w, "so the register cannot be requested")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		if moved {
			a.Notify.Wake()
		}
		httpapi.WriteJSON(w, http.StatusOK, v)
	}
}

// recordRegisterPayment is staff recording what they took at the register (the PRD's UC6 main
// and alternative flows, UC7). It closes a session waiting at the register, or an active one
// whose customer came straight to the register, fixing its total at that instant. The close, its
// receipt, its session.settled_outside notification and the staff member are stored in one
// transaction. Nothing is charged, whatever the amount.
func (a API) recordRegisterPayment(w http.ResponseWriter, req *http.Request) {
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
	err = staff.TxNow(ctx, a.Store, a.Clock, staff.Change{Staff: httpapi.StaffID(req), Operation: "recordRegisterPayment", Subject: id},
		func(tx store.Querier, now time.Time) error {
			s, err := get(ctx, tx, id)
			if err != nil {
				return err
			}
			if cur = s.Currency; amount.Currency() != cur {
				return errCurrencyMismatch
			}
			if v, err = end(ctx, tx, s, registerPaymentRecorded, now, settledRegister, proceeds{recorded: &amount}); err != nil {
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
		writeNotActive(w, "so no payment was recorded")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		a.Notify.Wake()
		httpapi.WriteJSON(w, http.StatusOK, v)
	}
}

// markUnpaid is staff closing a session waiting at the register whose customer left without
// paying (the PRD's UC6 exception, UC7). The close, its receipt with the unpaid total, its
// session.closed_unpaid notification and the staff member are stored in one transaction.
// Nothing is charged.
func (a API) markUnpaid(w http.ResponseWriter, req *http.Request) {
	ctx, id := req.Context(), req.PathValue("session_id")
	if err := a.closeIfDue(ctx, id); err != nil {
		httpapi.WriteInternal(w)
		return
	}
	var v View
	err := staff.TxNow(ctx, a.Store, a.Clock, staff.Change{Staff: httpapi.StaffID(req), Operation: "markUnpaid", Subject: id},
		func(tx store.Querier, now time.Time) error {
			s, err := get(ctx, tx, id)
			if err != nil {
				return err
			}
			if v, err = end(ctx, tx, s, markedUnpaid, now, settledUnpaid, proceeds{}); err != nil {
				return err
			}
			return enqueueUnpaid(ctx, tx, now, v)
		})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errRefused):
		httpapi.WriteError(w, http.StatusConflict, codeSessionNotAtRegister,
			"The session is not waiting at the register, so it was not marked unpaid.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		a.Notify.Wake()
		httpapi.WriteJSON(w, http.StatusOK, v)
	}
}

// enqueueUnpaid stores session.closed_unpaid for v, the session as closed unpaid at now.
func enqueueUnpaid(ctx context.Context, tx store.Querier, now time.Time, v View) error {
	_, err := notify.Enqueue(ctx, tx, now, notify.Event{
		Type: notify.SessionClosedUnpaid, SubjectID: v.SessionID, Data: sessionData{Session: v},
	})
	return err
}

var amountFields = httpapi.Fields{"amount_recorded": httpapi.MoneyFields()}

// decodeAmount reads the spec's AmountRecordedRequest. On a mismatch it returns the field at
// fault, if there is one.
func decodeAmount(body io.Reader) (amount money.Amount, field string, err error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return money.Amount{}, "", errors.New("the body cannot be read or is too large")
	}
	if field, err := httpapi.CheckKeys(data, amountFields); err != nil {
		return money.Amount{}, field, err
	}
	fail := func(f, format string, args ...any) (money.Amount, string, error) {
		return money.Amount{}, f, fmt.Errorf("%s %s", f, fmt.Sprintf(format, args...))
	}
	top, err := object(data)
	if err != nil {
		return money.Amount{}, "", errors.New("the body is not a JSON AmountRecordedRequest object")
	}
	raw, ok := top["amount_recorded"]
	if !ok {
		return fail("amount_recorded", "is required")
	}
	m, err := object(raw)
	if err != nil {
		return fail("amount_recorded", "must be a Total object")
	}
	raw, ok = m["value"]
	if !ok {
		return fail("amount_recorded.value", "is required")
	}
	value, ok := integer(raw)
	if !ok || value < 0 || value > money.MaxTotal {
		return fail("amount_recorded.value", "must be an integer from 0 to %d", int64(money.MaxTotal))
	}
	raw, ok = m["currency"]
	if !ok {
		return fail("amount_recorded.currency", "is required")
	}
	c, ok := str(raw)
	if !ok || !slices.Contains(money.Known, money.Currency(c)) {
		return fail("amount_recorded.currency", "must be one of JPY, USD")
	}
	return money.New(value, money.Currency(c)), "", nil
}

// writeNotActive refuses an operation on a session that is checking out, payment failed or ended.
func writeNotActive(w http.ResponseWriter, consequence string) {
	httpapi.WriteError(w, http.StatusConflict, codeSessionNotActive,
		"The session is checking out, its payment failed or it has ended, "+consequence+".", "")
}
