// Command barpay is the BarPay server. It reads the config file, assembles the parts in internal/
// and serves the HTTP API until it receives SIGINT or SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/config"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/startup"
	"github.com/sumiredc/barpay/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr, nil))
}

// run returns the exit code. listening, if not nil, is called with the address once the server accepts connections.
func run(ctx context.Context, args []string, stderr io.Writer, listening func(net.Addr)) int {
	flags := flag.NewFlagSet("barpay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", config.DefaultPath, "path of the config file")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if err := serve(ctx, *path, stderr, listening); err != nil {
		fmt.Fprintf(stderr, "barpay: %v\n", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, path string, stderr io.Writer, listening func(net.Addr)) error {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	clk := clock.System()

	cfg, err := config.Load(path, config.Options{})
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}
	st, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}
	defer st.Close()
	state, err := startup.Prepare(ctx, cfg, st, startup.NoSessions{}, clk)
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}
	// From here on, the currency and settings come from state: while a currency change is
	// pending, cfg's currency, starting rate and starting settings are not the ones running.

	router := httpapi.NewRouter(httpapi.Keys{Store: cfg.StoreAPIKey, Admin: cfg.AdminAPIKey, Edge: cfg.EdgeSharedSecret})
	router.Admin("GET /notifications", notify.ListHandler(st, log))
	httpapi.Pricing{Store: st}.Register(router)
	srv := &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}

	notifications := notify.NewDispatcher(st, notify.Options{
		URL: cfg.NotificationURL, Secret: cfg.NotificationSigningSecret, Clock: clk, Log: log,
	})
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatched := make(chan struct{})
	go func() { notifications.Run(dispatchCtx); close(dispatched) }()
	// Stop sending before the store closes; an attempt cut off here is sent again at the next start.
	defer func() { stopDispatch(); <-dispatched }()

	log.Info("BarPay started", "address", ln.Addr().String(), "currency", state.Currency,
		"currency_change_pending", state.CurrencyChangePending)
	if listening != nil {
		listening(ln.Addr())
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("stopped: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("BarPay stopped")
	return nil
}
