package member

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/store"
)

// linkLifetime is the PRD's Responsibilities, PayPal link: how long a link URL works. The return
// and cancel of a link count against it too.
const linkLifetime = 30 * time.Minute

// linkExpired tells whether a link made at created stopped working at now: from exactly 30
// minutes on it does not work.
func linkExpired(created, now time.Time) bool { return !now.Before(created.Add(linkLifetime)) }

// Link statuses (paypal_links.status).
const (
	linkOpen     = "open"
	linkDone     = "done"
	linkReplaced = "replaced"
)

// Steps of a link's PayPal calls, in its PayPal-Request-Id.
const (
	stepSetupToken   = "setup_token"
	stepPaymentToken = "payment_token"
)

// Results on the return URL: the spec's PaypalLinkResult.
const (
	resultApproved  = "approved"
	resultCancelled = "cancelled"
	resultFailed    = "failed"
	resultExpired   = "expired"
)

// link is a row of paypal_links.
type link struct {
	id, memberID, consentVersion, state, returnURL, status string
	createdAt                                              time.Time
	setupRequestID, setupTokenID, approveURL               sql.NullString
	codeIssuedAt, codeUsedAt                               sql.NullInt64
}

var errNoLink = errors.New("member: no link")

const linkColumns = `id, member_id, consent_version, state, return_url, status, created_at,
	setup_request_id, setup_token_id, approve_url, code_issued_at, code_used_at`

func scanLink(row *sql.Row) (link, error) {
	var l link
	var created int64
	err := row.Scan(&l.id, &l.memberID, &l.consentVersion, &l.state, &l.returnURL, &l.status, &created,
		&l.setupRequestID, &l.setupTokenID, &l.approveURL, &l.codeIssuedAt, &l.codeUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return link{}, errNoLink
	}
	if err != nil {
		return link{}, fmt.Errorf("member: link: %w", err)
	}
	l.createdAt = time.Unix(0, created).UTC()
	return l, nil
}

func getLink(ctx context.Context, q store.Querier, id string) (link, error) {
	return scanLink(q.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM paypal_links WHERE id = ?`, id))
}

// The spec's PaypalLinkState and ConsentVersion.
var (
	statePattern   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{16,128}$`)
	consentPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// maxReturnURL is the spec's PaypalLinkRequest.return_url maxLength.
const maxReturnURL = 2048

type linkRequest struct {
	state, consentVersion, returnURL string
}

var linkFields = httpapi.Fields{"return_url": nil, "consent_version": nil, "state": nil}

// decodeLinkRequest reads the spec's PaypalLinkRequest. On a mismatch it returns the field at
// fault, if there is one.
func decodeLinkRequest(body io.Reader) (linkRequest, string, error) {
	in, field, err := decodeObject(body, linkFields, "PaypalLinkRequest")
	if err != nil {
		return linkRequest{}, field, err
	}
	var r linkRequest
	for _, f := range []struct {
		name string
		to   *string
		ok   func(string) bool
		want string
	}{
		{"state", &r.state, statePattern.MatchString, "must be 16 to 128 letters, digits, '_', '.', ':' or '-'"},
		{"consent_version", &r.consentVersion, consentPattern.MatchString, "must be 1 to 64 letters, digits, '_', '.', ':' or '-'"},
		{"return_url", &r.returnURL, absoluteURL, "must be an absolute URL of at most 2048 characters"},
	} {
		raw, ok := in[f.name]
		if !ok {
			return linkRequest{}, f.name, fmt.Errorf("%s is required", f.name)
		}
		if json.Unmarshal(raw, f.to) != nil || !f.ok(*f.to) {
			return linkRequest{}, f.name, fmt.Errorf("%s %s", f.name, f.want)
		}
	}
	return r, "", nil
}

func absoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= maxReturnURL && u.IsAbs() && u.Host != "" && u.Opaque == ""
}

// decodeObject reads a JSON object with only the fields allowed, naming schema in its errors.
func decodeObject(body io.Reader, allowed httpapi.Fields, schema string) (map[string]json.RawMessage, string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, "", errors.New("the body cannot be read or is too large")
	}
	if field, err := httpapi.CheckKeys(data, allowed); err != nil {
		return nil, field, err
	}
	var in map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, "", fmt.Errorf("the body is not a JSON %s object", schema)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, "", errors.New("the body has more than one JSON value")
	}
	return in, "", nil
}

// onOrigin tells whether u is on the store app's origin.
func (a API) onOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && strings.ToLower(u.Scheme+"://"+u.Host) == a.StoreAppOrigin
}

// errSavedExists is returned when the member already has a saved PayPal, or its deletion is pending.
var errSavedExists = errors.New("member: a saved PayPal exists")

// createPaypalLink gives a member a link URL (the PRD's UC2 main flow, Responsibilities, PayPal
// link, Guarantees, Consent). Nothing is called in PayPal yet: the setup token is made when the
// member opens the URL. The new link replaces the member's earlier ones not yet returned from.
func (a API) createPaypalLink(w http.ResponseWriter, req *http.Request) {
	m, ok := memberOf(w, req)
	if !ok {
		return
	}
	in, field, err := decodeLinkRequest(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	if !a.onOrigin(in.returnURL) {
		httpapi.WriteError(w, http.StatusUnprocessableEntity, codeReturnURLNotAllowed,
			"return_url is not on the store app's origin set in the config file.", "return_url")
		return
	}
	ctx := req.Context()
	id := newID("pll_")
	var created time.Time
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		created = a.Clock.Now()
		if _, err := getSaved(ctx, tx, m); err == nil {
			return errSavedExists
		} else if !errors.Is(err, errNoSaved) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE paypal_links SET status = ? WHERE member_id = ? AND status = ?`,
			linkReplaced, m, linkOpen); err != nil {
			return fmt.Errorf("member: replace links: %w", err)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO paypal_links (id, member_id, consent_version, state, return_url, created_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, m, in.consentVersion, in.state, in.returnURL, created.UnixNano(), linkOpen)
		if err != nil {
			return fmt.Errorf("member: store link: %w", err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errSavedExists):
		writeSavedExists(w)
	case err != nil:
		httpapi.WriteInternal(w)
	default:
		httpapi.WriteJSON(w, http.StatusCreated, struct {
			LinkURL   string `json:"link_url"`
			ExpiresAt string `json:"expires_at"`
		}{a.linkURL(id, ""), httpapi.FormatTime(created.Add(linkLifetime))})
	}
}

func writeSavedExists(w http.ResponseWriter) {
	httpapi.WriteError(w, http.StatusConflict, codeSavedPaypalExists,
		"The member already has a saved PayPal, or its deletion is still pending.", "")
}

// linkURL is BarPay's URL for the link with id, followed by suffix ("", "/return" or "/cancel").
func (a API) linkURL(id, suffix string) string {
	return a.PublicURL + httpapi.BasePath + linkPath + id + suffix
}

// redirect sends the browser to location, with no referrer, so neither PayPal nor the store app
// learns BarPay's URL with the link ID in it, and nothing keeps the answer.
func redirect(w http.ResponseWriter, location string) {
	h := w.Header()
	h.Set("Location", location)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusSeeOther)
}

// back returns the member to the store app with result, l's state and, when approved, the code:
// only these, never a PayPal token (the PRD's Responsibilities, PayPal link).
func back(w http.ResponseWriter, l link, result, code string) {
	u, err := url.Parse(l.returnURL)
	if err != nil {
		notFound(w)
		return
	}
	q := u.Query()
	q.Set("paypal_link", result)
	q.Set("state", l.state)
	q.Del("code")
	if code != "" {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	redirect(w, u.String())
}

// notFound answers a link ID BarPay does not know: there is nowhere to send the browser back to.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
}

// openLink is the member opening the link URL: BarPay makes the link's setup token, once, and
// sends the browser to PayPal to approve it. A link that stopped working, was replaced or was
// already returned from sends the member back as expired, and nothing is called.
func (a API) openLink(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var l link
	expired := false
	err := a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		var err error
		if l, err = getLink(ctx, tx, req.PathValue("link_id")); err != nil {
			return err
		}
		if l.status != linkOpen || linkExpired(l.createdAt, now) {
			expired = true
			return nil
		}
		if l.setupRequestID.Valid {
			return nil
		}
		l.setupRequestID = sql.NullString{String: requestID(l.id, stepSetupToken, 1), Valid: true}
		_, err = tx.ExecContext(ctx, `UPDATE paypal_links SET setup_request_id = ? WHERE id = ?`, l.setupRequestID.String, l.id)
		return err
	})
	switch {
	case errors.Is(err, errNoLink):
		notFound(w)
		return
	case err != nil:
		httpapi.WriteInternal(w)
		return
	case expired:
		back(w, l, resultExpired, "")
		return
	case l.approveURL.Valid:
		redirect(w, l.approveURL.String)
		return
	}
	urls := paypal.URLs{Return: a.linkURL(l.id, "/return"), Cancel: a.linkURL(l.id, "/cancel")}
	var st paypal.SetupToken
	o := send(func() (o paypal.Outcome) {
		st, o = a.PayPal.CreateSetupToken(ctx, l.setupRequestID.String, urls)
		return o
	})
	switch o {
	case paypal.Succeeded:
		err = a.Store.Tx(ctx, func(tx store.Querier) error {
			_, err := tx.ExecContext(ctx, `UPDATE paypal_links SET setup_token_id = ?, approve_url = ? WHERE id = ? AND setup_token_id IS NULL`,
				st.ID, st.ApproveURL, l.id)
			return err
		})
		if err != nil {
			httpapi.WriteInternal(w)
			return
		}
		redirect(w, st.ApproveURL)
	case paypal.Failed:
		// PayPal refused to make it: this link is over, and the member starts again.
		if err := a.finish(ctx, l.id); err != nil {
			httpapi.WriteInternal(w)
			return
		}
		back(w, l, resultFailed, "")
	default:
		// The link stays open: opening it again sends the same PayPal-Request-Id.
		back(w, l, resultFailed, "")
	}
}

// send makes call up to sendAttempts times while its outcome is unknown, and returns the last outcome.
func send(call func() paypal.Outcome) paypal.Outcome {
	o := paypal.Unknown
	for range sendAttempts {
		if o = call(); o != paypal.Unknown {
			break
		}
	}
	return o
}

// finish ends the link with id if it is still open.
func (a API) finish(ctx context.Context, id string) error {
	return a.Store.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, `UPDATE paypal_links SET status = ? WHERE id = ? AND status = ?`, linkDone, id, linkOpen)
		return err
	})
}

// claim accepts the one return or cancel of the link with id, as read in the transaction: it
// gives the link, and expired when the link no longer works or was already returned from.
func (a API) claim(ctx context.Context, id string) (l link, expired bool, err error) {
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		now := a.Clock.Now()
		if l, err = getLink(ctx, tx, id); err != nil {
			return err
		}
		if l.status != linkOpen || linkExpired(l.createdAt, now) {
			expired = true
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE paypal_links SET status = ? WHERE id = ?`, linkDone, id)
		return err
	})
	return l, expired, err
}

// returnLink is PayPal returning the member (the PRD's Fixed constraints, Verify before trusting).
// It is accepted once per link, ignores everything added to the URL, and checks with PayPal that
// the setup token stored with the link was approved before giving the member a one-time code.
func (a API) returnLink(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	l, expired, err := a.claim(ctx, req.PathValue("link_id"))
	switch {
	case errors.Is(err, errNoLink):
		notFound(w)
		return
	case err != nil:
		httpapi.WriteInternal(w)
		return
	case expired:
		back(w, l, resultExpired, "")
		return
	case !l.setupTokenID.Valid:
		back(w, l, resultFailed, "")
		return
	}
	approved, ok := false, false
	for range sendAttempts {
		if approved, ok = a.PayPal.SetupTokenApproved(ctx, l.setupTokenID.String); ok {
			break
		}
	}
	if !approved {
		back(w, l, resultFailed, "")
		return
	}
	code := newCode()
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		h := sha256.Sum256([]byte(code))
		_, err := tx.ExecContext(ctx, `UPDATE paypal_links SET code_hash = ?, code_issued_at = ? WHERE id = ?`,
			h[:], a.Clock.Now().UnixNano(), l.id)
		return err
	})
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	back(w, l, resultApproved, code)
}

// cancelLink is PayPal returning a member who cancelled: nothing is saved.
func (a API) cancelLink(w http.ResponseWriter, req *http.Request) {
	l, expired, err := a.claim(req.Context(), req.PathValue("link_id"))
	switch {
	case errors.Is(err, errNoLink):
		notFound(w)
	case err != nil:
		httpapi.WriteInternal(w)
	case expired:
		back(w, l, resultExpired, "")
	default:
		back(w, l, resultCancelled, "")
	}
}

// newCode returns "plc_" and 52 random base32 characters (260 bits), which fit the spec's PaypalLinkCode.
func newCode() string { return newID("plc_") + rand.Text() }
