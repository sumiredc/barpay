package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func testApp() *App {
	return &App{cfg: &Config{StoreName: "The Copper Fox"}, pages: loadPages()}
}

func renderPage(t *testing.T, name string, v view) string {
	t.Helper()
	rec := httptest.NewRecorder()
	testApp().render(rec, http.StatusOK, name, v)
	if rec.Code != http.StatusOK {
		t.Fatalf("render %s: status %d: %s", name, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestCustomerPagesUseTheCustomerLayout(t *testing.T) {
	body := renderPage(t, "login", view{Data: loginInput{}})
	for _, want := range []string{`data-app="customer"`, `href="/static/customer.css"`, `src="/static/customer.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %s", want)
		}
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "<style>") {
		t.Error("login page has an inline script or style element")
	}
}

func TestStaffPagesKeepTheStaffLayout(t *testing.T) {
	body := renderPage(t, "staff_login", view{Admin: true})
	if strings.Contains(body, `data-app="customer"`) || strings.Contains(body, "/static/customer.css") {
		t.Error("staff login page uses the customer layout")
	}
	if !strings.Contains(body, "<style>") {
		t.Error("staff login page lacks the style of layout.html")
	}
}

func TestEveryCustomerPageHasALayout(t *testing.T) {
	p := loadPages()
	for name := range customerPages {
		if p.byName[name] == nil {
			t.Errorf("customerPages lists %s, which has no template", name)
		}
	}
}

// The tab bar is there for members and guests on every page but the entry pages, and it marks the
// tab the page belongs to.
func TestFrameOfEachCustomerPage(t *testing.T) {
	member := view{Member: &MemberAccount{Name: "Taro"}}
	guest := view{Guest: true}
	tests := []struct {
		page        string
		v           view
		tab         string
		header, bar bool
	}{
		{"menu", member, "menu", true, true},
		{"panel", guest, "bill", true, true},
		{"pay", member, "bill", true, true},
		{"exit", member, "bill", true, true},
		{"settings", guest, "account", true, true},
		{"receipts", member, "account", true, true},
		{"receipt", member, "account", true, true},
		{"login", view{}, "", false, false},
		{"signup", view{}, "", false, false},
		{"error", member, "", false, false},
		{"landing", member, "", false, false},
		{"settings", view{}, "account", true, false},
	}
	for _, tt := range tests {
		v := place(customerPages[tt.page], tt.v)
		if v.Tab != tt.tab || v.Header != tt.header || v.Tabbar != tt.bar {
			t.Errorf("%s: tab %q header %v tabbar %v, want %q %v %v", tt.page, v.Tab, v.Header, v.Tabbar, tt.tab, tt.header, tt.bar)
		}
	}
}

func TestLandingInAVisitShowsTabsWithNoneMarked(t *testing.T) {
	body := renderPage(t, "landing", view{Member: &MemberAccount{Name: "Taro"}, Tabbar: true, Data: landingData{InVisit: true}})
	if !strings.Contains(body, `class="tabbar"`) {
		t.Fatal("landing in a visit has no tab bar")
	}
	if strings.Contains(body, "aria-current") {
		t.Error("landing marks a tab")
	}
	if strings.Contains(body, `class="brandmark"`) {
		t.Error("landing has the small header")
	}
	if !strings.Contains(body, `class="has-tabbar"`) {
		t.Error("landing in a visit lacks has-tabbar on the body")
	}
}

func TestLandingBeforeCheckInHasNoTabs(t *testing.T) {
	body := renderPage(t, "landing", view{Member: &MemberAccount{Name: "Taro"}, Data: landingData{}})
	if strings.Contains(body, `class="tabbar"`) || strings.Contains(body, "has-tabbar") {
		t.Error("landing before check-in has the tab bar")
	}
}

func TestTheAccountTabPointsToSettings(t *testing.T) {
	body := renderPage(t, "receipts", view{Member: &MemberAccount{Name: "Taro"}, Data: []Visit{}})
	if !strings.Contains(body, `<a href="/settings" aria-current="page">`) {
		t.Error("receipts does not mark the Account tab, pointing to /settings")
	}
	if !strings.Contains(body, `<header class="brandmark">`) {
		t.Error("receipts lacks the small header")
	}
}

func TestACartbarGivesTheBodyRoomForIt(t *testing.T) {
	body := renderPage(t, "signup", view{Data: signupInput{}})
	if !strings.Contains(body, `<body data-app="customer" class="has-cart">`) {
		t.Error("sign-up page lacks has-cart, or has has-tabbar")
	}
	if !strings.Contains(body, `<div class="cartbar"><button class="button" type="submit" form="signup-form"`) {
		t.Error("sign-up page does not put its button in the cartbar")
	}
}

// A meta refresh would reload under an open sheet and close it; the script waits for it to close.
func TestCustomerPagesReloadByScript(t *testing.T) {
	body := renderPage(t, "receipts", view{Member: &MemberAccount{Name: "Taro"}, Data: []Visit{}, Refresh: 10})
	if !strings.Contains(body, ` data-refresh="10">`) {
		t.Error("the body does not carry the time to reload after")
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the page reloads by a meta refresh")
	}
	if strings.Contains(renderPage(t, "receipts", view{Member: &MemberAccount{Name: "Taro"}, Data: []Visit{}}), "data-refresh") {
		t.Error("a page with nothing to reload for carries data-refresh")
	}
}

func TestMessagesOfTheCustomerPages(t *testing.T) {
	body := renderPage(t, "login", view{Alert: "The email or password is wrong.", Flash: "Log in again.", Toast: "You're checked in", Data: loginInput{}})
	for _, want := range []string{
		`<p class="notice bad" role="alert"><i data-icon="alert"></i><span>The email or password is wrong.</span></p>`,
		`<p class="notice info" role="status"><i data-icon="info"></i><span>Log in again.</span></p>`,
		`<div class="toast" role="status"><i data-icon="circle-check"></i><span>You&#39;re checked in</span></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %s", want)
		}
	}
}

// The layout leaves the notices to each page, so a page that forgets them drops its messages. A
// page whose h1 is inside a state (.state) puts them first, as it has no h1 heading its content.
func TestEveryCustomerPageShowsTheNotices(t *testing.T) {
	heading := regexp.MustCompile(`(?m)^<h1>`)
	belowH1 := regexp.MustCompile(`(?m)^<h1>.*</h1>\n\{\{template "notices" \.\}\}$`)
	first := regexp.MustCompile(`(?m)^\{\{define "content"\}\}\n\{\{template "notices" \.\}\}\n`)
	for name := range customerPages {
		src, err := templateFS.ReadFile("templates/" + name + ".html")
		if err != nil {
			t.Fatal(err)
		}
		if heading.Match(src) && !belowH1.Match(src) {
			t.Errorf("%s does not put the notices right below its h1", name)
		} else if !heading.Match(src) && !first.Match(src) {
			t.Errorf("%s has no h1 and does not put the notices first", name)
		}
	}
}

func TestStaticDirectoriesAreNotListed(t *testing.T) {
	h := (&App{}).routes()
	for _, path := range []string{"/static/", "/static/missing.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

func TestStaticFilesAreServedWithTheirType(t *testing.T) {
	h := (&App{}).routes()
	for path, want := range map[string]string{
		"/static/customer.css": "text/css; charset=utf-8",
		"/static/customer.js":  "text/javascript; charset=utf-8",
		"/static/logo.svg":     "image/svg+xml",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", path, got, want)
		}
	}
}
