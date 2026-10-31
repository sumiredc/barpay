package pricing

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

func usdSettings() settings.Settings {
	return settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
}

func tx(t *testing.T, st *store.Store, fn func(store.Querier) error) {
	t.Helper()
	if err := st.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// started is a store as after the first start: the settings saved and the starting rate published.
func started(t *testing.T, starting int64) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	tx(t, st, func(q store.Querier) error {
		if err := settings.Save(ctx, q, usdSettings(), t0); err != nil {
			return err
		}
		_, _, err := PublishFirst(ctx, q, usd(starting), usdSettings(), t0)
		return err
	})
	return st
}

func propose(t *testing.T, st *store.Store, proposal int64, at time.Time) Point {
	t.Helper()
	var p Point
	tx(t, st, func(q store.Querier) error {
		var err error
		p, err = PublishProposal(context.Background(), q, usd(proposal), usd(300), at)
		return err
	})
	return p
}

func current(t *testing.T, q store.Querier) Point {
	t.Helper()
	p, err := Current(context.Background(), q, money.USD)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFirstRateHasChangeZero(t *testing.T) {
	st := started(t, 1200)
	if got := current(t, st.Read()); got != (Point{Rate: usd(1200), Change: usd(0), PublishedAt: t0}) {
		t.Errorf("first rate = %+v; want 1200 with change 0 at t0", got)
	}
}

// PublishFirst publishes only when no rate is published in the currency.
func TestPublishFirstKeepsAPublishedRate(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)
	propose(t, st, 1500, t0.Add(time.Minute))
	var published bool
	tx(t, st, func(q store.Querier) error {
		var err error
		_, published, err = PublishFirst(ctx, q, usd(1200), usdSettings(), t0.Add(time.Hour))
		return err
	})
	if got := current(t, st.Read()); published || got.Rate != usd(1500) {
		t.Errorf("after PublishFirst on a published rate: published %v, rate %v; want 1500 kept", published, got.Rate)
	}
}

// The proposal goes through the guardrails before it is stored.
func TestPublishProposalIsGuarded(t *testing.T) {
	st := started(t, 1200)
	if p := propose(t, st, 99_999_999, t0.Add(time.Minute)); p.Rate != usd(1500) || p.Change != usd(300) {
		t.Errorf("published %+v; want 1500 (+300, the max step)", p)
	}
	if p := propose(t, st, -1, t0.Add(2*time.Minute)); p.Rate != usd(1200) || p.Change != usd(-300) {
		t.Errorf("published %+v; want 1200 (-300)", p)
	}
	if got := current(t, st.Read()); got.Rate != usd(1200) {
		t.Errorf("current = %v, want 1200", got.Rate)
	}
}

func TestHistory(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)                       // t0: 1200
	propose(t, st, 1300, t0.Add(10*time.Minute)) // 1300
	propose(t, st, 1400, t0.Add(20*time.Minute)) // 1400
	st = storetest.Reopen(t, st)

	rates := func(since time.Time, limit int) []int64 {
		ps, err := History(ctx, st.Read(), money.USD, since, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, p := range ps {
			out = append(out, p.Rate.Value())
		}
		return out
	}
	cases := []struct {
		name  string
		since time.Time
		limit int
		want  []int64
	}{
		{"before the first rate", t0.Add(-time.Hour), MaxHistory, []int64{1200, 1300, 1400}},
		{"at the first", t0, MaxHistory, []int64{1200, 1300, 1400}},
		{"between: the rate in effect is included", t0.Add(15 * time.Minute), MaxHistory, []int64{1300, 1400}},
		{"exactly at a publication", t0.Add(10 * time.Minute), MaxHistory, []int64{1300, 1400}},
		{"just before a publication", t0.Add(10*time.Minute - 1), MaxHistory, []int64{1200, 1300, 1400}},
		{"after the last", t0.Add(time.Hour), MaxHistory, []int64{1400}},
		{"the most recent within the limit", t0.Add(-time.Hour), 2, []int64{1300, 1400}},
	}
	for _, c := range cases {
		if got := rates(c.since, c.limit); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// After a currency change, the new currency's history starts at its first rate, whatever since is.
func TestHistoryIsOnlyTheCurrentCurrency(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)
	propose(t, st, 1300, t0.Add(time.Minute))
	jpy := settings.Settings{
		Currency: money.JPY, Floor: money.New(600, money.JPY), Ceiling: money.New(2400, money.JPY),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: money.New(3000, money.JPY), Goal: 50,
	}
	at := t0.Add(time.Hour)
	tx(t, st, func(q store.Querier) error {
		if err := settings.Save(ctx, q, jpy, at); err != nil {
			return err
		}
		_, _, err := PublishFirst(ctx, q, money.New(1000, money.JPY), jpy, at)
		return err
	})
	ps, err := History(ctx, st.Read(), money.JPY, t0.Add(-time.Hour), MaxHistory)
	if err != nil {
		t.Fatal(err)
	}
	want := Point{Rate: money.New(1000, money.JPY), Change: money.New(0, money.JPY), PublishedAt: at}
	if len(ps) != 1 || ps[0] != want {
		t.Errorf("JPY history = %+v; want only %+v", ps, want)
	}
	if _, err := Current(ctx, st.Read(), money.USD); !errors.Is(err, ErrNoRate) {
		t.Errorf("Current(USD) after the change: %v, want ErrNoRate", err)
	}
}

// A clock set back does not put a rate before the one published earlier.
func TestPublishedTimesDoNotGoBack(t *testing.T) {
	st := started(t, 1200)
	if p := propose(t, st, 1300, t0.Add(-time.Minute)); p.PublishedAt != t0 {
		t.Errorf("published at %v, want t0 (the previous rate's time)", p.PublishedAt)
	}
}

func TestMaxStepIsKept(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	if _, ok, err := LoadMaxStep(ctx, st.Read()); ok || err != nil {
		t.Fatalf("LoadMaxStep on an empty store = %v, %v", ok, err)
	}
	tx(t, st, func(q store.Querier) error { return SaveMaxStep(ctx, q, usd(300)) })
	tx(t, st, func(q store.Querier) error { return SaveMaxStep(ctx, q, usd(250)) })
	st = storetest.Reopen(t, st)
	if m, ok, err := LoadMaxStep(ctx, st.Read()); !ok || err != nil || m != usd(250) {
		t.Errorf("LoadMaxStep after restart = %v, %v, %v; want 250 USD", m, ok, err)
	}
}

// At is the last rate published at or before the instant: a rate published exactly then counts.
func TestAtIsTheRateInEffect(t *testing.T) {
	st := started(t, 1200)
	propose(t, st, 1500, t0.Add(time.Minute))
	at := func(d time.Duration) (money.Amount, error) {
		p, err := At(context.Background(), st.Read(), money.USD, t0.Add(d))
		return p.Rate, err
	}
	if _, err := at(-time.Nanosecond); !errors.Is(err, ErrNoRate) {
		t.Errorf("before the first rate: err = %v, want ErrNoRate", err)
	}
	for d, want := range map[time.Duration]int64{0: 1200, time.Minute - time.Nanosecond: 1200, time.Minute: 1500, time.Hour: 1500} {
		if got, err := at(d); err != nil || got != usd(want) {
			t.Errorf("at t0+%v: %v, %v; want %d", d, got, err, want)
		}
	}
}
