package signals

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
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
)

type fixture struct {
	st     *store.Store
	clk    *clock.Fake
	router *httpapi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{st: storetest.Open(t), clk: clock.NewFake(t0)}
	f.build()
	return f
}

func (f *fixture) build() {
	r := httpapi.NewRouter(keys)
	API{Store: f.st, Clock: f.clk, Log: slog.New(slog.DiscardHandler)}.Register(r)
	f.router = r
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.st = storetest.Reopen(t, f.st)
	f.build()
}

// do sends a request; staffID "" sends no BarPay-Staff-Id.
func (f *fixture) do(method, path, auth, staffID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if staffID != "" {
		req.Header.Set("BarPay-Staff-Id", staffID)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.st.Read().QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
