package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

func usd(v int64) money.Amount { return money.New(v, money.USD) }

func startingSettings() settings.Settings {
	return settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
}

// started is a store as after the first start, with the starting rate 1200 published at t0.
func started(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t)
	err := st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, startingSettings(), t0); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, usd(1200), startingSettings(), t0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func publish(t *testing.T, st *store.Store, proposals []int64, from time.Time) {
	t.Helper()
	ctx := context.Background()
	err := st.Tx(ctx, func(tx store.Querier) error {
		for i, p := range proposals {
			if _, err := pricing.PublishProposal(ctx, tx, usd(p), usd(300), from.Add(time.Duration(i)*time.Minute)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func pricingRouter(st *store.Store) *Router {
	r := NewRouter(keys)
	Pricing{Store: st, Clock: clock.NewFake(t0.Add(time.Hour))}.Register(r)
	return r
}

var storeAuth = "Bearer " + keys.Store.Reveal()

type ratePoint struct {
	RatePerHour Money  `json:"rate_per_hour"`
	Change      Money  `json:"change"`
	PublishedAt string `json:"published_at"`
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(w.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	return v
}

func TestGetRateReturnsThePublishedRate(t *testing.T) {
	st := started(t)
	publish(t, st, []int64{1400}, t0.Add(time.Minute))
	w := do(pricingRouter(st), "/v1/rate", storeAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	want := ratePoint{RatePerHour: Money{1400, "USD"}, Change: Money{200, "USD"}, PublishedAt: "2026-10-31T18:01:00Z"}
	if got := decode[ratePoint](t, w); got != want {
		t.Errorf("rate = %+v, want %+v", got, want)
	}
}

func history(t *testing.T, r http.Handler, since string) (int, []ratePoint) {
	t.Helper()
	w := do(r, "/v1/rate/history?since="+url.QueryEscape(since), storeAuth)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	return w.Code, decode[struct {
		Rates []ratePoint `json:"rates"`
	}](t, w).Rates
}

// The rates from since, oldest first, with the one in effect at since.
func TestGetRateHistory(t *testing.T) {
	st := started(t)
	publish(t, st, []int64{1300, 1400}, t0.Add(10*time.Minute)) // at 10 and 11
	r := pricingRouter(st)
	for since, want := range map[string][]int64{
		"2026-10-31T17:00:00Z":      {1200, 1300, 1400},
		"2026-10-31T18:05:00Z":      {1200, 1300, 1400},
		"2026-10-31T18:10:30+00:00": {1300, 1400},
		"2026-10-31T19:10:30.5Z":    {1400},
		// The earliest and latest whole seconds that Unix nanoseconds hold.
		"1677-09-21T00:12:44Z": {1200, 1300, 1400},
		"2262-04-11T23:47:16Z": {1400},
	} {
		code, rates := history(t, r, since)
		var got []int64
		for _, p := range rates {
			got = append(got, p.RatePerHour.Value)
		}
		if code != http.StatusOK || !slices.Equal(got, want) {
			t.Errorf("since %s: %d %v, want %v", since, code, got, want)
		}
	}
}

// At most the spec's 1,000, the most recent.
func TestGetRateHistoryLimit(t *testing.T) {
	st := started(t)
	proposals := make([]int64, 1005)
	for i := range proposals {
		proposals[i] = 1200 + 5*int64(i%2+1)
	}
	publish(t, st, proposals, t0.Add(time.Minute))
	_, rates := history(t, pricingRouter(st), "2026-10-31T00:00:00Z")
	last := rates[len(rates)-1].PublishedAt
	if len(rates) != 1000 || last != t0.Add(1005*time.Minute).Format(time.RFC3339) {
		t.Errorf("%d rates, last at %s; want the 1,000 most recent", len(rates), last)
	}
}

func TestGetRateHistoryRefusesBadSince(t *testing.T) {
	r := pricingRouter(started(t))
	for _, path := range []string{"/v1/rate/history", "/v1/rate/history?since=", "/v1/rate/history?since=yesterday",
		"/v1/rate/history?since=2026-10-31", "/v1/rate/history?since=2026-10-31T18:00:00",
		// Outside what Unix nanoseconds hold: they would wrap around to another instant.
		"/v1/rate/history?since=1677-09-21T00:12:43Z", "/v1/rate/history?since=2262-04-11T23:47:17Z",
		"/v1/rate/history?since=9999-12-31T23:59:59Z", "/v1/rate/history?since=0001-01-01T00:00:00Z"} {
		w := do(r, path, storeAuth)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"since"`) ||
			!strings.Contains(w.Body.String(), `"invalid_request"`) {
			t.Errorf("%s: %d %s; want 400 invalid_request on since", path, w.Code, w.Body)
		}
	}
}

// An unreadable since is refused as unreadable, not as missing, and since given more than once is
// refused rather than one of them being used.
func TestGetRateHistoryRefusesUnreadableOrRepeatedSince(t *testing.T) {
	r := pricingRouter(started(t))
	at := "2026-10-31T18:00:00Z"
	for _, c := range []struct{ query, notMessage string }{
		{"since=%zz", "since is required."},
		{"since=" + at + ";x", "since is required."},
		{"since=" + at + "&since=" + at, ""},
		{"since=" + at + "&since=2026-10-31T19:00:00Z", ""},
	} {
		w := do(r, "/v1/rate/history?"+c.query, storeAuth)
		spectest.Valid(t, "Error", w.Body.Bytes())
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Details struct {
					Field string `json:"field"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != http.StatusBadRequest ||
			e.Error.Code != "invalid_request" || e.Error.Details.Field != "since" ||
			(c.notMessage != "" && e.Error.Message == c.notMessage) {
			t.Errorf("%s: %d %s; want 400 invalid_request on since", c.query, w.Code, w.Body)
		}
	}
}

// Only the store API key reads the rate.
func TestRateOperationsNeedTheStoreKey(t *testing.T) {
	r := pricingRouter(started(t))
	for _, path := range []string{"/v1/rate", "/v1/rate/history?since=2026-10-31T00:00:00Z"} {
		for name, auth := range map[string]string{
			"no key": "", "wrong key": "Bearer nope", "admin API key": "Bearer " + keys.Admin.Reveal(),
			"edge shared secret": "Bearer " + keys.Edge.Reveal(),
		} {
			t.Run(path+" "+name, func(t *testing.T) { assertUnauthorized(t, do(r, path, auth)) })
		}
	}
}
