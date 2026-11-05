package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
)

// The fixture's settings: blocks of 10 minutes and a pause of 30 minutes.

func resumesAt(v View) string {
	if v.CoverChargeResumesAt == nil {
		return "null"
	}
	return *v.CoverChargeResumesAt
}

func (f *fixture) assertPause(t *testing.T, name, id string, status pricing.Status, resumes string) {
	t.Helper()
	if got := f.looked(t, id); got.CoverChargeStatus != status || resumesAt(got) != resumes {
		t.Errorf("%s: %s until %s; want %s until %s", name, got.CoverChargeStatus, resumesAt(got), status, resumes)
	}
}

func (f *fixture) saveSettings(t *testing.T, mod func(*settings.Settings), at time.Time) {
	t.Helper()
	s := startingSettings()
	mod(&s)
	err := f.st.Tx(context.Background(), func(tx store.Querier) error { return settings.Save(context.Background(), tx, s, at) })
	if err != nil {
		t.Fatal(err)
	}
}

// Pricing model, Pause for additional orders: the lookup gives the instant the pause ends, the
// latest order after the first plus the pause, and from that instant on it is not paused.
func TestLookupShowsWhenThePauseEnds(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.assertPause(t, "the first order only", v.SessionID, pricing.Running, "null")

	f.clk.Set(t0.Add(15 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.assertPause(t, "the second order", v.SessionID, pricing.Paused, "2026-10-31T18:45:00Z")

	f.clk.Set(t0.Add(40 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-3", line("Fries", 1, 500))), http.StatusCreated)
	f.assertPause(t, "an order while paused", v.SessionID, pricing.Paused, "2026-10-31T19:10:00Z")

	f.clk.Set(t0.Add(70 * time.Minute).Add(-time.Nanosecond))
	f.assertPause(t, "just before the pause ends", v.SessionID, pricing.Paused, "2026-10-31T19:10:00Z")
	f.clk.Set(t0.Add(70 * time.Minute))
	f.assertPause(t, "when the pause ends", v.SessionID, pricing.Running, "null")
}

// UC4 exception: the same order reported again does not restart the pause, so the instant the
// pause ends stays.
func TestSameOrderAgainKeepsWhenThePauseEnds(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(time.Minute))
	second := orderBody("o-2", line("Beer", 1, 650))
	decodeOrder201(t, f.order(v.SessionID, second), http.StatusCreated)
	f.clk.Set(t0.Add(20 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, second), http.StatusOK)
	f.assertPause(t, "after the same order again", v.SessionID, pricing.Paused, "2026-10-31T18:31:00Z")
}

// Pricing model, Settings changes: a pause changed while paused applies from the next block, and
// the instant the pause ends is computed again with it then.
func TestChangedPauseMovesWhenThePauseEnds(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "")
	f.clk.Set(t0.Add(time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(12 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.saveSettings(t, func(s *settings.Settings) { s.PauseMinutes = 50 }, t0.Add(15*time.Minute))

	f.clk.Set(t0.Add(20 * time.Minute).Add(-time.Nanosecond))
	f.assertPause(t, "in the block the pause was changed in", v.SessionID, pricing.Paused, "2026-10-31T18:42:00Z")
	f.clk.Set(t0.Add(20 * time.Minute))
	f.assertPause(t, "in the next block", v.SessionID, pricing.Paused, "2026-10-31T19:02:00Z")
}

// Pricing model, Pause for additional orders: a pause of 0 pauses nothing.
func TestNoPauseShowsNoEnd(t *testing.T) {
	f := newFixture(t)
	f.saveSettings(t, func(s *settings.Settings) { s.PauseMinutes = 0 }, t0)
	v := f.checkedIn(t, "")
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.assertPause(t, "the second order with pause 0", v.SessionID, pricing.Running, "null")
}

// CoverChargeStatus: capped and stopped win over paused, and then no end of the pause is shown.
func TestCappedOrStoppedShowsNoEnd(t *testing.T) {
	f := newFixture(t)
	f.saveSettings(t, func(s *settings.Settings) { s.CapPerVisit = usd(400) }, t0)
	capped := f.checkedIn(t, "")
	stopped := f.checkedIn(t, "")
	f.clk.Set(t0.Add(time.Minute))
	for _, id := range []string{capped.SessionID, stopped.SessionID} {
		decodeOrder201(t, f.order(id, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	}

	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(stopped.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.assertPause(t, "before the register is requested", stopped.SessionID, pricing.Paused, "2026-10-31T18:35:00Z")
	if got := f.requested(t, stopped.SessionID); got.CoverChargeStatus != pricing.Stopped || got.CoverChargeResumesAt != nil {
		t.Errorf("register requested while paused: %s until %s; want stopped until null", got.CoverChargeStatus, resumesAt(got))
	}
	f.assertPause(t, "stopped while paused", stopped.SessionID, pricing.Stopped, "null")

	// The blocks ending at 10 and 20 reach the cap of 400; the order at 22 pauses until 52.
	f.clk.Set(t0.Add(22 * time.Minute))
	decodeOrder201(t, f.order(capped.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.assertPause(t, "capped while paused", capped.SessionID, pricing.Capped, "null")
}

// Responsibilities, Admin and Notifications: a listed session and a notified one carry the same
// end of the pause as the lookup at that instant.
func TestListAndNotificationShowWhenThePauseEnds(t *testing.T) {
	f := newFixture(t)
	v := f.checkedIn(t, "m_1")
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-2", line("Beer", 1, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(6 * time.Minute))
	if !f.link(t, v.SessionID) {
		t.Fatal("the session was not linked")
	}
	const want = "2026-10-31T18:35:00Z"
	f.assertPause(t, "lookup", v.SessionID, pricing.Paused, want)

	p := f.listed(t, "?limit=100")
	if len(p.Sessions) != 1 {
		t.Fatalf("%d listed sessions, want 1", len(p.Sessions))
	}
	if got := decodeView(t, p.Sessions[0]); resumesAt(got) != want {
		t.Errorf("listed: until %s, want %s", resumesAt(got), want)
	}

	evs := f.events(t, v.SessionID, "session.paypal_linked")
	if len(evs) != 1 {
		t.Fatalf("%d session.paypal_linked events, want 1", len(evs))
	}
	spectest.Valid(t, "SessionPaypalLinkedEvent", evs[0])
	var e struct {
		Data struct {
			Session View `json:"session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(evs[0], &e); err != nil {
		t.Fatal(err)
	}
	if got := e.Data.Session; got.CoverChargeStatus != pricing.Paused || resumesAt(got) != want {
		t.Errorf("notified: %s until %s; want paused until %s", got.CoverChargeStatus, resumesAt(got), want)
	}
}
