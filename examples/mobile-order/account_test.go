package main

import (
	"encoding/json"
	"io"
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

// accountBarPay stands in for BarPay with the operations the account pages use. A test sets what it
// answers before the customer opens a page.
type accountBarPay struct {
	mu       sync.Mutex
	sessions map[string]*Session
	// member is the answer to every member lookup; nil answers 401.
	member *Member
	// linkErr and deleteErr, when set, are the error codes the link and the deletion answer.
	linkErr, deleteErr string
	// returnURL is the return_url of the last link asked for.
	returnURL string
	links     int
}

func (f *accountBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, code string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
	}
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	member := regexp.MustCompile(`^/v1/members/([^/]+)(/.*)?$`).FindStringSubmatch(r.URL.Path)
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/checkins":
		var body struct {
			MemberID string `json:"member_id"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s := &Session{SessionID: "ses_" + randomID(""), State: "active", PaymentMethod: "register", CheckedInAt: time.Now().Add(-time.Hour),
			RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1200), Total: usd(1200)}
		if body.MemberID != "" {
			s.MemberID = &body.MemberID
		}
		f.sessions[s.SessionID] = s
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(s)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		s := f.sessions[strings.TrimPrefix(r.URL.Path, "/v1/sessions/")]
		if s == nil {
			fail(http.StatusNotFound, "session_not_found")
			return
		}
		json.NewEncoder(w).Encode(s)
	case member != nil && r.Method == "GET" && member[2] == "":
		if f.member == nil {
			fail(http.StatusUnauthorized, "unauthorized")
			return
		}
		m := *f.member
		m.MemberID = member[1]
		json.NewEncoder(w).Encode(m)
	case member != nil && r.Method == "POST" && member[2] == "/paypal-link":
		f.links++
		var body struct {
			ReturnURL string `json:"return_url"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.returnURL = body.ReturnURL
		if f.linkErr != "" {
			fail(http.StatusConflict, f.linkErr)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(PaypalLink{LinkURL: "http://" + r.Host + "/paypal", ExpiresAt: time.Now().Add(time.Hour)})
	case r.URL.Path == "/paypal":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "PayPal")
	case member != nil && r.Method == "DELETE" && member[2] == "/saved-paypal":
		if f.deleteErr != "" {
			fail(http.StatusConflict, f.deleteErr)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(http.StatusNotFound, "not_found")
	}
}

// shop is a browser on the sample app, which talks to a accountBarPay.
type shop struct {
	*customer
	barpay *accountBarPay
	a      *App
}

func newShop(t *testing.T) *shop {
	t.Helper()
	f := &accountBarPay{sessions: map[string]*Session{}}
	barpay := httptest.NewServer(f)
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
	return &shop{customer: &customer{t: t, app: app, client: &http.Client{Jar: jar}}, barpay: f, a: a}
}

// signUp makes the browser a member called Taro and returns the member ID.
func (s *shop) signUp() string {
	s.t.Helper()
	s.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
	var id string
	s.a.store.view(func(d *Data) {
		for _, m := range d.Members {
			id = m.ID
		}
	})
	return id
}

func (s *shop) setMember(m *Member) {
	s.barpay.mu.Lock()
	defer s.barpay.mu.Unlock()
	s.barpay.member = m
}

// visit returns the path the browser ends on after opening path, and the page there.
func (s *shop) visit(path string) (string, string) {
	s.t.Helper()
	resp, err := s.client.Get(s.app.URL + path)
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.Request.URL.Path, s.read(resp)
}

func linked(inUse *string, status string) *Member {
	return &Member{SavedPaypal: &SavedPaypal{Status: status, SavedAt: time.Date(2026, 11, 1, 19, 0, 0, 0, time.Local), ConsentVersion: consentVersion, InUseBySessionID: inUse}}
}

// The consent text and its version are bound together: the page shows them as they are.
func TestThePayPalPageShowsTheConsentBeforeLinking(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(&Member{})
	page := s.open("/account/paypal")
	for _, want := range []string{
		"<p>" + strings.ReplaceAll(consentText, "'", "&#39;") + "</p>",
		"Terms version walkout-v1",
		`<span>I agree</span>`,
		`<button class="button" type="submit" form="link-form" id="link-submit" disabled`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the PayPal page lacks %s", want)
		}
	}
}

func TestLinkingWithoutAgreeingIsRefused(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(&Member{})
	page := s.post("/account/paypal", "/account/paypal/link", url.Values{})
	if !strings.Contains(page, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>Please agree to the terms</span></p>`) {
		t.Error("linking without agreeing is not told in red")
	}
	if s.barpay.links != 0 {
		t.Error("a link was asked for without the consent")
	}
}

func TestPayPalReturnsToThePayPalPage(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(&Member{})
	s.post("/account/paypal", "/account/paypal/link", url.Values{"agree": {"yes"}})
	if s.barpay.returnURL != "http://127.0.0.1/account/paypal/return" {
		t.Errorf("return_url %q, want the PayPal page's return", s.barpay.returnURL)
	}
}

func TestAReturnFromAnotherBrowserIsRefusedInRed(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(&Member{})
	at, page := s.visit("/account/paypal/return?paypal_link=approved&state=other&code=c")
	if at != "/account/paypal" || !strings.Contains(page, "<span>This link was started in another browser or has expired. Nothing was saved. Please link again here</span>") {
		t.Errorf("a return from another browser ends on %s without the red notice", at)
	}
}

func TestLinkingWhenLinkedSaysPayPalIsAlreadyLinked(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(linked(nil, "active"))
	s.barpay.linkErr = "saved_paypal_exists"
	page := s.post("/account/paypal", "/account/paypal/link", url.Values{"agree": {"yes"}})
	if !strings.Contains(page, `<p class="notice info" role="status"><i data-icon="info"></i><span>PayPal is already linked</span></p>`) {
		t.Error("linking when linked does not say PayPal is already linked")
	}
}

func TestLinkingWhileUnlinkingShowsOnlyTheUnlinking(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(linked(nil, "deletion_pending"))
	s.barpay.linkErr = "saved_paypal_exists"
	page := s.post("/account", "/account/paypal/link", url.Values{"agree": {"yes"}})
	if !strings.Contains(page, "<h1>Unlinking…</h1>") || strings.Contains(page, "already linked") {
		t.Error("linking while unlinking does not show the unlinking alone")
	}
}

func TestALinkedPayPalCanBeUnlinkedAfterConfirming(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(linked(nil, "active"))
	page := s.open("/account/paypal")
	for _, want := range []string{
		"<strong>Linked</strong><span class=\"muted\">Since Sun, Nov 1</span>",
		`<button class="textbutton danger" type="button" data-sheet="delete-confirm">Unlink</button>`,
		`<h2>Unlink PayPal?</h2>`,
		`<form method="post" action="/account/paypal/delete">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the linked PayPal page lacks %s", want)
		}
	}
}

func TestAPayPalInUseCannotBeUnlinked(t *testing.T) {
	s := newShop(t)
	s.signUp()
	inUse := "ses_1"
	s.setMember(linked(&inUse, "active"))
	page := s.open("/account/paypal")
	if !strings.Contains(page, "<span>In use for your current visit. You can unlink it after you pay</span>") ||
		!strings.Contains(page, `<button class="textbutton muted" type="button" disabled>Unlink</button>`) ||
		strings.Contains(page, "delete-confirm") {
		t.Error("a PayPal in use is not shown as in use, with Unlink disabled")
	}
}

func TestUnlinkingIsToldByAToast(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(linked(nil, "active"))
	page := s.post("/account/paypal", "/account/paypal/delete", url.Values{})
	if !strings.Contains(page, `<div class="toast" role="status"><i data-icon="circle-check"></i><span>PayPal unlinked</span></div>`) {
		t.Error("unlinking is not told by a toast")
	}
}

func TestAFailedUnlinkIsToldInRed(t *testing.T) {
	s := newShop(t)
	s.signUp()
	s.setMember(linked(nil, "active"))
	s.barpay.deleteErr = "paypal_unavailable"
	page := s.post("/account/paypal", "/account/paypal/delete", url.Values{})
	if !strings.Contains(page, "<span>Couldn&#39;t unlink. Please try again later</span>") {
		t.Error("a failed unlink is not told")
	}
}

// A notification that BarPay removed the saved PayPal asks the member to act, so it stays in red;
// one of their own linking or unlinking only confirms it.
func TestThePayPalNotificationsAreShownByWhatTheyAsk(t *testing.T) {
	for notice, want := range map[string]string{
		paypalNoLongerValidNotice: `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>Your saved PayPal could no longer be used, so we unlinked it</span></p>`,
		paypalLinkedNotice:        `<div class="toast" role="status"><i data-icon="circle-check"></i><span>PayPal linked</span></div>`,
		savedPaypalDeletedNotice:  `<div class="toast" role="status"><i data-icon="circle-check"></i><span>PayPal unlinked</span></div>`,
	} {
		s := newShop(t)
		id := s.signUp()
		s.setMember(&Member{})
		s.a.store.update(func(d *Data) error { d.MemberNotices[id] = notice; return nil })
		if page := s.open("/account/paypal"); !strings.Contains(page, want) {
			t.Errorf("%q is not shown as %s", notice, want)
		}
	}
}

func TestAPayPalPageThatCannotBeReadOffersToTryAgain(t *testing.T) {
	s := newShop(t)
	s.signUp()
	page := s.open("/account/paypal")
	if !strings.Contains(page, "<h1>Couldn't load</h1>") || !strings.Contains(page, `<a class="button" href="/account/paypal"`) {
		t.Error("a PayPal page that cannot be read does not offer to try again")
	}
}

func (s *shop) memberName(id string) string {
	var name string
	s.a.store.view(func(d *Data) { name = d.Members[id].Name })
	return name
}

func TestAMemberChangesTheirName(t *testing.T) {
	for _, name := range []string{"T", strings.Repeat("a", 60), strings.Repeat("名", 60)} {
		s := newShop(t)
		id := s.signUp()
		s.setMember(&Member{})
		page := s.post("/account", "/account/name", url.Values{"name": {name}})
		if got := s.memberName(id); got != name {
			t.Errorf("name %q became %q", name, got)
		}
		if !strings.Contains(page, "<strong>"+name+"</strong>") || !strings.Contains(page, "<span>Name saved</span>") {
			t.Errorf("the account does not show %q with a toast", name)
		}
		if !strings.Contains(s.open("/"), name) {
			t.Errorf("the entrance does not show %q", name)
		}
	}
}

func TestANameThatIsEmptyOrTooLongIsRefused(t *testing.T) {
	for _, name := range []string{"", "   ", strings.Repeat("a", 61), strings.Repeat("名", 61)} {
		s := newShop(t)
		id := s.signUp()
		s.setMember(&Member{})
		page := s.post("/account", "/account/name", url.Values{"name": {name}})
		if got := s.memberName(id); got != "Taro" {
			t.Errorf("name %q was taken: the name is %q", name, got)
		}
		if !strings.Contains(page, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>Enter your name (up to 60 characters)</span></p>`) {
			t.Errorf("name %q is refused without the red notice", name)
		}
	}
}

func TestAGuestCannotChangeAName(t *testing.T) {
	s := newShop(t)
	s.post("/", "/guest", url.Values{})
	page := s.post("/menu", "/account/name", url.Values{"name": {"Hana"}})
	if strings.Contains(page, "Hana") || strings.Contains(page, "Name saved") {
		t.Error("a guest changed a name")
	}
	s.a.store.view(func(d *Data) {
		if len(d.Members) != 0 {
			t.Error("a guest's name change made a member")
		}
	})
}

func TestTheAccountShowsWhetherPayPalIsLinked(t *testing.T) {
	for m, want := range map[*Member]string{
		{}:                              `<span>PayPal</span><span class="muted">Not linked</span>`,
		linked(nil, "active"):           `<span>PayPal</span><span class="muted">Linked</span>`,
		linked(nil, "deletion_pending"): `<span>PayPal</span><span class="muted">Unlinking…</span>`,
	} {
		s := newShop(t)
		s.signUp()
		s.setMember(m)
		page := s.open("/account")
		for _, w := range []string{want, `<span class="avatar">T</span>`, `<a href="/receipts">`, `<a href="/account/paypal">`, `action="/logout"`} {
			if !strings.Contains(page, w) {
				t.Errorf("the account lacks %s", w)
			}
		}
	}
}

func TestTheAccountShowsAGuestTheRowsForMembers(t *testing.T) {
	s := newShop(t)
	s.post("/", "/guest", url.Values{})
	page := s.open("/account")
	if !strings.Contains(page, "<strong>Guest</strong>") || strings.Count(page, `<span class="muted">Members only</span>`) != 2 {
		t.Error("the account does not show a guest the rows for members")
	}
	if strings.Contains(page, `href="/receipts"`) || strings.Contains(page, `href="/account/paypal"`) || strings.Contains(page, `action="/logout"`) {
		t.Error("the account lets a guest open a member's row")
	}
}

func TestAGuestOpeningThePayPalPageIsSentToTheAccount(t *testing.T) {
	s := newShop(t)
	s.post("/", "/guest", url.Values{})
	if at, _ := s.visit("/account/paypal"); at != "/account" {
		t.Errorf("a guest opening the PayPal page ends on %s", at)
	}
}
