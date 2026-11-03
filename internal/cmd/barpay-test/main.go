// Command barpay-test is BarPay as barpaytest assembles it, served over HTTP for the edge device's
// tests. Besides BarPay's own API it serves three operations for the tests, under /test/: moving
// the fake clock forward, approving a PayPal link in the fake PayPal, and counting the fake
// PayPal's charges.
//
// It takes no argument and reads one environment variable, the edge device's shared secret. It
// listens on a free port of the loopback address and, once it accepts connections, writes one line
// of JSON to stdout:
//
//	{"url":"http://127.0.0.1:PORT","store_app_origin":"...","exit_token_refresh_seconds":30}
//
// It stops when stdin is closed, so it stops with the test that started it even if that test
// cannot stop it, or on SIGINT or SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sumiredc/barpay/internal/barpaytest"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
)

// The store API key and the admin API key are fixed test values: there is no setting for them, so
// a real key cannot be given to the test BarPay.
const (
	storeKey = "test-store-key"
	adminKey = "test-admin-key"
)

const (
	edgeSecretEnv  = "BARPAY_TEST_EDGE_SECRET"
	storeAppOrigin = "http://store.test"
	// The loopback address only, so the operations for the tests reach it from this machine only.
	listenAddress = "127.0.0.1:0"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// ready is the line written to stdout once the test BarPay accepts connections.
type ready struct {
	URL                     string `json:"url"`
	StoreAppOrigin          string `json:"store_app_origin"`
	ExitTokenRefreshSeconds int    `json:"exit_token_refresh_seconds"`
}

// run returns the exit code. Only the reasons it did not start or stopped go to stderr; BarPay's
// logs and the HTTP server's are discarded, so no key or exit token is written anywhere.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("barpay-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(stderr, "barpay-test: takes no argument")
		return 2
	}
	edge := getenv(edgeSecretEnv)
	if edge == "" || edge == storeKey || edge == adminKey {
		fmt.Fprintf(stderr, "barpay-test: %s must be set, to a value other than the test keys\n", edgeSecretEnv)
		return 2
	}
	if err := serve(ctx, secret.New(edge), stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "barpay-test: %v\n", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, edge secret.Value, stdin io.Reader, stdout io.Writer) error {
	ln, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return err
	}
	defer ln.Close()
	url := "http://" + ln.Addr().String()

	dir, err := os.MkdirTemp("", "barpay-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	b, err := barpaytest.New(ctx, barpaytest.Options{
		Dir:       dir,
		Keys:      httpapi.Keys{Store: secret.New(storeKey), Admin: secret.New(adminKey), Edge: edge},
		PublicURL: url, StoreAppOrigin: storeAppOrigin,
	})
	if err != nil {
		return err
	}
	// Runs after the server has stopped, as Close requires.
	defer b.Close()

	// The handlers still running, so BarPay is closed only after the last one has returned.
	var handling sync.WaitGroup
	mux := http.NewServeMux()
	mux.Handle("/", b.Handler)
	control{b}.register(mux)
	// Cancelled when stopping, so a request waiting for an unlock ends instead of holding the stop.
	requests, endRequests := context.WithCancel(context.Background())
	defer endRequests()
	srv := &http.Server{
		Handler:           tracked(mux, &handling),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requests },
		ErrorLog:          log.New(io.Discard, "", 0),
	}

	stdinClosed := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stdin); close(stdinClosed) }()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	if err := json.NewEncoder(stdout).Encode(ready{
		URL: url, StoreAppOrigin: storeAppOrigin, ExitTokenRefreshSeconds: int(barpaytest.ExitTokenRefresh / time.Second),
	}); err != nil {
		_ = srv.Close()
		<-served
		handling.Wait()
		return err
	}

	select {
	case err := <-served:
		_ = srv.Close()
		handling.Wait()
		return err
	case <-ctx.Done():
	case <-stdinClosed:
	}
	endRequests()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_ = srv.Close()
	}
	// Close does not wait for the handlers it cuts off, and their requests are already cancelled,
	// so they return soon; the deferred Close of BarPay must not close the store under them.
	<-served
	handling.Wait()
	return nil
}

func tracked(h http.Handler, handling *sync.WaitGroup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handling.Add(1)
		defer handling.Done()
		h.ServeHTTP(w, r)
	})
}
