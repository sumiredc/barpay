package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/sumiredc/barpay/cmd/barpay-dev/edge"
)

// sendInterval is how often the edge device sends its signals (edge.yaml's sendSignals).
const sendInterval = 10 * time.Second

// sendTimeout bounds one send; the next one comes at the next interval.
const sendTimeout = 5 * time.Second

// runSignals sends liveliness now and every sendInterval, until ctx ends. A number read from in
// replaces it and is sent at once. Only the liveliness is simulated: occupancy comes from the
// sessions, and staff events come through the admin API, which the tools do not hold.
func runSignals(ctx context.Context, c *edge.Client, liveliness float64, in io.Reader, out io.Writer) error {
	if !validLiveliness(liveliness) {
		return fmt.Errorf("-liveliness %v is not a number from 0 to 1", liveliness)
	}
	con := &console{out: out}
	con.printf("signal simulator: sending the liveliness every %s; type a number from 0 to 1 to change it", sendInterval)
	send := func() {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		defer cancel()
		if err := c.SendLiveliness(sctx, liveliness); err != nil {
			con.printf("liveliness %s not sent: %v", strconv.FormatFloat(liveliness, 'f', -1, 64), err)
			return
		}
		con.printf("liveliness %s sent", strconv.FormatFloat(liveliness, 'f', -1, 64))
	}
	send()
	tick := time.NewTicker(sendInterval)
	defer tick.Stop()
	lines := readLines(ctx, in)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			send()
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			v, err := strconv.ParseFloat(line, 64)
			if err != nil || !validLiveliness(v) {
				con.printf("%q is not a number from 0 to 1; still sending %s", line, strconv.FormatFloat(liveliness, 'f', -1, 64))
				continue
			}
			liveliness = v
			send()
			tick.Reset(sendInterval)
		}
	}
}

// validLiveliness is edge.yaml's Signals.liveliness. NaN fails both comparisons.
func validLiveliness(v float64) bool { return v >= 0 && v <= 1 }
