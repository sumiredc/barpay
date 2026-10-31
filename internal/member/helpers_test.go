package member

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var (
	t0   = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	keys = httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")}
)

const (
	storeAuth = "Bearer k-store"
	adminAuth = "Bearer k-admin"
	edgeAuth  = "Bearer k-edge"
	// noAuth as a call's auth sends no Authorization header.
	noAuth = "-"

	publicURL = "https://barpay.test"
	origin    = "https://store.test"
	returnURL = "https://store.test/settings/paypal?tab=pay"
	state     = "state-0123456789abcdef"
)

type waker struct{ n atomic.Int64 }

func (w *waker) Wake() { w.n.Add(1) }

// fixture is BarPay after its first start at t0, serving the session and member operations, with
// a fake PayPal.
type fixture struct {
	st       *store.Store
	clk      *clock.Fake
	pp       *paypaltest.Fake
	wake     *waker
	api      API
	sessions session.API
	router   *httpapi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	s := settings.Settings{
		Currency: money.USD, Floor: money.New(600, money.USD), Ceiling: money.New(2400, money.USD),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: money.New(3000, money.USD), Goal: 50,
	}
	err := st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, s, t0); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, money.New(1200, money.USD), s, t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, clk: clock.NewFake(t0), pp: paypaltest.New()}
	f.build()
	return f
}

// build makes every in-memory part of BarPay again from f.st, as a restart does. PayPal is
// outside BarPay and keeps what it holds.
func (f *fixture) build() {
	f.wake = &waker{}
	r := httpapi.NewRouter(keys)
	f.sessions = session.API{
		Store: f.st, Clock: f.clk, Keys: idempotency.New(f.st, f.clk), Notify: f.wake, PayPal: f.pp, Saved: Saved{},
		Gate: gate.New(), ExitTokenRefresh: 30 * time.Second, ReunlockWindow: 2 * time.Minute, MaxStay: 12 * time.Hour,
		Log: slog.New(slog.DiscardHandler),
	}
	f.sessions.Register(r)
	f.api = API{Store: f.st, Clock: f.clk, PayPal: f.pp, Notify: f.wake, PublicURL: publicURL, StoreAppOrigin: origin}
	f.api.Register(r)
	f.router = r
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.st = storetest.Reopen(t, f.st)
	f.build()
}

type call struct {
	method, path, auth, key, body string
}

func (f *fixture) do(c call) *httptest.ResponseRecorder {
	if c.auth == "" {
		c.auth = storeAuth
	}
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.auth != noAuth {
		req.Header.Set("Authorization", c.auth)
	}
	req.Header.Set("BarPay-Staff-Id", "staff_1")
	if c.key != "" {
		req.Header.Set("Idempotency-Key", c.key)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// browse is the member's browser opening u, a BarPay URL, with no key.
func (f *fixture) browse(t *testing.T, u string) *httptest.ResponseRecorder {
	t.Helper()
	if !strings.HasPrefix(u, publicURL+"/") {
		t.Fatalf("%s is not a BarPay URL", u)
	}
	return f.do(call{method: http.MethodGet, path: strings.TrimPrefix(u, publicURL), auth: noAuth})
}

func linkBody(consent string) string {
	return `{"state":"` + state + `","consent_version":"` + consent + `","return_url":"` + returnURL + `"}`
}

func (f *fixture) createLink(member, body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/members/" + member + "/paypal-link", body: body})
}

// linkURL asks for a link URL for member, failing the test unless one is created.
func (f *fixture) linkURL(t *testing.T, member string) string {
	t.Helper()
	w := f.createLink(member, linkBody("consent-v1"))
	if w.Code != http.StatusCreated {
		t.Fatalf("createPaypalLink: %d %s", w.Code, w.Body)
	}
	var out struct {
		LinkURL string `json:"link_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.LinkURL
}

// toPayPal opens linkURL and returns the setup token BarPay sent the browser to approve.
func (f *fixture) toPayPal(t *testing.T, linkURL string) string {
	t.Helper()
	w := f.browse(t, linkURL)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, paypaltest.ApproveBase) {
		t.Fatalf("opening the link URL: %d to %q", w.Code, loc)
	}
	return paypaltest.SetupTokenOf(loc)
}

// returned is where a return or cancel sent the browser: the store app's URL and its query.
func returned(t *testing.T, w *httptest.ResponseRecorder) url.Values {
	t.Helper()
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, origin+"/settings/paypal?") {
		t.Fatalf("not sent back to the store app: %d to %q", w.Code, loc)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// approvedCode runs a link for member up to the return, with payer approving in PayPal, and
// returns the one-time code.
func (f *fixture) approvedCode(t *testing.T, member, payer string) string {
	t.Helper()
	token := f.toPayPal(t, f.linkURL(t, member))
	f.pp.Approve(token, payer)
	q := returned(t, f.browse(t, f.pp.URLs(token).Return))
	if q.Get("paypal_link") != "approved" || q.Get("code") == "" {
		t.Fatalf("return: %v", q)
	}
	return q.Get("code")
}

func (f *fixture) confirm(member, code string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/members/" + member + "/paypal-link/confirm", body: `{"code":"` + code + `"}`})
}

// linked links PayPal for member with payer's PayPal account, failing the test unless it is saved.
func (f *fixture) linked(t *testing.T, member, payer string) View {
	t.Helper()
	w := f.confirm(member, f.approvedCode(t, member, payer))
	if w.Code != http.StatusCreated {
		t.Fatalf("confirmPaypalLink: %d %s", w.Code, w.Body)
	}
	return decodeMember(t, w.Body.Bytes())
}

func decodeMember(t *testing.T, b []byte) View {
	t.Helper()
	var v View
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return v
}

func (f *fixture) getMember(t *testing.T, member string) View {
	t.Helper()
	w := f.do(call{method: http.MethodGet, path: "/v1/members/" + member})
	if w.Code != http.StatusOK {
		t.Fatalf("getMember: %d %s", w.Code, w.Body)
	}
	return decodeMember(t, w.Body.Bytes())
}

func (f *fixture) deleteSaved(member string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodDelete, path: "/v1/members/" + member + "/saved-paypal"})
}

// sessionView is the spec's Session, as far as these tests read it.
type sessionView struct {
	SessionID     string `json:"session_id"`
	State         string `json:"state"`
	PaymentMethod string `json:"payment_method"`
	Receipt       *struct {
		Settlement string `json:"settlement"`
	} `json:"receipt"`
}

func decodeSession(t *testing.T, b []byte) sessionView {
	t.Helper()
	var v sessionView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return v
}

var keyN atomic.Int64

// checkin checks member in and returns the session as the response showed it.
func (f *fixture) checkin(t *testing.T, member string) (sessionView, []byte) {
	t.Helper()
	w := f.do(call{method: http.MethodPost, path: "/v1/checkins", key: "key-" + strconv.FormatInt(keyN.Add(1), 10),
		body: `{"member_id":"` + member + `"}`})
	if w.Code != http.StatusCreated {
		t.Fatalf("check-in: %d %s", w.Code, w.Body)
	}
	return decodeSession(t, w.Body.Bytes()), w.Body.Bytes()
}

func (f *fixture) lookup(t *testing.T, id string) sessionView {
	t.Helper()
	w := f.do(call{method: http.MethodGet, path: "/v1/sessions/" + id})
	if w.Code != http.StatusOK {
		t.Fatalf("getSession: %d %s", w.Code, w.Body)
	}
	return decodeSession(t, w.Body.Bytes())
}

// setState moves a session to state directly in the store, for states an unlinked session never
// reaches through the API: checkout needs a link.
func (f *fixture) setState(t *testing.T, id, state string) {
	t.Helper()
	err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), "UPDATE sessions SET state = ?, total_fixed_at = ? WHERE id = ?",
			state, f.clk.Now().UnixNano(), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// events returns the bodies of the stored notifications of type typ about subject, in order.
func (f *fixture) events(t *testing.T, subject, typ string) [][]byte {
	t.Helper()
	rows, err := f.st.Read().QueryContext(context.Background(),
		"SELECT body FROM notifications WHERE subject_id = ? AND type = ? ORDER BY seq", subject, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) checkDue(t *testing.T) int {
	t.Helper()
	n, err := f.api.CheckDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) deleteDue(t *testing.T) int {
	t.Helper()
	n, err := f.api.DeleteDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) (code, field, session string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Field     string `json:"field"`
				SessionID string `json:"session_id"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return body.Error.Code, body.Error.Details.Field, body.Error.Details.SessionID
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, body %s; want %d", w.Code, w.Body, status)
	}
	if got, _, _ := errorOf(t, w); got != code {
		t.Errorf("code = %q, want %q", got, code)
	}
}
