package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBarPay answers what the bill and the exit QR read. The test changes the session between
// requests.
type fakeBarPay struct {
	mu      sync.Mutex
	session Session
	// exitError is the error code the exit token is refused with, or "" to hand out a token.
	exitError string
	exitLeft  time.Duration
}

func (f *fakeBarPay) set(change func(s *Session)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.session)
}

func (f *fakeBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/checkins":
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(f.session)
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/ses_1":
		json.NewEncoder(w).Encode(f.session)
	case r.Method == "POST" && r.URL.Path == "/v1/sessions/ses_1/register-request":
		f.session.State, f.session.PaymentMethod = "at_register", "register"
		json.NewEncoder(w).Encode(f.session)
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/ses_1/exit-token":
		if f.exitError != "" {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":{"code":%q,"message":"no token"}}`, f.exitError)
			return
		}
		json.NewEncoder(w).Encode(ExitToken{Token: "exit-token-1", ExpiresAt: time.Now().Add(f.exitLeft)})
	case r.Method == "GET" && r.URL.Path == "/v1/rate/history":
		json.NewEncoder(w).Encode(map[string][]RatePoint{"rates": {
			{RatePerHour: usd(1050), PublishedAt: f.session.CheckedInAt.Add(-time.Minute)},
			{RatePerHour: usd(1200), Change: usd(150), PublishedAt: f.session.CheckedInAt.Add(30 * time.Minute)},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"not here"}}`)
	}
}

// newBillCustomer is a guest in a visit, at the bill.
func newBillCustomer(t *testing.T, s Session) (*customer, *fakeBarPay) {
	t.Helper()
	fake := &fakeBarPay{session: s, exitLeft: 30 * time.Second}
	barpay := httptest.NewServer(fake)
	t.Cleanup(barpay.Close)
	st, err := openStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://127.0.0.1")
	cfg := &Config{StoreName: "The Copper Fox", PublicURL: public, BarPayURL: barpay.URL}
	a := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages(), dummyHash: hashPassword("x")}
	app := httptest.NewServer(a.routes())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	c := &customer{t: t, app: app, client: &http.Client{Jar: jar}}
	c.post("/", "/guest", url.Values{})
	return c, fake
}

func activeSession() Session {
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	return Session{SessionID: "ses_1", State: "active", PaymentMethod: "register", CheckedInAt: time.Now().Add(-72 * time.Minute),
		RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1340), OrdersTotal: usd(2400),
		Total: usd(3740), OrderCount: 2}
}

func mustContain(t *testing.T, page string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}

func mustLack(t *testing.T, page string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(page, u) {
			t.Errorf("the page has %s", u)
		}
	}
}

func TestTheBillOfAnActiveVisitLeadsWithTheTotal(t *testing.T) {
	c, _ := newBillCustomer(t, activeSession())
	page := c.open("/panel")
	mustContain(t, page,
		"<title>Bill · The Copper Fox</title>",
		`<p class="label">Total so far</p>`, `<p class="amount">$37.40</p>`, "Here for 1 h 12 min",
		"<div>Cover charge</div>", "Now $12.00/h", `<span class="up">&#43;$1.50</span>`,
		`aria-label="Rate per hour since check-in, from $10.50 to $12.00"`,
		`Orders<span class="muted"> (2)</span>`, `<div>Total</div><div class="bill-amount">$37.40</div>`,
		`<button class="button" type="button" data-sheet="pay"><span>Pay</span><span>$37.40</span></button>`)
	mustLack(t, page, "<h1>", "PayPal</div>", "Register</div>", "cap", "Order more")
}

func TestTheBillTellsNeitherThePaymentMethodNorTheCap(t *testing.T) {
	s := activeSession()
	s.PaymentMethod, s.CoverChargeStatus = "paypal", "capped"
	c, _ := newBillCustomer(t, s)
	page := c.open("/panel")
	mustLack(t, page, "Payment</div>", "reached the cap", `class="notice`)
}

func TestTheBillCountsDownTheEndOfThePause(t *testing.T) {
	s := activeSession()
	c, fake := newBillCustomer(t, s)
	// The page is read a moment after this, so the time left is a little under 10 minutes.
	resumes := time.Now().Add(10 * time.Minute)
	fake.set(func(s *Session) { s.CoverChargeStatus, s.CoverChargeResumesAt = "paused", &resumes })
	m := regexp.MustCompile(`<p class="notice good"><i data-icon="gift"></i><span>Cover charge on the house　<strong data-countdown="(\d+)">(\d+:\d\d)</strong> left</span></p>`).
		FindStringSubmatch(c.open("/panel"))
	if m == nil {
		t.Fatal("the bill has no countdown of the pause")
	}
	if seconds, _ := strconv.Atoi(m[1]); seconds < 590 || seconds > 600 || m[2] != fmt.Sprintf("%d:%02d", seconds/60, seconds%60) {
		t.Errorf("the countdown starts at %s seconds, shown as %s; want just under 600, as m:ss", m[1], m[2])
	}
}

// The pause ends at cover_charge_resumes_at: from that instant the cover charge is no longer
// paused, so the time left is 0 there and is rounded up before it.
func TestPauseLeftAroundTheEndOfThePause(t *testing.T) {
	resumes := time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)
	s := &Session{CoverChargeStatus: "paused", CoverChargeResumesAt: &resumes}
	for _, tt := range []struct {
		now     time.Time
		seconds int
		clock   string
	}{
		{resumes.Add(-10 * time.Minute), 600, "10:00"},
		{resumes.Add(-time.Second - time.Nanosecond), 2, "0:02"},
		{resumes.Add(-time.Second), 1, "0:01"},
		{resumes.Add(-time.Nanosecond), 1, "0:01"},
		{resumes, 0, "0:00"},
		{resumes.Add(time.Nanosecond), 0, "0:00"},
		{resumes.Add(time.Minute), 0, "0:00"},
	} {
		got := pauseLeft(s, tt.now)
		if got == nil || got.Seconds != tt.seconds || got.Clock != tt.clock {
			t.Errorf("at %v: %+v, want %d %s", tt.now.Sub(resumes), got, tt.seconds, tt.clock)
		}
	}
}

func TestPauseLeftIsNoneWithoutAPauseOrItsEnd(t *testing.T) {
	resumes := time.Now().Add(time.Minute)
	if got := pauseLeft(&Session{CoverChargeStatus: "running", CoverChargeResumesAt: &resumes}, time.Now()); got != nil {
		t.Errorf("running: %+v", got)
	}
	if got := pauseLeft(&Session{CoverChargeStatus: "paused"}, time.Now()); got != nil {
		t.Errorf("paused with no end: %+v", got)
	}
}

func TestThePauseWithNoEndToldShowsNoCountdown(t *testing.T) {
	s := activeSession()
	s.CoverChargeStatus = "paused"
	c, _ := newBillCustomer(t, s)
	page := c.open("/panel")
	mustContain(t, page, `<span>Cover charge on the house</span>`)
	mustLack(t, page, "data-countdown")
}

// BarPay can still say paused at or just past the end it gave (its clock and ours differ); the
// bill then shows no time rather than "0:00 left" until the next reload.
func TestAPauseAtItsEndShowsNoCountdown(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	resumes := time.Now()
	fake.set(func(s *Session) { s.CoverChargeStatus, s.CoverChargeResumesAt = "paused", &resumes })
	page := c.open("/panel")
	mustContain(t, page, `<span>Cover charge on the house</span>`)
	mustLack(t, page, "data-countdown", "0:00")
}

// The bill reloads by script, which waits while a sheet is open; a meta refresh would close it.
func TestTheBillReloadsByScriptUntilTheVisitEnds(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	page := c.open("/panel")
	mustContain(t, page, `data-refresh="15"`)
	mustLack(t, page, `http-equiv="refresh"`)
	fake.set(func(s *Session) { s.State = "closed" })
	mustLack(t, c.open("/panel"), "data-refresh")
}

func TestThePaySheetOffersPayPalOnlyToAVisitPaidWithPayPal(t *testing.T) {
	s := activeSession()
	s.PaymentMethod = "paypal"
	c, fake := newBillCustomer(t, s)
	page := c.open("/panel")
	mustContain(t, page, `<a class="button" href="/exit"><i data-icon="qr"></i> Pay with PayPal</a>`,
		`<button class="button secondary" type="submit">Pay at the register</button>`)
	fake.set(func(s *Session) { s.PaymentMethod = "register" })
	page = c.open("/panel")
	mustContain(t, page, `<button class="button" type="submit">Pay at the register</button>`)
	mustLack(t, page, "Pay with PayPal", `href="/exit"`)
}

func TestPayingAtTheRegisterIsToldByTheBillAlone(t *testing.T) {
	c, _ := newBillCustomer(t, activeSession())
	page := c.post("/panel", "/pay/register", url.Values{})
	mustContain(t, page, `<i data-icon="credit-card"></i>`, "<h1>Please pay at the register</h1>",
		`<div>Total</div><div class="bill-amount">$37.40</div>`)
	mustLack(t, page, `class="notice`, "Please pay $", `data-sheet="pay"`, "Total so far", "Now $12.00/h")
}

func TestThereIsNoPayPage(t *testing.T) {
	c, _ := newBillCustomer(t, activeSession())
	resp, err := c.client.Get(c.app.URL + "/pay")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /pay: status %d", resp.StatusCode)
	}
}

func TestTheBillAfterCheckout(t *testing.T) {
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	unpaid := usd(3740)
	tests := []struct {
		state string
		wants []string
	}{
		{"checking_out", []string{`<i data-icon="clock"></i>`, "<h1>Paying with PayPal…</h1>"}},
		{"payment_failed", []string{`<div class="state bad">`, "<h1>Payment didn&#39;t go through</h1>", "<p>Please ask a staff member. Unpaid $37.40</p>"}},
		{"closed_unpaid", []string{`<div class="state bad">`, "<h1>Payment didn&#39;t go through</h1>", "<p>Please ask a staff member. Unpaid $37.40</p>"}},
		{"closed", []string{`<div class="state good">`, "<h1>Thank you</h1>", "<p>Total $37.40</p>",
			`<div class="cartbar"><a class="button" href="/receipts/ses_1" style="justify-content: center">View receipt</a></div>`}},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			c, fake := newBillCustomer(t, activeSession())
			fake.set(func(s *Session) {
				s.State = tt.state
				if tt.state == "payment_failed" || tt.state == "closed_unpaid" {
					s.UnpaidAmount = &unpaid
				}
			})
			page := c.open("/panel")
			mustContain(t, page, tt.wants...)
			// The breakdown stays, without the rate that no longer changes.
			mustContain(t, page, "<div>Cover charge</div>", `<div>Total</div><div class="bill-amount">$37.40</div>`)
			mustLack(t, page, "Total so far", "Now $12.00/h", `class="spark"`, `data-sheet="pay"`)
		})
	}
}

func TestTheExitQR(t *testing.T) {
	s := activeSession()
	s.PaymentMethod = "paypal"
	c, _ := newBillCustomer(t, s)
	page := c.open("/exit")
	mustContain(t, page, `<p class="exit-hint">Hold this up to the reader at the exit</p>`, `alt="Exit QR"`,
		`<p class="exit-amount"><span>PayPal total</span>$37.40</p>`, `class="qr-timer"`)
	mustLack(t, page, "Back to my visit", "screenshot", `class="cartbar"`)
}

// The bar under the QR is full when the token arrives and empty at its expires_at.
func TestTheExitQRBarRunsOutWithTheToken(t *testing.T) {
	c, _ := newBillCustomer(t, activeSession())
	page := c.open("/exit")
	i := strings.Index(page, `<span style="animation: qr-left `)
	if i < 0 {
		t.Fatal("the bar has no animation of its own")
	}
	var seconds float64
	if _, err := fmt.Sscanf(page[i+len(`<span style="animation: qr-left `):], "%gs linear 1 forwards", &seconds); err != nil {
		t.Fatal(err)
	}
	if seconds < 29 || seconds > 30 {
		t.Errorf("the bar runs for %gs, want the 30s the token has left", seconds)
	}
}

// The page reloads when the token changes and not before, however long the token lasts.
func TestTheExitQRReloadsWhenTheTokenChanges(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	fake.mu.Lock()
	fake.exitLeft = 90 * time.Second
	fake.mu.Unlock()
	page := c.open("/exit")
	if !strings.Contains(page, ` data-refresh="90">`) && !strings.Contains(page, ` data-refresh="91">`) {
		t.Error("the exit QR does not reload when its 90-second token changes")
	}
}

func TestTheExitQRShownAgainAfterPaying(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	fake.set(func(s *Session) { s.State = "closed" })
	mustContain(t, c.open("/exit?again=1"), `<p class="exit-amount"><span>Paid with PayPal</span>$37.40</p>`)
}

func TestTheExitQRStates(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		exitError string
		wants     []string
		unwanted  []string
	}{
		{"checking_out", "checking_out", "", []string{"<h1>Paying with PayPal…</h1>", `data-refresh="3"`}, []string{`class="cartbar"`}},
		{"payment_failed", "payment_failed", "", []string{`<div class="state bad">`, "<h1>Payment didn&#39;t go through</h1>", "<p>Please ask a staff member</p>"}, []string{`class="cartbar"`}},
		{"unavailable", "active", "exit_token_unavailable", []string{`<i data-icon="lock"></i>`, "<h1>No exit QR for this visit</h1>", "<p>Please pay at the register</p>",
			`<div class="cartbar"><button class="button" type="submit" form="register-form" style="justify-content: center">Pay at the register</button></div>`}, nil},
		{"read_failed", "active", "internal_error", []string{`<div class="state bad">`, "<h1>Couldn&#39;t show the QR</h1>",
			`<div class="cartbar"><a class="button" href="/exit" style="justify-content: center"><i data-icon="rotate-cw"></i> Try again</a></div>`}, []string{"data-refresh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, fake := newBillCustomer(t, activeSession())
			fake.set(func(s *Session) { s.State = tt.state })
			fake.mu.Lock()
			fake.exitError = tt.exitError
			fake.mu.Unlock()
			page := c.open("/exit")
			mustContain(t, page, tt.wants...)
			mustLack(t, page, tt.unwanted...)
			mustLack(t, page, "Back to my visit")
		})
	}
}

func TestTheExitQRIsNotOfferedAtTheRegister(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	fake.set(func(s *Session) { s.State = "at_register" })
	mustContain(t, c.open("/exit"), "<h1>Please pay at the register</h1>", "<title>Bill · The Copper Fox</title>")
}

// After paying, the QR is shown again only to open the gate: there is nothing left to pay.
func TestNoExitQRAfterPayingSendsToStaffNotTheRegister(t *testing.T) {
	c, fake := newBillCustomer(t, activeSession())
	fake.set(func(s *Session) { s.State = "closed" })
	fake.mu.Lock()
	fake.exitError = "exit_token_unavailable"
	fake.mu.Unlock()
	page := c.open("/exit?again=1")
	mustContain(t, page, "<h1>No exit QR for this visit</h1>", "<p>Please ask a staff member</p>")
	mustLack(t, page, "Please pay at the register", `class="cartbar"`)
}
