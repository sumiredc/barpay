package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// memberPattern is the spec's MemberId.
var memberPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// startedData is the data of the spec's SessionStartedEvent.
type startedData struct {
	Session View `json:"session"`
}

// createCheckin starts a session at once (the PRD's UC1). The session, its session.started
// notification and the response kept for the Idempotency-Key are stored in one transaction, so
// none is kept without the others. Refusing a member with an open session changes nothing, so
// that response is not kept.
func (a API) createCheckin(w http.ResponseWriter, req *http.Request) {
	member, field, err := decodeCheckin(req.Body)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	claim := idempotency.FromRequest(req)
	if claim == nil {
		httpapi.WriteInternal(w)
		return
	}
	ctx := req.Context()
	var out []byte
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		// Read inside the transaction, which waits for the write lock, so check-ins and orders
		// get times in the order they are stored.
		now := a.Clock.Now()
		s, ok, err := settings.Load(ctx, tx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("session: no settings are saved")
		}
		ses := Session{ID: newID(), MemberID: member, State: Active, Currency: s.Currency, CheckedInAt: now}
		if err := insert(ctx, tx, ses); err != nil {
			return err
		}
		v, err := view(ctx, tx, ses, now)
		if err != nil {
			return err
		}
		if _, err := notify.Enqueue(ctx, tx, now, notify.Event{
			Type: notify.SessionStarted, SubjectID: ses.ID, Data: startedData{Session: v},
		}); err != nil {
			return err
		}
		if out, err = json.Marshal(v); err != nil {
			return err
		}
		return claim.Keep(ctx, tx, now, http.StatusCreated, out)
	})
	switch {
	case errors.Is(err, errMemberHasOpenSession):
		httpapi.WriteError(w, http.StatusConflict, codeMemberHasOpenSession,
			"The member already has a session that has not ended.", "")
		return
	case err != nil:
		httpapi.WriteInternal(w)
		return
	}
	a.Notify.Wake()
	idempotency.Write(w, http.StatusCreated, out)
}

var checkinFields = httpapi.Fields{"member_id": nil}

// decodeCheckin reads the spec's CheckinRequest and returns its member ID, empty for a guest.
// On a mismatch it returns the field at fault, if there is one.
func decodeCheckin(body io.Reader) (member, field string, err error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", "", errors.New("the body cannot be read or is too large")
	}
	if field, err := httpapi.CheckKeys(data, checkinFields); err != nil {
		return "", field, err
	}
	var in map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil || in == nil {
		return "", "", errors.New("the body is not a JSON CheckinRequest object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", errors.New("the body has more than one JSON value")
	}
	raw, ok := in["member_id"]
	if !ok {
		return "", "", nil
	}
	if err := json.Unmarshal(raw, &member); err != nil || !memberPattern.MatchString(member) {
		return "", "member_id", errors.New("member_id must be 1 to 64 letters, digits, '_' or '-'; omit it for a guest")
	}
	return member, "", nil
}
