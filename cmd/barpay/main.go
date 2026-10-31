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
	"github.com/sumiredc/barpay/internal/evaluation"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/member"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/signals"
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
	state, err := startup.Prepare(ctx, cfg, st, session.OpenSessions{}, clk)
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}
	// From here on, the currency and settings come from state: while a currency change is
	// pending, cfg's currency, starting rate and starting settings are not the ones running.

	notifications := notify.NewDispatcher(st, notify.Options{
		URL: cfg.NotificationURL, Secret: cfg.NotificationSigningSecret, Clock: clk, Log: log,
	})
	router := httpapi.NewRouter(httpapi.Keys{Store: cfg.StoreAPIKey, Admin: cfg.AdminAPIKey, Edge: cfg.EdgeSharedSecret})
	router.Admin("GET /notifications", notify.ListHandler(st, log))
	httpapi.Pricing{Store: st, Clock: clk}.Register(router)
	pp := paypal.New(paypal.Options{
		BaseURL: paypal.SandboxURL, ClientID: cfg.PayPalClientID, ClientSecret: cfg.PayPalClientSecret, Clock: clk, Log: log,
	})
	sessions := session.API{
		Store: st, Clock: clk, Keys: idempotency.New(st, clk), Notify: notifications,
		PayPal: pp, Saved: member.Saved{}, Gate: gate.New(),
		ExitTokenRefresh: cfg.ExitTokenRefresh, ReunlockWindow: cfg.ReunlockWindow, MaxStay: cfg.MaxStay, Log: log,
	}
	sessions.Register(router)
	members := member.API{
		Store: st, Clock: clk, Notify: notifications, PublicURL: cfg.PublicURL, StoreAppOrigin: cfg.StoreAppOrigin, PayPal: pp,
	}
	members.Register(router)
	signals.API{Store: st, Clock: clk, Log: log}.Register(router)
	ev := evaluation.New(evaluation.Options{
		Store: st, Clock: clk, Model: llm.New(cfg.LLMAddress, cfg.LLMModel, cfg.LLMReasoningEffort), MaxStep: state.MaxStep,
		MinInterval: cfg.MinEvaluationInterval, Capacity: cfg.Capacity, Log: log,
	})
	// Runs before the store closes: an evaluation whose requests have gone still stores its decision.
	defer ev.Wait()
	evaluation.API{Evaluator: ev}.Register(router)
	srv := &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("not started: %w", err)
	}

	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatched := make(chan struct{})
	go func() { notifications.Run(dispatchCtx); close(dispatched) }()
	// Stop sending before the store closes; an attempt cut off here is sent again at the next start.
	defer func() { stopDispatch(); <-dispatched }()

	limitsCtx, stopLimits := context.WithCancel(context.Background())
	limitsDone := make(chan struct{})
	go func() { sessions.Run(limitsCtx, log); close(limitsDone) }()
	// Stop before the store closes; what was cut off here is due again at the next start.
	defer func() { stopLimits(); <-limitsDone }()

	membersCtx, stopMembers := context.WithCancel(context.Background())
	membersDone := make(chan struct{})
	go func() { members.Run(membersCtx, log); close(membersDone) }()
	// Stop checking and deleting before the store closes; what was cut off is due again at the next start.
	defer func() { stopMembers(); <-membersDone }()

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
	// Long enough for a request waiting on an evaluation, which the AI's time limit bounds.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), evaluation.ModelTimeout+10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("BarPay stopped")
	return nil
}
