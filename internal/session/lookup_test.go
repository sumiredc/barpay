package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

func (f *fixture) getSession(id string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodGet, path: "/v1/sessions/" + id})
}

func (f *fixture) looked(t *testing.T, id string) View {
	t.Helper()
	w := f.getSession(id)
	if w.Code != http.StatusOK {
		t.Fatalf("getSession: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	return decodeView(t, w.Body.Bytes())
}

func (f *fixture) propose(t *testing.T, rate int64, at time.Time) {
	t.Helper()
	err := f.st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := pricing.PublishProposal(context.Background(), tx, usd(rate), usd(300), at)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// chargeAt is pricing's computation for the session, independent of the lookup.
func (f *fixture) chargeAt(t *testing.T, id string, at time.Time) pricing.CoverCharge {
	t.Helper()
	var cc pricing.CoverCharge
	err := f.st.View(context.Background(), func(q store.Querier) error {
		s, err := get(context.Background(), q, id)
		if err != nil {
			return err
		}
		cc, err = pricing.ChargeAt(context.Background(), q, money.USD,
			pricing.Visit{CheckedInAt: s.CheckedInAt, TotalFixedAt: s.TotalFixedAt}, at)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

// UC3 main flow and the PRD's Guarantees, Transparency: the lookup's cover charge is pricing's
// at the instant of the lookup, its rate is the one published now, and the total adds them up.
func TestGetSessionFollowsPricingAtTheLookup(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.propose(t, 1500, t0.Add(15*time.Minute))
	f.propose(t, 1800, t0.Add(20*time.Minute)) // exactly at the end of the second block
	for _, d := range []time.Duration{0, 10*time.Minute - 1, 10 * time.Minute, 20 * time.Minute, 25 * time.Minute, 47 * time.Minute} {
		f.clk.Set(t0.Add(d))
		got := f.looked(t, v.SessionID)
		want := f.chargeAt(t, v.SessionID, t0.Add(d))
		rate, err := pricing.Current(context.Background(), f.st.Read(), money.USD)
		if err != nil {
			t.Fatal(err)
		}
		if got.CoverCharge.Value != want.Amount.Value() || got.CoverChargeStatus != want.Status ||
			got.Total.Value != got.OrdersTotal.Value+got.CoverCharge.Value || got.RatePerHour.Value != rate.Rate.Value() {
			t.Errorf("at t0+%v: %+v; want cover charge %v (%s), rate %v", d, got, want.Amount, want.Status, rate.Rate)
		}
	}
	// $12.00 for the first block, then $18.00 for the second, ending as $18.00 was published,
	// then 2 more blocks at $18.00.
	if got := f.looked(t, v.SessionID).CoverCharge.Value; got != 200+300+300+300 {
		t.Errorf("cover charge after 4 blocks = %d, want 1100", got)
	}
}

// The cap shows as capped once reached, and later blocks add nothing.
func TestGetSessionShowsTheCap(t *testing.T) {
	f := newFixture(t)
	capped := startingSettings()
	capped.CapPerVisit = usd(300)
	err := f.st.Tx(context.Background(), func(tx store.Querier) error { return settings.Save(context.Background(), tx, capped, t0) })
	if err != nil {
		t.Fatal(err)
	}
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(10 * time.Minute))
	if got := f.looked(t, v.SessionID); got.CoverChargeStatus != "running" || got.CoverCharge.Value != 200 {
		t.Errorf("after one block: %s %d; want running 200", got.CoverChargeStatus, got.CoverCharge.Value)
	}
	f.clk.Set(t0.Add(20 * time.Minute))
	if got := f.looked(t, v.SessionID); got.CoverChargeStatus != "capped" || got.CoverCharge.Value != 300 {
		t.Errorf("after the cap: %s %d; want capped 300", got.CoverChargeStatus, got.CoverCharge.Value)
	}
	f.clk.Set(t0.Add(time.Hour))
	if got := f.looked(t, v.SessionID); got.CoverCharge.Value != 300 || got.Total.Value != 300 {
		t.Errorf("later: cover %d, total %d; want 300", got.CoverCharge.Value, got.Total.Value)
	}
}

// A session whose total is fixed shows the values at that instant, however much time passes
// and whatever rate is published afterwards.
func TestGetSessionOfAFixedTotalDoesNotChange(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	f.propose(t, 1500, t0.Add(5*time.Minute))
	f.clk.Set(t0.Add(25 * time.Minute))
	f.setState(t, v.SessionID, CheckingOut)
	fixed := f.looked(t, v.SessionID)
	if fixed.CoverChargeStatus != "stopped" || fixed.TotalFixedAt != "2026-10-31T18:25:00Z" ||
		fixed.CoverCharge.Value != 250+250 || fixed.RatePerHour.Value != 1500 {
		t.Errorf("when fixed: %+v", fixed)
	}
	body := f.getSession(v.SessionID).Body.String()
	f.propose(t, 1800, t0.Add(30*time.Minute))
	for _, d := range []time.Duration{30 * time.Minute, 3 * time.Hour} {
		f.clk.Set(t0.Add(d))
		if got := f.getSession(v.SessionID).Body.String(); got != body {
			t.Errorf("at t0+%v: %s; want %s", d, got, body)
		}
	}
}

func TestGetUnknownSession(t *testing.T) {
	f := newFixture(t)
	f.checkedIn(t, "")
	for _, id := range []string{"ses_AAAAAAAAAAAAAAAAAAAAAAAAAA", "x", "ses_"} {
		assertError(t, f.getSession(id), http.StatusNotFound, "session_not_found")
	}
}

// One lookup reads one state: while rates are published at the instant its block ends, the block
// it charges and the rate it shows are always the same rate.
func TestGetSessionReadsOneState(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	end := t0.Add(10 * time.Minute)
	f.clk.Set(end)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			rate := int64(1200)
			if i%2 == 0 {
				rate = 1500
			}
			err := f.st.Tx(context.Background(), func(tx store.Querier) error {
				_, err := pricing.PublishProposal(context.Background(), tx, usd(rate), usd(300), end)
				return err
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 200 {
		got := decodeView(t, f.getSession(v.SessionID).Body.Bytes())
		if got.CoverCharge.Value != got.RatePerHour.Value*10/60 {
			t.Errorf("rate %d with cover charge %d", got.RatePerHour.Value, got.CoverCharge.Value)
			break
		}
	}
	close(stop)
	wg.Wait()
}
