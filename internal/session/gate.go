package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// Reader messages: what the QR reader at the exit shows after a scan (edge.yaml's ScanResult).
const (
	readThankYou      = "Thank you"
	readPayAtRegister = "Please pay at the register"
	readSeeStaff      = "Please see staff"
	readNewQR         = "Please show a new QR code"
)

// scanResultJSON is edge.yaml's ScanResult.
type scanResultJSON struct {
	Unlock        bool   `json:"unlock"`
	UnlockID      string `json:"unlock_id,omitempty"`
	ReaderMessage string `json:"reader_message"`
}

// Unlock sources, as stored.
const (
	unlockAfterScan = "scan"
	unlockByStaff   = "staff"
)

// newUnlockID returns "glk_" and 26 random base32 characters (130 bits).
func newUnlockID() string { return "glk_" + rand.Text() }

var unlockPattern = regexp.MustCompile(`^glk_[A-Za-z0-9]{16,32}$`)

// storeUnlock stores an unlock of the gate at now, for sessionID ("" for none), and returns its ID.
func storeUnlock(ctx context.Context, tx store.Querier, sessionID, source string, now time.Time) (string, error) {
	id := newUnlockID()
	var sid any
	if sessionID != "" {
		sid = sessionID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO gate_unlocks (id, session_id, source, requested_at) VALUES (?, ?, ?, ?)`,
		id, sid, source, now.UnixNano())
	if err != nil {
		return "", fmt.Errorf("session: store unlock: %w", err)
	}
	return id, nil
}

var scanFields = httpapi.Fields{"token": nil}

// scanExitToken is the edge device reading an exit QR at the gate (the PRD's UC5). For a current
// token of an active linked session it fixes the total, charges it once and unlocks only after
// the charge succeeded; within the re-unlock window after a session closed, its token unlocks
// again and nothing is charged. Anything else stays locked, with what the reader should say.
// Scans of one session run one at a time, so a second scan of the same QR at once sees the
// first one's result.
func (a API) scanExitToken(w http.ResponseWriter, req *http.Request) {
	token, field, msg := decodeScan(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if msg != "" {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg, field)
		return
	}
	id, ok := tokenSession(token)
	if !ok {
		httpapi.WriteJSON(w, http.StatusOK, scanResultJSON{ReaderMessage: readNewQR})
		return
	}
	ctx := req.Context()
	key, err := a.tokenKey(ctx)
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	defer a.Gate.Scan(id)()
	if err := a.closeIfDue(ctx, id); err != nil {
		httpapi.WriteInternal(w)
		return
	}
	res := scanResultJSON{ReaderMessage: readSeeStaff}
	var c *charge
	closedAtZero := false
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, id)
		if errors.Is(err, errNotFound) {
			res.ReaderMessage = readNewQR
			return nil
		}
		if err != nil {
			return err
		}
		active, _ := a.activeToken(key, s, now)
		isActive := equal(token, active)
		isClosed := !s.TotalFixedAt.IsZero() && equal(token, a.closedToken(key, s))
		until, closed, err := a.reunlockUntil(ctx, tx, s)
		if err != nil {
			return err
		}
		switch {
		case !isActive && !isClosed:
			res.ReaderMessage = readNewQR
		case s.State == Active && s.LinkedAt.IsZero(), s.State == AtRegister:
			res.ReaderMessage = readPayAtRegister
		case s.State == Active && isActive:
			var v View
			v, c, err = a.begin(ctx, tx, s, checkout, now)
			switch {
			case errors.Is(err, errNoSavedPaypal):
				// Not expected while linked: begin changed nothing, and the customer sees staff.
				return nil
			case err != nil || c != nil:
				return err
			}
			closedAtZero = true
			return a.unlockAfterCharge(ctx, tx, now, v, &res)
		case closed && isClosed && now.Before(until):
			// The re-unlock window (UC5 alternative flow): nothing is charged.
			res.UnlockID, err = storeUnlock(ctx, tx, s.ID, unlockAfterScan, now)
			res.Unlock, res.ReaderMessage = err == nil, readThankYou
		}
		return err
	})
	if err == nil && closedAtZero {
		a.Notify.Wake()
	}
	if err == nil && c != nil {
		_, err = a.send(ctx, *c, func(tx store.Querier, now time.Time, v View) error {
			return a.unlockAfterCharge(ctx, tx, now, v, &res)
		})
	}
	if err != nil {
		a.Log.Error("exit scan not handled", "error", err)
		httpapi.WriteInternal(w)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, res)
}

// unlockAfterCharge sets res for v, the session as the scan's checkout left it: it unlocks only
// when the checkout closed the session, and otherwise the customer is sent to staff at once, also
// while the outcome is unknown (the PRD's UC5 exceptions).
func (a API) unlockAfterCharge(ctx context.Context, tx store.Querier, now time.Time, v View, res *scanResultJSON) error {
	if v.State != Closed {
		res.ReaderMessage = readSeeStaff
		return nil
	}
	id, err := storeUnlock(ctx, tx, v.SessionID, unlockAfterScan, now)
	if err != nil {
		return err
	}
	*res = scanResultJSON{Unlock: true, UnlockID: id, ReaderMessage: readThankYou}
	return nil
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// decodeScan reads edge.yaml's ScanRequest. On a mismatch it returns the field at fault, if
// there is one, and a message.
func decodeScan(body io.Reader) (token, field, msg string) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", "", "The body cannot be read or is too large."
	}
	if f, err := httpapi.CheckKeys(data, scanFields); err != nil {
		return "", f, err.Error() + "."
	}
	var in map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil || in == nil {
		return "", "", "The body is not a JSON ScanRequest object."
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", "The body has more than one JSON value."
	}
	raw, ok := in["token"]
	if !ok || json.Unmarshal(raw, &token) != nil || token == "" || len(token) > 128 {
		return "", "token", "token must be a string of 1 to 128 characters."
	}
	return token, "", ""
}

// unlockWait is how long the edge device's wait for an unlock by hand lasts before it asks again.
const unlockWait = 25 * time.Second

// waitForUnlock is the edge device waiting for an unlock made by hand (unlockGate). While it waits,
// it is connected; it asks again as soon as an answer comes.
func (a API) waitForUnlock(w http.ResponseWriter, req *http.Request) {
	id, ok := a.Gate.Wait(req.Context(), unlockWait)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		UnlockID string `json:"unlock_id"`
	}{id})
}

var unlockResultFields = httpapi.Fields{"unlocked": nil}

var errUnlockNotFound = errors.New("session: no unlock has this ID")

// reportUnlockResult is the edge device saying whether an unlock worked. A failed unlock raises
// unlock_failed; after a scan whose session was paid, the store app is also told
// (gate.unlock_failed), so staff can open the gate by hand (the PRD's UC5 exception). The first
// report of an unlock stands; a repeat changes nothing.
func (a API) reportUnlockResult(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("unlock_id")
	if !unlockPattern.MatchString(id) {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, "unlock_id is not an unlock ID.", "unlock_id")
		return
	}
	unlocked, msg := decodeUnlockResult(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if msg != "" {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg, "unlocked")
		return
	}
	ctx := req.Context()
	notified := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		var sid sql.NullString
		var source string
		var reported bool
		err := tx.QueryRowContext(ctx, `SELECT session_id, source, result IS NOT NULL FROM gate_unlocks WHERE id = ?`, id).
			Scan(&sid, &source, &reported)
		if errors.Is(err, sql.ErrNoRows) {
			return errUnlockNotFound
		}
		if err != nil || reported {
			return err
		}
		result := "unlocked"
		if !unlocked {
			result = "failed"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE gate_unlocks SET result = ?, reported_at = ? WHERE id = ?`, result, now.UnixNano(), id); err != nil {
			return err
		}
		if unlocked {
			return nil
		}
		message := "The exit gate did not unlock when staff opened it by hand. Try again, or open it at the gate."
		if source == unlockAfterScan {
			message = "The customer paid, but the exit gate did not unlock. Open the gate by hand."
		}
		if _, err := alert.Raise(ctx, tx, alert.Alert{Kind: alert.UnlockFailed, RaisedAt: now, SessionID: sid.String, Message: message}); err != nil {
			return err
		}
		if source != unlockAfterScan {
			return nil
		}
		s, err := get(ctx, tx, sid.String)
		if err != nil {
			return err
		}
		v, err := view(ctx, tx, s, now)
		if err != nil {
			return err
		}
		notified = true
		_, err = notify.Enqueue(ctx, tx, now, notify.Event{Type: notify.GateUnlockFailed, SubjectID: s.ID, Data: sessionData{Session: v}})
		return err
	})
	switch {
	case errors.Is(err, errUnlockNotFound):
		httpapi.WriteError(w, http.StatusNotFound, codeUnlockNotFound, "No unlock has this ID.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		if notified {
			a.Notify.Wake()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeUnlockResult(body io.Reader) (unlocked bool, msg string) {
	data, err := io.ReadAll(body)
	if err != nil {
		return false, "The body cannot be read or is too large."
	}
	if _, err := httpapi.CheckKeys(data, unlockResultFields); err != nil {
		return false, err.Error() + "."
	}
	var in struct {
		Unlocked *bool `json:"unlocked"`
	}
	if err := json.Unmarshal(data, &in); err != nil || in.Unlocked == nil {
		return false, "unlocked must be true or false."
	}
	return *in.Unlocked, ""
}

var unlockGateFields = httpapi.Fields{"session_id": nil}

var errGateUnavailable = errors.New("session: no edge device is connected")

// unlockGate is staff opening the gate by hand (the PRD's UC7 alternative flow): BarPay hands one
// unlock to the edge device waiting for one, and records it with the staff member and the session
// it is for, if given. With no edge device waiting, nothing is sent or recorded.
func (a API) unlockGate(w http.ResponseWriter, req *http.Request) {
	sessionID, field, msg := decodeUnlockGate(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if msg != "" {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg, field)
		return
	}
	subject := sessionID
	if subject == "" {
		subject = "gate"
	}
	ctx := req.Context()
	err := staff.TxNow(ctx, a.Store, a.Clock, staff.Change{Staff: httpapi.StaffID(req), Operation: "unlockGate", Subject: subject},
		func(tx store.Querier, now time.Time) error {
			if sessionID != "" {
				if _, err := get(ctx, tx, sessionID); err != nil {
					return err
				}
			}
			id, err := storeUnlock(ctx, tx, sessionID, unlockByStaff, now)
			if err != nil {
				return err
			}
			// Last in fn, so nothing is handed when an earlier step fails. The staff action is stored
			// after fn returns and the transaction commits after that; should either fail, the
			// unlock was handed but is not recorded.
			if !a.Gate.Hand(id) {
				return errGateUnavailable
			}
			return nil
		})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errGateUnavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, codeGateUnavailable,
			"No edge device is connected, so the gate was not opened.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func decodeUnlockGate(body io.Reader) (sessionID, field, msg string) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", "", "The body cannot be read or is too large."
	}
	if f, err := httpapi.CheckKeys(data, unlockGateFields); err != nil {
		return "", f, err.Error() + "."
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(data, &in); err != nil || in == nil {
		return "", "", "The body is not a JSON object."
	}
	raw, ok := in["session_id"]
	if !ok {
		return "", "", ""
	}
	if json.Unmarshal(raw, &sessionID) != nil || !sessionPattern.MatchString(sessionID) {
		return "", "session_id", "session_id is not a session ID."
	}
	return sessionID, "", ""
}
