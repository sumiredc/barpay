package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pricing: the "Now" tab (the published rate, Update, and the history of the decisions as a
// chart) and the "Settings" tab.

// autoEvaluation is when the app last asked BarPay for an automatic evaluation. The next one is
// due evaluationInterval after it; a staff member's Update does not move it. It is kept in
// memory only, like the ticker it follows: a restart asks again at once and records that.
type autoEvaluation struct {
	mu sync.Mutex
	at time.Time
}

func (e *autoEvaluation) asked(at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.at = at
}

// next is when the next automatic evaluation is due, or false before the first one is asked.
func (e *autoEvaluation) next() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.at.IsZero() {
		return time.Time{}, false
	}
	return e.at.Add(evaluationInterval), true
}

// historyWindow is how far back the history chart goes, and historyMaxPages how many pages of
// the decision log it reads at most to get there. BarPay lists the decisions newest first and
// cannot filter them by time; the cap keeps the page opening when the log is dense.
const (
	historyWindow   = 3 * time.Hour
	historyMaxPages = 10
)

// history is what the chart draws: the decisions of the window, newest first, and the newest
// decision before the window, whose rate and range the chart starts from at its left edge.
type history struct {
	Decisions []Decision
	Before    *Decision
}

// readHistory pages through the decision log until a decision from before now-historyWindow, the
// last page, or historyMaxPages pages. A decision at exactly now-historyWindow is in the window.
func (a *App) readHistory(ctx context.Context, staffID string, now time.Time) (history, error) {
	start := now.Add(-historyWindow)
	var h history
	cursor := ""
	for range historyMaxPages {
		ds, next, err := a.barpay.listDecisions(ctx, staffID, cursor)
		if err != nil {
			return history{}, err
		}
		for i := range ds {
			if ds[i].DecidedAt.Before(start) {
				h.Before = &ds[i]
				return h, nil
			}
			h.Decisions = append(h.Decisions, ds[i])
		}
		if next == nil {
			break
		}
		cursor = *next
	}
	return h, nil
}

// rateStep is the currency's rate step, which the guardrails round a rate to (PRD: Pricing
// model, Guardrails).
func rateStep(currency string) int64 {
	if currency == "JPY" {
		return 10
	}
	return 5
}

// roundToStep rounds v to the nearest multiple of step, a tie going toward prev, as the
// guardrails do with the published rate before the decision.
func roundToStep(v, step, prev int64) int64 {
	down := v - v%step
	if v%step == 0 {
		return v
	}
	up := down + step
	switch d := v - down; {
	case 2*d < step:
		return down
	case 2*d > step:
		return up
	case prev <= down:
		return down
	default:
		return up
	}
}

// decisionOutput says what an evaluation proposed and, when the published rate differs, which
// guardrail made it differ. The guardrails clamp into the range, then to the max step, then
// round to the rate step; BarPay does not return the max step, so a rate that the range and the
// rounding alone do not explain was held by the max step.
func decisionOutput(d Decision) string {
	if d.Fallback != nil {
		switch *d.Fallback {
		case "ai_timeout":
			return "No answer in time; the rate stays"
		case "ai_unreadable":
			return "The answer couldn't be read; the rate stays"
		}
		return *d.Fallback + "; the rate stays"
	}
	if d.Proposal == nil {
		return ""
	}
	p, pub := *d.Proposal, d.Published.RatePerHour
	out := "Proposed " + formatMoney(p)
	if p.Value == pub.Value {
		return out
	}
	clamped := p.Value
	if r := d.Range; r != nil {
		clamped = max(r.Floor.Value, min(r.Ceiling.Value, clamped))
	}
	step := rateStep(pub.Currency)
	prev := pub.Value - d.Published.Change.Value
	why := "max change per update"
	if roundToStep(clamped, step, prev) == pub.Value {
		switch {
		case clamped == p.Value:
			why = "rounded to " + formatMoney(Money{Value: step, Currency: pub.Currency})
		case clamped == d.Range.Floor.Value:
			why = "floor"
		default:
			why = "ceiling"
		}
	}
	return out + " → " + formatMoney(pub) + " (" + why + ")"
}

// changedSettings says what a range change changed, such as "Floor $9.50 → $10.00".
func changedSettings(prev, cur *Range) string {
	if prev == nil || cur == nil {
		return ""
	}
	var parts []string
	if prev.Floor != cur.Floor {
		parts = append(parts, "Floor "+formatMoney(prev.Floor)+" → "+formatMoney(cur.Floor))
	}
	if prev.Ceiling != cur.Ceiling {
		parts = append(parts, "Ceiling "+formatMoney(prev.Ceiling)+" → "+formatMoney(cur.Ceiling))
	}
	return strings.Join(parts, " · ")
}

// decisionDetail is the dialog a point of the chart opens. An empty field is left out.
type decisionDetail struct {
	ID, Heading string
	Rate        RatePoint
	Trigger     string
	Changed     string
	Input       string
	Output      string
	Reasoning   string
	Explanation string
}

func detailOf(id string, d Decision) decisionDetail {
	dd := decisionDetail{ID: id, Heading: "Decision at " + d.DecidedAt.Local().Format("15:04"), Rate: d.Published,
		Trigger: "Evaluation", Explanation: d.Explanation}
	if d.Trigger == "range_change" {
		dd.Trigger = d.StaffID + " (settings)"
		dd.Changed = changedSettings(d.PreviousRange, d.Range)
	} else {
		if in := d.Inputs; in != nil {
			live := "none"
			if in.Liveliness != nil {
				live = fmt.Sprintf("%.0f%%", *in.Liveliness*100)
			}
			dd.Input = fmt.Sprintf("Occupancy %.0f%% · Liveliness %s · Goal %d", in.Occupancy*100, live, in.Goal)
		}
		dd.Output = decisionOutput(d)
	}
	if d.Reasoning != nil {
		dd.Reasoning = *d.Reasoning
	}
	return dd
}

// The chart's drawing area, as in the mockup: 8.8px a minute, 20px on each side of the window,
// the rate from y=20 (top) to y=224 (bottom).
const (
	chartPxPerMinute = 8.8
	chartPad         = 20.0
	chartHeight      = 260
	chartTop         = 20.0
	chartBottom      = 224.0
)

type historyChart struct {
	Width      int
	Day        string
	NowX       string
	YTicks     []chartTick
	XTicks     []chartTick
	RatePath   string
	FloorPath  string
	CeilPath   string
	Points     []chartPoint
	NoDecision bool
}

type chartTick struct{ X, Y, Label string }

type chartPoint struct {
	X, Y, Kind, Label string
	Detail            decisionDetail
}

func px(f float64) string { return fmt.Sprintf("%.1f", f) }

// buildChart draws the history up to now: the published rate as steps, a point for each
// decision, and the floor and ceiling from each decision's range, left out where the range is
// null.
func buildChart(h history, now time.Time) historyChart {
	start := now.Add(-historyWindow)
	x := func(t time.Time) float64 { return chartPad + t.Sub(start).Minutes()*chartPxPerMinute }
	nowX := x(now)
	c := historyChart{Width: int(nowX + chartPad), NowX: px(nowX)}

	// Oldest first, each with where its rate and range start on the chart.
	type seg struct {
		x float64
		d Decision
	}
	var segs []seg
	if h.Before != nil {
		segs = append(segs, seg{0, *h.Before})
	}
	for i := len(h.Decisions) - 1; i >= 0; i-- {
		segs = append(segs, seg{x(h.Decisions[i].DecidedAt), h.Decisions[i]})
	}
	c.Day = start.Local().Format("Jan 2")
	if d := now.Local().Format("Jan 2"); d != c.Day {
		c.Day += " – " + d
	}
	if len(segs) == 0 {
		c.NoDecision = true
		return c
	}

	lo, hi := int64(-1), int64(0)
	see := func(v int64) {
		if lo < 0 || v < lo {
			lo = v
		}
		hi = max(hi, v)
	}
	for _, s := range segs {
		see(s.d.Published.RatePerHour.Value)
		if r := s.d.Range; r != nil {
			see(r.Floor.Value)
			see(r.Ceiling.Value)
		}
	}
	lo, hi, step := axis(lo, hi)
	y := func(v int64) float64 {
		v = min(max(v, lo), hi)
		return chartBottom - float64(v-lo)/float64(hi-lo)*(chartBottom-chartTop)
	}
	currency := segs[len(segs)-1].d.Published.RatePerHour.Currency
	for i, v := 0, lo; i <= 7 && v <= hi; i, v = i+1, v+step {
		label := formatMoney(Money{Value: v, Currency: currency})
		c.YTicks = append(c.YTicks, chartTick{Y: px(y(v)), Label: strings.TrimSuffix(label, ".00")})
	}
	for t := start.Truncate(15 * time.Minute); !t.After(now); t = t.Add(15 * time.Minute) {
		if t.Before(start) {
			continue
		}
		c.XTicks = append(c.XTicks, chartTick{X: px(x(t)), Label: t.Local().Format("15:04")})
	}

	var rate, floor, ceil strings.Builder
	penDown := false
	for i, s := range segs {
		end := nowX
		if i+1 < len(segs) {
			end = segs[i+1].x
		}
		ry := px(y(s.d.Published.RatePerHour.Value))
		if i == 0 {
			fmt.Fprintf(&rate, "M%s,%s", px(s.x), ry)
		} else {
			fmt.Fprintf(&rate, "H%sV%s", px(s.x), ry)
		}
		if r := s.d.Range; r == nil {
			penDown = false
		} else {
			fy, cy := px(y(r.Floor.Value)), px(y(r.Ceiling.Value))
			if penDown {
				fmt.Fprintf(&floor, "V%s", fy)
				fmt.Fprintf(&ceil, "V%s", cy)
			} else {
				fmt.Fprintf(&floor, "M%s,%s", px(s.x), fy)
				fmt.Fprintf(&ceil, "M%s,%s", px(s.x), cy)
			}
			fmt.Fprintf(&floor, "H%s", px(end))
			fmt.Fprintf(&ceil, "H%s", px(end))
			penDown = true
		}
	}
	fmt.Fprintf(&rate, "H%s", px(nowX))
	c.RatePath, c.FloorPath, c.CeilPath = rate.String(), floor.String(), ceil.String()

	for i, s := range segs {
		if h.Before != nil && i == 0 {
			continue
		}
		kind, name := "ai", "AI decision"
		switch {
		case s.d.Trigger == "range_change":
			kind, name = "settings", "Settings change"
		case s.d.Fallback != nil:
			kind, name = "fallback", "AI didn't answer"
		}
		label := s.d.DecidedAt.Local().Format("15:04") + " " + formatMoney(s.d.Published.RatePerHour) + "/h"
		c.Points = append(c.Points, chartPoint{X: px(s.x), Y: px(y(s.d.Published.RatePerHour.Value)), Kind: kind,
			Label: label + " · " + name, Detail: detailOf(fmt.Sprintf("decision-%d", len(c.Points)), s.d)})
	}
	return c
}

// axisLimit is the largest amount the axis is drawn to. Amounts are clamped into [0, axisLimit]
// before the axis is worked out, so an absurd amount from BarPay cannot overflow it.
const axisLimit = 1_000_000_000_000_000

// axis widens [lo, hi] by a tenth of its span on each side and rounds it out to a step of 1, 2
// or 5 times a power of ten, so the axis has at most 7 steps between its ticks.
func axis(lo, hi int64) (int64, int64, int64) {
	lo, hi = min(max(lo, 0), axisLimit), min(max(hi, 0), axisLimit)
	pad := (hi - lo) / 10
	if pad == 0 {
		pad = max(hi/10, 1)
	}
	lo, hi = lo-pad, hi+pad
	// With amounts up to axisLimit, a step of 5 × 10^17 always fits; the bound only guarantees
	// the loop ends.
	for p, i := int64(1), 0; i < 18; p, i = p*10, i+1 {
		for _, m := range []int64{1, 2, 5} {
			step := m * p
			l := lo - ((lo%step)+step)%step
			h := hi + (step-hi%step)%step
			if (h-l)/step <= 7 {
				return max(l, 0), h, step
			}
		}
	}
	return max(lo, 0), hi, max(hi-max(lo, 0), 1)
}

func (a *App) adminPricing(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	now := time.Now()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	data := struct {
		Rate          *CurrentRate
		NextAutomatic string
		Chart         historyChart
		HistoryFailed bool
	}{}
	rate, err := a.barpay.getRate(ctx)
	if err == nil {
		data.Rate = rate
	}
	if next, ok := a.autoEval.next(); ok {
		data.NextAutomatic = next.Local().Format("15:04")
	}
	h, err := a.readHistory(ctx, b.StaffID, now)
	data.HistoryFailed = err != nil
	if err == nil {
		data.Chart = buildChart(h, now)
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_pricing", v)
}

// adminSettings is the "Settings" tab: the settings form, which is sent to adminUpdateSettings.
func (a *App) adminSettings(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	s, err := a.barpay.getSettings(ctx, b.StaffID)
	data := struct {
		Settings *Settings
		Failed   bool
	}{Failed: err != nil}
	if err == nil {
		data.Settings = s
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_settings", v)
}

func (a *App) adminEvaluate(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	e, err := a.barpay.requestRateEvaluation(ctx)
	switch {
	case err != nil:
		a.flash(id, "BarPay did not evaluate the rate: "+err.Error())
	case e.Fallback != nil:
		a.flash(id, "The AI gave no usable answer ("+*e.Fallback+"); the rate stays at "+formatMoney(e.Rate.RatePerHour)+"/h.")
	case !e.Evaluated:
		a.flash(id, "Too soon after the last evaluation; the rate stays at "+formatMoney(e.Rate.RatePerHour)+"/h.")
	default:
		a.flash(id, "Evaluated: "+formatMoney(e.Rate.RatePerHour)+"/h ("+formatSignedMoney(e.Rate.Change)+").")
	}
	redirect(w, r, "/admin/pricing")
}
