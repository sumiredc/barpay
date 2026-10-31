package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var (
	t0   = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	keys = httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")}
)

const storeAuth = "Bearer k-store"

// noAuth as a call's auth sends no Authorization header.
const noAuth = "-"

func usd(v int64) money.Amount { return money.New(v, money.USD) }

// startingSettings: blocks of 10 minutes, a pause of 30 minutes and a cap of $30.00.
func startingSettings() settings.Settings {
	return settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
}

type waker struct{ n atomic.Int64 }

func (w *waker) Wake() { w.n.Add(1) }

// fixture is BarPay after its first start at t0, with the starting rate $12.00 per hour
// published then, serving the session operations.
type fixture struct {
	st     *store.Store
	clk    *clock.Fake
	wake   *waker
	router *httpapi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	err := st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, startingSettings(), t0); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, usd(1200), startingSettings(), t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, clk: clock.NewFake(t0)}
	f.build()
	return f
}

// build makes every in-memory part again from f.st, as a restart does.
func (f *fixture) build() { f.buildWith(f.clk) }

// buildWith is build with another clock.
func (f *fixture) buildWith(clk clock.Clock) {
	f.wake = &waker{}
	r := httpapi.NewRouter(keys)
	API{Store: f.st, Clock: clk, Keys: idempotency.New(f.st, clk), Notify: f.wake}.Register(r)
	f.router = r
}

// restart closes and reopens the store and rebuilds everything from it.
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

func (f *fixture) checkin(key, body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/checkins", key: key, body: body})
}

// checkedIn checks in and returns the session, failing the test unless it started.
func (f *fixture) checkedIn(t *testing.T, member string) View {
	t.Helper()
	body := `{}`
	if member != "" {
		body = `{"member_id":"` + member + `"}`
	}
	w := f.checkin("key-"+newID(), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("check-in: %d %s", w.Code, w.Body)
	}
	return decodeView(t, w.Body.Bytes())
}

func decodeView(t *testing.T, b []byte) View {
	t.Helper()
	var v View
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return v
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) sessions(t *testing.T) int {
	return f.count(t, "SELECT count(*) FROM sessions")
}

func (f *fixture) notifications(t *testing.T) int {
	return f.count(t, "SELECT count(*) FROM notifications")
}

// setState moves a session to state directly in the store, standing in for the operations of
// later use cases; a state other than active gets its total fixed at the fake clock's time.
func (f *fixture) setState(t *testing.T, id string, state State) {
	t.Helper()
	var fixed any
	if state != Active {
		fixed = f.clk.Now().UnixNano()
	}
	err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), "UPDATE sessions SET state = ?, total_fixed_at = ? WHERE id = ?", string(state), fixed, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) (code, field string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Field string `json:"field"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return body.Error.Code, body.Error.Details.Field
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, body %s; want %d", w.Code, w.Body, status)
	}
	if got, _ := errorOf(t, w); got != code {
		t.Errorf("code = %q, want %q", got, code)
	}
}
