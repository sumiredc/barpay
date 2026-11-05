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
	"testing"
	"time"
)

// customer is a browser on the sample app, which talks to a stand-in for BarPay that checks in,
// reads the session and records orders.
type customer struct {
	t      *testing.T
	app    *httptest.Server
	client *http.Client
}

func newCustomer(t *testing.T) *customer {
	t.Helper()
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	session := Session{SessionID: "ses_1", State: "active", PaymentMethod: "register", CheckedInAt: time.Now().Add(-time.Hour),
		RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1200), Total: usd(1200)}
	orders := map[string]bool{}
	barpay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/checkins":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(session)
		case r.Method == "GET" && r.URL.Path == "/v1/sessions/ses_1":
			json.NewEncoder(w).Encode(session)
		case r.Method == "POST" && r.URL.Path == "/v1/sessions/ses_1/orders":
			var body struct {
				OrderID string      `json:"order_id"`
				Lines   []OrderLine `json:"lines"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			// The tests send an order again only with other quantities, which BarPay refuses.
			if orders[body.OrderID] {
				w.WriteHeader(http.StatusConflict)
				io.WriteString(w, `{"error":{"code":"order_conflict","message":"already recorded"}}`)
				return
			}
			orders[body.OrderID] = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Order{OrderID: body.OrderID, Lines: body.Lines, Amount: usd(700), RecordedAt: time.Now()})
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"not here"}}`)
		}
	}))
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

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// open returns the page at path, after redirects.
func (c *customer) open(path string) string {
	c.t.Helper()
	resp, err := c.client.Get(c.app.URL + path)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.read(resp)
}

// post sends a form of the page at from, with that page's CSRF token, and returns the page it
// leads to.
func (c *customer) post(from, path string, form url.Values) string {
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
	return c.read(resp)
}

func (c *customer) read(resp *http.Response) string {
	c.t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s: status %d: %s", resp.Request.URL.Path, resp.StatusCode, b)
	}
	return string(b)
}

const toastClass = `class="toast"`

func TestAWrongLoginIsARedNoticeShownOnce(t *testing.T) {
	c := newCustomer(t)
	page := c.post("/login", "/login", url.Values{"email": {"nobody@example.com"}, "password": {"wrong password"}})
	notice := strings.Index(page, `<p class="notice bad" role="alert"><i data-icon="alert"></i><span>The email or password is wrong.</span></p>`)
	if notice < 0 {
		t.Fatal("a wrong login is not shown as a red notice")
	}
	if h1 := strings.Index(page, "<h1>Log in</h1>"); h1 < 0 || notice < h1 {
		t.Error("the notice is not below the h1")
	}
	if strings.Contains(c.open("/login"), "notice bad") {
		t.Error("the notice is shown again on the next page")
	}
}

func TestCheckingInIsToldByAToastShownOnce(t *testing.T) {
	c := newCustomer(t)
	page := c.post("/", "/guest", url.Values{})
	if !strings.Contains(page, `<span>You&#39;re checked in</span>`) || !strings.Contains(page, toastClass) {
		t.Fatal("checking in is not told by a toast")
	}
	if strings.Contains(c.open("/menu"), toastClass) {
		t.Error("the toast is shown again on the next page")
	}
}

func TestARecordedOrderIsToldByAToast(t *testing.T) {
	c := newCustomer(t)
	menu := c.post("/", "/guest", url.Values{})
	orderID := regexp.MustCompile(`name="order_id" value="([^"]+)"`).FindStringSubmatch(menu)
	if orderID == nil {
		t.Fatal("the menu has no order form")
	}
	page := c.post("/menu", "/orders", url.Values{"order_id": {orderID[1]}, "qty_beer": {"1"}})
	if !strings.Contains(page, `<div class="toast" role="status"><i data-icon="circle-check"></i><span>Order received</span></div>`) {
		t.Error("a recorded order is not told by a toast")
	}
}

func TestAnOrderSentAgainIsToldByAToast(t *testing.T) {
	c := newCustomer(t)
	menu := c.post("/", "/guest", url.Values{})
	orderID := regexp.MustCompile(`name="order_id" value="([^"]+)"`).FindStringSubmatch(menu)
	if orderID == nil {
		t.Fatal("the menu has no order form")
	}
	c.post("/menu", "/orders", url.Values{"order_id": {orderID[1]}, "qty_beer": {"1"}})
	page := c.post("/menu", "/orders", url.Values{"order_id": {orderID[1]}, "qty_beer": {"2"}})
	if !strings.Contains(page, `<div class="toast" role="status"><i data-icon="circle-check"></i><span>This order was already received</span></div>`) {
		t.Error("an order sent again is not told by a toast")
	}
}

func TestSigningUpIsToldByAToast(t *testing.T) {
	c := newCustomer(t)
	page := c.post("/signup", "/signup", url.Values{"name": {"Taro"}, "email": {"taro@example.com"}, "password": {"Correct horse 1"}})
	if !strings.Contains(page, `<span>Welcome, Taro. Your account is ready</span>`) || !strings.Contains(page, toastClass) {
		t.Error("signing up is not told by a toast")
	}
}
