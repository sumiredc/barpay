package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pricingBarPay is an adminBarPay that also answers the decision log in pages, the rate
// evaluation and the settings save.
type pricingBarPay struct {
	*adminBarPay
	mu sync.Mutex
	// pages are the pages of the decision log, newest first; a cursor is a page's index.
	pages         [][]Decision
	decisionReads int
	decisionsDown bool
	evaluations   int
	saved         []Settings
}

func (f *pricingBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/admin/decisions":
		f.decisionReads++
		w.Header().Set("Content-Type", "application/json")
		if f.decisionsDown {
			// A refusal, which the app does not send again, so the page is drawn without waiting.
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unauthorized", "message": "down for the test"}})
			return
		}
		i, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		page := map[string]any{"decisions": []Decision{}, "next_cursor": nil}
		if i < len(f.pages) {
			page["decisions"] = f.pages[i]
		}
		if i+1 < len(f.pages) {
			page["next_cursor"] = strconv.Itoa(i + 1)
		}
		json.NewEncoder(w).Encode(page)
	case r.Method == "POST" && r.URL.Path == "/v1/rate-evaluations":
		f.evaluations++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(RateEvaluation{Rate: RatePoint{RatePerHour: usd(1250), Change: usd(50), PublishedAt: time.Now()}, Evaluated: true})
	case r.Method == "PUT" && r.URL.Path == "/v1/admin/settings":
		var s Settings
		json.NewDecoder(r.Body).Decode(&s)
		f.saved = append(f.saved, s)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	default:
		f.adminBarPay.ServeHTTP(w, r)
	}
}

func usd(v int64) Money { return Money{Value: v, Currency: "USD"} }
func jpy(v int64) Money { return Money{Value: v, Currency: "JPY"} }

// newPricingStaff is newStaff with the app talking to a pricingBarPay.
func newPricingStaff(t *testing.T) (*staff, *pricingBarPay) {
	t.Helper()
	s := newStaff(t)
	f := &pricingBarPay{adminBarPay: s.barpay}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.a.cfg.BarPayURL = srv.URL
	s.a.barpay = newBarPay(s.a.cfg)
	return s, f
}

// clockAt is a time today in the local time zone, which the pages draw times in.
func clockAt(hour, minute int) time.Time {
	return time.Date(2026, 11, 5, hour, minute, 0, 0, time.Local)
}

func evaluation(t time.Time, rate int64) Decision {
	return Decision{DecidedAt: t, Trigger: "evaluation", Published: RatePoint{RatePerHour: usd(rate), PublishedAt: t},
		Range: &Range{Floor: usd(1000), Ceiling: usd(2000)}, Explanation: "Steady room."}
}

// everyFiveMinutes is n evaluations, newest first, from newest back.
func everyFiveMinutes(newest time.Time, n int) []Decision {
	var ds []Decision
	for i := range n {
		ds = append(ds, evaluation(newest.Add(-time.Duration(i)*5*time.Minute), 1000))
	}
	return ds
}

func TestTheHistoryStopsAtTheFirstPageThatGoesBackThreeHours(t *testing.T) {
	s, f := newPricingStaff(t)
	now := clockAt(22, 0)
	// 20 a page, every 5 minutes: page 1 goes back to 20:25, page 2 to 18:45, past 19:00.
	all := everyFiveMinutes(clockAt(22, 0), 60)
	f.pages = [][]Decision{all[:20], all[20:40], all[40:]}
	h, err := s.a.readHistory(context.Background(), "staff-01", now)
	if err != nil {
		t.Fatal(err)
	}
	if f.decisionReads != 2 {
		t.Errorf("read %d pages, want 2", f.decisionReads)
	}
	// 22:00 back to 19:00 inclusive is 37 decisions; 18:55 is the first before the window.
	if len(h.Decisions) != 37 || !h.Decisions[36].DecidedAt.Equal(clockAt(19, 0)) {
		t.Fatalf("the window has %d decisions, want 37 down to 19:00", len(h.Decisions))
	}
	if h.Before == nil || !h.Before.DecidedAt.Equal(clockAt(18, 55)) {
		t.Errorf("the decision before the window is %+v, want the one at 18:55", h.Before)
	}
	if c := buildChart(h, now); len(c.Points) != 37 {
		t.Errorf("the chart has %d points, want 37: one before the window is not a point", len(c.Points))
	}
}

func TestTheHistoryStopsAfterTenPages(t *testing.T) {
	s, f := newPricingStaff(t)
	now := clockAt(22, 0)
	for range 12 {
		f.pages = append(f.pages, everyFiveMinutes(now, 20))
	}
	h, err := s.a.readHistory(context.Background(), "staff-01", now)
	if err != nil {
		t.Fatal(err)
	}
	if f.decisionReads != 10 || len(h.Decisions) != 200 || h.Before != nil {
		t.Errorf("read %d pages and %d decisions (before: %v), want 10 pages and 200", f.decisionReads, len(h.Decisions), h.Before)
	}
}

func TestTheChartStartsFromTheNewestDecisionBeforeTheWindow(t *testing.T) {
	s, f := newPricingStaff(t)
	now := clockAt(22, 0)
	before := evaluation(clockAt(18, 50), 950)
	before.Range = &Range{Floor: usd(950), Ceiling: usd(1900)}
	older := evaluation(clockAt(18, 45), 1500)
	f.pages = [][]Decision{everyFiveMinutes(clockAt(21, 0), 1), {before, older}}
	h, err := s.a.readHistory(context.Background(), "staff-01", now)
	if err != nil {
		t.Fatal(err)
	}
	c := buildChart(h, now)
	// The axis runs $8 (y 224) to $22 (y 20): $9.50 is at y 202.1 and $19.00 at 63.7. 21:00 is x 1076.
	if !strings.HasPrefix(c.RatePath, "M0.0,202.1H1076.0") {
		t.Errorf("the rate line is %s, want it to start at the left edge at $9.50", c.RatePath)
	}
	if !strings.HasPrefix(c.FloorPath, "M0.0,202.1H1076.0") || !strings.HasPrefix(c.CeilPath, "M0.0,63.7H1076.0") {
		t.Errorf("the range is %s and %s, want it to start at the left edge at $9.50 to $19.00", c.FloorPath, c.CeilPath)
	}
}

func TestNoRangeIsDrawnWhereADecisionHasNone(t *testing.T) {
	fallback := evaluation(clockAt(21, 30), 1100)
	reason := "ai_timeout"
	fallback.Fallback, fallback.Range = &reason, nil
	h := history{Decisions: []Decision{evaluation(clockAt(21, 45), 1200), fallback, evaluation(clockAt(21, 0), 1000)}}
	c := buildChart(h, clockAt(22, 0))
	// 21:00 is x 1076, 21:30 x 1340, 21:45 x 1472 and now x 1604; $10 is y 194.9 and $20 y 49.1.
	if c.FloorPath != "M1076.0,194.9H1340.0M1472.0,194.9H1604.0" {
		t.Errorf("the floor line is %s, want a gap from 21:30 to 21:45", c.FloorPath)
	}
	if c.CeilPath != "M1076.0,49.1H1340.0M1472.0,49.1H1604.0" {
		t.Errorf("the ceiling line is %s, want a gap from 21:30 to 21:45", c.CeilPath)
	}
	if kinds := [3]string{c.Points[0].Kind, c.Points[1].Kind, c.Points[2].Kind}; kinds != [3]string{"ai", "fallback", "ai"} {
		t.Errorf("the points are %v, want ai, fallback, ai", kinds)
	}
}

func proposed(proposal, published, change int64, r *Range, currency string) Decision {
	m := func(v int64) Money { return Money{Value: v, Currency: currency} }
	p := m(proposal)
	return Decision{Trigger: "evaluation", Proposal: &p, Range: r,
		Published: RatePoint{RatePerHour: m(published), Change: m(change)}}
}

func TestTheOutputSaysWhichGuardrailChangedTheProposal(t *testing.T) {
	usdRange := &Range{Floor: usd(1000), Ceiling: usd(2000)}
	for _, c := range []struct {
		name string
		d    Decision
		want string
	}{
		{"below the floor", proposed(950, 1000, 0, usdRange, "USD"), "Proposed $9.50 → $10.00 (floor)"},
		{"above the ceiling", proposed(2150, 2000, 50, usdRange, "USD"), "Proposed $21.50 → $20.00 (ceiling)"},
		{"between steps", proposed(1103, 1105, 5, usdRange, "USD"), "Proposed $11.03 → $11.05 (rounded to $0.05)"},
		{"beyond the max step", proposed(1250, 1200, 100, usdRange, "USD"), "Proposed $12.50 → $12.00 (max change per update)"},
		{"below the floor and beyond the max step", proposed(950, 1100, -100, usdRange, "USD"), "Proposed $9.50 → $11.00 (max change per update)"},
		{"a tie in JPY", proposed(1105, 1100, 0, &Range{Floor: jpy(800), Ceiling: jpy(2000)}, "JPY"), "Proposed ¥1,105 → ¥1,100 (rounded to ¥10)"},
		{"as proposed", proposed(1100, 1100, 100, usdRange, "USD"), "Proposed $11.00"},
	} {
		if got := decisionOutput(c.d); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestATieIsRoundedTowardThePreviousRate(t *testing.T) {
	if got := roundToStep(1105, 10, 1110); got != 1110 {
		t.Errorf("¥1,105 from ¥1,110 rounds to %d, want 1110", got)
	}
	if got := roundToStep(1105, 10, 1100); got != 1100 {
		t.Errorf("¥1,105 from ¥1,100 rounds to %d, want 1100", got)
	}
}

func TestTheOutputOfAFallbackSaysTheRateStays(t *testing.T) {
	for code, want := range map[string]string{
		"ai_timeout":    "No answer in time; the rate stays",
		"ai_unreadable": "The answer couldn't be read; the rate stays",
	} {
		d := evaluation(clockAt(21, 50), 1100)
		d.Fallback = &code
		if got := decisionOutput(d); got != want {
			t.Errorf("%s: %q, want %q", code, got, want)
		}
	}
}

func TestTheChartIsDrawnForAmountsAtTheEdgesOfInt64(t *testing.T) {
	for _, v := range []int64{math.MaxInt64, math.MaxInt64 - 1, math.MinInt64, 0} {
		d := evaluation(clockAt(21, 0), v)
		d.Range = &Range{Floor: usd(v), Ceiling: usd(math.MaxInt64)}
		done := make(chan historyChart, 1)
		go func() { done <- buildChart(history{Decisions: []Decision{d}}, clockAt(22, 0)) }()
		select {
		case c := <-done:
			if n := len(c.YTicks); n < 2 || n > 8 {
				t.Errorf("with %d the axis has %d ticks, want 2 to 8", v, n)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the chart for %d is not drawn within 5 seconds", v)
		}
	}
}

func TestARangeChangeSaysWhatItChanged(t *testing.T) {
	d := Decision{DecidedAt: clockAt(21, 30), Trigger: "range_change", StaffID: "staff-01",
		Published:     RatePoint{RatePerHour: usd(1000), Change: usd(50)},
		PreviousRange: &Range{Floor: usd(950), Ceiling: usd(2000)}, Range: &Range{Floor: usd(1000), Ceiling: usd(2000)}}
	dd := detailOf("decision-0", d)
	if dd.Changed != "Floor $9.50 → $10.00" || dd.Trigger != "staff-01 (settings)" || dd.Output != "" || dd.Input != "" {
		t.Errorf("the detail is %+v, want Floor $9.50 → $10.00 by staff-01 (settings) without input or output", dd)
	}
}

func TestTheNextAutomaticUpdateIsNotMovedByUpdate(t *testing.T) {
	s, f := newPricingStaff(t)
	s.logIn()
	if _, err := s.a.evaluateAutomatically(context.Background(), clockAt(21, 55)); err != nil {
		t.Fatal(err)
	}
	// Update, pressed at 21:57.
	page := s.post("/admin/pricing", "/admin/evaluate", url.Values{})
	if f.evaluations != 2 {
		t.Fatalf("BarPay was asked %d times, want 2", f.evaluations)
	}
	if next, _ := s.a.autoEval.next(); !next.Equal(clockAt(22, 0)) {
		t.Errorf("the next automatic update is %s, want 22:00", next)
	}
	if !strings.Contains(page, "Next automatic update at 22:00") {
		t.Errorf("the pricing page does not say the next automatic update is at 22:00:\n%s", page)
	}
	if !strings.Contains(page, "Evaluated: $12.50/h (&#43;$0.50).") {
		t.Error("the pricing page does not tell what Update did")
	}
}

func TestThePricingPageDrawsTheRateAndTheHistory(t *testing.T) {
	s, f := newPricingStaff(t)
	now := time.Now()
	reason := "Busy room."
	d := evaluation(now.Add(-time.Minute), 1200)
	d.Reasoning = &reason
	f.pages = [][]Decision{{d}}
	s.logIn()
	page := s.open("/admin/pricing")
	for _, want := range []string{
		`<a href="/admin/pricing" aria-current="page">Now</a>`, `<a href="/admin/settings">Settings</a>`,
		"Published rate", `$12.00<small>/h</small>`, `<span class="up">&#43;$0.50</span> (changed at `,
		`<i data-icon="rotate-cw"></i> Update</button>`, "<h2>History</h2>",
		`<i class="sw rate"></i>Rate`, `<i class="sw bound"></i>Range`, `<i class="dotkey ai"></i>AI decision`,
		`<i class="dotkey fallback"></i>AI didn't answer`, `<i class="dotkey settings"></i>Settings change`,
		`>Now</text>`, `<g class="pt ai" data-sheet="decision-0"`, `<dialog class="sheet" id="decision-0">`,
		"<dt>Rate</dt><dd>$12.00/h (<span class=\"muted\">No change</span>)</dd>", "<dt>Trigger</dt><dd>Evaluation</dd>",
		"<dt>AI's reasoning</dt><dd>Busy room.</dd>", "<dt>BarPay's explanation</dt><dd>Steady room.</dd>",
		`data-close>Close</button>`, `class="hscroll" data-start="end"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the pricing page has no %s", want)
		}
	}
	for _, gone := range []string{`name="floor"`, "Evaluate now", "Older decisions", "BarPay didn't answer"} {
		if strings.Contains(page, gone) {
			t.Errorf("the pricing page still has %s", gone)
		}
	}
}

func TestAHistoryThatCannotBeReadLeavesTheRate(t *testing.T) {
	s, f := newPricingStaff(t)
	f.decisionsDown = true
	s.logIn()
	page := s.open("/admin/pricing")
	if !strings.Contains(page, `$12.00<small>/h</small>`) {
		t.Error("the published rate is not drawn")
	}
	if !strings.Contains(page, `BarPay didn't return the history. Wait a moment, then <a href="/admin/pricing">reload</a>. The published rate above is current.`) {
		t.Errorf("the page does not say the history could not be read:\n%s", page)
	}
	if strings.Contains(page, `class="rate-chart"`) {
		t.Error("a chart is drawn without the history")
	}
}

func TestAHistoryThatCannotBeReadIsToldWithoutTheRateToo(t *testing.T) {
	s, f := newPricingStaff(t)
	s.logIn()
	f.decisionsDown = true
	f.adminBarPay.mu.Lock()
	f.adminBarPay.down = true
	f.adminBarPay.mu.Unlock()
	page := s.open("/admin/pricing")
	if strings.Contains(page, `$12.00<small>/h</small>`) {
		t.Error("a published rate is drawn that was not read")
	}
	if !strings.Contains(page, `BarPay didn't return the history. Wait a moment, then <a href="/admin/pricing">reload</a>.</span>`) {
		t.Errorf("the page does not say the history could not be read, without saying the rate is current:\n%s", page)
	}
}

func TestTheSettingsTabSavesTheFormAndComesBack(t *testing.T) {
	s, f := newPricingStaff(t)
	s.logIn()
	page := s.open("/admin/settings")
	for _, want := range []string{`<a href="/admin/settings" aria-current="page">Settings</a>`, `name="floor" type="text" value="8.00"`,
		`<a href="/admin/pricing" aria-current="page"><i data-icon="trending-up"></i><span>Pricing</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings tab has no %s", want)
		}
	}
	resp := s.postTo("/admin/settings", url.Values{"currency": {"USD"}, "floor": {"9.50"}, "ceiling": {"20.00"},
		"cap_per_visit": {"25.00"}, "block_minutes": {"10"}, "pause_minutes": {"0"}, "goal": {"60"}})
	if resp.path != "/admin/settings" || !strings.Contains(resp.body, "The settings were saved.") {
		t.Errorf("saving went to %s, want /admin/settings telling it was saved", resp.path)
	}
	want := Settings{Floor: usd(950), Ceiling: usd(2000), CapPerVisit: usd(2500), BlockMinutes: 10, Goal: 60}
	if len(f.saved) != 1 || f.saved[0] != want {
		t.Errorf("BarPay got %+v, want %+v", f.saved, want)
	}
	if resp := s.postTo("/admin/settings", url.Values{"floor": {"x"}}); resp.path != "/admin/settings" {
		t.Errorf("a form that cannot be read went to %s, want /admin/settings", resp.path)
	}
}

func TestTheGateCanBeUnlockedFromTheSettingsTab(t *testing.T) {
	s, _ := newPricingStaff(t)
	s.logIn()
	path, body := s.unlock("/admin/settings", "none")
	if path != "/admin/settings" || !strings.Contains(body, "The exit gate was told to unlock once.") {
		t.Errorf("an unlock from the settings tab went to %s", path)
	}
}

type landed struct{ path, body string }

// postTo sends a form from the settings tab and returns where the browser lands.
func (s *staff) postTo(path string, form url.Values) landed {
	s.t.Helper()
	m := csrfPattern.FindStringSubmatch(s.open("/admin/settings"))
	form.Set("csrf", m[1])
	resp, err := s.client.PostForm(s.app.URL+path, form)
	if err != nil {
		s.t.Fatal(err)
	}
	return landed{resp.Request.URL.Path, s.read(resp)}
}
