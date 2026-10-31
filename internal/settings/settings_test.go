package settings

import (
	"context"
	"errors"
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
