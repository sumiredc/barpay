package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BarPay is the only place the sample app calls BarPay, from its server, with the operations of
// docs/api/openapi.yaml. Each method is named after the operationId it calls. The browser never
// sees either API key.
type BarPay struct {
	base     string
	storeKey string
	adminKey string
	http     *http.Client
}

func newBarPay(cfg *Config) *BarPay {
	return &BarPay{
		base:     cfg.BarPayURL + "/v1",
		storeKey: cfg.StoreAPIKey,
		adminKey: cfg.AdminAPIKey,
		// requestRateEvaluation answers within 30 seconds; closeSession and retryCharge answer
		// once the charge was attempted, or with checking_out when its outcome is unknown.
		http: &http.Client{
			Timeout:       45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Money is the spec's Money, Total and SignedMoney.
type Money struct {
	Value    int64  `json:"value"`
	Currency string `json:"currency"`
}

type Session struct {
	SessionID         string     `json:"session_id"`
	State             string     `json:"state"`
	MemberID          *string    `json:"member_id"`
	PaymentMethod     string     `json:"payment_method"`
	CheckedInAt       time.Time  `json:"checked_in_at"`
	TotalFixedAt      *time.Time `json:"total_fixed_at,omitempty"`
	RatePerHour       Money      `json:"rate_per_hour"`
	CoverChargeStatus string     `json:"cover_charge_status"`
	OrdersTotal       Money      `json:"orders_total"`
	CoverCharge       Money      `json:"cover_charge"`
	Total             Money      `json:"total"`
	OrderCount        int        `json:"order_count"`
	UnpaidAmount      *Money     `json:"unpaid_amount"`
	Receipt           *Receipt   `json:"receipt"`

	// CoverChargeResumesAt is when the pause ends, while CoverChargeStatus is "paused"; nil
	// otherwise, and from a BarPay that does not tell it.
	CoverChargeResumesAt *time.Time `json:"cover_charge_resumes_at"`
}

// ended says whether the session can change no more.
func (s *Session) ended() bool { return s.State == "closed" || s.State == "closed_unpaid" }

// Receipt holds the fields of every settlement; the ones another settlement has are left zero.
type Receipt struct {
	Settlement      string    `json:"settlement"`
	OrdersTotal     Money     `json:"orders_total"`
	CoverCharge     Money     `json:"cover_charge"`
	Total           Money     `json:"total"`
	EndedAt         time.Time `json:"ended_at"`
	PaypalCaptureID *string   `json:"paypal_capture_id,omitempty"`
	AmountRecorded  *Money    `json:"amount_recorded,omitempty"`
	UnpaidAmount    *Money    `json:"unpaid_amount,omitempty"`
}

type OrderLine struct {
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
	UnitPrice Money  `json:"unit_price"`
}

type Order struct {
	OrderID    string      `json:"order_id"`
	Lines      []OrderLine `json:"lines"`
	Amount     Money       `json:"amount"`
	RecordedAt time.Time   `json:"recorded_at"`
}

type Member struct {
	MemberID    string       `json:"member_id"`
	SavedPaypal *SavedPaypal `json:"saved_paypal"`
}

type SavedPaypal struct {
	Status           string    `json:"status"`
	SavedAt          time.Time `json:"saved_at"`
	ConsentVersion   string    `json:"consent_version"`
	InUseBySessionID *string   `json:"in_use_by_session_id"`
}

type PaypalLink struct {
	LinkURL   string    `json:"link_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ExitToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RatePoint struct {
	RatePerHour Money     `json:"rate_per_hour"`
	Change      Money     `json:"change"`
	PublishedAt time.Time `json:"published_at"`
}

// CurrentRate is the rate published now, with the cap per visit of the settings in effect, so a
// customer sees the cap before checking in.
type CurrentRate struct {
	RatePoint
	CapPerVisit Money `json:"cap_per_visit"` // 0 for no cap
}

type RateEvaluation struct {
	Rate      RatePoint `json:"rate"`
	Evaluated bool      `json:"evaluated"`
	Fallback  *string   `json:"fallback"`
}

type Settings struct {
	Floor        Money `json:"floor"`
	Ceiling      Money `json:"ceiling"`
	BlockMinutes int   `json:"block_minutes"`
	PauseMinutes int   `json:"pause_minutes"`
	CapPerVisit  Money `json:"cap_per_visit"`
	Goal         int   `json:"goal"`
}

type Decision struct {
	DecidedAt time.Time `json:"decided_at"`
	Trigger   string    `json:"trigger"`
	StaffID   string    `json:"staff_id,omitempty"`
	Inputs    *struct {
		Occupancy  float64  `json:"occupancy"`
		Liveliness *float64 `json:"liveliness"`
		Goal       int      `json:"goal"`
	} `json:"inputs"`
	Proposal    *Money    `json:"proposal"`
	Reasoning   *string   `json:"reasoning"`
	Published   RatePoint `json:"published"`
	Explanation string    `json:"explanation"`
	Fallback    *string   `json:"fallback"`
	// Range is the floor and ceiling the decision was made with; nil only for a fallback with
	// no range in effect. PreviousRange is set for a range_change only.
	Range         *Range `json:"range"`
	PreviousRange *Range `json:"previous_range"`
}

type Range struct {
	Floor   Money `json:"floor"`
	Ceiling Money `json:"ceiling"`
}

type Alert struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	RaisedAt  time.Time `json:"raised_at"`
	SessionID string    `json:"session_id,omitempty"`
	MemberID  string    `json:"member_id,omitempty"`
	Message   string    `json:"message"`
}

// NotificationRecord has exactly one of SessionID and MemberID.
type NotificationRecord struct {
	EventID       string     `json:"event_id"`
	Type          string     `json:"type"`
	SessionID     string     `json:"session_id,omitempty"`
	MemberID      string     `json:"member_id,omitempty"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	CreatedAt     time.Time  `json:"created_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at"`
	// NextAttemptAt is nil once delivered or given up.
	NextAttemptAt *time.Time `json:"next_attempt_at"`
}

// APIError is an error response from BarPay.
type APIError struct {
	Status    int
	Code      string
	Message   string
	SessionID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("BarPay answered %d %s: %s", e.Status, e.Code, e.Message)
}

// errorCode returns the spec's error code of err, or "" when BarPay did not answer with one.
func errorCode(err error) string {
	var e *APIError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrNoAnswer is a request whose answer never arrived, so whether it ran is not known.
var ErrNoAnswer = errors.New("BarPay did not answer")

type call struct {
	method  string
	path    string
	admin   bool
	staffID string
	idemKey string
	body    any
	out     any
}

// send makes one attempt. A transport error, a timeout or an answer that cannot be read is
// ErrNoAnswer: the request may or may not have run.
func (b *BarPay) send(ctx context.Context, c call) (int, error) {
	var body io.Reader
	var raw []byte
	if c.body != nil {
		var err error
		if raw, err = json.Marshal(c.body); err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, b.base+c.path, body)
	if err != nil {
		return 0, err
	}
	key := b.storeKey
	if c.admin {
		key = b.adminKey
		req.Header.Set("BarPay-Staff-Id", c.staffID)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.idemKey != "" {
		req.Header.Set("Idempotency-Key", c.idemKey)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if resp.StatusCode == http.StatusNoContent || c.out == nil {
			return resp.StatusCode, nil
		}
		if err := json.Unmarshal(data, c.out); err != nil {
			return 0, fmt.Errorf("%w: unreadable %d answer", ErrNoAnswer, resp.StatusCode)
		}
		return resp.StatusCode, nil
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				SessionID string `json:"session_id"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error.Code == "" {
		return 0, fmt.Errorf("%w: unreadable %d answer", ErrNoAnswer, resp.StatusCode)
	}
	return resp.StatusCode, &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, SessionID: e.Error.Details.SessionID}
}

// resendDelays are the waits before sending a request again. Only requests that are safe to send
// twice are resent: reads, and requests with an Idempotency-Key or another key BarPay
// deduplicates by (an order's order_id).
var resendDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// do sends c, and resends it when no answer arrived, when BarPay answered 5xx, or when the same
// Idempotency-Key is still being processed. A request that is not safe to send twice is sent
// once.
func (b *BarPay) do(ctx context.Context, c call, resend bool) (int, error) {
	for attempt := 0; ; attempt++ {
		status, err := b.send(ctx, c)
		var apiErr *APIError
		retryable := errors.Is(err, ErrNoAnswer) ||
			errors.As(err, &apiErr) && (apiErr.Status >= 500 || apiErr.Code == "idempotency_key_in_progress")
		if !resend || !retryable || attempt == len(resendDelays) {
			return status, err
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("%w: %v", ErrNoAnswer, ctx.Err())
		case <-time.After(resendDelays[attempt]):
		}
	}
}

// Store API key operations.

func (b *BarPay) createCheckin(ctx context.Context, idemKey string, memberID string) (*Session, error) {
	body := map[string]string{}
	if memberID != "" {
		body["member_id"] = memberID
	}
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/checkins", idemKey: idemKey, body: body, out: &s}, true)
	return &s, err
}

func (b *BarPay) getSession(ctx context.Context, id string) (*Session, error) {
	return b.readSession(ctx, id, true)
}

// getSessionOnce reads a session without resending, for a page that would rather go without it
// than wait.
func (b *BarPay) getSessionOnce(ctx context.Context, id string) (*Session, error) {
	return b.readSession(ctx, id, false)
}

func (b *BarPay) readSession(ctx context.Context, id string, resend bool) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "GET", path: "/sessions/" + url.PathEscape(id), out: &s}, resend)
	return &s, err
}

// reportOrder resends with the same order_id, which BarPay records once.
func (b *BarPay) reportOrder(ctx context.Context, sessionID, orderID string, lines []OrderLine) (*Order, error) {
	var o Order
	body := map[string]any{"order_id": orderID, "lines": lines}
	_, err := b.do(ctx, call{method: "POST", path: "/sessions/" + url.PathEscape(sessionID) + "/orders", body: body, out: &o}, true)
	return &o, err
}

// requestRegister is safe to resend: a session already at the register is returned as it is.
func (b *BarPay) requestRegister(ctx context.Context, sessionID string) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/sessions/" + url.PathEscape(sessionID) + "/register-request", out: &s}, true)
	return &s, err
}

func (b *BarPay) getExitToken(ctx context.Context, sessionID string) (*ExitToken, error) {
	var t ExitToken
	_, err := b.do(ctx, call{method: "GET", path: "/sessions/" + url.PathEscape(sessionID) + "/exit-token", out: &t}, true)
	return &t, err
}

func (b *BarPay) getRate(ctx context.Context) (*CurrentRate, error) {
	var r CurrentRate
	_, err := b.do(ctx, call{method: "GET", path: "/rate", out: &r}, true)
	return &r, err
}

func (b *BarPay) getRateHistory(ctx context.Context, since time.Time) ([]RatePoint, error) {
	var r struct {
		Rates []RatePoint `json:"rates"`
	}
	path := "/rate/history?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339Nano))
	_, err := b.do(ctx, call{method: "GET", path: path, out: &r}, true)
	return r.Rates, err
}

// requestRateEvaluation is safe to resend: BarPay merges requests and keeps the minimum interval.
func (b *BarPay) requestRateEvaluation(ctx context.Context) (*RateEvaluation, error) {
	var r RateEvaluation
	_, err := b.do(ctx, call{method: "POST", path: "/rate-evaluations", out: &r}, true)
	return &r, err
}

func (b *BarPay) getMember(ctx context.Context, memberID string) (*Member, error) {
	var m Member
	_, err := b.do(ctx, call{method: "GET", path: "/members/" + url.PathEscape(memberID), out: &m}, true)
	return &m, err
}

// createPaypalLink is sent once: each call replaces the member's earlier link URL.
func (b *BarPay) createPaypalLink(ctx context.Context, memberID, state, consentVersion, returnURL string) (*PaypalLink, error) {
	var l PaypalLink
	body := map[string]string{"state": state, "consent_version": consentVersion, "return_url": returnURL}
	_, err := b.do(ctx, call{method: "POST", path: "/members/" + url.PathEscape(memberID) + "/paypal-link", body: body, out: &l}, false)
	return &l, err
}

// confirmPaypalLink is sent once: the first request uses up the code.
func (b *BarPay) confirmPaypalLink(ctx context.Context, memberID, code string) (*Member, error) {
	var m Member
	_, err := b.do(ctx, call{method: "POST", path: "/members/" + url.PathEscape(memberID) + "/paypal-link/confirm", body: map[string]string{"code": code}, out: &m}, false)
	return &m, err
}

// deleteSavedPaypal is sent once. It returns true when PayPal confirmed the deletion within the request (204) and
// false when BarPay accepted it and keeps trying (202).
func (b *BarPay) deleteSavedPaypal(ctx context.Context, memberID string) (bool, error) {
	var m Member
	status, err := b.do(ctx, call{method: "DELETE", path: "/members/" + url.PathEscape(memberID) + "/saved-paypal", out: &m}, false)
	return status == http.StatusNoContent, err
}

// Admin API key operations. staffID is the staff member the sample app authenticated.

func pageQuery(limit int, cursor string, extra url.Values) string {
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return "?" + q.Encode()
}

func (b *BarPay) listSessions(ctx context.Context, staffID, states, cursor string) ([]Session, *string, error) {
	var r struct {
		Sessions   []Session `json:"sessions"`
		NextCursor *string   `json:"next_cursor"`
	}
	extra := url.Values{}
	if states != "" {
		extra.Set("state", states)
	}
	_, err := b.do(ctx, call{method: "GET", path: "/admin/sessions" + pageQuery(20, cursor, extra), admin: true, staffID: staffID, out: &r}, true)
	return r.Sessions, r.NextCursor, err
}

// closeSession and retryCharge can start a charge. They are resent only with the same
// Idempotency-Key, which BarPay answers with the first response and never charges again for.
func (b *BarPay) closeSession(ctx context.Context, staffID, idemKey, sessionID string) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/admin/sessions/" + url.PathEscape(sessionID) + "/close", admin: true, staffID: staffID, idemKey: idemKey, out: &s}, true)
	return &s, err
}

func (b *BarPay) retryCharge(ctx context.Context, staffID, idemKey, sessionID string) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/admin/sessions/" + url.PathEscape(sessionID) + "/retry", admin: true, staffID: staffID, idemKey: idemKey, out: &s}, true)
	return &s, err
}

// recordRegisterPayment, settleOutside and markUnpaid are sent once: a resend after a lost answer
// gets 409 when the first one closed the session, so staff read the session instead.
func (b *BarPay) recordRegisterPayment(ctx context.Context, staffID, sessionID string, amount Money) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/admin/sessions/" + url.PathEscape(sessionID) + "/register-payment", admin: true, staffID: staffID, body: map[string]Money{"amount_recorded": amount}, out: &s}, false)
	return &s, err
}

func (b *BarPay) settleOutside(ctx context.Context, staffID, sessionID string, amount Money) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/admin/sessions/" + url.PathEscape(sessionID) + "/settle-outside", admin: true, staffID: staffID, body: map[string]Money{"amount_recorded": amount}, out: &s}, false)
	return &s, err
}

func (b *BarPay) markUnpaid(ctx context.Context, staffID, sessionID string) (*Session, error) {
	var s Session
	_, err := b.do(ctx, call{method: "POST", path: "/admin/sessions/" + url.PathEscape(sessionID) + "/mark-unpaid", admin: true, staffID: staffID, out: &s}, false)
	return &s, err
}

// unlockGate is sent once: a resend could open the gate twice.
func (b *BarPay) unlockGate(ctx context.Context, staffID, sessionID string) error {
	body := map[string]string{}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	_, err := b.do(ctx, call{method: "POST", path: "/admin/gate/unlock", admin: true, staffID: staffID, body: body}, false)
	return err
}

func (b *BarPay) getSettings(ctx context.Context, staffID string) (*Settings, error) {
	var s Settings
	_, err := b.do(ctx, call{method: "GET", path: "/admin/settings", admin: true, staffID: staffID, out: &s}, true)
	return &s, err
}

// updateSettings replaces every setting, so sending it twice leaves the same settings.
func (b *BarPay) updateSettings(ctx context.Context, staffID string, s Settings) (*Settings, error) {
	var out Settings
	_, err := b.do(ctx, call{method: "PUT", path: "/admin/settings", admin: true, staffID: staffID, body: s, out: &out}, true)
	return &out, err
}

func (b *BarPay) listDecisions(ctx context.Context, staffID, cursor string) ([]Decision, *string, error) {
	var r struct {
		Decisions  []Decision `json:"decisions"`
		NextCursor *string    `json:"next_cursor"`
	}
	_, err := b.do(ctx, call{method: "GET", path: "/admin/decisions" + pageQuery(20, cursor, nil), admin: true, staffID: staffID, out: &r}, true)
	return r.Decisions, r.NextCursor, err
}

func (b *BarPay) listAlerts(ctx context.Context, staffID, cursor string) ([]Alert, *string, error) {
	return b.readAlerts(ctx, staffID, cursor, true)
}

// listAlertsOnce reads a page of the alerts without resending, for a page that would rather go
// without them than wait.
func (b *BarPay) listAlertsOnce(ctx context.Context, staffID, cursor string) ([]Alert, *string, error) {
	return b.readAlerts(ctx, staffID, cursor, false)
}

func (b *BarPay) readAlerts(ctx context.Context, staffID, cursor string, resend bool) ([]Alert, *string, error) {
	var r struct {
		Alerts     []Alert `json:"alerts"`
		NextCursor *string `json:"next_cursor"`
	}
	_, err := b.do(ctx, call{method: "GET", path: "/admin/alerts" + pageQuery(20, cursor, nil), admin: true, staffID: staffID, out: &r}, resend)
	return r.Alerts, r.NextCursor, err
}

func (b *BarPay) listNotifications(ctx context.Context, staffID, cursor string) ([]NotificationRecord, *string, error) {
	var r struct {
		Notifications []NotificationRecord `json:"notifications"`
		NextCursor    *string              `json:"next_cursor"`
	}
	_, err := b.do(ctx, call{method: "GET", path: "/admin/notifications" + pageQuery(20, cursor, nil), admin: true, staffID: staffID, out: &r}, true)
	return r.Notifications, r.NextCursor, err
}
