package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// orderingBarPay stands in for BarPay for guests who check in and order, and answers the reads
// of the admin pages as adminBarPay does. Each order is recorded a minute after the one before.
type orderingBarPay struct {
	*adminBarPay
	mu       sync.Mutex
	visits   map[string]*Session
	recorded time.Time
	// refused counts the orders refused as sent after the visit ended.
	refused int
}

// guestsCheckedIn is when every guest of orderingBarPay checks in.
var guestsCheckedIn = time.Date(2026, 11, 5, 21, 5, 0, 0, time.Local)

func (f *orderingBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.answer(w, r) {
		f.adminBarPay.ServeHTTP(w, r)
	}
}

// answer answers a request of a guest's visit, or returns false for one of the admin pages.
func (f *orderingBarPay) answer(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	id, orders := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/orders")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/checkins":
		s := &Session{SessionID: fmt.Sprintf("ses_%02d", len(f.visits)+1), State: "active", PaymentMethod: "register", CheckedInAt: guestsCheckedIn,
			RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1200), Total: usd(1200)}
		f.visits[s.SessionID] = s
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(s)
	case r.Method == "GET" && !orders && f.visits[id] != nil:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.visits[id])
	case r.Method == "POST" && orders && f.visits[id] != nil:
		w.Header().Set("Content-Type", "application/json")
		if f.visits[id].ended() {
			f.refused++
			apiError(w, http.StatusConflict, "session_not_active")
			return true
		}
		var o Order
		json.NewDecoder(r.Body).Decode(&o)
		for _, l := range o.Lines {
			o.Amount.Value += l.Amount().Value
		}
		o.Amount.Currency = "USD"
		f.recorded = f.recorded.Add(time.Minute)
		o.RecordedAt = f.recorded
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(o)
	default:
		return false
	}
	return true
}

// orderDesk is the sample app with staff on its admin pages, and guests who order on it.
type orderDesk struct {
	*staff
	barpay   *orderingBarPay
	dataPath string
}

func newOrderDesk(t *testing.T) *orderDesk {
	t.Helper()
	f := &orderingBarPay{adminBarPay: &adminBarPay{sessions: map[string]*Session{}}, visits: map[string]*Session{},
		recorded: time.Date(2026, 11, 5, 21, 50, 0, 0, time.Local)}
	barpay := httptest.NewServer(f)
	t.Cleanup(barpay.Close)
	o := &orderDesk{barpay: f, dataPath: filepath.Join(t.TempDir(), "data.json")}
	o.staff = o.start(t, barpay.URL)
	return o
}

// start runs the app on the data file, as after a restart, with a staff browser logged out.
func (o *orderDesk) start(t *testing.T, barpayURL string) *staff {
	t.Helper()
	st, err := openStore(o.dataPath)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://127.0.0.1")
	cfg := &Config{StoreName: "The Copper Fox", PublicURL: public, BarPayURL: barpayURL,
		Staff: map[string]string{"staff-01": hashPassword(staffPassword), "staff-02": hashPassword(staffPassword)}}
	a := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages(), dummyHash: hashPassword("x")}
	app := httptest.NewServer(a.routes())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	return &staff{customer: &customer{t: t, app: app, client: &http.Client{Jar: jar}}, barpay: o.barpay.adminBarPay, a: a}
}

// restart stops nothing but opens the app again on the same data file, which is what a restart
// keeps.
func (o *orderDesk) restart() {
	o.t.Helper()
	o.staff = o.start(o.t, o.a.cfg.BarPayURL)
}

// guest checks a guest in on the app.
func (o *orderDesk) guest() *customer {
	o.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &customer{t: o.t, app: o.app, client: &http.Client{Jar: jar}}
	c.post("/", "/guest", url.Values{})
	return c
}

// settle ends the guest's visit as the notification of their payment at the register would.
func (o *orderDesk) settle(sessionID string) {
	o.t.Helper()
	o.barpay.mu.Lock()
	s := *o.barpay.visits[sessionID]
	s.State = "closed"
	s.Receipt = &Receipt{Settlement: "register", EndedAt: time.Now()}
	o.barpay.visits[sessionID] = &s
	o.barpay.mu.Unlock()
	o.a.store.update(func(d *Data) error { d.remember(&s); return nil })
}

// logInAs logs the staff browser in as another staff member.
func (o *orderDesk) logInAs(staffID string) {
	o.t.Helper()
	jar, _ := cookiejar.New(nil)
	o.client = &http.Client{Jar: jar}
	o.post("/admin/login", "/admin/login", url.Values{"staff_id": {staffID}, "password": {staffPassword}})
}

var (
	orderRowPattern  = regexp.MustCompile(`(?s)<tr(?: class="done")? data-sheet="order-\d+" tabindex="0">(.*?)</tr>`)
	cellPattern      = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	servedFormAction = regexp.MustCompile(`action="(/admin/sessions/[^"]+/served)"`)
)

// rows returns the cells of each row of the orders table on the page, as text.
func rows(page string) [][]string {
	var out [][]string
	for _, row := range orderRowPattern.FindAllStringSubmatch(page, -1) {
		var cells []string
		for _, c := range cellPattern.FindAllStringSubmatch(row[1], -1) {
			cells = append(cells, html.UnescapeString(strings.TrimSpace(tagPattern.ReplaceAllString(c[1], ""))))
		}
		out = append(out, cells)
	}
	return out
}

// serve presses Served in the dialog of the nth row (from 0) of the orders to serve, and
// returns the page it leads to.
func (o *orderDesk) serve(n int) string {
	o.t.Helper()
	forms := servedFormAction.FindAllStringSubmatch(o.open("/admin/orders"), -1)
	if len(forms) <= n {
		o.t.Fatalf("no row %d to serve", n)
	}
	return o.post("/admin/orders", forms[n][1], url.Values{})
}

func toServeBadge(n int) string {
	return fmt.Sprintf(`<span>Orders</span><span class="badge" aria-label="%d to serve">%d</span>`, n, n)
}

// twoOrders is a guest who ordered a pint of draft beer with edamame at 21:51, then two ginger
// ales at 21:52.
func (o *orderDesk) twoOrders() *customer {
	o.t.Helper()
	c := o.guest()
	c.order([]string{"beer:1:1", "edamame::1"}, nil)
	c.order([]string{"soda::2"}, nil)
	return c
}

func TestTwoOrdersAreTwoRowsToServeNewestFirst(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	page := o.open("/admin/orders")
	got := rows(page)
	want := [][]string{
		{"21:52", "Guest, checked in 21:05", "Ginger ale ×2", "$8.00", "To serve"},
		{"21:51", "Guest, checked in 21:05", "Draft beer (Pint) ×1 + 1 more", "$14.00", "To serve"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rows to serve %q, want %q", got, want)
	}
	if !strings.Contains(page, toServeBadge(2)) {
		t.Error("the side menu has no badge of 2 to serve")
	}
	if !strings.Contains(page, `aria-current="page">To serve<span class="badge">2</span></a>`) {
		t.Error("the To serve tab is not open with a badge of 2")
	}
}

func TestAServedOrderMovesToServedWithWhoAndWhen(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	before := time.Now()
	page := o.serve(0)
	after := time.Now()
	got := rows(page)
	if len(got) != 1 || got[0][2] != "Draft beer (Pint) ×1 + 1 more" {
		t.Errorf("rows to serve after serving the ginger ales: %q", got)
	}
	if !strings.Contains(page, toServeBadge(1)) {
		t.Error("the badge did not go down to 1")
	}
	var at time.Time
	o.a.store.view(func(d *Data) { at = d.Visits["ses_01"].Served[d.Visits["ses_01"].Orders[1].OrderID].At })
	if at.Before(before) || at.After(after) {
		t.Errorf("served at %v, want the time it was pressed", at)
	}
	served := o.open("/admin/orders?tab=served")
	if got := rows(served); len(got) != 1 || got[0][2] != "Ginger ale ×2" || got[0][4] != "Served" {
		t.Errorf("served rows %q, want the ginger ales", got)
	}
	if !strings.Contains(served, "<p class=\"muted\">Served (staff-01, "+at.Local().Format("15:04")+")</p>") {
		t.Error("the served order does not say who served it and when")
	}
}

func TestAnOrderServedAgainKeepsWhoServedItFirst(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	page := o.open("/admin/orders")
	action := servedFormAction.FindStringSubmatch(page)[1]
	o.post("/admin/orders", action, url.Values{})
	var first Served
	o.a.store.view(func(d *Data) { first = d.Visits["ses_01"].Served[d.Visits["ses_01"].Orders[1].OrderID] })
	o.logInAs("staff-02")
	o.post("/admin/orders", action, url.Values{})
	var again Served
	o.a.store.view(func(d *Data) { again = d.Visits["ses_01"].Served[d.Visits["ses_01"].Orders[1].OrderID] })
	if again.StaffID != "staff-01" || !again.At.Equal(first.At) || first.StaffID != "staff-01" {
		t.Errorf("served again: %+v, want the first %+v by staff-01", again, first)
	}
}

func TestAnOrderOfASettledVisitIsStillToServe(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	o.settle("ses_01")
	if got := rows(o.open("/admin/orders")); len(got) != 2 {
		t.Errorf("rows to serve after the visit was settled: %q, want both orders", got)
	}
}

func TestAnOrderBarPayRefusedAfterCheckoutIsOnNeitherTab(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	c := o.twoOrders()
	o.serve(0)
	// The guest had the menu open while they paid at the register.
	menu := c.open("/menu")
	o.settle("ses_01")
	resp, err := c.client.PostForm(c.app.URL+"/orders", url.Values{"csrf": {csrfPattern.FindStringSubmatch(menu)[1]},
		"order_id": {orderIDField.FindStringSubmatch(menu)[1]}, "line": {"karaage:0:1"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	o.barpay.mu.Lock()
	refused := o.barpay.refused
	o.barpay.mu.Unlock()
	if refused != 1 {
		t.Fatalf("BarPay refused %d orders, want the one sent after checkout", refused)
	}
	o.a.store.view(func(d *Data) {
		if n := len(d.Visits["ses_01"].Orders); n != 2 {
			t.Errorf("the visit holds %d orders, want the 2 BarPay recorded", n)
		}
	})
	for _, tab := range []string{"/admin/orders", "/admin/orders?tab=served"} {
		page := o.open(tab)
		if strings.Contains(page, "Fried chicken") {
			t.Errorf("%s shows the order BarPay refused", tab)
		}
		if len(rows(page)) != 1 {
			t.Errorf("%s has %d rows, want 1", tab, len(rows(page)))
		}
	}
}

func TestServedOrdersAreKeptAcrossARestart(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	o.serve(0)
	o.restart()
	o.logIn()
	if got := rows(o.open("/admin/orders?tab=served")); len(got) != 1 || got[0][2] != "Ginger ale ×2" {
		t.Errorf("served rows after a restart: %q, want the ginger ales", got)
	}
	if got := rows(o.open("/admin/orders")); len(got) != 1 {
		t.Errorf("rows to serve after a restart: %q, want 1", got)
	}
}

func TestAnOrderToServeOpensItsLinesWithTheServedButton(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	page := o.open("/admin/orders")
	_, dialog, _ := strings.Cut(page, `<dialog class="sheet" id="order-2">`)
	dialog, _, _ = strings.Cut(dialog, "</dialog>")
	for _, want := range []string{
		"<h2>Guest, checked in 21:05 · 21:51</h2>",
		`<span class="pill at_register">To serve</span>`,
		"<li><span>Draft beer (Pint) ×1</span><span>$9.00</span></li>",
		"<li><span>Edamame ×1</span><span>$5.00</span></li>",
		`<li class="sum"><span>Total</span><span>$14.00</span></li>`,
		`data-close>Close</button>`,
		`<button class="button" type="submit">Served</button>`,
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("the dialog of the order lacks %s:\n%s", want, dialog)
		}
	}
	if !strings.Contains(page, `<tr data-sheet="order-2" tabindex="0">`) {
		t.Error("the row does not open its dialog")
	}
	if strings.Count(page, `type="submit">Served</button>`) != 2 {
		t.Error("the Served button is somewhere other than in each dialog")
	}
}

func TestAServedOrderOpensWithoutTheServedButton(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	o.serve(0)
	page := o.open("/admin/orders?tab=served")
	if strings.Contains(page, `>Served</button>`) || strings.Contains(page, "/served") {
		t.Error("a served order can be marked served again from its dialog")
	}
	if !strings.Contains(page, `<tr class="done" data-sheet="order-1" tabindex="0">`) {
		t.Error("the served row is not drawn as done")
	}
}

func TestNothingToServeSaysSo(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	page := o.open("/admin/orders")
	if !strings.Contains(page, `<i data-icon="circle-check"></i><span>Nothing to serve</span>`) {
		t.Error("an empty list of orders to serve does not say Nothing to serve")
	}
	if strings.Contains(page, `class="badge"`) {
		t.Error("a badge is shown with nothing to serve")
	}
}

// fillServed stores n served orders of one visit, a minute apart, the newest first in the list.
func (o *orderDesk) fillServed(n int) {
	o.a.store.update(func(d *Data) error {
		v := d.visit("ses_99")
		v.Served = map[string]Served{}
		for i := range n {
			id := fmt.Sprintf("ord_%020d", i)
			v.Orders = append(v.Orders, Order{OrderID: id, Lines: []OrderLine{line("Gin and tonic", 1, 900)}, Amount: Money{Value: 900, Currency: "USD"},
				RecordedAt: guestsCheckedIn.Add(time.Duration(i) * time.Minute)})
			v.Served[id] = Served{StaffID: "staff-01", At: guestsCheckedIn.Add(time.Duration(i)*time.Minute + 30*time.Second)}
		}
		return nil
	})
}

func TestServedOrdersComeTwentyAtATime(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.fillServed(21)
	page := o.open("/admin/orders?tab=served")
	got := rows(page)
	if len(got) != 20 || got[0][0] != "21:25" || got[19][0] != "21:06" {
		t.Fatalf("the first page of served orders: %q", got)
	}
	next := regexp.MustCompile(`<a class="textbutton" href="([^"]+)">Older orders<i data-icon="chevron-right"></i></a>`).FindStringSubmatch(page)
	if next == nil {
		t.Fatal("no Older orders")
	}
	older := o.open(strings.ReplaceAll(next[1], "&amp;", "&"))
	if got := rows(older); len(got) != 1 || got[0][0] != "21:05" {
		t.Errorf("the older served orders: %q, want the one at 21:05", got)
	}
	if strings.Contains(older, "Older orders") {
		t.Error("the last page offers older orders")
	}
}

func TestTheOrdersPageIsInTheSideMenuOfEveryAdminPage(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	for _, p := range adminPages {
		if !strings.Contains(o.open(p.path), `<a href="/admin/orders"><i data-icon="utensils"></i><span>Orders</span>`) {
			t.Errorf("%s has no Orders in the side menu", p.path)
		}
	}
	page := o.open("/admin/orders")
	if marked := currentItem.FindAllStringSubmatch(page, -1); len(marked) != 1 || marked[0][1] != "Orders" {
		t.Errorf("the orders page marks %v in the side menu, want only Orders", marked)
	}
	for _, want := range []string{"<h1>Orders</h1>", `href="/static/admin.css"`, `src="/static/admin.js"`, `data-reload="30"`,
		"<th>Ordered</th><th>Customer</th><th>Items</th><th class=\"num\">Amount</th><th>State</th>", ">Served</a>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the orders page lacks %s", want)
		}
	}
}

func TestAnUnlockSentFromTheOrdersGoesBackToThem(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	for _, from := range []string{"/admin/orders", "/admin/orders?tab=served"} {
		path, body := o.unlock(from, "none")
		if path != from {
			t.Errorf("an unlock sent from %s went to %s", from, path)
		}
		if !strings.Contains(body, "The exit gate was told to unlock once. Check at the gate that it opened.") {
			t.Errorf("%s does not tell the unlock was sent", from)
		}
	}
}

func TestMarkingAnOrderThereIsNotIsNotFound(t *testing.T) {
	o := newOrderDesk(t)
	o.logIn()
	o.twoOrders()
	status, _ := o.send("/admin/orders", "/admin/sessions/ses_01/orders/ord_nothere/served", url.Values{})
	if status != http.StatusNotFound {
		t.Errorf("status %d, want 404", status)
	}
}
