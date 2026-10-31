package member

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// codeLifetime is the spec's PaypalLinkCode: how long a one-time code works after it was issued.
const codeLifetime = 10 * time.Minute

// codeExpired tells whether a code issued at issued stopped working at now: from exactly 10
// minutes on it does not work.
func codeExpired(issued, now time.Time) bool { return !now.Before(issued.Add(codeLifetime)) }

// codePattern is the spec's PaypalLinkCode.
var codePattern = regexp.MustCompile(`^plc_[A-Za-z0-9]{32,64}$`)

var confirmFields = httpapi.Fields{"code": nil}

func decodeConfirm(body io.Reader) (string, string, error) {
	in, field, err := decodeObject(body, confirmFields, "PaypalLinkConfirmRequest")
	if err != nil {
		return "", field, err
	}
	raw, ok := in["code"]
	if !ok {
		return "", "code", errors.New("code is required")
	}
	var code string
	if json.Unmarshal(raw, &code) != nil || !codePattern.MatchString(code) {
		return "", "code", errors.New("code must be the code BarPay added to the return URL")
	}
	return code, "", nil
}

var (
	errCodeInvalid   = errors.New("member: the code is unknown, used, expired or another member's")
	errAlreadyLinked = errors.New("member: the PayPal is linked to another member")
)

// linkedData is the data of the spec's MemberPaypalLinkedEvent.
type linkedData struct {
	Member View `json:"member"`
}

// confirmPaypalLink saves PayPal for the member (the PRD's UC2, Fixed constraints, Verify before
// trusting). The code is used up first, in a transaction of its own, so that it never works twice,
// whatever follows; the save's PayPal-Request-Id is stored with it, before PayPal is called. The
// saved PayPal is kept for the member only after PayPal saved it, and only if no other member
// holds it.
func (a API) confirmPaypalLink(w http.ResponseWriter, req *http.Request) {
	m, ok := memberOf(w, req)
	if !ok {
		return
	}
	code, field, err := decodeConfirm(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	ctx := req.Context()
	l, err := a.useCode(ctx, m, code)
	switch {
	case errors.Is(err, errCodeInvalid):
		httpapi.WriteError(w, http.StatusUnprocessableEntity, codeLinkCodeInvalid,
			"The code is unknown, already used, expired, or was issued for another member. Nothing was saved.", "code")
		return
	case errors.Is(err, errSavedExists):
		writeSavedExists(w)
		return
	case err != nil:
		httpapi.WriteInternal(w)
		return
	}
	var vault string
	o := send(func() (o paypal.Outcome) {
		vault, o = a.PayPal.CreatePaymentToken(ctx, requestID(l.id, stepPaymentToken, 1), l.setupTokenID.String)
		return o
	})
	if o != paypal.Succeeded {
		msg := "PayPal did not save it. Nothing was saved; the member can start linking again."
		if o == paypal.Unknown {
			msg = "PayPal's answer could not be read. Nothing was saved in BarPay; the member can start linking again."
		}
		httpapi.WriteError(w, http.StatusBadGateway, codePaypalSaveFailed, msg, "")
		return
	}
	v, err := a.keep(ctx, l, vault)
	switch {
	case errors.Is(err, errAlreadyLinked):
		httpapi.WriteError(w, http.StatusConflict, codePaypalAccountAlreadyLinked,
			"This PayPal account is linked to another member. Nothing was saved.", "")
	case errors.Is(err, errSavedExists):
		writeSavedExists(w)
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		a.Notify.Wake()
		httpapi.WriteJSON(w, http.StatusCreated, v)
	}
}

// useCode uses up code for member and returns its link, ready to save: errCodeInvalid for a code
// that is unknown, used, expired or issued for another member, and errSavedExists for a member
// who has a saved PayPal or one being deleted. Either way the code is used up. The stored
// save_request_id is not sent again after this request: a save still unknown then is a failure,
// and what PayPal may have saved goes to the next link of that PayPal account (the PRD's UC2).
func (a API) useCode(ctx context.Context, member, code string) (link, error) {
	h := sha256.Sum256([]byte(code))
	var l link
	var refusal error
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		var err error
		l, err = scanLink(tx.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM paypal_links WHERE code_hash = ?`, h[:]))
		if errors.Is(err, errNoLink) {
			refusal = errCodeInvalid
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE paypal_links SET code_used_at = ? WHERE id = ? AND code_used_at IS NULL`, now.UnixNano(), l.id)
		if err != nil {
			return fmt.Errorf("member: use code: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 || l.memberID != member || codeExpired(time.Unix(0, l.codeIssuedAt.Int64), now) {
			refusal = errCodeInvalid
			return nil
		}
		if _, err := getSaved(ctx, tx, member); err == nil {
			refusal = errSavedExists
			return nil
		} else if !errors.Is(err, errNoSaved) {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE paypal_links SET save_request_id = ? WHERE id = ?`,
			requestID(l.id, stepPaymentToken, 1), l.id)
		return err
	})
	if err == nil {
		err = refusal
	}
	return l, err
}

// keep stores the saved PayPal vault for l's member, links the member's active session to it,
// and stores member.paypal_linked and session.paypal_linked, in one transaction. A vault BarPay
// holds for another member is refused and left as it is (the PRD's UC2 exception): one PayPal
// account is linked to at most one member. One BarPay holds for no member is the member's, also
// when PayPal returned one it already had for this PayPal account.
func (a API) keep(ctx context.Context, l link, vault string) (View, error) {
	var v View
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		var holder string
		err := tx.QueryRowContext(ctx, `SELECT member_id FROM saved_paypals WHERE vault_id = ?`, vault).Scan(&holder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("member: holder: %w", err)
		case holder != l.memberID:
			return errAlreadyLinked
		}
		if _, err := getSaved(ctx, tx, l.memberID); err == nil {
			// Another link of the member was confirmed meanwhile.
			return errSavedExists
		} else if !errors.Is(err, errNoSaved) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO saved_paypals (member_id, vault_id, consent_version, saved_at, status)
			VALUES (?, ?, ?, ?, ?)`, l.memberID, vault, l.consentVersion, now.UnixNano(), statusSaved)
		if err != nil {
			return fmt.Errorf("member: keep: %w", err)
		}
		if o, ok, err := session.OpenOf(ctx, tx, l.memberID); err != nil {
			return err
		} else if ok {
			if _, err := session.Link(ctx, tx, o.ID, now); err != nil {
				return err
			}
		}
		if v, err = view(ctx, tx, l.memberID); err != nil {
			return err
		}
		_, err = notify.Enqueue(ctx, tx, now, notify.Event{Type: notify.MemberPaypalLinked, SubjectID: l.memberID, Data: linkedData{Member: v}})
		return err
	})
	return v, err
}
