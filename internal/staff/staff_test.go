package staff_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var at = time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC)

func usdSettings(goal int) settings.Settings {
	usd := func(v int64) money.Amount { return money.New(v, money.USD) }
	return settings.Settings{Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: goal}
}

func TestStaffIsReadWithTheChangeAfterRestart(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	change := staff.Change{Staff: "staff_7", Operation: "updateSettings", Subject: "settings", At: at}
	err := staff.Tx(ctx, st, change, func(tx store.Querier) error {
		return settings.Save(ctx, tx, usdSettings(80), at)
	})
	if err != nil {
		t.Fatal(err)
	}

	st = storetest.Reopen(t, st)
	s, ok, err := settings.Load(ctx, st.Read())
	if err != nil || !ok || s.Goal != 80 {
		t.Fatalf("settings after restart = %+v, %v, %v; want the change", s, ok, err)
	}
	actions, err := staff.Actions(ctx, st.Read(), "settings")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Change != change {
		t.Errorf("actions after restart = %+v; want exactly %+v", actions, change)
	}
}

func TestFailedChangeRecordsNoStaff(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	boom := errors.New("boom")
	err := staff.Tx(ctx, st, staff.Change{Staff: "staff_7", Operation: "updateSettings", Subject: "settings", At: at},
		func(tx store.Querier) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the change's error", err)
	}
	if actions, _ := staff.Actions(ctx, st.Read(), "settings"); len(actions) != 0 {
		t.Errorf("actions = %+v; want none for a change that failed", actions)
	}
}

func TestChangeWithoutValidStaffIsRefused(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	ran := false
	for _, id := range []staff.ID{"", "a b"} {
		err := staff.Tx(ctx, st, staff.Change{Staff: id, Operation: "updateSettings", Subject: "settings", At: at},
			func(store.Querier) error { ran = true; return nil })
		if !errors.Is(err, staff.ErrInvalidID) {
			t.Errorf("staff %q: err = %v, want ErrInvalidID", id, err)
		}
	}
	if ran {
		t.Error("the change ran without a valid staff ID")
	}
}

func TestParseID(t *testing.T) {
	if _, err := staff.ParseID("staff_1-A"); err != nil {
		t.Errorf("valid ID: %v", err)
	}
	for _, bad := range []string{"", "a b", "ä", "x@y"} {
		if _, err := staff.ParseID(bad); !errors.Is(err, staff.ErrInvalidID) {
			t.Errorf("ParseID(%q) err = %v, want ErrInvalidID", bad, err)
		}
	}
}
