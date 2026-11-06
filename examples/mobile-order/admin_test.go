package main

import (
	"encoding/json"
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

// adminBarPay stands in for BarPay with the reads every admin page makes. A test sets what it
// answers before staff open a page.
type adminBarPay struct {
	mu sync.Mutex
	// down refuses every request, which the app reads as BarPay not giving the page what it
	// needs. A refusal is not sent again, so the page is drawn without waiting.
	down bool
	// alerts is the first page of the alerts, and sessions the sessions BarPay reads out by ID.
	alerts   []Alert
	sessions map[string]*Session
	// gateErr, when set, is the error code the gate unlock answers with.
	gateErr string
	// unlocks are the bodies of the gate unlocks received, as JSON objects.
	unlocks []map[string]any
	// alertsHang keeps every read of the alerts waiting until the app gives up on it, and
	// alertsDown answers it with a 503; alertReads counts the reads.
	alertsHang, alertsDown bool
	alertReads             int
}

func (f *adminBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hang := false
	if r.URL.Path == "/v1/admin/alerts" {
		f.alertReads++
		hang = f.alertsHang
	}
	f.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.down {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unauthorized", "message": "down for the test"}})
		return
	}
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	page := func(key string) map[string]any { return map[string]any{key: []any{}, "next_cursor": nil} }
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/admin/sessions":
		json.NewEncoder(w).Encode(page("sessions"))
	case r.Method == "GET" && r.URL.Path == "/v1/admin/alerts" && f.alertsDown:
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unavailable", "message": "down for the test"}})
	case r.Method == "GET" && r.URL.Path == "/v1/admin/alerts":
		json.NewEncoder(w).Encode(map[string]any{"alerts": append([]Alert{}, f.alerts...), "next_cursor": nil})
	case r.Method == "POST" && r.URL.Path == "/v1/admin/gate/unlock":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.unlocks = append(f.unlocks, body)
		if f.gateErr != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": f.gateErr, "message": f.gateErr}})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		s := f.sessions[strings.TrimPrefix(r.URL.Path, "/v1/sessions/")]
		if s == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "session_not_found", "message": "no session"}})
			return
		}
		json.NewEncoder(w).Encode(s)
	case r.Method == "GET" && r.URL.Path == "/v1/admin/decisions":
		json.NewEncoder(w).Encode(page("decisions"))
	case r.Method == "GET" && r.URL.Path == "/v1/admin/notifications":
		json.NewEncoder(w).Encode(page("notifications"))
	case r.Method == "GET" && r.URL.Path == "/v1/admin/settings":
		json.NewEncoder(w).Encode(Settings{Floor: usd(800), Ceiling: usd(2000), BlockMinutes: 10, CapPerVisit: usd(2500), Goal: 50})
	case r.Method == "GET" && r.URL.Path == "/v1/rate":
		json.NewEncoder(w).Encode(CurrentRate{RatePoint: RatePoint{RatePerHour: usd(1200), Change: usd(50), PublishedAt: time.Now()}})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "not_found", "message": r.Method + " " + r.URL.Path}})
	}
}

// staff is a browser on the sample app's admin pages, which talk to an adminBarPay.
type staff struct {
	*customer
	barpay *adminBarPay
	a      *App
}

const staffPassword = "Abcdef1!"

func newStaff(t *testing.T) *staff {
	t.Helper()
	f := &adminBarPay{sessions: map[string]*Session{}}
	barpay := httptest.NewServer(f)
	t.Cleanup(barpay.Close)
	st, err := openStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://127.0.0.1")
	cfg := &Config{StoreName: "The Copper Fox", PublicURL: public, BarPayURL: barpay.URL,
		Staff: map[string]string{"staff-01": hashPassword(staffPassword)}}
	a := &App{cfg: cfg, barpay: newBarPay(cfg), store: st, pages: loadPages(), dummyHash: hashPassword("x")}
	app := httptest.NewServer(a.routes())
	t.Cleanup(app.Close)
	jar, _ := cookiejar.New(nil)
	return &staff{customer: &customer{t: t, app: app, client: &http.Client{Jar: jar}}, barpay: f, a: a}
}

// logIn logs the browser in as staff-01 and returns the page it lands on.
func (s *staff) logIn() string {
	s.t.Helper()
	return s.post("/admin/login", "/admin/login", url.Values{"staff_id": {"staff-01"}, "password": {staffPassword}})
}

// adminPages are the admin pages there are, by the side menu item each belongs to.
var adminPages = []struct{ path, item string }{
	{"/admin", "Visits"},
	{"/admin/alerts", "Alerts"},
	{"/admin/pricing", "Pricing"},
	{"/admin/notifications", "Log"},
}

var currentItem = regexp.MustCompile(`<a href="[^"]*" aria-current="page"><i data-icon="[^"]*"></i><span>([^<]*)</span>`)

func TestEveryAdminPageHasTheSideMenuWithItsOwnItemMarked(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	for _, p := range adminPages {
		body := s.open(p.path)
		if !strings.Contains(body, `<aside class="sidenav">`) {
			t.Errorf("%s has no side menu", p.path)
		}
		marked := currentItem.FindAllStringSubmatch(body, -1)
		if len(marked) != 1 || marked[0][1] != p.item {
			t.Errorf("%s marks %v in the side menu, want only %s", p.path, marked, p.item)
		}
		for _, item := range []string{"Visits", "Alerts", "Pricing", "Log", "Log out"} {
			if !strings.Contains(body, "<span>"+item) {
				t.Errorf("%s: the side menu lacks %s", p.path, item)
			}
		}
		if !strings.Contains(body, "<small>staff-01</small>") {
			t.Errorf("%s: the side menu does not show the staff ID beside Log out", p.path)
		}
	}
}

func TestASideMenuItemShowsItsBadgeOnlyAboveZero(t *testing.T) {
	v := view{Admin: true, StaffID: "staff-01", Frame: &staffFrame{Menu: []sideItem{
		{Href: "/admin", Icon: "users", Label: "Visits", Badge: 3, BadgeLabel: "3 waiting to pay"},
		{Href: "/admin/alerts", Icon: "bell", Label: "Alerts"},
	}}}
	body := renderPage(t, "staff_login", v)
	if !strings.Contains(body, `<span>Visits</span><span class="badge" aria-label="3 waiting to pay">3</span>`) {
		t.Error("Visits has no badge of 3")
	}
	if strings.Count(body, `class="badge"`) != 1 {
		t.Error("an item with no count has a badge")
	}
}

// The admin pages take their look and their behaviour from files of the app's own origin: its
// Content-Security-Policy runs no inline script.
func TestAdminPagesLoadTheAdminStyleAndScript(t *testing.T) {
	s := newStaff(t)
	pages := []string{s.open("/admin/login")}
	s.logIn()
	for _, p := range adminPages {
		pages = append(pages, s.open(p.path))
	}
	for i, body := range pages {
		for _, want := range []string{`data-app="admin"`, `href="/static/admin.css"`, `src="/static/admin.js"`} {
			if !strings.Contains(body, want) {
				t.Errorf("page %d lacks %s", i, want)
			}
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, "<style>") {
			t.Errorf("page %d has an inline script or style element", i)
		}
		if strings.Contains(body, "http-equiv") {
			t.Errorf("page %d reloads by a meta element", i)
		}
	}
}

func TestTheStaffLoginIsOneCardWithoutTheSideMenu(t *testing.T) {
	s := newStaff(t)
	body := s.open("/admin/login")
	if strings.Contains(body, "sidenav") {
		t.Error("the staff login has the side menu")
	}
	for _, want := range []string{`<main class="login">`, "<h1>Staff login</h1>", "Staff ID", "Password", ">Log in</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the staff login lacks %s", want)
		}
	}
}

func TestAWrongStaffLoginIsARedNotice(t *testing.T) {
	s := newStaff(t)
	body := s.post("/admin/login", "/admin/login", url.Values{"staff_id": {"staff-01"}, "password": {"wrong"}})
	if !strings.Contains(body, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>The staff ID or password is wrong.</span></p>`) {
		t.Errorf("no red notice of the wrong login:\n%s", body)
	}
}

// The visits and the alerts change by themselves, so they are read again every 30 seconds; the
// script does it, and holds off while a dialog is open.
func TestTheVisitsAndTheAlertsReloadEvery30Seconds(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	for _, p := range adminPages {
		body := s.open(p.path)
		reloads := strings.Contains(body, `data-reload="30"`)
		if want := p.path == "/admin" || p.path == "/admin/alerts"; reloads != want {
			t.Errorf("%s reloads every 30 seconds: %v, want %v", p.path, reloads, want)
		}
	}
}

func TestAnAdminPageWhoseReadFailedSaysSoOnceInTheSameWords(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()
	for _, p := range adminPages {
		body, _, _ := strings.Cut(s.open(p.path), gateDialogStart)
		want := `<span>BarPay didn't answer. Wait a moment, then <a href="` + p.path + `">reload</a>.</span>`
		if strings.Count(body, want) != 1 {
			t.Errorf("%s does not say once that BarPay didn't answer:\n%s", p.path, body)
		}
	}
}

const gateDialogStart = `<dialog class="sheet" id="gate-manual">`

// paidWithPayPal is a member's session that ended paid with PayPal at 21:59 today, and an
// unlock_failed alert raised for it.
func (s *staff) paidWithPayPal(sessionID, name string) {
	s.t.Helper()
	memberID := "mem_" + strings.ToLower(strings.Fields(name)[0])
	ended := time.Date(2026, 11, 5, 21, 59, 0, 0, time.Local)
	s.a.store.update(func(d *Data) error {
		d.Members[memberID] = &MemberAccount{ID: memberID, Name: name}
		return nil
	})
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	s.barpay.sessions[sessionID] = &Session{SessionID: sessionID, State: "closed", MemberID: &memberID, PaymentMethod: "paypal",
		Receipt: &Receipt{Settlement: "paypal", EndedAt: ended}}
	s.barpay.alerts = append(s.barpay.alerts, Alert{ID: "alt_" + sessionID, Kind: "unlock_failed", RaisedAt: ended, SessionID: sessionID, MemberID: memberID, Message: "The gate did not open."})
}

func (s *staff) lastUnlock() map[string]any {
	s.t.Helper()
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	if len(s.barpay.unlocks) == 0 {
		s.t.Fatal("BarPay got no gate unlock")
	}
	return s.barpay.unlocks[len(s.barpay.unlocks)-1]
}

// unlock sends the gate dialog of the page at from, choosing the radio of value, and returns the
// path the browser ends on and the page there.
func (s *staff) unlock(from, value string) (string, string) {
	s.t.Helper()
	page := s.open(from)
	m := csrfPattern.FindStringSubmatch(page)
	back := regexp.MustCompile(`name="back" value="([^"]*)"`).FindStringSubmatch(page)
	if m == nil || back == nil {
		s.t.Fatalf("%s has no gate dialog", from)
	}
	resp, err := s.client.PostForm(s.app.URL+"/admin/gate", url.Values{"csrf": {m[1]}, "back": {back[1]}, "for": {value}})
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.Request.URL.RequestURI(), s.read(resp)
}

func TestTheGateDialogOffersTheCustomersTheGateFailedToOpenFor(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	_, dialog, _ := strings.Cut(s.open("/admin/alerts"), gateDialogStart)
	for _, want := range []string{
		"<h2>Unlock the exit gate</h2>",
		"Who is it for?",
		`<input type="radio" name="for" value="ses_01hx36w1e3r5t7y9u1i3" required> Yui Tanaka (paid with PayPal at 21:59)</label>`,
		`<input type="radio" name="for" value="none" required> Not for a particular customer</label>`,
		"While the gate is open, anyone can walk through, paid or not. Your staff ID is recorded.",
		`data-close>Cancel</button>`,
		`type="submit">Unlock</button>`,
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("the gate dialog lacks %s:\n%s", want, dialog)
		}
	}
}

func TestUnlockingForACustomerSendsTheirSession(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.unlock("/admin", "ses_01hx36w1e3r5t7y9u1i3")
	if got := s.lastUnlock()["session_id"]; got != "ses_01hx36w1e3r5t7y9u1i3" {
		t.Errorf("session_id %v, want the session of the alert", got)
	}
}

func TestUnlockingForNoParticularCustomerSendsNoSession(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.unlock("/admin", "none")
	if body, ok := s.lastUnlock()["session_id"]; ok {
		t.Errorf("the unlock carries session_id %v", body)
	}
}

// Every admin page offers the dialog, and the outcome is told on the page it was opened on.
func TestAnUnlockSentGoesBackToThePageAndSaysToCheckTheGate(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	for _, p := range adminPages {
		path, body := s.unlock(p.path+"?cursor=x", "none")
		if path != p.path+"?cursor=x" {
			t.Errorf("an unlock sent from %s went to %s", p.path, path)
		}
		if !strings.Contains(body, `<p class="notice info" role="status"><i data-icon="info"></i><span>The exit gate was told to unlock once. Check at the gate that it opened.</span></p>`) {
			t.Errorf("%s does not tell the unlock was sent", p.path)
		}
	}
}

func TestAGateWithNoDeviceConnectedIsToldOnThePageItWasOpenedOn(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.barpay.gateErr = "gate_unavailable"
	path, body := s.unlock("/admin/alerts", "none")
	if path != "/admin/alerts" {
		t.Errorf("went to %s, want back to /admin/alerts", path)
	}
	if !strings.Contains(body, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>The exit gate device isn&#39;t connected, so the gate wasn&#39;t unlocked, and it won&#39;t unlock later. Check the device&#39;s power and connection.</span></p>`) {
		t.Errorf("no red notice of the gate device:\n%s", body)
	}
}

// An unlock_failed without a session is an unlock by hand that failed: staff open the gate again
// for no particular customer, so it is not offered as a customer.
func TestAnUnlockFailureWithoutASessionIsNotOffered(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.alerts = append(s.barpay.alerts,
		Alert{ID: "alt_hand", Kind: "unlock_failed", RaisedAt: time.Now(), Message: "The gate did not open for an unlock by hand."},
		Alert{ID: "alt_pay", Kind: "payment_failed", RaisedAt: time.Now(), SessionID: "ses_01hx39p4s6d8f0g2h4j6", Message: "PayPal refused."})
	_, dialog, _ := strings.Cut(s.open("/admin"), gateDialogStart)
	if got := strings.Count(dialog, `name="for"`); got != 2 {
		t.Errorf("the dialog offers %d choices, want Yui Tanaka and no particular customer:\n%s", got, dialog)
	}
}

func TestAFormCannotSendStaffOffTheAdminPages(t *testing.T) {
	for back, want := range map[string]string{
		"/admin/alerts?cursor=x": "/admin/alerts?cursor=x",
		"/admin":                 "/admin",
		"//evil.example/admin":   "/admin",
		"https://evil.example/":  "/admin",
		"/administrator":         "/admin",
		"/menu":                  "/admin",
		"":                       "/admin",
		"javascript:alert(1)":    "/admin",
		"/admin/../account":      "/admin",
		"/admin/.%2e/x":          "/admin",
		"/admin/a%2fb":           "/admin",
		"/admin/alerts?x=1":      "/admin/alerts?x=1",
		"/admin/./alerts?x=1":    "/admin/alerts?x=1",
	} {
		if got := adminPath(back); got != want {
			t.Errorf("adminPath(%q) = %q, want %q", back, got, want)
		}
	}
}

func (s *staff) alertReads() int {
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	return s.barpay.alertReads
}

// openGateDialog opens path and returns how long it took and its gate dialog.
func (s *staff) openGateDialog(path string) (time.Duration, string) {
	s.t.Helper()
	start := time.Now()
	body := s.open(path)
	_, dialog, _ := strings.Cut(body, gateDialogStart)
	return time.Since(start), dialog
}

const noParticularCustomer = `<input type="radio" name="for" value="none" required> Not for a particular customer</label>`

// The customers of the gate dialog are not worth holding every admin page up for: a BarPay that
// does not answer them in time leaves the dialog with no particular customer only.
func TestAPageWhoseGateCustomersHangIsDrawnWithoutThem(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.mu.Lock()
	s.barpay.alertsHang = true
	s.barpay.mu.Unlock()
	took, dialog := s.openGateDialog("/admin")
	if took > gateLookupTimeout+2*time.Second {
		t.Errorf("the page took %s", took)
	}
	if strings.Count(dialog, `name="for"`) != 1 || !strings.Contains(dialog, noParticularCustomer) {
		t.Errorf("the dialog offers more than no particular customer:\n%s", dialog)
	}
	if !strings.Contains(dialog, "BarPay didn't answer. Wait a moment, then") {
		t.Errorf("the dialog does not say BarPay didn't answer:\n%s", dialog)
	}
}

func TestAFailedReadOfTheGateCustomersIsNotSentAgain(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	s.barpay.mu.Lock()
	s.barpay.alertsDown = true
	s.barpay.mu.Unlock()
	before := s.alertReads()
	_, dialog := s.openGateDialog("/admin")
	if got := s.alertReads() - before; got != 1 {
		t.Errorf("the alerts were read %d times, want once", got)
	}
	if strings.Count(dialog, `name="for"`) != 1 || !strings.Contains(dialog, noParticularCustomer) {
		t.Errorf("the dialog offers more than no particular customer:\n%s", dialog)
	}
}

// The alerts page reads the first page of the alerts once, for itself and for the gate dialog.
func TestTheAlertsPageReadsTheAlertsOnce(t *testing.T) {
	s := newStaff(t)
	s.logIn()
	s.paidWithPayPal("ses_01hx36w1e3r5t7y9u1i3", "Yui Tanaka")
	before := s.alertReads()
	_, dialog := s.openGateDialog("/admin/alerts")
	if got := s.alertReads() - before; got != 1 {
		t.Errorf("the alerts were read %d times, want once", got)
	}
	if !strings.Contains(dialog, "Yui Tanaka (paid with PayPal at 21:59)") {
		t.Errorf("the dialog does not offer Yui Tanaka:\n%s", dialog)
	}
}
