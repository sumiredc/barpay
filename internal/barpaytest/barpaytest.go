// Package barpaytest assembles BarPay from internal/ as cmd/barpay assembles it, with a fake
// PayPal, a fake LLM and a fake clock, for tests that talk to BarPay as its callers do. The Go tests
// of the development tools and the test BarPay that the edge device's tests start both use it, so
// both test against the same BarPay.
package barpaytest

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/evaluation"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/member"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/signals"
	"github.com/sumiredc/barpay/internal/store"
)

// Start is where the fake clock stands when BarPay is assembled.
var Start = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

// The time limits BarPay is assembled with.
const (
	ExitTokenRefresh = 30 * time.Second
	ReunlockWindow   = 2 * time.Minute
	MaxStay          = 12 * time.Hour
)

// fakeModel is the local LLM, proposing $13.00 per hour to every evaluation.
type fakeModel struct{}

func (fakeModel) Chat(context.Context, []llm.Message) (string, error) {
	return `{"rate_per_hour":1300,"reason":"Busy","reasoning":"r"}`, nil
}

// Options are what the caller decides.
type Options struct {
	// Dir is where the store's file is made. The caller makes it and removes it after Close.
	Dir  string
	Keys httpapi.Keys
	// PublicURL and StoreAppOrigin are as in member.API.
	PublicURL, StoreAppOrigin string
}

// BarPay is one assembled BarPay. Handler serves its whole HTTP API; nothing runs in the
// background, so notifications are only stored and time limits act when a request reads them.
type BarPay struct {
	Store   *store.Store
	Clock   *clock.Fake
	PayPal  *paypaltest.Fake
	Handler *httpapi.Router

	ev *evaluation.Evaluator
}

// New assembles BarPay over a new store in o.Dir, with its settings saved and its first rate
// published at Start. Its logs are discarded, so they hold no key or exit token.
func New(ctx context.Context, o Options) (*BarPay, error) {
	st, err := store.Open(ctx, filepath.Join(o.Dir, "barpay.db"))
	if err != nil {
		return nil, err
	}
	b := &BarPay{Store: st, Clock: clock.NewFake(Start), PayPal: paypaltest.New()}
	usd := func(v int64) money.Amount { return money.New(v, money.USD) }
	s := settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
	if err := st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, s, Start); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, usd(1200), s, Start)
		return err
	}); err != nil {
		_ = st.Close()
		return nil, err
	}

	log := slog.New(slog.DiscardHandler)
	notifications := notify.NewDispatcher(st, notify.Options{
		URL: "https://store.test/notify", Secret: secret.New("sig"), Clock: b.Clock, Log: log,
	})
	b.Handler = httpapi.NewRouter(o.Keys)
	b.Handler.Admin("GET /notifications", notify.ListHandler(st, log))
	alert.API{Store: st, Log: log}.Register(b.Handler)
	httpapi.Pricing{Store: st, Clock: b.Clock}.Register(b.Handler)
	session.API{
		Store: st, Clock: b.Clock, Keys: idempotency.New(st, b.Clock), Notify: notifications,
		PayPal: b.PayPal, Saved: member.Saved{}, Gate: gate.New(),
		ExitTokenRefresh: ExitTokenRefresh, ReunlockWindow: ReunlockWindow, MaxStay: MaxStay, Log: log,
	}.Register(b.Handler)
	member.API{
		Store: st, Clock: b.Clock, Notify: notifications, PublicURL: o.PublicURL, StoreAppOrigin: o.StoreAppOrigin, PayPal: b.PayPal,
	}.Register(b.Handler)
	signals.API{Store: st, Clock: b.Clock, Log: log}.Register(b.Handler)
	b.ev = evaluation.New(evaluation.Options{
		Store: st, Clock: b.Clock, Model: fakeModel{}, MaxStep: usd(300),
		MinInterval: time.Minute, Capacity: 40, Log: log,
	})
	evaluation.API{Evaluator: b.ev}.Register(b.Handler)
	return b, nil
}

// Close waits for the evaluations still running and closes the store. Call it after the HTTP
// server in front of Handler has stopped.
func (b *BarPay) Close() error {
	b.ev.Wait()
	return b.Store.Close()
}
