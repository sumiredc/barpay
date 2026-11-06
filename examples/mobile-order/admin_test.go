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
}

func (f *adminBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case r.Method == "GET" && r.URL.Path == "/v1/admin/alerts":
		json.NewEncoder(w).Encode(page("alerts"))
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
}

const staffPassword = "Abcdef1!"

func newStaff(t *testing.T) *staff {
	t.Helper()
	f := &adminBarPay{}
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
	return &staff{customer: &customer{t: t, app: app, client: &http.Client{Jar: jar}}, barpay: f}
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
		body := s.open(p.path)
		want := `<span>BarPay didn't answer. Wait a moment, then <a href="` + p.path + `">reload</a>.</span>`
		if strings.Count(body, want) != 1 {
			t.Errorf("%s does not say once that BarPay didn't answer:\n%s", p.path, body)
		}
	}
}
