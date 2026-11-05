package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// orderingCustomer is a guest checked in on the sample app, which talks to a stand-in for BarPay
// that keeps the lines of every order reported to it. With refuse, BarPay refuses each order with
// that error code.
func orderingCustomer(t *testing.T, refuse string) (*customer, *[][]OrderLine) {
	t.Helper()
	usd := func(v int64) Money { return Money{Value: v, Currency: "USD"} }
	session := Session{SessionID: "ses_1", State: "active", PaymentMethod: "register", CheckedInAt: time.Now().Add(-time.Hour),
		RatePerHour: usd(1200), CoverChargeStatus: "running", CoverCharge: usd(1200), Total: usd(1200)}
	var sent [][]OrderLine
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
			sent = append(sent, body.Lines)
			if refuse != "" {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprintf(w, `{"error":{"code":%q,"message":"refused"}}`, refuse)
				return
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Order{OrderID: body.OrderID, Lines: body.Lines, Amount: usd(0), RecordedAt: time.Now()})
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
	c := &customer{t: t, app: app, client: &http.Client{Jar: jar}}
	c.post("/", "/guest", url.Values{})
	return c, &sent
}

var orderIDField = regexp.MustCompile(`name="order_id" value="([^"]+)"`)

// order places an order of the menu's form with the given lines and other fields, and returns the
// page it leads to.
func (c *customer) order(lines []string, other url.Values) string {
	c.t.Helper()
	m := orderIDField.FindStringSubmatch(c.open("/menu"))
	if m == nil {
		c.t.Fatal("the menu has no order form")
	}
	form := url.Values{"order_id": {m[1]}, "line": lines}
	for k, v := range other {
		form[k] = v
	}
	return c.post("/menu", "/orders", form)
}

func line(name string, qty int, cents int64) OrderLine {
	return OrderLine{Name: name, Quantity: qty, UnitPrice: Money{cents, "USD"}}
}

func checkSent(t *testing.T, sent [][]OrderLine, want ...OrderLine) {
	t.Helper()
	if len(sent) != 1 {
		t.Fatalf("%d orders sent to BarPay, want 1", len(sent))
	}
	if !reflect.DeepEqual(sent[0], want) {
		t.Errorf("sent %+v, want %+v", sent[0], want)
	}
}

func TestTwoPintsOfDraftBeerAreSentAtNineDollarsEach(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"beer:1:2"}, nil)
	checkSent(t, *sent, line("Draft beer (Pint)", 2, 900))
}

func TestAChoiceThatCostsNothingIsStillNamed(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"fries:0:1"}, nil)
	checkSent(t, *sent, line("French fries (Ketchup)", 1, 600))
}

func TestAnItemWithoutOptionsIsSentByItsName(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"gin::1"}, nil)
	checkSent(t, *sent, line("Gin and tonic", 1, 900))
}

func TestOneItemWithOtherOptionsIsSentAsTwoLines(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"beer:0:1", "beer:1:1"}, nil)
	checkSent(t, *sent, line("Draft beer (Glass)", 1, 700), line("Draft beer (Pint)", 1, 900))
}

func TestLinesOfTheSameItemAndOptionsAreSentAsOne(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"karaage:1:2", "edamame::1", "karaage:1:3"}, nil)
	checkSent(t, *sent, line("Fried chicken (8 pcs)", 5, 1250), line("Edamame", 1, 500))
}

func TestAnAmountFromThePhoneIsIgnored(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"highball:1:1"}, url.Values{"unit_price": {"1"}, "price": {"1"}, "amount": {"1"}, "total": {"1"}})
	checkSent(t, *sent, line("Whisky highball (Strong)", 1, 900))
}

// An order with any line off the menu is refused whole, and the customer chooses again.
func TestAnOrderOffTheMenuIsRefused(t *testing.T) {
	for name, lines := range map[string][]string{
		"unknown item":               {"gin::1", "wine::1"},
		"unknown choice":             {"beer:2:1"},
		"negative choice":            {"beer:-1:1"},
		"choice that is no number":   {"beer:x:1"},
		"no choice for an option":    {"beer::1"},
		"choice for no option":       {"gin:0:1"},
		"quantity 0":                 {"beer:0:0"},
		"quantity 100":               {"beer:0:100"},
		"quantity that is no number": {"beer:0:two"},
		"merged quantity 100":        {"beer:1:50", "beer:1:50"},
		"line with an amount":        {"beer:0:1:700"},
	} {
		t.Run(name, func(t *testing.T) {
			c, sent := orderingCustomer(t, "")
			page := c.order(lines, nil)
			if len(*sent) != 0 {
				t.Errorf("sent %+v to BarPay", *sent)
			}
			if !strings.Contains(page, `<p class="notice info" role="status"><i data-icon="info"></i><span>Please choose again</span></p>`) {
				t.Error("the menu does not ask to choose again")
			}
		})
	}
}

func TestNinetyNineOfOneLineAreTaken(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	c.order([]string{"beer:1:49", "beer:1:50"}, nil)
	checkSent(t, *sent, line("Draft beer (Pint)", 99, 900))
}

func TestAnOrderOfNothingAsksForAnItem(t *testing.T) {
	c, sent := orderingCustomer(t, "")
	page := c.order(nil, nil)
	if len(*sent) != 0 {
		t.Errorf("sent %+v to BarPay", *sent)
	}
	if !strings.Contains(page, `<p class="notice info" role="status"><i data-icon="info"></i><span>Choose at least one item</span></p>`) {
		t.Error("the menu does not ask for an item")
	}
}

// Two choices with the same label and other prices stay two lines, each at its own price.
func TestLinesAreMergedByTheirChoicesNotTheirName(t *testing.T) {
	saved := menuItems
	t.Cleanup(func() { menuItems = saved })
	menuItems = []menuItem{{"beer", "Draft beer", 700, "Drinks", []menuOption{{"Size", []menuChoice{{"Glass", 0}, {"Glass", 200}}}}}}
	lines, ok := orderLines([]string{"beer:0:1", "beer:1:1"})
	want := []OrderLine{line("Draft beer (Glass)", 1, 700), line("Draft beer (Glass)", 1, 900)}
	if !ok || !reflect.DeepEqual(lines, want) {
		t.Errorf("got %+v %v, want %+v", lines, ok, want)
	}
}

// A line of a million commas is refused without a slice of a million parts.
func TestALongLineIsRefusedWithoutSplittingIt(t *testing.T) {
	for _, raw := range []string{
		"beer:" + strings.Repeat(",", 1<<20) + ":1",
		"beer:0:1" + strings.Repeat(":", 1<<20),
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, ok := orderLines([]string{raw})
		runtime.ReadMemStats(&after)
		if ok {
			t.Errorf("a line of %d bytes is taken", len(raw))
		}
		if used := after.TotalAlloc - before.TotalAlloc; used > 1<<20 {
			t.Errorf("refusing a line of %d bytes allocated %d bytes", len(raw), used)
		}
	}
}

// The page's script draws the item sheet from the menu it is given, and reads the options of each
// item as a list.
func TestTheMenuPageGivesItsScriptEveryItemWithItsOptions(t *testing.T) {
	c, _ := orderingCustomer(t, "")
	m := regexp.MustCompile(`data-items="([^"]+)"`).FindStringSubmatch(c.open("/menu"))
	if m == nil {
		t.Fatal("the menu has no data-items")
	}
	var items map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range menuItems {
		got := items[item.ID]
		if got == nil {
			t.Errorf("%s is missing", item.ID)
			continue
		}
		if opts := string(got["options"]); !strings.HasPrefix(opts, "[") {
			t.Errorf("%s: options %s, want a list", item.ID, opts)
		}
	}
	if got := string(items["beer"]["options"]); got != `[{"label":"Size","choices":[{"label":"Glass","extra":0},{"label":"Pint","extra":200}]}]` {
		t.Errorf("beer: options %s", got)
	}
}

// menuData is what the menu handler gives the menu page.
type menuData struct {
	Sections  []menuSection
	ItemsJSON string
	Session   *Session
	Pending   *PendingOrder
	OrderID   string
}

func renderMenu(t *testing.T, state string, pending *PendingOrder, v view) string {
	t.Helper()
	v.Guest = true
	v.Data = menuData{menuSections(), "{}", &Session{State: state}, pending, "ord_00000000000000000000"}
	return renderPage(t, "menu", v)
}

func TestTheMenuListsItemsUnderDrinksAndFood(t *testing.T) {
	page := renderMenu(t, "active", nil, view{})
	drinks, food := strings.Index(page, "<h2>Drinks</h2>"), strings.Index(page, "<h2>Food</h2>")
	beer, fries := strings.Index(page, `<img class="thumb" src="/static/img/beer.svg" alt=""><span class="name">Draft beer</span><span class="price">$7.00</span>`), strings.Index(page, `data-id="fries"`)
	if drinks < 0 || food < 0 || beer < drinks || beer > food || fries < food {
		t.Error("the items are not under their headings with their image, name and price")
	}
	if strings.Contains(page, "has-cart") {
		t.Error("the menu leaves room for the cart bar before anything is in the cart")
	}
}

func TestEveryItemHasItsImage(t *testing.T) {
	for _, item := range menuItems {
		if _, err := staticFS.Open("static/img/" + item.ID + ".svg"); err != nil {
			t.Errorf("%s has no image: %v", item.ID, err)
		}
	}
}
