package settings

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

func usd(v int64) money.Amount { return money.New(v, money.USD) }

func valid() Settings {
	return Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
}

func TestValidSettingsPass(t *testing.T) {
	ok := []func(*Settings){
		func(*Settings) {},
		func(s *Settings) { s.Floor, s.Ceiling = usd(5), usd(10) },
		func(s *Settings) { s.BlockMinutes = 1 },
		func(s *Settings) { s.BlockMinutes = 240 },
		func(s *Settings) { s.PauseMinutes = 0 },
		func(s *Settings) { s.PauseMinutes = 240 },
		func(s *Settings) { s.CapPerVisit = usd(0) },
		func(s *Settings) { s.Goal = 0 },
		func(s *Settings) { s.Goal = 100 },
		func(s *Settings) { s.Ceiling = usd(100_000_000) },
	}
	for i, mod := range ok {
		s := valid()
		mod(&s)
		if err := s.Validate(); err != nil {
			t.Errorf("case %d: Validate() = %v, want nil", i, err)
		}
	}
}

// Each broken condition is reported under the spec's property name.
func TestEachConditionIsChecked(t *testing.T) {
	cases := []struct {
		name  string
		mod   func(*Settings)
		field string
	}{
		{"floor equal to ceiling", func(s *Settings) { s.Floor = s.Ceiling }, "floor"},
		{"floor above ceiling", func(s *Settings) { s.Floor, s.Ceiling = usd(2400), usd(600) }, "floor"},
		{"floor 0", func(s *Settings) { s.Floor = usd(0) }, "floor"},
		{"ceiling 0", func(s *Settings) { s.Floor, s.Ceiling = usd(0), usd(0) }, "ceiling"},
		{"ceiling above the spec's maximum", func(s *Settings) { s.Ceiling = usd(100_000_001) }, "ceiling"},
		{"floor off the rate step", func(s *Settings) { s.Floor = usd(601) }, "floor"},
		{"floor 1", func(s *Settings) { s.Floor = usd(1) }, "floor"},
		{"ceiling off the rate step", func(s *Settings) { s.Ceiling = usd(2399) }, "ceiling"},
		{"floor in another currency", func(s *Settings) { s.Floor = money.New(600, money.JPY) }, "floor"},
		{"block 0", func(s *Settings) { s.BlockMinutes = 0 }, "block_minutes"},
		{"block above 240", func(s *Settings) { s.BlockMinutes = 241 }, "block_minutes"},
		{"pause negative", func(s *Settings) { s.PauseMinutes = -1 }, "pause_minutes"},
		{"pause above 240", func(s *Settings) { s.PauseMinutes = 241 }, "pause_minutes"},
		{"cap negative", func(s *Settings) { s.CapPerVisit = usd(-1) }, "cap_per_visit"},
		{"cap in another currency", func(s *Settings) { s.CapPerVisit = money.New(3000, money.JPY) }, "cap_per_visit"},
		{"goal negative", func(s *Settings) { s.Goal = -1 }, "goal"},
		{"goal above 100", func(s *Settings) { s.Goal = 101 }, "goal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := valid()
			c.mod(&s)
			var es Errors
			if err := s.Validate(); !errors.As(err, &es) {
				t.Fatalf("Validate() = %v, want Errors", err)
			}
			found := false
			for _, e := range es {
				found = found || e.Field == c.field
			}
			if !found {
				t.Errorf("Validate() = %v, want an error for %s", es, c.field)
			}
		})
	}
}

func TestSavedSettingsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	if _, ok, err := Load(ctx, s.Read()); err != nil || ok {
		t.Fatalf("Load on an empty store = ok %v, err %v; want nothing saved", ok, err)
	}
	want := valid()
	want.Goal = 80
	if err := s.Tx(ctx, func(tx store.Querier) error { return Save(ctx, tx, want, time.Unix(0, 0)) }); err != nil {
		t.Fatal(err)
	}

	s = storetest.Reopen(t, s)
	got, ok, err := Load(ctx, s.Read())
	if err != nil || !ok || got != want {
		t.Errorf("after restart Load = %+v, %v, %v; want %+v", got, ok, err, want)
	}
}

func TestInvalidSettingsAreNotSaved(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	bad := valid()
	bad.BlockMinutes = 0
	err := s.Tx(ctx, func(tx store.Querier) error { return Save(ctx, tx, bad, time.Unix(0, 0)) })
	if err == nil {
		t.Fatal("Save of invalid settings succeeded")
	}
	if _, ok, _ := Load(ctx, s.Read()); ok {
		t.Error("invalid settings were saved")
	}
}

func save(t *testing.T, st *store.Store, s Settings, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx store.Querier) error { return Save(ctx, tx, s, at) }); err != nil {
		t.Fatal(err)
	}
}

// Every save is kept with its time, so the settings in effect at any past instant can be read
// after a restart (the PRD's Pricing model, Settings changes; Guarantees, Durability).
func TestHistoryKeepsEveryChange(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	t0 := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	a, b, c := valid(), valid(), valid()
	b.BlockMinutes, c.BlockMinutes = 15, 20
	save(t, st, a, t0)
	save(t, st, b, t0.Add(time.Hour))
	save(t, st, c, t0.Add(2*time.Hour))
	st = storetest.Reopen(t, st)

	cases := []struct {
		since time.Time
		want  []int // block minutes, oldest first
	}{
		{t0.Add(-time.Minute), []int{10, 15, 20}}, // before the first change: every change
		{t0, []int{10, 15, 20}},
		{t0.Add(time.Hour - 1), []int{10, 15, 20}},
		{t0.Add(time.Hour), []int{15, 20}}, // a change at since is in effect at since
		{t0.Add(3 * time.Hour), []int{20}},
	}
	for _, tc := range cases {
		h, err := History(ctx, st.Read(), tc.since)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, ch := range h {
			got = append(got, ch.BlockMinutes)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("History(%v) = %v, want %v", tc.since.Sub(t0), got, tc.want)
		}
	}
	h, _ := History(ctx, st.Read(), t0)
	if h[1].At != t0.Add(time.Hour) || h[1].Settings != b {
		t.Errorf("second change = %+v, want %+v at +1h", h[1], b)
	}
}

// A clock set back does not put a change before the one saved earlier.
func TestHistoryTimesDoNotGoBack(t *testing.T) {
	st := storetest.Open(t)
	t0 := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	save(t, st, valid(), t0)
	save(t, st, valid(), t0.Add(-time.Minute))
	h, err := History(context.Background(), st.Read(), t0.Add(-time.Hour))
	if err != nil || len(h) != 2 || h[1].At.Before(h[0].At) {
		t.Errorf("History = %+v, %v; want two changes in time order", h, err)
	}
}

func TestInvalidSettingsAddNoChange(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	bad := valid()
	bad.Floor = usd(601)
	_ = st.Tx(ctx, func(tx store.Querier) error { return Save(ctx, tx, bad, time.Unix(0, 0)) })
	if h, _ := History(ctx, st.Read(), time.Unix(0, 0)); len(h) != 0 {
		t.Errorf("History = %+v after an invalid save, want none", h)
	}
}
