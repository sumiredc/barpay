// Command mobile-order is a sample store app: a bar's mobile order system that uses BarPay only
// through the API and the notifications of docs/api/openapi.yaml. Its backend holds the store
// API key and the admin API key and draws every screen; the browser never talks to BarPay except
// when it is sent to PayPal and back.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// evaluationInterval is how often the app asks BarPay to evaluate the rate (PRD: Responsibilities,
// Rate evaluation). BarPay never evaluates on its own.
const evaluationInterval = 5 * time.Minute

type App struct {
	cfg       *Config
	barpay    *BarPay
	store     *Store
	pages     *pages
	dummyHash string
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		os.Exit(hashPasswordCommand())
	}
	configPath := flag.String("config", "mobile-order.conf", "path of the config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("not started: %v", err)
	}
	st, err := openStore(cfg.DataPath)
	if err != nil {
		log.Fatalf("not started: %v", err)
	}
	app := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages(), dummyHash: hashPassword(randomToken(16))}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.evaluateRate(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s, open %s", cfg.ListenAddress, cfg.PublicURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// hashPasswordCommand reads a password from standard input and prints the hash a staff line of
// the config file takes.
func hashPasswordCommand() int {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	password := strings.TrimRight(line, "\r\n")
	if (err != nil && password == "") || password == "" {
		fmt.Fprintln(os.Stderr, "no password read")
		return 1
	}
	fmt.Println(hashPassword(password))
	return 0
}

// evaluateRate asks BarPay for a rate evaluation now and every evaluationInterval.
func (a *App) evaluateRate(ctx context.Context) {
	ticker := time.NewTicker(evaluationInterval)
	defer ticker.Stop()
	for {
		req, cancel := context.WithTimeout(ctx, time.Minute)
		r, err := a.barpay.requestRateEvaluation(req)
		cancel()
		switch {
		case err != nil:
			log.Printf("rate evaluation: %v", err)
		case r.Fallback != nil:
			log.Printf("rate evaluation: kept %s (%s)", formatMoney(r.Rate.RatePerHour), *r.Fallback)
		case r.Evaluated:
			log.Printf("rate evaluation: %s", formatMoney(r.Rate.RatePerHour))
		default:
			log.Printf("rate evaluation: too soon after the last one, rate %s", formatMoney(r.Rate.RatePerHour))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", staticFiles())
	mux.HandleFunc("GET /{$}", a.landing)
	mux.HandleFunc("GET /signup", a.signupForm)
	mux.HandleFunc("POST /signup", a.signup)
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("POST /guest", a.joinAsGuest)
	mux.HandleFunc("POST /checkin", a.checkin)
	mux.HandleFunc("GET /menu", a.menu)
	mux.HandleFunc("POST /orders", a.placeOrder)
	mux.HandleFunc("POST /orders/resend", a.resendOrder)
	mux.HandleFunc("GET /panel", a.panel)
	mux.HandleFunc("POST /pay/register", a.payAtRegister)
	mux.HandleFunc("GET /exit", a.exitQR)
	mux.HandleFunc("GET /receipts", a.receipts)
	mux.HandleFunc("GET /receipts/{session_id}", a.receipt)
	mux.HandleFunc("POST /visit/done", a.visitDone)
	mux.HandleFunc("GET /account", a.account)
	mux.HandleFunc("POST /account/name", a.saveName)
	mux.HandleFunc("GET /account/paypal", a.paypal)
	mux.HandleFunc("POST /account/paypal/link", a.linkPaypal)
	mux.HandleFunc("GET /account/paypal/return", a.paypalReturn)
	mux.HandleFunc("POST /account/paypal/delete", a.deletePaypal)

	mux.HandleFunc("GET /admin/login", a.staffLoginForm)
	mux.HandleFunc("POST /admin/login", a.staffLoginSubmit)
	mux.HandleFunc("POST /admin/logout", a.staffLogout)
	mux.HandleFunc("GET /admin", a.staffOnly(a.adminSessions))
	mux.HandleFunc("POST /admin/sessions/{session_id}/{action}", a.staffOnly(a.adminSessionAction))
	mux.HandleFunc("POST /admin/gate", a.staffOnly(a.adminUnlockGate))
	mux.HandleFunc("GET /admin/orders", a.staffOnly(a.adminOrders))
	mux.HandleFunc("POST /admin/sessions/{session_id}/orders/{order_id}/served", a.staffOnly(a.adminMarkServed))
	mux.HandleFunc("GET /admin/pricing", a.staffOnly(a.adminPricing))
	mux.HandleFunc("POST /admin/evaluate", a.staffOnly(a.adminEvaluate))
	mux.HandleFunc("GET /admin/settings", a.staffOnly(a.adminSettings))
	mux.HandleFunc("POST /admin/settings", a.staffOnly(a.adminUpdateSettings))
	mux.HandleFunc("GET /admin/alerts", a.staffOnly(a.adminAlerts))
	mux.HandleFunc("GET /admin/notifications", a.staffOnly(a.adminNotifications))

	mux.HandleFunc("POST /barpay/notifications", a.handleNotification)
	return withHeaders(mux)
}

// withHeaders sets the headers every page needs and logs each request by its path only: the
// PayPal return carries a one-time code in its query, which must not reach the log.
func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
