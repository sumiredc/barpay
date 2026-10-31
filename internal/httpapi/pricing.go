package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// Pricing serves the published rate (getRate, getRateHistory).
type Pricing struct {
	Store *store.Store
}

// Register adds Pricing's operations to r.
func (p Pricing) Register(r *Router) {
	r.Store("GET /rate", http.HandlerFunc(p.getRate))
	r.Store("GET /rate/history", http.HandlerFunc(p.getRateHistory))
}

type moneyJSON struct {
	Value    int64          `json:"value"`
	Currency money.Currency `json:"currency"`
}

func toMoney(a money.Amount) moneyJSON { return moneyJSON{Value: a.Value(), Currency: a.Currency()} }

type ratePointJSON struct {
	RatePerHour moneyJSON `json:"rate_per_hour"`
	Change      moneyJSON `json:"change"`
	PublishedAt string    `json:"published_at"`
}

func toRatePoint(p pricing.Point) ratePointJSON {
	return ratePointJSON{RatePerHour: toMoney(p.Rate), Change: toMoney(p.Change), PublishedAt: timeJSON(p.PublishedAt)}
}

var (
	minTime = time.Unix(0, math.MinInt64)
	maxTime = time.Unix(0, math.MaxInt64)
)

func timeJSON(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (p Pricing) currency(req *http.Request) (money.Currency, error) {
	s, _, err := settings.Load(req.Context(), p.Store.Read())
	return s.Currency, err
}

func (p Pricing) getRate(w http.ResponseWriter, req *http.Request) {
	cur, err := p.currency(req)
	if err != nil {
		writeInternal(w)
		return
	}
	pt, err := pricing.Current(req.Context(), p.Store.Read(), cur)
	if err != nil {
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, toRatePoint(pt))
}

func (p Pricing) getRateHistory(w http.ResponseWriter, req *http.Request) {
	raw := req.URL.Query().Get("since")
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
		writeInternal(w)
		return
	}
	pts, err := pricing.History(req.Context(), p.Store.Read(), cur, since, pricing.MaxHistory)
	if err != nil {
		writeInternal(w)
		return
	}
	out := struct {
		Rates []ratePointJSON `json:"rates"`
	}{Rates: make([]ratePointJSON, len(pts))}
	for i, pt := range pts {
		out.Rates[i] = toRatePoint(pt)
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
