package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// entryBarPay stands in for BarPay at the entrance: the rate, a member's saved PayPal, check-in,
// and the reads and the register request of the visit it starts.
type entryBarPay struct {
	// rate is the body of GET /rate; empty, the rate cannot be read.
	rate        string
	savedPaypal bool
	// checkin answers POST /checkins; nil checks in.
	checkin func(w http.ResponseWriter)
	// sessionFails makes reading the session fail; registerFails, the register request.
	sessionFails, registerFails bool
}

const rateWithCap = `{"rate_per_hour":{"value":1200,"currency":"USD"},"change":{"value":0,"currency":"USD"},"published_at":"2026-11-05T20:00:00Z","cap_per_visit":{"value":2500,"currency":"USD"}}`
const rateWithoutCap = `{"rate_per_hour":{"value":1200,"currency":"USD"},"change":{"value":0,"currency":"USD"},"published_at":"2026-11-05T20:00:00Z","cap_per_visit":{"value":0,"currency":"USD"}}`

func apiError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	io.WriteString(w, `{"error":{"code":"`+code+`","message":"refused"}}`)
}

func (f *entryBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	session := Session{SessionID: "ses_1", State: "active", PaymentMethod: "register", CheckedInAt: time.Now().Add(-time.Hour),
		RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1200), Total: usd(1200)}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/rate":
		if f.rate == "" {
			apiError(w, http.StatusNotFound, "rate_not_found")
			return
		}
		io.WriteString(w, f.rate)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/members/"):
		m := Member{MemberID: strings.TrimPrefix(r.URL.Path, "/v1/members/")}
		if f.savedPaypal {
			m.SavedPaypal = &SavedPaypal{Status: "saved", SavedAt: time.Now(), ConsentVersion: consentVersion}
		}
		json.NewEncoder(w).Encode(m)
	case r.Method == "POST" && r.URL.Path == "/v1/checkins":
		if f.checkin != nil {
			f.checkin(w)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(session)
	case r.Method == "GET" && r.URL.Path == "/v1/sessions/ses_1":
		if f.sessionFails {
			apiError(w, http.StatusNotFound, "not_here")
			return
		}
		json.NewEncoder(w).Encode(session)
	case r.Method == "POST" && r.URL.Path == "/v1/sessions/ses_1/register-request":
		if f.registerFails {
			apiError(w, http.StatusConflict, "session_not_active")
			return
		}
		session.State = "at_register"
		json.NewEncoder(w).Encode(session)
	default:
		apiError(w, http.StatusNotFound, "not_found")
	}
}

// newEntryCustomer is a browser on the sample app, which talks to f in place of BarPay.
func newEntryCustomer(t *testing.T, f *entryBarPay) *customer {
	t.Helper()
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
	return &customer{t: t, app: app, client: &http.Client{Jar: jar}}
}

// send posts like post, and returns the status with the page, which may be an error page.
func (c *customer) send(from, path string, form url.Values) (int, string) {
	c.t.Helper()
	m := csrfPattern.FindStringSubmatch(c.open(from))
	if m == nil {
		c.t.Fatalf("%s has no form", from)
	}
	form.Set("csrf", m[1])
	resp, err := c.client.PostForm(c.app.URL+path, form)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// get opens path like open, and returns the status with the page.
func (c *customer) get(path string) (int, string) {
	c.t.Helper()
	resp, err := c.client.Get(c.app.URL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const passwordError = "Use 8&#43; characters with upper and lower case letters, a number and a symbol"

func TestSignUpRefusesAPasswordWithoutEveryKindOfCharacterAndKeepsNameAndEmail(t *testing.T) {
	for _, password := range []string{"abcdefgh", "Abcdefgh", "Abcdefg1", "ABCDEF1!", "Abcde1!"} {
		c := newEntryCustomer(t, &entryBarPay{})
		page := c.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {password}})
		if !strings.Contains(page, passwordError) {
			t.Errorf("%s: the sign-up page does not tell the password is refused", password)
		}
		if !strings.Contains(page, `name="name" type="text" required maxlength="60" autocomplete="name" value="Taro"`) ||
			!strings.Contains(page, `name="email" type="email" required autocomplete="email" value="taro@example.com"`) {
			t.Errorf("%s: the name or the email is not kept", password)
		}
		if strings.Contains(c.open("/"), "Hi, Taro") {
			t.Errorf("%s: the customer became a member", password)
		}
	}
}

func TestSignUpTakesAPasswordWithEveryKindOfCharacter(t *testing.T) {
	c := newEntryCustomer(t, &entryBarPay{})
	page := c.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
	if !strings.Contains(page, "Hi, Taro") {
		t.Error("a password of 8 characters with every kind does not make a member")
	}
}

func TestSignUpCountsTheNameInCharacters(t *testing.T) {
	const nameError = "Enter your name (up to 60 characters)"
	for _, tt := range []struct {
		length int
		taken  bool
	}{{60, true}, {61, false}} {
		c := newEntryCustomer(t, &entryBarPay{})
		name := strings.Repeat("太", tt.length)
		page := c.post("/signup", "/signup", url.Values{"name": {name}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
		if member := strings.Contains(page, "Hi, "+name); member != tt.taken {
			t.Errorf("a name of %d Japanese characters: member %v, want %v", tt.length, member, tt.taken)
		}
		if refused := strings.Contains(page, nameError); refused == tt.taken {
			t.Errorf("a name of %d Japanese characters: name error shown %v", tt.length, refused)
		}
	}
}

// sheet returns the dialog with the id from page, or "".
func sheet(page, id string) string {
	start := strings.Index(page, `<dialog class="sheet" id="`+id+`">`)
	if start < 0 {
		return ""
	}
	end := strings.Index(page[start:], "</dialog>")
	return page[start : start+end]
}

func TestTheEntranceShowsTheRateAndTheCapInEachSheet(t *testing.T) {
	visitor := newEntryCustomer(t, &entryBarPay{rate: rateWithCap}).open("/")
	member := newEntryCustomer(t, &entryBarPay{rate: rateWithCap})
	page := member.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
	for _, s := range []string{sheet(visitor, "rate-info"), sheet(visitor, "guest-confirm"), sheet(page, "checkin-confirm")} {
		if !strings.Contains(s, "$12.00<small>/h</small>") || !strings.Contains(s, "Cap per visit") || !strings.Contains(s, "$25.00") {
			t.Errorf("a sheet lacks the rate or the cap: %q", s)
		}
	}
}

func TestTheEntranceLeavesOutACapOfZero(t *testing.T) {
	visitor := newEntryCustomer(t, &entryBarPay{rate: rateWithoutCap}).open("/")
	member := newEntryCustomer(t, &entryBarPay{rate: rateWithoutCap})
	page := member.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
	for id, s := range map[string]string{"rate-info": sheet(visitor, "rate-info"), "guest-confirm": sheet(visitor, "guest-confirm"), "checkin-confirm": sheet(page, "checkin-confirm")} {
		if s == "" || !strings.Contains(s, "$12.00<small>/h</small>") {
			t.Errorf("%s lacks the rate", id)
		}
		if strings.Contains(s, "Cap per visit") {
			t.Errorf("%s shows a cap of 0", id)
		}
	}
}

func TestAnEntranceThatCannotReadTheRateStillLetsInWithoutFigures(t *testing.T) {
	page := newEntryCustomer(t, &entryBarPay{}).open("/")
	if !strings.Contains(page, `<span class="chip off"><i data-icon="clock"></i>Can't load the rate</span>`) {
		t.Error("the chip does not say the rate cannot be read")
	}
	if strings.Contains(page, `data-sheet="rate-info"`) || sheet(page, "rate-info") != "" {
		t.Error("the rate sheet can be opened")
	}
	guest := sheet(page, "guest-confirm")
	if guest == "" || strings.Contains(guest, "rate-pair") || !strings.Contains(guest, `action="/guest"`) {
		t.Error("the guest sheet does not let in without the figures")
	}
}

func TestTheEntranceOffersToLinkPayPalOnlyWithoutASavedOne(t *testing.T) {
	for _, saved := range []bool{false, true} {
		c := newEntryCustomer(t, &entryBarPay{rate: rateWithCap, savedPaypal: saved})
		page := c.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Abcdef1!"}})
		if offered := strings.Contains(page, "PayPal not linked"); offered == saved {
			t.Errorf("saved PayPal %v: PayPal not linked shown %v", saved, offered)
		}
	}
}

func TestAFailedCheckInTellsWhatToDoByItsCause(t *testing.T) {
	defer func(d []time.Duration) { resendDelays = d }(resendDelays)
	resendDelays = []time.Duration{0, 0, 0}
	tests := []struct {
		cause   string
		answer  func(w http.ResponseWriter)
		title   string
		text    string
		forward string
	}{
		{"no answer", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, "Couldn&#39;t connect", "Trying again won&#39;t check you in twice",
			`<form method="post" action="/checkin">`},
		{"open elsewhere", func(w http.ResponseWriter) { apiError(w, http.StatusConflict, "member_has_open_session") }, "You&#39;re checked in on another device", "Please ask a staff member",
			`<a class="button" href="/" style="justify-content: center">Back</a>`},
		{"other", func(w http.ResponseWriter) { apiError(w, http.StatusConflict, "member_not_found") }, "Couldn&#39;t check in", "Please ask a staff member",
			`<a class="button" href="/" style="justify-content: center">Back</a>`},
	}
	for _, tt := range tests {
		c := newEntryCustomer(t, &entryBarPay{checkin: tt.answer})
		_, page := c.send("/", "/guest", url.Values{})
		if !strings.Contains(page, "<h1>"+tt.title+"</h1>") || !strings.Contains(page, "<p>"+tt.text+"</p>") {
			t.Errorf("%s: the error page does not say %q and %q", tt.cause, tt.title, tt.text)
		}
		if !strings.Contains(page, `<div class="cartbar">`+tt.forward) {
			t.Errorf("%s: the button at the bottom is not %s", tt.cause, tt.forward)
		}
	}
}

func TestAPageThatCannotReadTheVisitOffersToTryAgain(t *testing.T) {
	f := &entryBarPay{}
	c := newEntryCustomer(t, f)
	c.post("/", "/guest", url.Values{})
	f.sessionFails = true
	status, page := c.get("/panel")
	if status != http.StatusBadGateway || !strings.Contains(page, "<h1>Couldn&#39;t load</h1>") {
		t.Errorf("status %d: the panel does not say it could not load", status)
	}
	if !strings.Contains(page, `<div class="cartbar"><a class="button" href="/panel" style="justify-content: center">Try again</a>`) {
		t.Error("Try again does not open the same page")
	}
}

func TestARegisterRequestThatFailsLeadsBackToTheBill(t *testing.T) {
	c := newEntryCustomer(t, &entryBarPay{registerFails: true})
	c.post("/", "/guest", url.Values{})
	_, page := c.send("/menu", "/pay/register", url.Values{})
	if !strings.Contains(page, "<h1>Couldn&#39;t send you to the register</h1>") || !strings.Contains(page, "<p>Please ask a staff member</p>") {
		t.Error("the error page does not say the register request failed")
	}
	if !strings.Contains(page, `<div class="cartbar"><a class="button" href="/panel" style="justify-content: center">Back</a>`) {
		t.Error("Back does not lead to the bill")
	}
}
