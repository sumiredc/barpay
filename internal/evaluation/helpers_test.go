package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/signals"
	"github.com/sumiredc/barpay/internal/spectest"
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

	minInterval = time.Minute
	capacity    = 4
)

func usd(v int64) money.Amount { return money.New(v, money.USD) }

// fakeModel answers with answer, which the test sets; it records every call.
type fakeModel struct {
	mu     sync.Mutex
	answer func(ctx context.Context) (string, error)
	calls  [][]llm.Message
}

func (m *fakeModel) Chat(ctx context.Context, msgs []llm.Message) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, msgs)
	answer := m.answer
	m.mu.Unlock()
	return answer(ctx)
}

func (m *fakeModel) set(answer func(ctx context.Context) (string, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer = answer
}

// says makes the model answer text.
func (m *fakeModel) says(text string) {
	m.set(func(context.Context) (string, error) { return text, nil })
}

// proposes makes the model answer the format with rate.
func (m *fakeModel) proposes(rate int64) {
	m.says(fmt.Sprintf(`{"rate_per_hour":%d,"reason":"Busy room","reasoning":"Serves the goal"}`, rate))
}

func (m *fakeModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// lastInput is the user message of the last call, decoded.
func (m *fakeModel) lastInput(t *testing.T) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		t.Fatal("the model was not called")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(m.calls[len(m.calls)-1][1].Content), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// fixture is BarPay after its first start at t0: settings from 600 to 2400, goal 50, and the
// starting rate 1200 published, serving requestRateEvaluation and the signals' operations.
type fixture struct {
	st     *store.Store
	clk    *clock.Fake
	model  *fakeModel
	ev     *Evaluator
	router *httpapi.Router
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	s := settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
	err := st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, s, t0); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, usd(1200), s, t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, clk: clock.NewFake(t0), model: &fakeModel{}}
	f.model.proposes(1200)
	f.build()
	return f
}

// build makes every in-memory part again from f.st, as a restart does.
func (f *fixture) build() {
	log := slog.New(slog.DiscardHandler)
	f.ev = New(Options{
		Store: f.st, Clock: f.clk, Model: f.model, MaxStep: usd(300), MinInterval: minInterval, Capacity: capacity, Log: log,
	})
	r := httpapi.NewRouter(keys)
	API{Evaluator: f.ev}.Register(r)
	signals.API{Store: f.st, Clock: f.clk, Log: log}.Register(r)
	f.router = r
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.st = storetest.Reopen(t, f.st)
	f.build()
}

func (f *fixture) do(method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("BarPay-Staff-Id", "staff_1")
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

type rateEvaluation struct {
	Rate struct {
		RatePerHour httpapi.Money `json:"rate_per_hour"`
		Change      httpapi.Money `json:"change"`
		PublishedAt string        `json:"published_at"`
	} `json:"rate"`
	Evaluated bool    `json:"evaluated"`
	Fallback  *string `json:"fallback"`
}

// request calls requestRateEvaluation and checks the body against the spec.
func (f *fixture) request(t *testing.T) rateEvaluation {
	t.Helper()
	w := f.do(http.MethodPost, "/v1/rate-evaluations", storeAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("requestRateEvaluation: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "RateEvaluation", w.Body.Bytes())
	var r rateEvaluation
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) current(t *testing.T) int64 {
	t.Helper()
	p, err := pricing.Current(context.Background(), f.st.Read(), money.USD)
	if err != nil {
		t.Fatal(err)
	}
	return p.Rate.Value()
}

func (f *fixture) decisions(t *testing.T) []pricing.Decision {
	t.Helper()
	ds, _, err := pricing.ListDecisions(context.Background(), f.st.Read(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func (f *fixture) lastDecision(t *testing.T) pricing.Decision {
	t.Helper()
	ds := f.decisions(t)
	if len(ds) == 0 {
		t.Fatal("no decision recorded")
	}
	return ds[0]
}

// addActive stores n active guest sessions, standing in for check-in.
func (f *fixture) addSessions(t *testing.T, state string, n int) {
	t.Helper()
	ctx := context.Background()
	err := f.st.Tx(ctx, func(tx store.Querier) error {
		for range n {
			var fixed any
			if state != "active" {
				fixed = t0.UnixNano()
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, state, currency, checked_in_at, total_fixed_at)
				VALUES ('ses_' || hex(randomblob(13)), ?, 'USD', ?, ?)`, state, t0.UnixNano(), fixed); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
