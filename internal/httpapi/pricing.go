package httpapi

import (
	"math"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// Pricing serves the published rate (getRate, getRateHistory) and the settings (getSettings,
// updateSettings), and the decision log (listDecisions).
type Pricing struct {
	Store *store.Store
	Clock clock.Clock
}

// Register adds Pricing's operations to r.
func (p Pricing) Register(r *Router) {
	r.Store("GET /rate", http.HandlerFunc(p.getRate))
	r.Store("GET /rate/history", http.HandlerFunc(p.getRateHistory))
	r.Admin("GET /settings", http.HandlerFunc(p.getSettings))
	r.Admin("PUT /settings", http.HandlerFunc(p.updateSettings))
	r.Admin("GET /decisions", http.HandlerFunc(p.listDecisions))
}

// RatePoint is the spec's RatePoint.
type RatePoint struct {
	RatePerHour Money  `json:"rate_per_hour"`
	Change      Money  `json:"change"`
	PublishedAt string `json:"published_at"`
}

// ToRatePoint writes p as the spec's RatePoint.
func ToRatePoint(p pricing.Point) RatePoint {
	return RatePoint{RatePerHour: ToMoney(p.Rate), Change: ToMoney(p.Change), PublishedAt: FormatTime(p.PublishedAt)}
}

var (
	minTime = time.Unix(0, math.MinInt64)
	maxTime = time.Unix(0, math.MaxInt64)
)

func (p Pricing) currency(req *http.Request) (money.Currency, error) {
	s, _, err := settings.Load(req.Context(), p.Store.Read())
	return s.Currency, err
}

// currentRate is the spec's CurrentRate.
type currentRate struct {
	RatePoint
	CapPerVisit Money `json:"cap_per_visit"`
}

// getRate reads the rate and the settings from one state of the store, so a settings change
// that moves the rate is shown with its cap.
func (p Pricing) getRate(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var out currentRate
	err := p.Store.View(ctx, func(q store.Querier) error {
		s, _, err := settings.Load(ctx, q)
		if err != nil {
			return err
		}
		pt, err := pricing.Current(ctx, q, s.Currency)
		if err != nil {
			return err
		}
		out = currentRate{RatePoint: ToRatePoint(pt), CapPerVisit: ToMoney(s.CapPerVisit)}
		return nil
	})
	if err != nil {
		WriteInternal(w)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

func (p Pricing) getRateHistory(w http.ResponseWriter, req *http.Request) {
	q, ok := Query(w, req)
	if !ok {
		return
	}
	raw, _, ok := single(w, q, "since")
	if !ok {
		return
	}
	if raw == "" {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "since is required.", "since")
		return
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "since must be an RFC 3339 date-time.", "since")
		return
	}
	// Times are stored as Unix nanoseconds; outside their range UnixNano wraps around.
	if since.Before(minTime) || since.After(maxTime) {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "since must be between the years 1678 and 2262.", "since")
		return
	}
	cur, err := p.currency(req)
	if err != nil {
		WriteInternal(w)
		return
	}
	pts, err := pricing.History(req.Context(), p.Store.Read(), cur, since, pricing.MaxHistory)
	if err != nil {
		WriteInternal(w)
		return
	}
	out := struct {
		Rates []RatePoint `json:"rates"`
	}{Rates: make([]RatePoint, len(pts))}
	for i, pt := range pts {
		out.Rates[i] = ToRatePoint(pt)
	}
	WriteJSON(w, http.StatusOK, out)
}
