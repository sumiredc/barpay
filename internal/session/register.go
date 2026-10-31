package session

import (
	"errors"
	"net/http"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/store"
)

// sessionData is the data of the session.* events that carry the session as of the event.
type sessionData struct {
	Session View `json:"session"`
}

// requestRegister is the customer choosing to pay at the register (the PRD's UC6 main flow). The
// move to at_register, its alert and its session.register_requested notification are stored in
// one transaction. A repeat for a session already at the register changes nothing and returns
// it as it is: the request cannot be withdrawn, and the store app is notified once.
func (a API) requestRegister(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
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
		if errors.Is(err, errRefused) && s.State == AtRegister {
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

// writeNotActive refuses an operation on a session that is checking out, payment failed or ended.
func writeNotActive(w http.ResponseWriter, consequence string) {
	httpapi.WriteError(w, http.StatusConflict, codeSessionNotActive,
		"The session is checking out, its payment failed or it has ended, "+consequence+".", "")
}
