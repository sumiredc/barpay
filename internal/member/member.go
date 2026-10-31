// Package member keeps a member's saved PayPal: linking it through PayPal's approval (the PRD's
// UC2 and Responsibilities, PayPal link), checking it when the member checks in (UC1), deleting it
// (UC9), and showing it by lookup. It serves those operations of the API spec and the browser's
// way to and from PayPal.
package member

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/store"
)

// Waker is told when a notification was stored, so it is sent at once (notify.Dispatcher).
type Waker interface{ Wake() }

// API serves getMember, createPaypalLink, confirmPaypalLink and deleteSavedPaypal, and the link
// URL with PayPal's return and cancel; Run checks saved PayPals at check-in and retries deletions.
type API struct {
	Store  *store.Store
	Clock  clock.Clock
	PayPal paypal.Client
	Notify Waker
	// PublicURL is BarPay's base URL as the member's browser and PayPal reach it, without a
	// trailing "/".
	PublicURL string
	// StoreAppOrigin is the only origin a member is returned to, in lower case.
	StoreAppOrigin string
}

// linkPath is the path of the link URLs, below httpapi.BasePath. It needs no key: the
// unguessable link ID in it is the only thing the member's browser has.
const linkPath = "/paypal-links/"

// Register adds the operations to r.
func (a API) Register(r *httpapi.Router) {
	r.Store("GET /members/{member_id}", http.HandlerFunc(a.getMember))
	r.Store("POST /members/{member_id}/paypal-link", http.HandlerFunc(a.createPaypalLink))
	r.Store("POST /members/{member_id}/paypal-link/confirm", http.HandlerFunc(a.confirmPaypalLink))
	r.Store("DELETE /members/{member_id}/saved-paypal", http.HandlerFunc(a.deleteSavedPaypal))
	r.Public("GET "+linkPath+"{link_id}", http.HandlerFunc(a.openLink))
	r.Public("GET "+linkPath+"{link_id}/return", http.HandlerFunc(a.returnLink))
	r.Public("GET "+linkPath+"{link_id}/cancel", http.HandlerFunc(a.cancelLink))
}

// Error codes of the spec's Error that these operations return.
const (
	codeSavedPaypalExists          httpapi.Code = "saved_paypal_exists"
	codePaypalAccountAlreadyLinked httpapi.Code = "paypal_account_already_linked"
	codeSavedPaypalNotFound        httpapi.Code = "saved_paypal_not_found"
	codeSavedPaypalInUse           httpapi.Code = "saved_paypal_in_use"
	codeReturnURLNotAllowed        httpapi.Code = "return_url_not_allowed"
	codeLinkCodeInvalid            httpapi.Code = "link_code_invalid"
	codePaypalSaveFailed           httpapi.Code = "paypal_save_failed"
)

// sendAttempts is how many times one request sends a PayPal call whose outcome is unknown, each
// time with the same PayPal-Request-Id: network retries (the PRD's Fixed constraints, Idempotency).
const sendAttempts = 3

// requestID is the PRD's Fixed constraints, Idempotency, for the calls that have no session: the
// PayPal-Request-Id of an attempt at step, for the link or the deletion with id.
func requestID(id, step string, attempt int) string {
	return fmt.Sprintf("barpay-%s-%s-%d", id, step, attempt)
}

// memberPattern is the spec's MemberId.
var memberPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// memberOf reads the member ID in req's path, writing 400 and returning false when it is not one.
func memberOf(w http.ResponseWriter, req *http.Request) (string, bool) {
	m := req.PathValue("member_id")
	if !memberPattern.MatchString(m) {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"member_id must be 1 to 64 letters, digits, '_' or '-'.", "member_id")
		return "", false
	}
	return m, true
}

// Saved PayPal statuses: the spec's SavedPaypal.status.
const (
	statusSaved           = "saved"
	statusDeletionPending = "deletion_pending"
)

// saved is a member's saved PayPal as stored.
type saved struct {
	memberID, vaultID, consentVersion, status string
	savedAt                                   time.Time
}

// errNoSaved is returned by getSaved for a member without a saved PayPal.
var errNoSaved = errors.New("member: no saved PayPal")

func getSaved(ctx context.Context, q store.Querier, memberID string) (saved, error) {
	s := saved{memberID: memberID}
	var at int64
	err := q.QueryRowContext(ctx, `SELECT vault_id, consent_version, saved_at, status FROM saved_paypals WHERE member_id = ?`,
		memberID).Scan(&s.vaultID, &s.consentVersion, &at, &s.status)
	if errors.Is(err, sql.ErrNoRows) {
		return saved{}, errNoSaved
	}
	if err != nil {
		return saved{}, fmt.Errorf("member: saved PayPal: %w", err)
	}
	s.savedAt = time.Unix(0, at).UTC()
	return s, nil
}

// stillSaved tells whether memberID still has the saved PayPal vaultID, not being deleted.
func stillSaved(ctx context.Context, q store.Querier, memberID, vaultID string) (bool, error) {
	s, err := getSaved(ctx, q, memberID)
	if errors.Is(err, errNoSaved) {
		return false, nil
	}
	return err == nil && s.vaultID == vaultID && s.status == statusSaved, err
}

// View is the spec's Member.
type View struct {
	MemberID    string       `json:"member_id"`
	SavedPaypal *SavedPaypal `json:"saved_paypal"`
}

// SavedPaypal is the spec's SavedPaypal. It never shows PayPal's token.
type SavedPaypal struct {
	Status           string  `json:"status"`
	SavedAt          string  `json:"saved_at"`
	ConsentVersion   string  `json:"consent_version"`
	InUseBySessionID *string `json:"in_use_by_session_id"`
}

// view shows memberID as stored, reading through q.
func view(ctx context.Context, q store.Querier, memberID string) (View, error) {
	v := View{MemberID: memberID}
	s, err := getSaved(ctx, q, memberID)
	if errors.Is(err, errNoSaved) {
		return v, nil
	}
	if err != nil {
		return View{}, err
	}
	v.SavedPaypal = &SavedPaypal{Status: s.status, SavedAt: httpapi.FormatTime(s.savedAt), ConsentVersion: s.consentVersion}
	inUse, err := inUseBy(ctx, q, memberID)
	if err != nil {
		return View{}, err
	}
	if inUse != "" {
		v.SavedPaypal.InUseBySessionID = &inUse
	}
	return v, nil
}

// inUseBy is the session that uses memberID's saved PayPal: the member's session linked to it
// that has not ended (the PRD's UC9 exception), or "" when none does.
func inUseBy(ctx context.Context, q store.Querier, memberID string) (string, error) {
	o, ok, err := session.OpenOf(ctx, q, memberID)
	if err != nil || !ok || !o.Linked {
		return "", err
	}
	return o.ID, nil
}

// getMember is the lookup of a member's saved PayPal (the PRD's Responsibilities, Lookup).
func (a API) getMember(w http.ResponseWriter, req *http.Request) {
	m, ok := memberOf(w, req)
	if !ok {
		return
	}
	var v View
	err := a.Store.View(req.Context(), func(q store.Querier) (err error) {
		v, err = view(req.Context(), q, m)
		return err
	})
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, v)
}

// newID returns prefix and 26 random base32 characters (130 bits).
func newID(prefix string) string { return prefix + rand.Text() }
