package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/evaluation"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/member"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/notify"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/session"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/signals"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

// The keys of the BarPay under test. The development tools are given the edge secret only.
const (
	storeKey   = "k-store"
	adminKey   = "k-admin"
	edgeSecret = "k-edge-0123456789"

	publicURL = "https://barpay.test"
	origin    = "https://store.test"
)

var t0 = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)

// fakeModel is the local LLM, proposing $13.00 per hour to every evaluation.
type fakeModel struct{}

func (fakeModel) Chat(context.Context, []llm.Message) (string, error) {
	return `{"rate_per_hour":1300,"reason":"Busy","reasoning":"r"}`, nil
}

// devRequest is one request the development tools sent to BarPay.
type devRequest struct {
	Method, Path string
	Header       http.Header
}

// barpay is BarPay assembled from internal/ as cmd/barpay assembles it, with a fake PayPal, a
// fake LLM and a fake clock starting at t0. The development tools reach it over HTTP at devURL,
// where every request they send is recorded; the test drives the store API and the admin API
// directly, as the store app would.
type barpay struct {
	st     *store.Store
	clk    *clock.Fake
	pp     *paypaltest.Fake
	router *httpapi.Router
	devURL string

	mu  sync.Mutex
	dev []devRequest
}

func newBarPay(t *testing.T) *barpay {
	t.Helper()
	ctx := context.Background()
	b := &barpay{st: storetest.Open(t), clk: clock.NewFake(t0), pp: paypaltest.New()}
	usd := func(v int64) money.Amount { return money.New(v, money.USD) }
	s := settings.Settings{
		Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
		BlockMinutes: 10, PauseMinutes: 30, CapPerVisit: usd(3000), Goal: 50,
	}
	if err := b.st.Tx(ctx, func(tx store.Querier) error {
		if err := settings.Save(ctx, tx, s, t0); err != nil {
			return err
		}
		_, _, err := pricing.PublishFirst(ctx, tx, usd(1200), s, t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	notifications := notify.NewDispatcher(b.st, notify.Options{
		URL: "https://store.test/notify", Secret: secret.New("sig"), Clock: b.clk, Log: log,
	})
	b.router = httpapi.NewRouter(httpapi.Keys{Store: secret.New(storeKey), Admin: secret.New(adminKey), Edge: secret.New(edgeSecret)})
	b.router.Admin("GET /notifications", notify.ListHandler(b.st, log))
	httpapi.Pricing{Store: b.st, Clock: b.clk}.Register(b.router)
	session.API{
		Store: b.st, Clock: b.clk, Keys: idempotency.New(b.st, b.clk), Notify: notifications,
		PayPal: b.pp, Saved: member.Saved{}, Gate: gate.New(),
		ExitTokenRefresh: 30 * time.Second, ReunlockWindow: 2 * time.Minute, MaxStay: 12 * time.Hour, Log: log,
	}.Register(b.router)
	member.API{
		Store: b.st, Clock: b.clk, Notify: notifications, PublicURL: publicURL, StoreAppOrigin: origin, PayPal: b.pp,
	}.Register(b.router)
	signals.API{Store: b.st, Clock: b.clk, Log: log}.Register(b.router)
	ev := evaluation.New(evaluation.Options{
		Store: b.st, Clock: b.clk, Model: fakeModel{}, MaxStep: usd(300),
		MinInterval: time.Minute, Capacity: 40, Log: log,
	})
	t.Cleanup(ev.Wait)
	evaluation.API{Evaluator: ev}.Register(b.router)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.dev = append(b.dev, devRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
		b.mu.Unlock()
		b.router.ServeHTTP(w, r)
	}))
	// Closed before the evaluator is waited for: cleanups run last added first.
	t.Cleanup(srv.Close)
	b.devURL = srv.URL
	return b
}

// devRequests returns what the development tools have sent so far.
func (b *barpay) devRequests() []devRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]devRequest(nil), b.dev...)
}

var keyN atomic.Int64

// call is the store app calling BarPay with key (storeKey or adminKey), or with none when key is "".
func (b *barpay) call(t *testing.T, method, path, key, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("BarPay-Staff-Id", "staff_1")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", keyN.Add(1)))
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// must is call, failing the test unless BarPay answers want; it decodes the body into out if not nil.
func (b *barpay) must(t *testing.T, want int, method, path, key, body string, out any) {
	t.Helper()
	code, data := b.call(t, method, path, key, body)
	if code != want {
		t.Fatalf("%s %s: %d %s, want %d", method, path, code, data, want)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

// checkIn starts a session for memberID, or a guest's when it is "", and returns its ID.
func (b *barpay) checkIn(t *testing.T, memberID string) string {
	t.Helper()
	body := `{}`
	if memberID != "" {
		body = `{"member_id":"` + memberID + `"}`
	}
	var s struct {
		SessionID string `json:"session_id"`
	}
	b.must(t, http.StatusCreated, http.MethodPost, "/v1/checkins", storeKey, body, &s)
	return s.SessionID
}

// browse is the member's browser opening u, a BarPay URL, with no key; it returns where BarPay sent it.
func (b *barpay) browse(t *testing.T, u string) string {
	t.Helper()
	if !strings.HasPrefix(u, publicURL+"/") {
		t.Fatalf("%s is not a BarPay URL", u)
	}
	req := httptest.NewRequest(http.MethodGet, strings.TrimPrefix(u, publicURL), nil)
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("GET %s: %d %s", u, w.Code, w.Body)
	}
	return w.Header().Get("Location")
}

// payer is the PayPal account of member m in the fake PayPal.
func payer(m string) string { return "payer-" + m }

// linkedSession checks memberID in and links PayPal through the member's browser and PayPal
// (UC2), then reports an order of $13.00, so leaving charges the member.
func (b *barpay) linkedSession(t *testing.T, memberID string) string {
	t.Helper()
	id := b.checkIn(t, memberID)
	var link struct {
		LinkURL string `json:"link_url"`
	}
	b.must(t, http.StatusCreated, http.MethodPost, "/v1/members/"+memberID+"/paypal-link", storeKey,
		`{"state":"state-0123456789abcdef","consent_version":"v1","return_url":"`+origin+`/settings"}`, &link)
	token := paypaltest.SetupTokenOf(b.browse(t, link.LinkURL))
	b.pp.Approve(token, payer(memberID))
	back := b.browse(t, b.pp.URLs(token).Return)
	i := strings.Index(back, "code=")
	if i < 0 {
		t.Fatalf("not returned with a code: %s", back)
	}
	code := strings.SplitN(back[i+len("code="):], "&", 2)[0]
	b.must(t, http.StatusCreated, http.MethodPost, "/v1/members/"+memberID+"/paypal-link/confirm", storeKey, `{"code":"`+code+`"}`, nil)
	b.must(t, http.StatusCreated, http.MethodPost, "/v1/sessions/"+id+"/orders", storeKey,
		`{"order_id":"o-1","lines":[{"name":"Beer","quantity":2,"unit_price":{"value":650,"currency":"USD"}}]}`, nil)
	return id
}

// exitToken is the session's current exit token, as the store app shows it in the exit QR.
func (b *barpay) exitToken(t *testing.T, sessionID string) string {
	t.Helper()
	var tok struct {
		Token string `json:"token"`
	}
	b.must(t, http.StatusOK, http.MethodGet, "/v1/sessions/"+sessionID+"/exit-token", storeKey, "", &tok)
	return tok.Token
}

// state is the session's state as getSession answers it.
func (b *barpay) state(t *testing.T, sessionID string) string {
	t.Helper()
	var s struct {
		State string `json:"state"`
	}
	b.must(t, http.StatusOK, http.MethodGet, "/v1/sessions/"+sessionID, storeKey, "", &s)
	return s.State
}

// syncBuffer is an output safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// tool is one development tool running against BarPay, as a person runs it in a terminal.
type tool struct {
	in   *io.PipeWriter
	out  *syncBuffer
	seen int // how much of out the test has read past
	code chan int
	stop context.CancelFunc
}

// start runs barpay-dev with args against b, with only the edge secret in the environment.
func start(t *testing.T, b *barpay, args ...string) *tool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	tl := &tool{in: inW, out: &syncBuffer{}, code: make(chan int, 1), stop: cancel}
	env := map[string]string{secretEnv: edgeSecret}
	args = append(args[:1:1], append([]string{"-barpay", b.devURL}, args[1:]...)...)
	go func() { tl.code <- run(ctx, args, func(k string) string { return env[k] }, inR, tl.out, tl.out) }()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		if c := <-tl.code; c != 0 {
			t.Errorf("barpay-dev %v exited with %d:\n%s", args, c, tl.out)
		}
		if strings.Contains(tl.out.String(), edgeSecret) {
			t.Errorf("barpay-dev %v showed the shared secret:\n%s", args, tl.out)
		}
	})
	return tl
}

// typeLine is a person typing line into the tool.
func (tl *tool) typeLine(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(tl.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// expect waits until the tool has shown each of lines, in order, after what the test read before,
// and returns everything shown up to the last of them.
func (tl *tool) expect(t *testing.T, lines ...string) string {
	t.Helper()
	giveUpAt := time.Now().Add(10 * time.Second)
	for {
		out := tl.out.String()
		rest, at := out[tl.seen:], tl.seen
		all := true
		for _, l := range lines {
			i := strings.Index(rest, l+"\n")
			if i < 0 {
				all = false
				break
			}
			at += i + len(l) + 1
			rest = rest[i+len(l)+1:]
		}
		if all {
			shown := out[tl.seen:at]
			tl.seen = at
			return shown
		}
		if time.Now().After(giveUpAt) {
			t.Fatalf("the tool did not show %q; it showed:\n%s", lines, out[tl.seen:])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
