package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/cmd/barpay-dev/edge"
)

// latestLiveliness is the liveliness in the inputs of the newest decision in the decision log.
func (b *barpay) latestLiveliness(t *testing.T) *float64 {
	t.Helper()
	var log struct {
		Decisions []struct {
			Inputs *struct {
				Liveliness *float64 `json:"liveliness"`
			} `json:"inputs"`
		} `json:"decisions"`
	}
	b.must(t, http.StatusOK, http.MethodGet, "/v1/admin/decisions", adminKey, "", &log)
	if len(log.Decisions) == 0 || log.Decisions[0].Inputs == nil {
		t.Fatalf("no decision with inputs: %+v", log.Decisions)
	}
	return log.Decisions[0].Inputs.Liveliness
}

func (b *barpay) evaluate(t *testing.T) {
	t.Helper()
	var ev struct {
		Evaluated bool `json:"evaluated"`
	}
	b.must(t, http.StatusOK, http.MethodPost, "/v1/rate-evaluations", storeKey, "", &ev)
	if !ev.Evaluated {
		t.Fatal("the rate was not evaluated")
	}
}

// Pricing model, Signals: the liveliness the simulator sends is in the inputs of the next
// evaluation, and a value typed in replaces it.
func TestSimulatedLivelinessIsInTheNextEvaluation(t *testing.T) {
	b := newBarPay(t)
	s := start(t, b, "signals", "-liveliness", "0.73")
	s.expect(t, "liveliness 0.73 sent")
	b.evaluate(t)
	if l := b.latestLiveliness(t); l == nil || *l != 0.73 {
		t.Errorf("liveliness in the decision log = %v, want 0.73", l)
	}

	b.clk.Advance(time.Minute) // the minimum evaluation interval
	s.typeLine(t, "0.2")
	s.expect(t, "liveliness 0.2 sent")
	b.evaluate(t)
	if l := b.latestLiveliness(t); l == nil || *l != 0.2 {
		t.Errorf("liveliness in the decision log = %v, want 0.2", l)
	}
}

// A value outside 0 to 1 is not sent; the simulator keeps sending the last one.
func TestSimulatorRefusesLivelinessOutsideZeroToOne(t *testing.T) {
	b := newBarPay(t)
	s := start(t, b, "signals", "-liveliness", "0.5")
	s.expect(t, "liveliness 0.5 sent")
	n := len(b.devRequests())
	s.typeLine(t, "1.5")
	s.expect(t, `"1.5" is not a number from 0 to 1; still sending 0.5`)
	if got := len(b.devRequests()); got != n {
		t.Errorf("%d requests sent for 1.5, want none", got-n)
	}
}

// fakeClock is a clock that moves only when the test advances it. A ticker fires when the clock
// reaches its next tick, at most once per Advance, as time.Ticker drops ticks nobody took.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

type fakeTicker struct {
	clk     *fakeClock
	c       chan time.Time
	every   time.Duration
	next    time.Time
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 5, 1, 20, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) NewTicker(d time.Duration) signalTicker {
	f.mu.Lock()
	defer f.mu.Unlock()
	tk := &fakeTicker{clk: f, c: make(chan time.Time, 1), every: d, next: f.now.Add(d)}
	f.tickers = append(f.tickers, tk)
	return tk
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for _, tk := range f.tickers {
		if tk.stopped || tk.next.After(f.now) {
			continue
		}
		select {
		case tk.c <- f.now:
		default:
		}
		for !tk.next.After(f.now) {
			tk.next = tk.next.Add(tk.every)
		}
	}
}

func (tk *fakeTicker) C() <-chan time.Time { return tk.c }

func (tk *fakeTicker) Reset(d time.Duration) {
	tk.clk.mu.Lock()
	defer tk.clk.mu.Unlock()
	tk.every, tk.next = d, tk.clk.now.Add(d)
}

func (tk *fakeTicker) Stop() {
	tk.clk.mu.Lock()
	defer tk.clk.mu.Unlock()
	tk.stopped = true
}

// readCounter is an input that counts how often it is read.
type readCounter struct {
	r     io.Reader
	reads atomic.Int64
}

func (rc *readCounter) Read(p []byte) (int, error) {
	rc.reads.Add(1)
	return rc.r.Read(p)
}

// simulate runs the signal simulator against b on clk until the test ends.
func simulate(t *testing.T, b *barpay, clk *fakeClock, o signalsOptions, in io.Reader) *tool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tl := &tool{out: &syncBuffer{}, code: make(chan int, 1), stop: cancel}
	go func() {
		code := 0
		if err := runSignals(ctx, edge.New(b.devURL, edgeSecret), clk, o, in, tl.out); err != nil {
			fmt.Fprintln(tl.out, err)
			code = 1
		}
		tl.code <- code
	}()
	t.Cleanup(func() {
		cancel()
		if c := <-tl.code; c != 0 {
			t.Errorf("the simulator ended with an error:\n%s", tl.out)
		}
	})
	return tl
}

// sent waits until the simulator has sent n signals to b, answered or not, and returns the
// liveliness of each.
func (b *barpay) sent(t *testing.T, n int) []float64 {
	t.Helper()
	giveUpAt := time.Now().Add(10 * time.Second)
	for {
		var got []float64
		for _, r := range b.devRequests() {
			if r.Method != http.MethodPost || r.Path != "/v1/edge/signals" {
				continue
			}
			var s struct {
				Liveliness float64 `json:"liveliness"`
			}
			if err := json.Unmarshal([]byte(r.Body), &s); err != nil {
				t.Fatalf("signals body %q: %v", r.Body, err)
			}
			got = append(got, s.Liveliness)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(giveUpAt) {
			t.Fatalf("%d signals sent, want %d", len(got), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// sendFor moves clk on one send interval at a time, each once the send before it has reached b,
// so that each value is the one for the time the test set. It returns the n values sent, the
// first of them at the start.
func (b *barpay) sendFor(t *testing.T, clk *fakeClock, n int) []float64 {
	t.Helper()
	for i := 1; i < n; i++ {
		b.sent(t, i)
		clk.Advance(sendInterval)
	}
	return b.sent(t, n)
}

// With -vary the liveliness moves up and down by the swing around -liveliness, once per period,
// and is back where it started after one period.
func TestVaryingLivelinessMovesAroundTheCentreOncePerPeriod(t *testing.T) {
	b := newBarPay(t)
	clk := newFakeClock()
	o := signalsOptions{liveliness: 0.4, vary: true, period: 30 * time.Minute, swing: 0.3}
	simulate(t, b, clk, o, strings.NewReader(""))
	n := int(o.period/sendInterval) + 1 // one period, both ends included
	got := b.sendFor(t, clk, n)
	lo, hi := got[0], got[0]
	for _, v := range got {
		lo, hi = min(lo, v), max(hi, v)
	}
	if got[0] != 0.4 || got[n-1] != 0.4 {
		t.Errorf("liveliness at the start and after one period = %v and %v, want 0.4 (the centre)", got[0], got[n-1])
	}
	if lo != 0.1 || hi != 0.7 {
		t.Errorf("liveliness over one period went from %v to %v, want 0.1 to 0.7", lo, hi)
	}
}

// Every value sent with -vary is from 0 to 1, even when the centre and the swing reach past them.
func TestVaryingLivelinessStaysFromZeroToOne(t *testing.T) {
	for _, centre := range []float64{0, 1} {
		t.Run(fmt.Sprint(centre), func(t *testing.T) {
			b := newBarPay(t)
			clk := newFakeClock()
			o := signalsOptions{liveliness: centre, vary: true, period: 10 * time.Minute, swing: 1}
			simulate(t, b, clk, o, strings.NewReader(""))
			changed := false
			for _, v := range b.sendFor(t, clk, int(o.period/sendInterval)+1) {
				if !validLiveliness(v) {
					t.Errorf("sent liveliness %v, want it from 0 to 1", v)
				}
				changed = changed || v != centre
			}
			if !changed {
				t.Errorf("the liveliness stayed at %v", centre)
			}
		})
	}
}

// With -vary the simulator reads no input, so a typed value is never sent, and it says so when it
// starts.
func TestVaryingLivelinessIgnoresInput(t *testing.T) {
	b := newBarPay(t)
	clk := newFakeClock()
	in := &readCounter{r: strings.NewReader("0.95\n0.95\n")}
	o := signalsOptions{liveliness: 0.5, vary: true, period: 30 * time.Minute, swing: 0.3}
	s := simulate(t, b, clk, o, in)
	s.expect(t, "signal simulator: sending a liveliness that moves by up to 0.3 around 0.5 once every 30m0s, every 10s; input is not read")
	for _, v := range b.sendFor(t, clk, 6) {
		if v == 0.95 {
			t.Errorf("sent the typed liveliness 0.95")
		}
	}
	if r := in.reads.Load(); r != 0 {
		t.Errorf("read the input %d times, want none", r)
	}
}

// The flags reach the simulator: -vary starts it at the centre, saying it reads no input.
func TestVaryFlagStartsTheVaryingSimulator(t *testing.T) {
	b := newBarPay(t)
	s := start(t, b, "signals", "-vary", "-liveliness", "0.6", "-period", "45m", "-swing", "0.2")
	s.expect(t,
		"signal simulator: sending a liveliness that moves by up to 0.2 around 0.6 once every 45m0s, every 10s; input is not read",
		"liveliness 0.6 sent")
}

// The period and the swing, like -liveliness, are refused when they cannot be, and without -vary,
// where they would do nothing.
func TestVaryFlagsOutsideTheirRangeAreRefused(t *testing.T) {
	b := newBarPay(t)
	env := func(k string) string {
		if k == secretEnv {
			return edgeSecret
		}
		return ""
	}
	for _, args := range [][]string{
		{"-vary", "-period", "0s"},
		{"-vary", "-period", "-1m"},
		{"-vary", "-swing", "-0.1"},
		{"-vary", "-swing", "1.1"},
		{"-vary", "-swing", "NaN"},
		{"-vary", "-liveliness", "1.5"},
		{"-period", "45m"},
		{"-swing", "0.2"},
		{"-swing", "5"},
	} {
		var out syncBuffer
		c := run(context.Background(), append([]string{"signals", "-barpay", b.devURL}, args...), env, strings.NewReader(""), &out, &out)
		if c != 1 {
			t.Errorf("%v: exit code %d, want 1:\n%s", args, c, &out)
		}
	}
	if r := b.devRequests(); len(r) != 0 {
		t.Errorf("sent %d requests with a value out of range", len(r))
	}
}

func TestUsageNamesTheVaryFlags(t *testing.T) {
	var out syncBuffer
	if c := run(context.Background(), nil, func(string) string { return "" }, strings.NewReader(""), &out, &out); c != 2 {
		t.Fatalf("exit code %d, want 2", c)
	}
	for _, f := range []string{"-vary", "-period", "-swing"} {
		if !strings.Contains(out.String(), f) {
			t.Errorf("usage does not name %s:\n%s", f, &out)
		}
	}
}

// Pricing model, Signals: after a send fails the simulator sends at the next interval, the value
// for then rather than the one that failed, and goes on once BarPay answers again.
func TestSimulatorSendsAtTheNextIntervalAfterAFailure(t *testing.T) {
	for _, o := range []signalsOptions{
		{liveliness: 0.5},
		{liveliness: 0.5, vary: true, period: 30 * time.Minute, swing: 0.3},
	} {
		t.Run(fmt.Sprintf("vary=%v", o.vary), func(t *testing.T) {
			b := newBarPay(t)
			b.down.Store(true)
			clk := newFakeClock()
			s := simulate(t, b, clk, o, strings.NewReader(""))
			s.expect(t, "liveliness 0.5 not sent: BarPay answered 503")
			b.sendFor(t, clk, 3) // two more fail
			b.down.Store(false)
			clk.Advance(sendInterval)
			got := b.sent(t, 4)
			s.expect(t, "liveliness "+strconv.FormatFloat(got[3], 'f', -1, 64)+" sent")
			if o.vary && got[3] == got[0] {
				t.Errorf("sent %v again after BarPay came back, want the value for 30 seconds later", got[0])
			}
			if !o.vary && got[3] != 0.5 {
				t.Errorf("sent %v after BarPay came back, want 0.5", got[3])
			}
		})
	}
}
