package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/store"
)

// errHasMember is returned by attach for a session that already has a member.
var errHasMember = errors.New("session: the session already has a member")

// attach is the one place that gives a guest's session a member (the PRD's UC10): it attaches
// memberID to s, as read in tx, and returns it as attached. It refuses, in the spec's order, a
// session that already has a member (errHasMember), one that is not active (errRefused) and a
// member with another open session (errMemberHasOpenSession), and then changes nothing.
//
// s must have been read in tx. The write lock the transaction takes when it begins is what
// serialises an attach against other attaches, check-ins and the register transitions: between
// that read and the update nothing else can change the session or the member's sessions, so
// whichever request takes the lock first holds and the other is judged on what it left.
//
// Linking the session to the member's saved PayPal is left to the check that UC1 makes for every
// active member's session that is not linked (package member), so it is the same check.
func attach(ctx context.Context, tx store.Querier, s Session, memberID string) (Session, error) {
	if err := judgeAttach(ctx, tx, s, memberID); err != nil {
		return s, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET member_id = ? WHERE id = ?`, memberID, s.ID); err != nil {
		return s, fmt.Errorf("session: attach: %w", err)
	}
	s.MemberID = memberID
	return s, nil
}

// judgeAttach tells whether memberID can be attached to s, reading the member's sessions
// through q: nil if it can, or attach's error for the first rule it breaks.
func judgeAttach(ctx context.Context, q store.Querier, s Session, memberID string) error {
	if s.MemberID != "" {
		return errHasMember
	}
	if s.State != Active {
		return errRefused
	}
	if _, open, err := OpenOf(ctx, q, memberID); err != nil {
		return err
	} else if open {
		return errMemberHasOpenSession
	}
	return nil
}

// attachMember is the store app attaching the member it has just signed up or logged in to the
// guest's session (the PRD's UC10). The attach, its session.member_attached notification and
// the response kept for the Idempotency-Key are stored in one transaction, so none is kept
// without the others. A refusal changes nothing, so its response is not kept.
func (a API) attachMember(w http.ResponseWriter, req *http.Request) {
	member, field, err := decodeAttach(req.Body)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	claim := idempotency.FromRequest(req)
	if claim == nil {
		httpapi.WriteInternal(w)
		return
	}
	ctx, id := req.Context(), req.PathValue("session_id")
	var out []byte
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		s, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		if s, err = attach(ctx, tx, s, member); err != nil {
			return err
		}
		v, err := view(ctx, tx, s, now)
		if err != nil {
			return err
		}
		if _, err := notify.Enqueue(ctx, tx, now, notify.Event{
			Type: notify.SessionMemberAttached, SubjectID: s.ID, Data: sessionData{Session: v},
		}); err != nil {
			return err
		}
		if out, err = json.Marshal(v); err != nil {
			return err
		}
		return claim.Keep(ctx, tx, now, http.StatusOK, out)
	})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errHasMember):
		httpapi.WriteError(w, http.StatusConflict, codeSessionHasMember,
			"The session already has a member, so no member was attached.", "")
	case errors.Is(err, errRefused):
		httpapi.WriteError(w, http.StatusConflict, codeSessionNotActive,
			"The session is checking out, at the register, its payment failed or it has ended, so no member was attached.", "")
	case errors.Is(err, errMemberHasOpenSession):
		httpapi.WriteError(w, http.StatusConflict, codeMemberHasOpenSession,
			"The member already has a session that has not ended.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		a.Notify.Wake()
		idempotency.Write(w, http.StatusOK, out)
	}
}

var attachFields = httpapi.Fields{"member_id": nil}

// decodeAttach reads the spec's AttachMemberRequest and returns its member ID. On a mismatch it
// returns the field at fault, if there is one.
func decodeAttach(body io.Reader) (member, field string, err error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", "", errors.New("the body cannot be read or is too large")
	}
	if field, err := httpapi.CheckKeys(data, attachFields); err != nil {
		return "", field, err
	}
	in, err := object(data)
	if err != nil {
		return "", "", errors.New("the body is not a JSON AttachMemberRequest object")
	}
	raw, ok := in["member_id"]
	if !ok {
		return "", "member_id", errors.New("member_id is required")
	}
	if member, ok = str(raw); !ok || !memberPattern.MatchString(member) {
		return "", "member_id", errors.New("member_id must be 1 to 64 letters, digits, '_' or '-'")
	}
	return member, "", nil
}
