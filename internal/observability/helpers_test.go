// Package observability_test checks the PRD's Guarantees, Observability across BarPay: each
// situation it names raises its alert, readable through the admin API, with no secret in what the
// admin API returns. BarPay is assembled as cmd/barpay assembles it, with a fake clock, a fake
// PayPal and a fake store app.
package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/config"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/member"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/signals"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/startup"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

// Every secret of the config file, each a value that appears nowhere else.
var secrets = map[string]string{
	"PayPal client ID":            "secret-paypal-client-id-7f3a",
	"PayPal client secret":        "secret-paypal-client-secret-9c2e",
	"store API key":               "secret-store-api-key-4b8d",
	"admin API key":               "secret-admin-api-key-1e6f",
	"edge device's shared secret": "secret-edge-shared-secret-5a0c",
	"notification signing secret": "secret-notification-signing-3d7b",
}

const (
	publicURL = "https://barpay.test"
	origin    = "https://store.test"
	notifyURL = "https://store.test/barpay/notifications"
)

// configFile is a config file with the secrets above, in currency cur.
func configFile(t *testing.T, cur money.Currency) *config.Config {
	t.Helper()
	text := `
listen_address = ":8080"
database_path = "barpay.db"
currency = "` + string(cur) + `"
starting_rate = 1200
max_step = 300
min_evaluation_interval = "1m"
capacity = 40
store_app_origin = "` + origin + `"
public_url = "` + publicURL + `"
[keys]
store_api_key = "` + secrets["store API key"] + `"
admin_api_key = "` + secrets["admin API key"] + `"
edge_shared_secret = "` + secrets["edge device's shared secret"] + `"
[paypal]
client_id = "` + secrets["PayPal client ID"] + `"
client_secret = "` + secrets["PayPal client secret"] + `"
[notifications]
url = "` + notifyURL + `"
signing_secret = "` + secrets["notification signing secret"] + `"
[llm]
address = "http://llm.test"
model = "m"
[settings]
floor = 600
ceiling = 2400
goal = 50
`
	cfg, err := config.Parse([]byte(text), config.Options{Currencies: []money.Currency{money.USD, money.JPY}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// storeApp is the store app's notification URL. It acknowledges a notification while ack is set
// and keeps the body of every one it received.
type storeApp struct {
	ack atomic.Bool

	mu       sync.Mutex
	received [][]byte
}

func (s *storeApp) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.received = append(s.received, body)
	s.mu.Unlock()
	status := http.StatusInternalServerError
	if s.ack.Load() {
		status = http.StatusNoContent
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}, nil
}

// event is a notification the store app received, as far as these tests read it.
type event struct {
	Type string `json:"type"`
	Data struct {
		Session struct {
			SessionID string `json:"session_id"`
		} `json:"session"`
	} `json:"data"`
}

// got tells whether the store app received a notification of typ about the session id.
func (s *storeApp) got(t *testing.T, typ, id string) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.received {
		var e event
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == typ && e.Data.Session.SessionID == id {
			return true
		}
	}
	return false
}

// barpay is BarPay started at t0 from configFile, with a fake PayPal and a fake store app.
type barpay struct {
	st     *store.Store
	clk    *clock.Fake
	pp     *paypaltest.Fake
	app    *storeApp
	cfg    *config.Config
	notify *notify.Dispatcher
	sess   session.API
	mem    member.API
	router *httpapi.Router
	keyN   int
}

func start(t *testing.T) *barpay {
	t.Helper()
	b := &barpay{st: storetest.Open(t), clk: clock.NewFake(t0), pp: paypaltest.New(), app: &storeApp{}}
	b.app.ack.Store(true)
	b.build(t, configFile(t, money.USD))
	return b
}

// build assembles BarPay from cfg and b.st as cmd/barpay does, without its loops: the tests run
// what falls due themselves.
func (b *barpay) build(t *testing.T, cfg *config.Config) {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	if _, err := startup.Prepare(ctx, cfg, b.st, session.OpenSessions{}, b.clk); err != nil {
		t.Fatal(err)
	}
	b.cfg = cfg
	b.notify = notify.NewDispatcher(b.st, notify.Options{
		URL: cfg.NotificationURL, Secret: cfg.NotificationSigningSecret, Clock: b.clk, Log: log, Transport: b.app,
	})
	r := httpapi.NewRouter(httpapi.Keys{Store: cfg.StoreAPIKey, Admin: cfg.AdminAPIKey, Edge: cfg.EdgeSharedSecret})
	r.Admin("GET /notifications", notify.ListHandler(b.st, log))
	alert.API{Store: b.st, Log: log}.Register(r)
	httpapi.Pricing{Store: b.st, Clock: b.clk}.Register(r)
	b.sess = session.API{
		Store: b.st, Clock: b.clk, Keys: idempotency.New(b.st, b.clk), Notify: b.notify,
		PayPal: b.pp, Saved: member.Saved{}, Gate: gate.New(),
		ExitTokenRefresh: cfg.ExitTokenRefresh, ReunlockWindow: cfg.ReunlockWindow, MaxStay: cfg.MaxStay, Log: log,
	}
	b.sess.Register(r)
	b.mem = member.API{
		Store: b.st, Clock: b.clk, Notify: b.notify, PublicURL: cfg.PublicURL, StoreAppOrigin: cfg.StoreAppOrigin, PayPal: b.pp,
	}
	b.mem.Register(r)
	signals.API{Store: b.st, Clock: b.clk, Log: log}.Register(r)
	b.router = r
}

// restart stops BarPay and starts it again from the same database with cfg.
func (b *barpay) restart(t *testing.T, cfg *config.Config) {
	t.Helper()
	b.st = storetest.Reopen(t, b.st)
	b.build(t, cfg)
}

// Who a request comes from.
const (
	storeKey = iota
	adminKey
	edgeKey
	browser // no key
)

func (b *barpay) do(t *testing.T, who int, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	switch who {
	case storeKey:
		req.Header.Set("Authorization", "Bearer "+secrets["store API key"])
	case adminKey:
		req.Header.Set("Authorization", "Bearer "+secrets["admin API key"])
		req.Header.Set("BarPay-Staff-Id", "staff_1")
	case edgeKey:
		req.Header.Set("Authorization", "Bearer "+secrets["edge device's shared secret"])
	}
	if method == http.MethodPost && who != edgeKey {
		b.keyN++
		req.Header.Set("Idempotency-Key", "key-"+strconv.Itoa(b.keyN))
	}
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, req)
	return w
}

// want fails the test unless w has status, and decodes its body into out when out is not nil.
func want(t *testing.T, what string, w *httptest.ResponseRecorder, status int, out any) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s: %d %s; want %d", what, w.Code, w.Body, status)
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
}

// checkin checks member in (a guest when empty) and returns the session ID.
func (b *barpay) checkin(t *testing.T, member string) string {
	t.Helper()
	body := `{}`
	if member != "" {
		body = `{"member_id":"` + member + `"}`
	}
	var s struct {
		SessionID string `json:"session_id"`
	}
	want(t, "createCheckin", b.do(t, storeKey, http.MethodPost, "/v1/checkins", body), http.StatusCreated, &s)
	return s.SessionID
}

func (b *barpay) order(t *testing.T, id, orderID string) *httptest.ResponseRecorder {
	t.Helper()
	return b.do(t, storeKey, http.MethodPost, "/v1/sessions/"+id+"/orders",
		`{"order_id":"`+orderID+`","lines":[{"name":"Beer","quantity":2,"unit_price":{"value":650,"currency":"USD"}}]}`)
}

func (b *barpay) requestRegister(t *testing.T, id string) {
	t.Helper()
	want(t, "requestRegister", b.do(t, storeKey, http.MethodPost, "/v1/sessions/"+id+"/register-request", ""), http.StatusOK, nil)
}

// link saves payer's PayPal for member through UC2: the link URL, PayPal's approval, the return
// and the store app's confirmation.
func (b *barpay) link(t *testing.T, member, payer string) {
	t.Helper()
	var link struct {
		LinkURL string `json:"link_url"`
	}
	want(t, "createPaypalLink", b.do(t, storeKey, http.MethodPost, "/v1/members/"+member+"/paypal-link",
		`{"state":"state-0123456789abcdef","consent_version":"consent-v1","return_url":"`+origin+`/settings/paypal"}`),
		http.StatusCreated, &link)
	toPayPal := b.do(t, browser, http.MethodGet, strings.TrimPrefix(link.LinkURL, publicURL), "")
	token := paypaltest.SetupTokenOf(toPayPal.Header().Get("Location"))
	b.pp.Approve(token, payer)
	back := b.do(t, browser, http.MethodGet, strings.TrimPrefix(b.pp.URLs(token).Return, publicURL), "")
	u, err := url.Parse(back.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	want(t, "confirmPaypalLink", b.do(t, storeKey, http.MethodPost, "/v1/members/"+member+"/paypal-link/confirm",
		`{"code":"`+u.Query().Get("code")+`"}`), http.StatusCreated, nil)
}

// linkedIn checks member in, whose PayPal is saved, and returns the session ID once the check of
// the saved PayPal has linked the session (UC1 alternative flow).
func (b *barpay) linkedIn(t *testing.T, member string) string {
	t.Helper()
	id := b.checkin(t, member)
	n, err := b.mem.CheckDue(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("CheckDue linked %d sessions, %v; want 1", n, err)
	}
	return id
}

// scan gets the session's exit token and reads it at the exit gate, returning the unlock ID
// when BarPay unlocks.
func (b *barpay) scan(t *testing.T, id string) (unlock bool, unlockID string) {
	t.Helper()
	var tok struct {
		Token string `json:"token"`
	}
	want(t, "getExitToken", b.do(t, storeKey, http.MethodGet, "/v1/sessions/"+id+"/exit-token", ""), http.StatusOK, &tok)
	var r struct {
		Unlock   bool   `json:"unlock"`
		UnlockID string `json:"unlock_id"`
	}
	want(t, "scan", b.do(t, edgeKey, http.MethodPost, "/v1/edge/exit-scans", `{"token":"`+tok.Token+`"}`), http.StatusOK, &r)
	return r.Unlock, r.UnlockID
}

func (b *barpay) deliverDue(t *testing.T) {
	t.Helper()
	if _, err := b.notify.DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// listedAlert is the spec's Alert as listAlerts returns it.
type listedAlert struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	SessionID *string `json:"session_id"`
	MemberID  *string `json:"member_id"`
}

// adminReads reads every page of listAlerts and listSessions, checks them against the spec, and
// returns the alerts and the raw bodies.
func (b *barpay) adminReads(t *testing.T) ([]listedAlert, [][]byte) {
	t.Helper()
	var alerts []listedAlert
	var bodies [][]byte
	for _, list := range []string{"alerts", "sessions"} {
		q := url.Values{"limit": {"1"}}
		for {
			w := b.do(t, adminKey, http.MethodGet, "/v1/admin/"+list+"?"+q.Encode(), "")
			var page struct {
				Alerts     []json.RawMessage `json:"alerts"`
				Sessions   []json.RawMessage `json:"sessions"`
				NextCursor *string           `json:"next_cursor"`
			}
			want(t, "list "+list, w, http.StatusOK, &page)
			bodies = append(bodies, w.Body.Bytes())
			for _, a := range page.Alerts {
				spectest.Valid(t, "Alert", a)
				var l listedAlert
				if err := json.Unmarshal(a, &l); err != nil {
					t.Fatal(err)
				}
				alerts = append(alerts, l)
			}
			for _, s := range page.Sessions {
				spectest.Valid(t, "Session", s)
			}
			if page.NextCursor == nil {
				break
			}
			q.Set("cursor", *page.NextCursor)
		}
	}
	return alerts, bodies
}

// noSecrets fails the test if any secret appears in bodies.
func noSecrets(t *testing.T, bodies [][]byte) {
	t.Helper()
	for name, v := range secrets {
		for _, body := range bodies {
			if bytes.Contains(body, []byte(v)) {
				t.Errorf("the %s is in an admin response: %s", name, body)
			}
		}
	}
}
