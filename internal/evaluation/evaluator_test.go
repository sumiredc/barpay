package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/pricing"
)

// A request evaluates, publishes the guarded proposal and answers evaluated: true.
func TestRequestEvaluatesAndPublishes(t *testing.T) {
	f := newFixture(t)
	f.model.proposes(1350)
	r := f.request(t)
	if !r.Evaluated || r.Fallback != nil || r.Rate.RatePerHour.Value != 1350 || r.Rate.Change.Value != 150 {
		t.Errorf("response = %+v; want 1350, evaluated, no fallback", r)
	}
	if got := f.current(t); got != 1350 || f.model.count() != 1 {
		t.Errorf("published %d after %d calls; want 1350 after 1", got, f.model.count())
	}
	d := f.lastDecision(t)
	if d.Trigger != pricing.TriggerEvaluation || d.Proposal.Value() != 1350 || *d.Reasoning != "Serves the goal" ||
		d.Explanation != "Busy room — rate up $1.50 to $13.50/h." || d.Inputs.Goal != 50 {
		t.Errorf("decision = %+v", d)
	}
}

// A proposal beyond the max step publishes the max step, and the explanation states that.
func TestExplanationStatesTheGuardedChange(t *testing.T) {
	f := newFixture(t)
	f.model.proposes(99999)
	if r := f.request(t); r.Rate.RatePerHour.Value != 1500 || r.Rate.Change.Value != 300 {
		t.Fatalf("response = %+v; want 1500, up 300", r)
	}
	d := f.lastDecision(t)
	if d.Proposal.Value() != 99999 || d.Explanation != "Busy room — rate up $3.00 to $15.00/h." {
		t.Errorf("proposal %v, explanation %q", d.Proposal, d.Explanation)
	}
}

// Requests that arrive while an evaluation runs get its result; the model is called once.
func TestConcurrentRequestsShareOneEvaluation(t *testing.T) {
	const n = 8
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.model.set(func(context.Context) (string, error) {
		once.Do(func() { close(entered) })
		<-release
		return `{"rate_per_hour":1400,"reason":"Busy","reasoning":"r"}`, nil
	})
	var joined sync.WaitGroup
	joined.Add(n - 1)
	f.ev.joined = joined.Done

	bodies := make(chan string, n)
	send := func() { bodies <- f.do(http.MethodPost, "/v1/rate-evaluations", storeAuth, "").Body.String() }
	go send()
	<-entered
	for range n - 1 {
		go send()
	}
	joined.Wait()
	close(release)
	for range n {
		var r rateEvaluation
		if err := json.Unmarshal([]byte(<-bodies), &r); err != nil {
			t.Fatal(err)
		}
		if !r.Evaluated || r.Rate.RatePerHour.Value != 1400 {
			t.Errorf("response = %+v; want the shared evaluation's 1400", r)
		}
	}
	if c, d := f.model.count(), len(f.decisions(t)); c != 1 || d != 1 {
		t.Errorf("%d model calls, %d decisions; want 1 each", c, d)
	}
}

// An evaluation outlives the request that started it, and Wait returns once its decision is stored.
func TestWaitOutlastsTheRequest(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.model.set(func(context.Context) (string, error) {
		close(entered)
		<-release
		return `{"rate_per_hour":1300,"reason":"Busy","reasoning":"r"}`, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.ev.Request(ctx); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Request after cancel: %v", err)
	}
	waited := make(chan struct{})
	go func() { f.ev.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("Wait returned while the evaluation was running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-waited
	if d := f.decisions(t); len(d) != 1 || d[0].Published.Rate.Value() != 1300 {
		t.Errorf("decisions after Wait = %+v; want the evaluation's 1300", d)
	}
}

// A request sooner than the minimum interval after the last evaluation started returns the
// current rate without calling the model; exactly the interval later evaluates.
func TestMinimumIntervalFromTheLastStart(t *testing.T) {
	f := newFixture(t)
	f.model.proposes(1300)
	f.request(t)
	f.model.proposes(1500)
	f.clk.Advance(minInterval - time.Nanosecond)
	if r := f.request(t); r.Evaluated || r.Fallback != nil || r.Rate.RatePerHour.Value != 1300 || f.model.count() != 1 {
		t.Errorf("just before the interval: %+v after %d calls; want 1300, not evaluated", r, f.model.count())
	}
	f.clk.Advance(time.Nanosecond)
	if r := f.request(t); !r.Evaluated || r.Rate.RatePerHour.Value != 1500 || f.model.count() != 2 {
		t.Errorf("exactly at the interval: %+v after %d calls; want evaluated to 1500", r, f.model.count())
	}
}

// The interval is read from the stored start, so a restart does not open it early.
func TestMinimumIntervalHoldsAcrossARestart(t *testing.T) {
	f := newFixture(t)
	f.request(t)
	f.restart(t)
	f.clk.Advance(minInterval / 2)
	if r := f.request(t); r.Evaluated || f.model.count() != 1 {
		t.Errorf("after a restart, within the interval: %+v after %d calls", r, f.model.count())
	}
	f.clk.Advance(minInterval / 2)
	if r := f.request(t); !r.Evaluated || f.model.count() != 2 {
		t.Errorf("after a restart, at the interval: %+v after %d calls", r, f.model.count())
	}
}

// No answer within the time limit keeps the rate, says ai_timeout and is logged; the request
// returns instead of waiting on the model, and the fallback counts as the last evaluation.
func TestTimeoutFallsBackAndCountsAsAnEvaluation(t *testing.T) {
	f := newFixture(t)
	f.ev.timeout = 20 * time.Millisecond
	hang := make(chan struct{})
	defer close(hang)
	f.model.set(func(context.Context) (string, error) { <-hang; return "", nil }) // ignores its context
	r := f.request(t)
	if !r.Evaluated || r.Fallback == nil || *r.Fallback != "ai_timeout" || r.Rate.RatePerHour.Value != 1200 {
		t.Errorf("response = %+v; want 1200 kept with ai_timeout", r)
	}
	if d := f.lastDecision(t); d.Fallback != pricing.FallbackTimeout || d.Published.Rate.Value() != 1200 || d.Proposal != nil {
		t.Errorf("decision = %+v", d)
	}
	f.model.proposes(1500)
	f.clk.Advance(minInterval - time.Nanosecond)
	if r := f.request(t); r.Evaluated || f.model.count() != 1 {
		t.Errorf("within the interval after a fallback: %+v after %d calls", r, f.model.count())
	}
	f.clk.Advance(time.Nanosecond)
	if r := f.request(t); !r.Evaluated || r.Fallback != nil || r.Rate.RatePerHour.Value != 1500 {
		t.Errorf("at the interval after a fallback: %+v", r)
	}
}

func TestFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(context.Context) (string, error)
		want   pricing.Fallback
	}{
		"prose": {func(context.Context) (string, error) { return "Raise it a third.", nil }, pricing.FallbackUnreadable},
		"reason only dots": {func(context.Context) (string, error) {
			return `{"rate_per_hour":1300,"reason":"...","reasoning":"x"}`, nil
		}, pricing.FallbackUnreadable},
		"not a completion": {func(context.Context) (string, error) {
			return "", fmt.Errorf("%w: HTTP status 500", llm.ErrUnreadable)
		}, pricing.FallbackUnreadable},
		"unreachable":       {func(context.Context) (string, error) { return "", errors.New("connection refused") }, pricing.FallbackTimeout},
		"context respected": {func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }, pricing.FallbackTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.ev.timeout = 20 * time.Millisecond
			f.model.set(tc.answer)
			r := f.request(t)
			if !r.Evaluated || r.Fallback == nil || *r.Fallback != string(tc.want) || r.Rate.RatePerHour.Value != 1200 || f.current(t) != 1200 {
				t.Errorf("response = %+v; want 1200 kept with %s", r, tc.want)
			}
			if d := f.lastDecision(t); d.Fallback != tc.want || d.Inputs == nil {
				t.Errorf("decision = %+v", d)
			}
		})
	}
}

// Whatever the model answers, the published rate is within the floor and ceiling and one max step
// of the one before, and every evaluation is in the decision log.
func TestAnyModelOutputIsGuarded(t *testing.T) {
	f := newFixture(t)
	f.ev.timeout = 20 * time.Millisecond
	r := rand.New(rand.NewPCG(5, 0))
	junk := []string{"", "{}", "null", "[]", "rate: 1300", `{"rate_per_hour":"1300"}`, "```json\n{\"rate_per_hour\":", "NaN", "∞", "\x00"}
	const rounds = 200
	for range rounds {
		var text string
		switch r.IntN(5) {
		case 0:
			text = junk[r.IntN(len(junk))]
		case 1:
			text = fmt.Sprintf(`{"rate_per_hour":%d,"reason":"r","reasoning":"x"}`, r.Int64N(200_000_000)-50_000_000)
		case 2:
			text = fmt.Sprintf(`{"rate_per_hour":%d,"reason":"r","reasoning":"x"}`, r.Int64N(3000))
		case 3:
			b := make([]byte, r.IntN(40))
			for j := range b {
				b[j] = byte(r.IntN(256))
			}
			text = string(b)
		default:
			text = fmt.Sprintf(`{"rate_per_hour":%g,"reason":"r","reasoning":"x"}`, r.Float64()*3000)
		}
		f.model.says(text)
		before := f.current(t)
		res := f.request(t)
		after := f.current(t)
		if !res.Evaluated || res.Rate.RatePerHour.Value != after || after < 600 || after > 2400 || after-before > 300 || before-after > 300 {
			t.Fatalf("answer %q: %d -> %d, response %+v", text, before, after, res)
		}
		f.clk.Advance(minInterval)
	}
	if n := len(f.decisions(t)); n != rounds {
		t.Errorf("%d decisions for %d evaluations", n, rounds)
	}
}

// The inputs are the signals at the start: occupancy from active sessions, liveliness while it
// is fresh, and the goal; they reach the model and the decision log.
func TestInputs(t *testing.T) {
	f := newFixture(t)
	f.addSessions(t, "active", 2)
	for _, s := range []string{"checking_out", "at_register", "payment_failed", "closed", "closed_unpaid"} {
		f.addSessions(t, s, 1)
	}
	f.request(t)
	in := f.lastDecision(t).Inputs
	if in.Occupancy != 0.5 || in.Liveliness != nil || in.Goal != 50 {
		t.Errorf("inputs = %+v; want occupancy 2/4, no liveliness, goal 50", *in)
	}
	if got := f.model.lastInput(t); got["occupancy"] != 0.5 || got["liveliness"] != nil || got["goal"] != float64(50) {
		t.Errorf("prompt = %v", got)
	}

	f.clk.Advance(minInterval)
	if w := f.do(http.MethodPost, "/v1/edge/signals", edgeAuth, `{"liveliness":0.8}`); w.Code != http.StatusNoContent {
		t.Fatal(w.Body)
	}
	f.request(t)
	in = f.lastDecision(t).Inputs
	if in.Liveliness == nil || *in.Liveliness != 0.8 {
		t.Errorf("inputs = %+v; want liveliness 0.8", *in)
	}
	if p := f.model.lastInput(t); p["liveliness"] != 0.8 {
		t.Errorf("prompt = %v", p)
	}

	f.clk.Advance(minInterval) // the liveliness is exactly a minute old: no longer used
	f.request(t)
	in = f.lastDecision(t).Inputs
	if in.Liveliness != nil {
		t.Errorf("inputs = %+v; want no liveliness", *in)
	}
}

// The signals the model is given are occupancy, liveliness and the goal only: staff events are
// no longer an input, so the prompt neither describes nor carries them.
func TestInputsAreOccupancyLivelinessAndGoalOnly(t *testing.T) {
	f := newFixture(t)
	f.request(t)
	var keys []string
	for k := range f.model.lastInput(t) {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"ceiling", "currency", "current_rate_per_hour", "floor", "goal", "liveliness", "max_step", "occupancy", "rate_step", "unit"}
	if !slices.Equal(keys, want) {
		t.Errorf("user message fields = %v, want %v", keys, want)
	}
	if strings.Contains(systemPrompt, "staff") {
		t.Error("the system prompt still describes staff events")
	}
}

// Liveliness just under a minute old is used.
func TestInputsUseLivelinessJustBeforeItGoesStale(t *testing.T) {
	f := newFixture(t)
	if w := f.do(http.MethodPost, "/v1/edge/signals", edgeAuth, `{"liveliness":0.3}`); w.Code != http.StatusNoContent {
		t.Fatal(w.Body)
	}
	f.clk.Advance(time.Minute - time.Nanosecond)
	f.request(t)
	if in := f.lastDecision(t).Inputs; in.Liveliness == nil || *in.Liveliness != 0.3 {
		t.Errorf("inputs = %+v; want liveliness 0.3", *in)
	}
}

// Only the store API key asks for an evaluation.
func TestRequestRateEvaluationAccess(t *testing.T) {
	f := newFixture(t)
	for name, auth := range map[string]string{"no key": "", "admin API key": adminAuth, "edge shared secret": edgeAuth} {
		w := f.do(http.MethodPost, "/v1/rate-evaluations", auth, "")
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "unauthorized") {
			t.Errorf("%s: %d %s, want 401", name, w.Code, w.Body)
		}
	}
	if f.model.count() != 0 || len(f.decisions(t)) != 0 {
		t.Error("a refused request evaluated")
	}
}
