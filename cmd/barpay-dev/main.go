// Command barpay-dev holds the development tools that stand in for the edge device's hardware (the
// PRD's Fixed constraints, Working without hardware): the virtual exit gate and the signal
// simulator. They call BarPay only through the edge device's API, with the edge device's shared
// secret from the environment, and are not part of a production build.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sumiredc/barpay/cmd/barpay-dev/edge"
)

// secretEnv names the environment variable holding the edge device's shared secret. The tools
// read nothing else for credentials, and never BarPay's config file, which holds the store API
// key and the admin API key.
const secretEnv = "BARPAY_EDGE_SHARED_SECRET"

const usage = `Usage:
  barpay-dev gate [-barpay URL] [-fail-unlock]
      The virtual exit gate. Type or paste the text of an exit QR, one per line, to scan it.
      "fail on" makes the gate fail to unlock from then on, "fail off" makes it work again.
      Unlocks staff make by hand through the admin API arrive while it runs.
  barpay-dev signals [-barpay URL] [-liveliness N] [-vary [-period D] [-swing W]]
      The signal simulator. Sends the liveliness N (0 quiet to 1 loudest) every 10 seconds.
      Type another number, one per line, to send it at once and from then on.
      With -vary it reads no input and moves the liveliness by itself, up to W above and below N
      and back once every D (0.3 and 30m unless given), kept from 0 to 1.

The edge device's shared secret is read from ` + secretEnv + `.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// run returns the exit code. It ends when ctx ends.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("barpay-dev "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	base := flags.String("barpay", "http://127.0.0.1:8080", "BarPay's base URL")
	var start func(*edge.Client) error
	switch args[0] {
	case "gate":
		fail := flags.Bool("fail-unlock", false, "start with the gate failing to unlock")
		start = func(c *edge.Client) error { return runGate(ctx, c, *fail, stdin, stdout) }
	case "signals":
		liveliness := flags.Float64("liveliness", 0.5, "the liveliness to send, from 0 to 1, or with -vary the centre")
		vary := flags.Bool("vary", false, "move the liveliness by itself instead of reading new values")
		period := flags.Duration("period", 30*time.Minute, "with -vary, how long the liveliness takes to go up, down and back")
		swing := flags.Float64("swing", 0.3, "with -vary, how far the liveliness goes either side of the centre, from 0 to 1")
		start = func(c *edge.Client) error {
			o := signalsOptions{liveliness: *liveliness, vary: *vary, period: *period, swing: *swing}
			return runSignals(ctx, c, systemClock{}, o, stdin, stdout)
		}
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	secret := getenv(secretEnv)
	if secret == "" {
		fmt.Fprintf(stderr, "barpay-dev: set %s to the edge device's shared secret\n", secretEnv)
		return 2
	}
	if err := start(edge.New(*base, secret)); err != nil {
		fmt.Fprintf(stderr, "barpay-dev: %v\n", err)
		return 1
	}
	return 0
}
