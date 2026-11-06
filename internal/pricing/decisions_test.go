package pricing

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

func half() *float64 { v := 0.5; return &v }

var someInputs = Inputs{Occupancy: 0.25, Liveliness: half(), Goal: 50}

func evaluate(t *testing.T, st *store.Store, p Proposal, at time.Time) Decision {
	t.Helper()
	var d Decision
	tx(t, st, func(q store.Querier) error {
		var err error
		d, err = PublishEvaluation(context.Background(), q, someInputs, p, usd(300), at)
		return err
	})
	return d
}

func keep(t *testing.T, st *store.Store, fb Fallback, at time.Time) Decision {
	t.Helper()
	var d Decision
	tx(t, st, func(q store.Querier) error {
		var err error
		d, err = KeepRate(context.Background(), q, someInputs, fb, at)
		return err
	})
	return d
}

func list(t *testing.T, q store.Querier, after int64, limit int) ([]Decision, bool) {
	t.Helper()
	ds, more, err := ListDecisions(context.Background(), q, after, limit)
	if err != nil {
		t.Fatal(err)
	}
	return ds, more
}

// A proposal beyond the max step publishes the max step, and the explanation states that change,
// not the one proposed.
func TestPublishEvaluationExplainsTheGuardedChange(t *testing.T) {
	st := started(t, 1200)
	d := evaluate(t, st, Proposal{RatePerHour: 5000, Reason: "Packed room and a live show.", Reasoning: "r"}, t0.Add(time.Minute))
	if got := current(t, st.Read()); got.Rate != usd(1500) || d.Published != got {
		t.Fatalf("published %+v, current %+v; want 1500", d.Published, got)
	}
	if want := "Packed room and a live show — rate up $3.00 to $15.00/h."; d.Explanation != want {
		t.Errorf("explanation = %q, want %q", d.Explanation, want)
	}
	if *d.Proposal != usd(5000) || *d.Reasoning != "r" || d.Fallback != "" {
		t.Errorf("decision = %+v", d)
	}
	d = evaluate(t, st, Proposal{RatePerHour: 0, Reason: "Room is empty", Reasoning: "r"}, t0.Add(2*time.Minute))
	if want := "Room is empty — rate down $3.00 to $12.00/h."; d.Explanation != want {
		t.Errorf("explanation = %q, want %q", d.Explanation, want)
	}
	d = evaluate(t, st, Proposal{RatePerHour: 1202, Reason: "Steady room", Reasoning: "r"}, t0.Add(3*time.Minute))
	if want := "Steady room — rate unchanged at $12.00/h."; d.Explanation != want {
		t.Errorf("explanation = %q, want %q", d.Explanation, want)
	}
}

// For any proposal and any range, the published rate is within the floor and ceiling and the
// max step, and the explanation names the change and rate actually published.
func TestPublishEvaluationPropertyAnyProposal(t *testing.T) {
	st := started(t, 1200)
	r := rand.New(rand.NewPCG(17, 0))
	for i := range 300 {
		before := current(t, st.Read())
		var p int64
		switch i % 3 {
		case 0:
			p = r.Int64N(money.MaxValue + 1)
		case 1:
			p = r.Int64N(4000)
		default:
			p = before.Rate.Value() + r.Int64N(700) - 350
			p = max(p, 0)
		}
		d := evaluate(t, st, Proposal{RatePerHour: p, Reason: "x", Reasoning: "y"}, t0.Add(time.Duration(i+1)*time.Second))
		got := d.Published
		v, c := got.Rate.Value(), got.Change.Value()
		if v < 600 || v > 2400 || v%5 != 0 || c != v-before.Rate.Value() || c > 300 || c < -300 {
			t.Fatalf("proposal %d from %v: published %+v", p, before.Rate, got)
		}
		if !strings.Contains(d.Explanation, format(got.Rate)+"/h") ||
			(c > 0) != strings.Contains(d.Explanation, "rate up "+format(got.Change)) ||
			(c < 0) != strings.Contains(d.Explanation, "rate down") || (c == 0) != strings.Contains(d.Explanation, "unchanged") {
			t.Fatalf("explanation %q for %+v", d.Explanation, got)
		}
	}
}

func TestPublishEvaluationRefusesAProposalOutsideItsLimits(t *testing.T) {
	st := started(t, 1200)
	for name, p := range map[string]Proposal{
		"negative":       {RatePerHour: -1, Reason: "x", Reasoning: "y"},
		"above Money":    {RatePerHour: money.MaxValue + 1, Reason: "x", Reasoning: "y"},
		"no reason":      {RatePerHour: 1200, Reasoning: "y"},
		"long reason":    {RatePerHour: 1200, Reason: strings.Repeat("a", MaxReason+1), Reasoning: "y"},
		"newline":        {RatePerHour: 1200, Reason: "a\nb", Reasoning: "y"},
		"NEL":            {RatePerHour: 1200, Reason: "a\u0085b", Reasoning: "y"},
		"line separator": {RatePerHour: 1200, Reason: "a\u2028b", Reasoning: "y"},
		"paragraph sep":  {RatePerHour: 1200, Reason: "a\u2029b", Reasoning: "y"},
		"RLO":            {RatePerHour: 1200, Reason: "a\u202eb", Reasoning: "y"},
		"zero width":     {RatePerHour: 1200, Reason: "a\u200bb", Reasoning: "y"},
		"only dots":      {RatePerHour: 1200, Reason: "...", Reasoning: "y"},
		"spaces, dots":   {RatePerHour: 1200, Reason: " . . ", Reasoning: "y"},
		"no reasoning":   {RatePerHour: 1200, Reason: "x"},
		"long reasoning": {RatePerHour: 1200, Reason: "x", Reasoning: strings.Repeat("あ", MaxReasoning+1)},
	} {
		err := st.Tx(context.Background(), func(q store.Querier) error {
			_, err := PublishEvaluation(context.Background(), q, someInputs, p, usd(300), t0)
			return err
		})
		if !errors.Is(err, ErrInvalidProposal) {
			t.Errorf("%s: %v, want ErrInvalidProposal", name, err)
		}
	}
	if n, d := countRates(t, st.Read()), decisions(t, st.Read()); n != 1 || len(d) != 0 {
		t.Errorf("%d rates, %d decisions; want nothing published or recorded", n, len(d))
	}
	// The longest reason, counted once whitespace around it is removed, still fits the spec's
	// 200-character explanation with the largest change.
	evaluate(t, st, Proposal{RatePerHour: 1500, Reason: "  " + strings.Repeat("w", MaxReason) + " ", Reasoning: strings.Repeat("あ", MaxReasoning)}, t0)
	if d := decisions(t, st.Read()); len(d) != 1 || utf8.RuneCountInString(d[0].explanation) > 200 {
		t.Errorf("decisions = %+v", d)
	}
}

// A fallback keeps the rate, publishes nothing, and says why.
func TestKeepRateRecordsTheFallback(t *testing.T) {
	st := started(t, 1200)
	for fb, want := range map[Fallback]string{
		FallbackTimeout:    "No answer from the AI in time — rate kept at $12.00/h.",
		FallbackUnreadable: "The AI's answer could not be read — rate kept at $12.00/h.",
	} {
		d := keep(t, st, fb, t0.Add(time.Minute))
		if d.Fallback != fb || d.Explanation != want || d.Proposal != nil || d.Reasoning != nil || d.Published != current(t, st.Read()) {
			t.Errorf("%s: %+v", fb, d)
		}
	}
	if n := countRates(t, st.Read()); n != 1 {
		t.Errorf("%d rates; want the first only", n)
	}
	err := st.Tx(context.Background(), func(q store.Querier) error {
		_, err := KeepRate(context.Background(), q, someInputs, "ai_slow", t0)
		return err
	})
	if err == nil {
		t.Error("an unknown fallback was recorded")
	}
}

// Newest first, in pages, with every field, including range changes, after a restart.
func TestListDecisions(t *testing.T) {
	st := started(t, 1200)
	evaluate(t, st, Proposal{RatePerHour: 1400, Reason: "Busy", Reasoning: "Fill less, earn more"}, t0.Add(time.Minute))
	keep(t, st, FallbackTimeout, t0.Add(2*time.Minute))
	if _, err := UpdateSettings(context.Background(), st, "staff_1", withRange(1500, 2400), t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	evaluate(t, st, Proposal{RatePerHour: 1600, Reason: "Live show", Reasoning: "Show"}, t0.Add(4*time.Minute))
	st = storetest.Reopen(t, st)

	all, more := list(t, st.Read(), 0, 10)
	if len(all) != 4 || more {
		t.Fatalf("%d decisions, more %v", len(all), more)
	}
	var rates []int64
	for _, d := range all {
		rates = append(rates, d.Published.Rate.Value())
	}
	if want := []int64{1600, 1500, 1400, 1400}; !slices.Equal(rates, want) {
		t.Errorf("published rates %v, want %v (newest first)", rates, want)
	}
	if d := all[1]; d.Trigger != TriggerRangeChange || d.Staff != "staff_1" || d.Inputs != nil || d.Proposal != nil {
		t.Errorf("range change = %+v", d)
	}
	if d := all[2]; d.Fallback != FallbackTimeout || d.Inputs == nil || d.Proposal != nil || !d.DecidedAt.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("fallback = %+v", d)
	}
	d := all[3]
	if d.Trigger != TriggerEvaluation || *d.Proposal != usd(1400) || *d.Reasoning != "Fill less, earn more" ||
		d.Inputs.Occupancy != 0.25 || *d.Inputs.Liveliness != 0.5 ||
		d.Inputs.Goal != 50 || d.Explanation != "Busy — rate up $2.00 to $14.00/h." || !d.DecidedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("evaluation = %+v %+v", d, *d.Inputs)
	}

	page1, more := list(t, st.Read(), 0, 3)
	page2, more2 := list(t, st.Read(), page1[2].Seq, 3)
	if len(page1) != 3 || !more || len(page2) != 1 || more2 || page2[0].Seq != all[3].Seq {
		t.Errorf("pages: %d %v, %d %v", len(page1), more, len(page2), more2)
	}
	if _, _, err := ListDecisions(context.Background(), st.Read(), 99, 3); !errors.Is(err, ErrUnknownCursor) {
		t.Errorf("unknown cursor: %v", err)
	}
}

func rangeOf(floor, ceiling int64) *Range { return &Range{Floor: usd(floor), Ceiling: usd(ceiling)} }

func sameRange(a, b *Range) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// newest lists the decision log and returns the newest decision.
func newest(t *testing.T, st *store.Store) Decision {
	t.Helper()
	ds, _ := list(t, st.Read(), 0, 1)
	if len(ds) != 1 {
		t.Fatal("no decision")
	}
	return ds[0]
}

// An evaluation keeps the range it was guarded with, though the range changes before it is read.
func TestListDecisionsGivesTheRangeAnEvaluationWasGuardedWith(t *testing.T) {
	st := started(t, 1500)
	if err := update(t, st, withRange(1000, 2000), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	evaluate(t, st, Proposal{RatePerHour: 1500, Reason: "Steady", Reasoning: "r"}, t0.Add(2*time.Minute))
	if d := newest(t, st); !sameRange(d.Range, rangeOf(1000, 2000)) || d.PreviousRange != nil {
		t.Fatalf("range %+v, previous %+v; want 1000-2000 and none", d.Range, d.PreviousRange)
	}
	if err := update(t, st, withRange(1200, 2200), t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ds := decisions(t, st.Read()); len(ds) != 1 {
		t.Fatalf("%d decisions; the change inside the range records none", len(ds))
	}
	st = storetest.Reopen(t, st)
	if d := newest(t, st); !sameRange(d.Range, rangeOf(1000, 2000)) {
		t.Errorf("range %+v after the change; want 1000-2000 still", d.Range)
	}
}

// A settings change timed before an evaluation it did not take part in (a clock set back) does
// not become that evaluation's range.
func TestListDecisionsRangeIgnoresAChangeTimedBefore(t *testing.T) {
	st := started(t, 1500)
	evaluate(t, st, Proposal{RatePerHour: 1500, Reason: "Steady", Reasoning: "r"}, t0.Add(10*time.Minute))
	if err := update(t, st, withRange(1000, 2000), t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err := settings.History(context.Background(), st.Read(), t0.Add(10*time.Minute))
	if err != nil || len(h) != 1 || !h[0].At.Equal(t0.Add(5*time.Minute)) || h[0].Floor != usd(1000) {
		t.Fatalf("history %+v %v; want the change in effect at the evaluation, timed at t0+5m", h, err)
	}
	if d := newest(t, st); !sameRange(d.Range, rangeOf(600, 2400)) {
		t.Errorf("range %+v; want the 600-2400 the evaluation was guarded with", d.Range)
	}
}

// A range change gives the range after it, and the one before it, which an earlier change inside
// the range may have set without a decision.
func TestListDecisionsGivesTheRangesAroundARangeChange(t *testing.T) {
	st := started(t, 1000)
	if err := update(t, st, withRange(950, 2400), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := update(t, st, withRange(900, 2400), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := update(t, st, withRange(1200, 2400), t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ds := decisions(t, st.Read()); len(ds) != 1 {
		t.Fatalf("%d decisions; want the range change that moved the rate only", len(ds))
	}
	d := newest(t, st)
	if d.Trigger != TriggerRangeChange || d.Published.Rate != usd(1200) {
		t.Fatalf("decision %+v; want the range change to 1200", d)
	}
	if !sameRange(d.Range, rangeOf(1200, 2400)) || !sameRange(d.PreviousRange, rangeOf(900, 2400)) {
		t.Errorf("range %+v, previous %+v; want 1200-2400 after 900-2400", d.Range, d.PreviousRange)
	}
}

// A fallback guards nothing: its range is the one in effect when it was decided, or none.
func TestListDecisionsGivesAFallbackTheRangeInEffect(t *testing.T) {
	st := started(t, 1500)
	if err := update(t, st, withRange(1200, 2200), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	keep(t, st, FallbackTimeout, t0.Add(2*time.Minute))
	if d := newest(t, st); !sameRange(d.Range, rangeOf(1200, 2200)) || d.PreviousRange != nil {
		t.Errorf("range %+v, previous %+v; want 1200-2200 and none", d.Range, d.PreviousRange)
	}
	keep(t, st, FallbackUnreadable, t0.Add(-time.Minute)) // a clock set back, before every settings change
	if d := newest(t, st); d.Range != nil {
		t.Errorf("range %+v; want none before the first settings change", d.Range)
	}
}

// A currency change timed before a fallback in the old currency (a clock set back) does not
// become that fallback's range.
func TestListDecisionsGivesAFallbackARangeInItsCurrency(t *testing.T) {
	st := started(t, 1500)
	keep(t, st, FallbackTimeout, t0.Add(2*time.Minute))
	jpy := settings.Settings{
		Currency: money.JPY, Floor: money.New(600, money.JPY), Ceiling: money.New(2400, money.JPY),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: money.New(3000, money.JPY), Goal: 50,
	}
	tx(t, st, func(q store.Querier) error {
		if err := settings.Save(context.Background(), q, jpy, t0.Add(time.Minute)); err != nil {
			return err
		}
		_, _, err := PublishFirst(context.Background(), q, money.New(1000, money.JPY), jpy, t0.Add(time.Minute))
		return err
	})
	if d := newest(t, st); d.Published.Rate != usd(1500) || !sameRange(d.Range, rangeOf(600, 2400)) {
		t.Errorf("fallback %+v, range %+v; want the USD 600-2400 in effect", d.Published, d.Range)
	}
}
