package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sumiredc/barpay/cmd/barpay-dev/edge"
)

// The gate's commands. An exit token is at least 16 characters (openapi.yaml's ExitToken), so
// neither is taken for one.
const (
	failOn  = "fail on"
	failOff = "fail off"
)

const (
	// scanTimeout bounds a scan, which may wait for PayPal to answer a charge.
	scanTimeout = time.Minute
	// reportTimeout bounds one report of an unlock's result.
	reportTimeout = 10 * time.Second
	// reportAttempts is how many times a report is sent before giving up; a repeat is safe.
	reportAttempts = 3
	// retryPause is the wait before sending again after BarPay did not answer.
	retryPause = 2 * time.Second
)

// console writes whole lines from several goroutines.
type console struct {
	mu  sync.Mutex
	out io.Writer
}

func (c *console) printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.out, format+"\n", args...)
}

// virtualGate does what the edge device does at the exit gate (edge.yaml), with a gate that
// unlocks unless told to fail.
type virtualGate struct {
	edge *edge.Client
	out  *console
	fail atomic.Bool
}

// runGate scans each exit token read from in, and meanwhile waits for unlocks staff make by
// hand, until ctx ends.
func runGate(ctx context.Context, c *edge.Client, fail bool, in io.Reader, out io.Writer) error {
	g := &virtualGate{edge: c, out: &console{out: out}}
	g.fail.Store(fail)
	g.out.printf("virtual exit gate: paste an exit QR's text to scan it; %q or %q to make unlocking fail or work", failOn, failOff)
	g.showFail()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); g.waitForUnlocks(ctx) }()
	lines := readLines(ctx, in)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case line, ok := <-lines:
			if !ok {
				// Without input the gate still answers unlocks by hand.
				lines = nil
				continue
			}
			g.handle(ctx, line)
		}
	}
}

// readLines sends each trimmed, non-empty line of in until in ends. It returns at once; the
// reading goroutine ends with in, or is left blocked on it when ctx ends first.
func readLines(ctx context.Context, in io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		s := bufio.NewScanner(in)
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			if line == "" {
				continue
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return lines
}

func (g *virtualGate) handle(ctx context.Context, line string) {
	switch line {
	case failOn:
		g.fail.Store(true)
		g.showFail()
	case failOff:
		g.fail.Store(false)
		g.showFail()
	default:
		g.scan(ctx, line)
	}
}

func (g *virtualGate) showFail() {
	if g.fail.Load() {
		g.out.printf("setting: unlocking fails")
	} else {
		g.out.printf("setting: unlocking works")
	}
}

// scan sends a token as the QR reader read it. The gate stays locked unless BarPay says to unlock;
// when no answer comes, the customer scans again, and BarPay answers the second scan with the
// result of the first.
func (g *virtualGate) scan(ctx context.Context, token string) {
	sctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	r, err := g.edge.Scan(sctx, token)
	if err != nil {
		g.out.printf("scan: %v; gate: locked", err)
		return
	}
	g.out.printf("reader: %s", r.ReaderMessage)
	if !r.Unlock {
		g.out.printf("gate: locked")
		return
	}
	g.unlock(ctx, r.UnlockID, "")
}

// unlock opens the gate once, or fails to as set, and reports how it went.
func (g *virtualGate) unlock(ctx context.Context, id, by string) {
	unlocked := !g.fail.Load()
	if unlocked {
		g.out.printf("gate: unlocked%s", by)
	} else {
		g.out.printf("gate: did not unlock%s", by)
	}
	for attempt := 1; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, reportTimeout)
		err := g.edge.ReportUnlock(rctx, id, unlocked)
		cancel()
		var refused *edge.Error
		switch {
		case err == nil:
			return
		case errors.As(err, &refused), attempt == reportAttempts, ctx.Err() != nil:
			g.out.printf("report: %v; BarPay does not know how the unlock went", err)
			return
		}
		select {
		case <-time.After(retryPause):
		case <-ctx.Done():
		}
	}
}

// waitForUnlocks keeps a call to waitForUnlock waiting, which is what makes the gate connected, and
// unlocks for each unlock staff make by hand.
func (g *virtualGate) waitForUnlocks(ctx context.Context) {
	failing := false
	for ctx.Err() == nil {
		wctx, cancel := context.WithTimeout(ctx, edge.UnlockWait+10*time.Second)
		id, err := g.edge.WaitForUnlock(wctx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			if !failing {
				g.out.printf("waiting for unlocks by hand: %v; trying again", err)
				failing = true
			}
			select {
			case <-time.After(retryPause):
			case <-ctx.Done():
			}
			continue
		}
		if failing {
			g.out.printf("waiting for unlocks by hand again")
			failing = false
		}
		if id != "" {
			g.unlock(ctx, id, " by staff")
		}
	}
}
