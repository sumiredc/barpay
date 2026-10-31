package pricing

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

type decision struct {
	trigger     Trigger
	staffID     string
	rate        int64
	explanation string
	decidedAt   time.Time
}

func decisions(t *testing.T, q store.Querier) []decision {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `SELECT d.trigger, d.staff_id, r.rate, d.explanation, d.decided_at
		FROM decisions d JOIN rates r ON r.seq = d.rate_seq ORDER BY d.seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []decision
	for rows.Next() {
		var d decision
		var at int64
		if err := rows.Scan(&d.trigger, &d.staffID, &d.rate, &d.explanation, &at); err != nil {
			t.Fatal(err)
		}
		d.decidedAt = time.Unix(0, at).UTC()
		out = append(out, d)
	}
	return out
}

func countRates(t *testing.T, q store.Querier) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(context.Background(), "SELECT count(*) FROM rates").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func withRange(floor, ceiling int64) settings.Settings {
	s := usdSettings()
	s.Floor, s.Ceiling = usd(floor), usd(ceiling)
	return s
}

func update(t *testing.T, st *store.Store, s settings.Settings, at time.Time) error {
	t.Helper()
	_, err := UpdateSettings(context.Background(), st, "staff-7", s, at)
	return err
}

// A new floor above the rate moves it up at once, beyond the max step, and the decision log
// records it with the staff member and an explanation of the change actually published.
func TestFloorAboveTheRateMovesIt(t *testing.T) {
	st := started(t, 700)
	at := t0.Add(time.Hour)
	if err := update(t, st, withRange(1500, 2400), at); err != nil {
		t.Fatal(err)
	}
	st = storetest.Reopen(t, st)
	if p := current(t, st.Read()); p != (Point{Rate: usd(1500), Change: usd(800), PublishedAt: at}) {
		t.Errorf("rate = %+v; want 1500 (+800, beyond the 300 max step) at the change", p)
	}
	want := []decision{{TriggerRangeChange, "staff-7", 1500, "Floor raised to $15.00/h: rate up $8.00 to $15.00/h.", at}}
	if got := decisions(t, st.Read()); len(got) != 1 || got[0] != want[0] {
		t.Errorf("decisions = %+v, want %+v", got, want)
	}
}

func TestCeilingBelowTheRateMovesIt(t *testing.T) {
	st := started(t, 2400)
	if err := update(t, st, withRange(600, 1000), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p := current(t, st.Read()); p.Rate != usd(1000) || p.Change != usd(-1400) {
		t.Errorf("rate = %+v; want 1000 (-1400)", p)
	}
	got := decisions(t, st.Read())
	if len(got) != 1 || got[0].explanation != "Ceiling lowered to $10.00/h: rate down $14.00 to $10.00/h." || got[0].staffID != "staff-7" {
		t.Errorf("decisions = %+v", got)
	}
}

// A change that leaves the rate inside the new range adds no rate and no decision, including a
// rate exactly on the new bound.
func TestRateInsideTheNewRangeStays(t *testing.T) {
	for _, r := range [][2]int64{{1000, 1500}, {1200, 1500}, {600, 1200}, {5, 100_000_000}} {
		st := started(t, 1200)
		if err := update(t, st, withRange(r[0], r[1]), t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, d := countRates(t, st.Read()), decisions(t, st.Read()); n != 1 || len(d) != 0 {
			t.Errorf("range %v: %d rates, decisions %+v; want the first rate only and no decision", r, n, d)
		}
	}
}

// Valid settings are saved with the staff member and survive a restart.
func TestUpdateSettingsIsSavedWithTheStaffMember(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)
	s := usdSettings()
	s.BlockMinutes, s.PauseMinutes, s.CapPerVisit, s.Goal = 15, 0, usd(0), 90
	at := t0.Add(time.Hour)
	got, err := UpdateSettings(ctx, st, "staff-7", s, at)
	if err != nil || got != s {
		t.Fatalf("UpdateSettings = %+v, %v; want %+v", got, err, s)
	}
	st = storetest.Reopen(t, st)
	if saved, _, _ := settings.Load(ctx, st.Read()); saved != s {
		t.Errorf("saved after restart = %+v, want %+v", saved, s)
	}
	actions, err := staff.Actions(ctx, st.Read(), "settings")
	if err != nil || len(actions) != 1 || actions[0].Staff != "staff-7" || actions[0].Operation != "updateSettings" {
		t.Errorf("staff actions = %+v, %v; want one updateSettings by staff-7", actions, err)
	}
}

// Invalid settings change nothing: not the settings, the rate, the decision log or the staff record.
func TestInvalidSettingsChangeNothing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]settings.Settings{
		"floor off the step": withRange(1501, 2400),
		"floor not below":    withRange(2400, 2400),
		"floor 0":            withRange(0, 2400),
		"ceiling in another": func() settings.Settings { s := withRange(1500, 2400); s.Ceiling = money.New(2400, money.JPY); return s }(),
		"cap in another": func() settings.Settings {
			s := withRange(1500, 2400)
			s.CapPerVisit = money.New(1, money.JPY)
			return s
		}(),
		"block 0":        func() settings.Settings { s := withRange(1500, 2400); s.BlockMinutes = 0; return s }(),
		"pause negative": func() settings.Settings { s := withRange(1500, 2400); s.PauseMinutes = -1; return s }(),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			st := started(t, 700)
			var es settings.Errors
			if err := update(t, st, s, t0.Add(time.Hour)); !errors.As(err, &es) {
				t.Fatalf("UpdateSettings = %v, want settings.Errors", err)
			}
			saved, _, _ := settings.Load(ctx, st.Read())
			actions, _ := staff.Actions(ctx, st.Read(), "settings")
			if saved != usdSettings() || countRates(t, st.Read()) != 1 || len(decisions(t, st.Read())) != 0 || len(actions) != 0 {
				t.Errorf("something changed: settings %+v, %d rates, %d staff actions", saved, countRates(t, st.Read()), len(actions))
			}
		})
	}
}

// Range changes and guarded publications at the same time: every published rate is inside the
// range in effect when it was written, and moved at most the max step from the rate before it,
// except a range change, which the decision log records.
func TestConcurrentRangeChangesAndPublications(t *testing.T) {
	ctx := context.Background()
	st := started(t, 1200)
	const maxStep = 300
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 7))
			for range 25 {
				var err error
				if g%2 == 0 {
					floor := 5 * (1 + r.Int64N(400))
					_, err = UpdateSettings(ctx, st, "staff-1", withRange(floor, floor+5*(1+r.Int64N(400))), t0)
				} else {
					err = st.Tx(ctx, func(tx store.Querier) error {
						_, err := PublishProposal(ctx, tx, usd(r.Int64N(5000)-1000), usd(maxStep), t0)
						return err
					})
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()

	rows, err := st.Read().QueryContext(ctx, `SELECT r.seq, r.rate, r.change, s.floor, s.ceiling,
			EXISTS (SELECT 1 FROM decisions d WHERE d.rate_seq = r.seq AND d.trigger = 'range_change')
		FROM rates r JOIN settings_changes s ON s.seq = r.settings_seq ORDER BY r.seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev int64
	n, ranged := 0, 0
	for rows.Next() {
		var seq, rate, change, floor, ceiling int64
		var rangeChange bool
		if err := rows.Scan(&seq, &rate, &change, &floor, &ceiling, &rangeChange); err != nil {
			t.Fatal(err)
		}
		if rate < floor || rate > ceiling || rate%5 != 0 {
			t.Errorf("rate %d: %d is outside %d–%d in effect when it was written", seq, rate, floor, ceiling)
		}
		if n > 0 {
			if change != rate-prev {
				t.Errorf("rate %d: change %d, but it moved %d from the rate before it", seq, change, rate-prev)
			}
			if !rangeChange && abs(rate-prev) > maxStep {
				t.Errorf("rate %d: moved %d, beyond the max step, without a range change", seq, rate-prev)
			}
		}
		if rangeChange {
			ranged++
		}
		prev = rate
		n++
	}
	if n < 50 || ranged == 0 {
		t.Errorf("%d rates, %d range changes; the test did not exercise both paths", n, ranged)
	}
}

func TestFormat(t *testing.T) {
	for a, want := range map[money.Amount]string{
		usd(0): "$0.00", usd(5): "$0.05", usd(1250): "$12.50", usd(123456789): "$1,234,567.89",
		money.New(1200, money.JPY): "¥1,200", money.New(100, money.JPY): "¥100",
	} {
		if got := format(a); got != want {
			t.Errorf("format(%v) = %q, want %q", a, got, want)
		}
	}
}
