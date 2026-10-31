package startup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/config"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

const file = `
listen_address = ":8080"
database_path = "barpay.db"
currency = "CURRENCY"
starting_rate = 1200
max_step = 300
min_evaluation_interval = "1m"
capacity = 40
store_app_origin = "https://store.example"
[keys]
store_api_key = "k1"
admin_api_key = "k2"
edge_shared_secret = "k3"
[paypal]
client_id = "id"
client_secret = "s"
[notifications]
url = "https://store.example/notify"
signing_secret = "sig"
[llm]
address = "http://llm.example"
model = "m"
[settings]
floor = 600
ceiling = 2400
goal = 50
`

// Both currencies, so the switch can be tested while P0 refuses JPY.
var both = config.Options{Currencies: []money.Currency{money.USD, money.JPY}}

func load(t *testing.T, cur money.Currency) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(strings.Replace(file, "CURRENCY", string(cur), 1)), both)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type openSessions bool

func (o openSessions) AnyOpen(context.Context, store.Querier) (bool, error) { return bool(o), nil }

var clk = clock.NewFake(time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC))

func prepare(t *testing.T, cfg *config.Config, st *store.Store, open bool) State {
	t.Helper()
	s, err := Prepare(context.Background(), cfg, st, openSessions(open), clk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func alertsOfKind(t *testing.T, st *store.Store, k alert.Kind) int {
	t.Helper()
	var n int
	if err := st.Read().QueryRowContext(context.Background(), "SELECT count(*) FROM alerts WHERE kind = ?", string(k)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func saveSettings(t *testing.T, st *store.Store, s settings.Settings) {
	t.Helper()
	err := st.Tx(context.Background(), func(tx store.Querier) error { return settings.Save(context.Background(), tx, s, clk.Now()) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestFirstStartUsesStartingSettings(t *testing.T) {
	cfg := load(t, money.USD)
	st := storetest.Open(t)
	if got := prepare(t, cfg, st, false); got.Settings != cfg.StartingSettings || got.CurrencyChangePending {
		t.Errorf("first start: %+v; want the starting settings", got)
	}
	st = storetest.Reopen(t, st)
	if got, ok, _ := settings.Load(context.Background(), st.Read()); !ok || got != cfg.StartingSettings {
		t.Errorf("after restart the stored settings are %+v (%v); want the starting settings", got, ok)
	}
}

func TestSavedSettingsWinOverStartingValues(t *testing.T) {
	cfg := load(t, money.USD)
	st := storetest.Open(t)
	prepare(t, cfg, st, false)
	changed := cfg.StartingSettings
	changed.Goal, changed.Ceiling = 90, money.New(3000, money.USD)
	saveSettings(t, st, changed)

	st = storetest.Reopen(t, st)
	if got := prepare(t, cfg, st, false); got.Settings != changed {
		t.Errorf("after restart settings = %+v; want the saved %+v", got.Settings, changed)
	}
}

func TestCurrencyChangeWaitsForNoOpenSession(t *testing.T) {
	st := storetest.Open(t)
	usdCfg := load(t, money.USD)
	prepare(t, usdCfg, st, false)
	changed := usdCfg.StartingSettings
	changed.Goal = 10
	saveSettings(t, st, changed)

	// A session is open: start with USD and the saved settings, and raise the alert.
	jpyCfg := load(t, money.JPY)
	st = storetest.Reopen(t, st)
	got := prepare(t, jpyCfg, st, true)
	if got.Currency != money.USD || got.Settings != changed || !got.CurrencyChangePending {
		t.Errorf("with an open session: %+v; want USD with the saved settings, change pending", got)
	}
	if n := alertsOfKind(t, st, alert.CurrencyChangePending); n != 1 {
		t.Errorf("%d currency_change_pending alerts, want 1", n)
	}

	// No session open at the next restart: JPY, and the starting settings again.
	st = storetest.Reopen(t, st)
	got = prepare(t, jpyCfg, st, false)
	if got.Settings != jpyCfg.StartingSettings || got.Currency != money.JPY || got.CurrencyChangePending {
		t.Errorf("with no open session: %+v; want JPY with the starting settings", got)
	}
	st = storetest.Reopen(t, st)
	if s, _, _ := settings.Load(context.Background(), st.Read()); s != jpyCfg.StartingSettings {
		t.Errorf("stored settings after the switch = %+v; want the JPY starting settings", s)
	}
	if n := alertsOfKind(t, st, alert.CurrencyChangePending); n != 1 {
		t.Errorf("%d currency_change_pending alerts after the switch, want still 1", n)
	}
}

func TestSameCurrencyRaisesNothing(t *testing.T) {
	st := storetest.Open(t)
	cfg := load(t, money.USD)
	prepare(t, cfg, st, true)
	prepare(t, cfg, st, true)
	if n := alertsOfKind(t, st, alert.CurrencyChangePending); n != 0 {
		t.Errorf("%d alerts without a currency change, want 0", n)
	}
}

// Each start while the change waits raises the alert again.
func TestPendingChangeRaisesAnAlertOnEveryStart(t *testing.T) {
	st := storetest.Open(t)
	prepare(t, load(t, money.USD), st, false)
	jpyCfg := load(t, money.JPY)
	for range 2 {
		st = storetest.Reopen(t, st)
		prepare(t, jpyCfg, st, true)
	}
	if n := alertsOfKind(t, st, alert.CurrencyChangePending); n != 2 {
		t.Errorf("%d currency_change_pending alerts after 2 pending starts, want 2", n)
	}
}

func rateNow(t *testing.T, st *store.Store, cur money.Currency) pricing.Point {
	t.Helper()
	p, err := pricing.Current(context.Background(), st.Read(), cur)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func publishProposal(t *testing.T, st *store.Store, proposal money.Amount, maxStep money.Amount) {
	t.Helper()
	err := st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := pricing.PublishProposal(context.Background(), tx, proposal, maxStep, clk.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The PRD's Pricing model, First rate: the starting rate with change 0 on the first start; the
// last published rate on every other start.
func TestFirstStartPublishesTheStartingRate(t *testing.T) {
	cfg := load(t, money.USD)
	st := storetest.Open(t)
	got := prepare(t, cfg, st, false)
	if got.MaxStep != money.New(300, money.USD) {
		t.Errorf("max step = %v, want the config file's 300 USD", got.MaxStep)
	}
	want := pricing.Point{Rate: money.New(1200, money.USD), Change: money.New(0, money.USD), PublishedAt: clk.Now()}
	if p := rateNow(t, st, money.USD); p != want {
		t.Errorf("rate after the first start = %+v, want %+v", p, want)
	}

	// A proposal far beyond the max step moves the rate by exactly the config file's max step.
	publishProposal(t, st, money.New(99_999, money.USD), got.MaxStep)
	st = storetest.Reopen(t, st)
	prepare(t, cfg, st, false)
	if p := rateNow(t, st, money.USD); p.Rate != money.New(1500, money.USD) || p.Change != money.New(300, money.USD) {
		t.Errorf("rate after a restart = %+v; want the last published 1500 (+300)", p)
	}
	h, err := pricing.History(context.Background(), st.Read(), money.USD, clk.Now().Add(-time.Hour), pricing.MaxHistory)
	if err != nil || len(h) != 2 {
		t.Errorf("history after a restart = %+v, %v; want the two rates, no starting rate added", h, err)
	}
}

// After the currency change, the starting rate is published again in the new currency with
// change 0, and the history holds only the new currency's rates.
func TestCurrencyChangePublishesTheStartingRate(t *testing.T) {
	st := storetest.Open(t)
	before := clk.Now().Add(-time.Hour)
	prepare(t, load(t, money.USD), st, false)
	publishProposal(t, st, money.New(1500, money.USD), money.New(300, money.USD))

	jpyCfg := load(t, money.JPY)
	st = storetest.Reopen(t, st)
	got := prepare(t, jpyCfg, st, false)
	want := pricing.Point{Rate: money.New(1200, money.JPY), Change: money.New(0, money.JPY), PublishedAt: clk.Now()}
	if p := rateNow(t, st, money.JPY); p != want || got.MaxStep != money.New(300, money.JPY) {
		t.Errorf("after the change: rate %+v, max step %v; want %+v and 300 JPY", p, got.MaxStep, want)
	}
	h, err := pricing.History(context.Background(), st.Read(), money.JPY, before, pricing.MaxHistory)
	if err != nil || len(h) != 1 || h[0] != want {
		t.Errorf("history since before the change = %+v, %v; want only %+v", h, err, want)
	}
}

// While the change waits, the previous currency's rate and max step stay.
func TestPendingCurrencyChangeKeepsRateAndMaxStep(t *testing.T) {
	st := storetest.Open(t)
	prepare(t, load(t, money.USD), st, false)
	publishProposal(t, st, money.New(1500, money.USD), money.New(300, money.USD))

	jpyCfg := load(t, money.JPY)
	jpyCfg.MaxStep = money.New(50, money.JPY)
	st = storetest.Reopen(t, st)
	got := prepare(t, jpyCfg, st, true)
	if got.MaxStep != money.New(300, money.USD) {
		t.Errorf("max step while pending = %v, want the previous 300 USD", got.MaxStep)
	}
	if p := rateNow(t, st, money.USD); p.Rate != money.New(1500, money.USD) {
		t.Errorf("rate while pending = %+v, want the last USD rate 1500", p)
	}
	if _, err := pricing.Current(context.Background(), st.Read(), money.JPY); err == nil {
		t.Error("a JPY rate was published while the change is pending")
	}
}
