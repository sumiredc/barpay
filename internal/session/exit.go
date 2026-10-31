package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// An exit token is the session ID's random part followed by a MAC of the session ID and the
// refresh period it belongs to, made with BarPay's own key: the gate finds the session from it
// and knows whether it is that session's token now, and nothing is stored per token.
const (
	tokenMACBytes = 20
	tokenMACLen   = 32 // base32 characters of tokenMACBytes
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// sessionPattern is the spec's SessionId.
var sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{16,32}$`)

// tokenKey returns the key exit tokens are made with, making it the first time one is needed.
func (a API) tokenKey(ctx context.Context) ([]byte, error) {
	read := func() ([]byte, error) {
		var key []byte
		err := a.Store.Read().QueryRowContext(ctx, `SELECT key FROM exit_token_key WHERE id = 1`).Scan(&key)
		return key, err
	}
	key, err := read()
	if !errors.Is(err, sql.ErrNoRows) {
		return key, err
	}
	key = make([]byte, 32)
	_, _ = rand.Read(key) // never fails
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO exit_token_key (id, key) VALUES (1, ?) ON CONFLICT DO NOTHING`, key)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("session: make exit token key: %w", err)
	}
	return read()
}

func makeToken(key []byte, sessionID string, period int64) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "exit-token\x00%s\x00%d", sessionID, period)
	return strings.TrimPrefix(sessionID, "ses_") + tokenEncoding.EncodeToString(m.Sum(nil)[:tokenMACBytes])
}

// tokenSession returns the ID of the session token claims to be for, or false when it cannot
// be a token at all.
func tokenSession(token string) (string, bool) {
	if len(token) <= tokenMACLen {
		return "", false
	}
	id := "ses_" + token[:len(token)-tokenMACLen]
	return id, sessionPattern.MatchString(id)
}

// period is the refresh period of a session checked in at checkedIn that t falls in: a token
// changes when the next one begins (the PRD's Responsibilities, Exit token).
func (a API) period(checkedIn, t time.Time) int64 {
	return int64(t.Sub(checkedIn) / a.ExitTokenRefresh)
}

// activeToken is s's exit token at now while it is active, and when the next one replaces it.
func (a API) activeToken(key []byte, s Session, now time.Time) (string, time.Time) {
	p := a.period(s.CheckedInAt, now)
	return makeToken(key, s.ID, p), s.CheckedInAt.Add(time.Duration(p+1) * a.ExitTokenRefresh)
}

// closedToken is the exit token of s once its total is fixed: the token of the period checkout
// began in, so the token scanned at the gate keeps working, and it no longer changes.
func (a API) closedToken(key []byte, s Session) string {
	return makeToken(key, s.ID, a.period(s.CheckedInAt, s.TotalFixedAt))
}

// reunlockUntil is when the re-unlock window of s, closed, ends, and whether s is closed. A
// closed session was charged, or recorded by staff at the register or outside BarPay; one
// closed as unpaid is not closed. Until just before that instant its exit token opens the gate.
func (a API) reunlockUntil(ctx context.Context, q store.Querier, s Session) (time.Time, bool, error) {
	if s.State != Closed {
		return time.Time{}, false, nil
	}
	r, err := getReceipt(ctx, q, s.ID)
	if err != nil {
		return time.Time{}, false, err
	}
	return r.endedAt.Add(a.ReunlockWindow), true, nil
}

// exitTokenJSON is the spec's ExitToken.
type exitTokenJSON struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

var errNoExitToken = errors.New("session: no exit token")

// getExitToken returns the session's current exit token (the PRD's Responsibilities, Exit token;
// UC5 and UC6): for an active linked session, and for a closed one within its re-unlock window.
func (a API) getExitToken(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := req.Context()
	key, err := a.tokenKey(ctx)
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	var out exitTokenJSON
	err = a.Store.View(ctx, func(q store.Querier) error {
		s, err := get(ctx, q, req.PathValue("session_id"))
		if err != nil {
			return err
		}
		now := a.Clock.Now()
		if s.State == Active && !s.LinkedAt.IsZero() {
			token, expires := a.activeToken(key, s, now)
			out = exitTokenJSON{Token: token, ExpiresAt: httpapi.FormatTime(expires)}
			return nil
		}
		until, closed, err := a.reunlockUntil(ctx, q, s)
		if err != nil {
			return err
		}
		if !closed || !now.Before(until) {
			return errNoExitToken
		}
		out = exitTokenJSON{Token: a.closedToken(key, s), ExpiresAt: httpapi.FormatTime(until)}
		return nil
	})
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errNoExitToken):
		httpapi.WriteError(w, http.StatusConflict, codeExitTokenUnavailable,
			"The session has no PayPal linked, is at the register, checking out or payment failed, "+
				"or closed longer ago than the re-unlock window.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		httpapi.WriteJSON(w, http.StatusOK, out)
	}
}
