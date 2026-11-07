package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/sumiredc/barpay/cmd/barpay-dev/edge"
)

// sendInterval is how often the edge device sends its signals (edge.yaml's sendSignals).
const sendInterval = 10 * time.Second

// sendTimeout bounds one send; the next one comes at the next interval.
const sendTimeout = 5 * time.Second

// signalsOptions is what the simulator is started with.
type signalsOptions struct {
	// liveliness is the value sent, or with vary the centre it moves around.
	liveliness float64
	// vary moves the liveliness by itself, by up to swing either side of the centre, once every
	// period, instead of taking new values from the input.
	vary   bool
	period time.Duration
	swing  float64
}

// signalClock is where the simulator takes the time from.
type signalClock interface {
	Now() time.Time
	NewTicker(d time.Duration) signalTicker
}

// signalTicker is the part of time.Ticker the simulator uses.
type signalTicker interface {
	C() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTicker(d time.Duration) signalTicker { return systemTicker{time.NewTicker(d)} }

type systemTicker struct{ *time.Ticker }

func (t systemTicker) C() <-chan time.Time { return t.Ticker.C }

// runSignals sends the liveliness now and every sendInterval, until ctx ends. Without o.vary, a
// number read from in replaces it and is sent at once; with o.vary, in is not read. A send that
// fails is not repeated: the next interval sends the value for then. Only the liveliness is
// simulated: occupancy comes from the sessions.
func runSignals(ctx context.Context, c *edge.Client, clk signalClock, o signalsOptions, in io.Reader, out io.Writer) error {
	if err := o.check(); err != nil {
		return err
	}
	con := &console{out: out}
	liveliness := func(time.Time) float64 { return o.liveliness }
	if o.vary {
		con.printf("signal simulator: sending a liveliness that moves by up to %s around %s once every %s, every %s; input is not read",
			formatLiveliness(o.swing), formatLiveliness(o.liveliness), o.period, sendInterval)
		started := clk.Now()
		liveliness = func(now time.Time) float64 { return o.varied(now.Sub(started)) }
		in = nil
	} else {
		con.printf("signal simulator: sending the liveliness every %s; type a number from 0 to 1 to change it", sendInterval)
	}
	send := func() {
		v := liveliness(clk.Now())
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		defer cancel()
		if err := c.SendLiveliness(sctx, v); err != nil {
			con.printf("liveliness %s not sent: %v", formatLiveliness(v), err)
			return
		}
		con.printf("liveliness %s sent", formatLiveliness(v))
	}
	// Made before the first send, so the ticks keep to the interval however long a send takes.
	tick := clk.NewTicker(sendInterval)
	defer tick.Stop()
	send()
	var lines <-chan string
	if in != nil {
		lines = readLines(ctx, in)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C():
			send()
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			v, err := strconv.ParseFloat(line, 64)
			if err != nil || !validLiveliness(v) {
				con.printf("%q is not a number from 0 to 1; still sending %s", line, formatLiveliness(o.liveliness))
				continue
			}
			o.liveliness = v
			send()
			tick.Reset(sendInterval)
		}
	}
}

func (o signalsOptions) check() error {
	if !validLiveliness(o.liveliness) {
		return fmt.Errorf("-liveliness %v is not a number from 0 to 1", o.liveliness)
	}
	if !o.vary {
		return nil
	}
	if o.period <= 0 {
		return fmt.Errorf("-period %s is not longer than 0", o.period)
	}
	if !validLiveliness(o.swing) {
		return fmt.Errorf("-swing %v is not a number from 0 to 1", o.swing)
	}
	return nil
}

// varied is the liveliness at elapsed since the start: a sine wave, so it starts at the centre and
// passes every value between its lowest and highest, kept from 0 to 1 and to two decimals so the
// output stays readable.
func (o signalsOptions) varied(elapsed time.Duration) float64 {
	v := o.liveliness + o.swing*math.Sin(2*math.Pi*float64(elapsed)/float64(o.period))
	return min(max(math.Round(v*100)/100, 0), 1)
}

// validLiveliness is edge.yaml's Signals.liveliness. NaN fails both comparisons.
func validLiveliness(v float64) bool { return v >= 0 && v <= 1 }

func formatLiveliness(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
