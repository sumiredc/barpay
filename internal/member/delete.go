package member

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/store"
)

// deleteRetryAfter is the PRD's UC9 alternative flow: a deletion PayPal has not confirmed is
// tried again this long after the previous try ended.
const deleteRetryAfter = time.Minute

// deleteFailingAfter is the PRD's UC9 alternative flow: a deletion PayPal has still not confirmed
// this long after the member asked is raised as an alert.
const deleteFailingAfter = time.Hour

// failing tells whether, at ended, a deletion asked for at requested has kept failing long
// enough to raise its alert: from exactly 1 hour on.
func failing(requested, ended time.Time) bool {
	return !ended.Before(requested.Add(deleteFailingAfter))
}

// stepDelete is the step of a deletion's PayPal call, in its PayPal-Request-Id.
const stepDelete = "delete"

// Reasons of the spec's MemberSavedPaypalDeletedEvent.
const (
	reasonMemberRequest = "member_request"
	reasonNoLongerValid = "no_longer_valid"
)

// deletedData is the data of the spec's MemberSavedPaypalDeletedEvent.
type deletedData struct {
	Member View   `json:"member"`
	Reason string `json:"reason"`
}

// deletion is a row of paypal_deletions that PayPal has not confirmed.
type deletion struct {
	id, memberID, vaultID, requestID string
	requestedAt                      time.Time
	attempt                          int
	alerted                          bool
}

var (
	errInUse = errors.New("member: a session uses the saved PayPal")
	// errPending is returned when the deletion was already asked for and is pending.
	errPending = errors.New("member: the deletion is pending")
)

// deleteSavedPaypal deletes a member's saved PayPal (the PRD's UC9). The saved PayPal is marked as
// being deleted and the deletion, with its PayPal-Request-Id, is stored before PayPal is called;
// from then on the member cannot link and a check-in starts unlinked. If PayPal confirms within
// the request the answer is 204; otherwise 202, and Run tries again until PayPal confirms.
func (a API) deleteSavedPaypal(w http.ResponseWriter, req *http.Request) {
	m, ok := memberOf(w, req)
	if !ok {
		return
	}
	ctx := req.Context()
	var d deletion
	var inUse string
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := getSaved(ctx, tx, m)
		if err != nil {
			return err
		}
		if s.status == statusDeletionPending {
			return errPending
		}
		if inUse, err = inUseBy(ctx, tx, m); err != nil {
			return err
		} else if inUse != "" {
			return errInUse
		}
		if _, err := tx.ExecContext(ctx, `UPDATE saved_paypals SET status = ? WHERE member_id = ?`, statusDeletionPending, m); err != nil {
			return fmt.Errorf("member: mark deletion: %w", err)
		}
		d = deletion{id: newID("pld_"), memberID: m, vaultID: s.vaultID, requestedAt: now, attempt: 1}
		d.requestID = requestID(d.id, stepDelete, d.attempt)
		// The request makes the first try; Run makes it only if this one did not end.
		_, err = tx.ExecContext(ctx, `INSERT INTO paypal_deletions (id, member_id, vault_id, requested_at, attempt, request_id, next_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, d.id, m, d.vaultID, now.UnixNano(), d.attempt, d.requestID, now.Add(deleteRetryAfter).UnixNano())
		if err != nil {
			return fmt.Errorf("member: store deletion: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoSaved):
		httpapi.WriteError(w, http.StatusNotFound, codeSavedPaypalNotFound, "The member has no saved PayPal.", "")
		return
	case errors.Is(err, errInUse):
		httpapi.WriteSessionError(w, http.StatusConflict, codeSavedPaypalInUse,
			"A session that uses the saved PayPal is not yet closed. Nothing was deleted; delete it after paying.", inUse)
		return
	case errors.Is(err, errPending):
	case err != nil:
		httpapi.WriteInternal(w)
		return
	default:
		if confirmed, err := a.try(ctx, d); err != nil {
			httpapi.WriteInternal(w)
			return
		} else if confirmed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	var v View
	err = a.Store.View(ctx, func(q store.Querier) (err error) {
		v, err = view(ctx, q, m)
		return err
	})
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	httpapi.WriteJSON(w, http.StatusAccepted, v)
}

// try sends d to PayPal and stores what came of it, telling whether PayPal confirmed it.
func (a API) try(ctx context.Context, d deletion) (bool, error) {
	o := send(func() paypal.Outcome { return a.PayPal.DeletePaymentToken(ctx, d.requestID, d.vaultID) })
	if o == paypal.Succeeded {
		return true, a.confirmed(ctx, d)
	}
	return false, a.retryLater(ctx, d, o)
}

// confirmed finishes d once PayPal confirmed it: the saved PayPal is removed and
// member.saved_paypal_deleted stored, once, even when two tries were confirmed.
func (a API) confirmed(ctx context.Context, d deletion) error {
	done := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		res, err := tx.ExecContext(ctx, `UPDATE paypal_deletions SET confirmed_at = ?, next_at = NULL
			WHERE id = ? AND confirmed_at IS NULL`, now.UnixNano(), d.id)
		if err != nil {
			return fmt.Errorf("member: confirm deletion: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		done = true
		return a.removeSaved(ctx, tx, d.memberID, d.vaultID, reasonMemberRequest, now)
	})
	if done {
		a.Notify.Wake()
	}
	return err
}

// removeSaved removes memberID's saved PayPal vaultID and stores member.saved_paypal_deleted
// for reason, inside tx.
func (a API) removeSaved(ctx context.Context, tx store.Querier, memberID, vaultID, reason string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM saved_paypals WHERE member_id = ? AND vault_id = ?`, memberID, vaultID); err != nil {
		return fmt.Errorf("member: remove saved PayPal: %w", err)
	}
	v, err := view(ctx, tx, memberID)
	if err != nil {
		return err
	}
	_, err = notify.Enqueue(ctx, tx, now, notify.Event{
		Type: notify.MemberSavedPaypalDeleted, SubjectID: memberID, Data: deletedData{Member: v, Reason: reason},
	})
	return err
}

// retryLater stores when d is tried again: deleteRetryAfter after this try ended. An unknown
// outcome is sent again with the same PayPal-Request-Id; after PayPal refused, the next try is a
// new attempt with a new one, stored here before it is sent. Once d has kept failing for
// deleteFailingAfter, deletion_failing is raised, once.
func (a API) retryLater(ctx context.Context, d deletion, o paypal.Outcome) error {
	return a.Store.Tx(ctx, func(tx store.Querier) error {
		ended := a.Clock.Now()
		if o == paypal.Failed {
			d.attempt++
			d.requestID = requestID(d.id, stepDelete, d.attempt)
		}
		res, err := tx.ExecContext(ctx, `UPDATE paypal_deletions SET next_at = ?, attempt = ?, request_id = ?
			WHERE id = ? AND confirmed_at IS NULL`, ended.Add(deleteRetryAfter).UnixNano(), d.attempt, d.requestID, d.id)
		if err != nil {
			return fmt.Errorf("member: retry deletion: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 || d.alerted || !failing(d.requestedAt, ended) {
			return err
		}
		if _, err := alert.Raise(ctx, tx, alert.Alert{
			Kind: alert.DeletionFailing, RaisedAt: ended, MemberID: d.memberID,
			Message: "PayPal has not confirmed deleting this member's saved PayPal for an hour. BarPay keeps trying; " +
				"the member cannot link PayPal again until it is confirmed.",
		}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE paypal_deletions SET alerted = 1 WHERE id = ?`, d.id)
		return err
	})
}

// DeleteDue tries every deletion due at the clock's time, and returns how many PayPal confirmed.
// It decides from the stored times alone, so after a restart it tries those that fell due while
// BarPay was stopped.
func (a API) DeleteDue(ctx context.Context) (int, error) {
	now := a.Clock.Now()
	rows, err := a.Store.Read().QueryContext(ctx, `SELECT id, member_id, vault_id, request_id, requested_at, attempt, alerted
		FROM paypal_deletions WHERE confirmed_at IS NULL AND next_at <= ? ORDER BY next_at`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("member: due deletions: %w", err)
	}
	var due []deletion
	for rows.Next() {
		var d deletion
		var at int64
		if err := rows.Scan(&d.id, &d.memberID, &d.vaultID, &d.requestID, &at, &d.attempt, &d.alerted); err != nil {
			rows.Close()
			return 0, fmt.Errorf("member: due deletions: %w", err)
		}
		d.requestedAt = time.Unix(0, at).UTC()
		due = append(due, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, fmt.Errorf("member: due deletions: %w", err)
	}
	n := 0
	for _, d := range due {
		confirmed, err := a.try(ctx, d)
		if err != nil {
			return n, err
		}
		if confirmed {
			n++
		}
	}
	return n, nil
}
