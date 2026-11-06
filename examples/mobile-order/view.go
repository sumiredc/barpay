package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS holds what the customer pages load besides the page: the style sheet, the script and
// the store's logo. The app serves them itself, as its Content-Security-Policy takes scripts from
// the same origin only.
//
//go:embed static
var staticFS embed.FS

// staticFiles serves the files of staticFS under /static/, and answers 404 for a directory
// rather than listing it.
func staticFiles() http.Handler {
	files := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := fs.Stat(staticFS, strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}

type pages struct {
	byName map[string]*template.Template
}

// customerPage is where a customer page sits in the frame that customer_layout.html draws.
type customerPage struct {
	// tab is the tab the page belongs to: "menu", "bill", "account", or "" for none.
	tab string
	// header is the small logo and store name at the top. The entrance has its own large logo,
	// and the pages without the tab bar have no header either.
	header bool
	// tabbar shows the tab bar to a member or a guest. There is no back button, so without it a
	// page would be a dead end. The entrance shows it only during a visit, as its handler decides.
	tabbar bool
}

// customerPages lists every customer page; loadPages stops on a page that is neither here nor a
// staff page.
var customerPages = map[string]customerPage{
	"landing":  {},
	"login":    {},
	"signup":   {},
	"error":    {},
	"menu":     {tab: "menu", header: true, tabbar: true},
	"panel":    {tab: "bill", header: true, tabbar: true},
	"exit":     {tab: "bill", header: true, tabbar: true},
	"account":  {tab: "account", header: true, tabbar: true},
	"paypal":   {tab: "account", header: true, tabbar: true},
	"receipts": {tab: "account", header: true, tabbar: true},
	"receipt":  {tab: "account", header: true, tabbar: true},
}

// isStaffPage tells the pages under /admin, which keep layout.html.
func isStaffPage(name string) bool {
	return strings.HasPrefix(name, "admin_") || name == "staff_login"
}

var funcs = template.FuncMap{
	"money":       formatMoney,
	"signedMoney": formatSignedMoney,
	"time":        func(t time.Time) string { return t.Local().Format("Jan 2 15:04:05") },
	"day":         func(t time.Time) string { return t.Local().Format("Mon, Jan 2") },
	"clock":       func(t time.Time) string { return t.Local().Format("15:04") },
	"duration":    formatDuration,
	"deref": func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	},
	"stateLabel": stateLabel,
	"percent":    func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"dollars":    func(m Money) string { return fmt.Sprintf("%d.%02d", m.Value/100, m.Value%100) },
	"usd":        func(cents int64) Money { return Money{Value: cents, Currency: menuCurrency} },
	"has":        slices.Contains[[]string],
}

func loadPages() *pages {
	names, err := templateFS.ReadDir("templates")
	if err != nil {
		panic(err)
	}
	p := &pages{byName: map[string]*template.Template{}}
	for _, n := range names {
		if n.Name() == "layout.html" || n.Name() == "customer_layout.html" {
			continue
		}
		name := strings.TrimSuffix(n.Name(), ".html")
		layout := "customer_layout.html"
		if isStaffPage(name) {
			layout = "layout.html"
		} else if _, ok := customerPages[name]; !ok {
			panic("page " + name + " is neither a staff page nor in customerPages")
		}
		t := template.Must(template.New(layout).Funcs(funcs).ParseFS(templateFS, "templates/"+layout, "templates/"+n.Name()))
		p.byName[name] = t
	}
	return p
}

// view is what every page gets: the page's own data, plus who is logged in.
type view struct {
	Title     string
	StoreName string
	CSRF      string
	Member    *MemberAccount
	Guest     bool
	StaffID   string
	Admin     bool
	Flash     string
	Refresh   int
	Data      any

	// Frame and Here are drawn on the admin pages only. Frame is nil on the staff login. Here is
	// the page's own path and query, where a form sent from its frame comes back to.
	Frame *staffFrame
	Here  string

	// Alert is an error, shown in red at the top of the page and left there; Flash is shown there
	// in yellow, as guidance. The customer pages and the admin pages both draw them.
	Alert string

	// The rest is drawn on the customer pages only.

	// Toast says that an operation went through. It covers the top of the page for 4 seconds.
	Toast string
	// Tabbar is set by the entrance's handler during a visit, and by render on the other pages.
	Tabbar    bool
	Tab       string
	Header    bool
	Cartbar   template.HTML
	BodyClass string
}

func (a *App) render(w http.ResponseWriter, status int, name string, v view) {
	t := a.pages.byName[name]
	if t == nil {
		panic("no page " + name)
	}
	v.StoreName = a.cfg.StoreName
	var err error
	if page, ok := customerPages[name]; ok {
		v, err = frame(t, page, v)
	}
	var b strings.Builder
	if err == nil {
		err = t.Execute(&b, v)
	}
	if err != nil {
		log.Printf("page %s: %v", name, err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, b.String())
}

// frame fills what customer_layout.html draws around a customer page. A page's "cartbar"
// template is its primary action, fixed at the bottom. It is drawn before the page, because the
// body leaves room for it only when it draws something.
func frame(t *template.Template, page customerPage, v view) (view, error) {
	v = place(page, v)
	if c := t.Lookup("cartbar"); c != nil {
		var b strings.Builder
		if err := c.Execute(&b, v); err != nil {
			return v, err
		}
		// html/template has escaped it already.
		v.Cartbar = template.HTML(strings.TrimSpace(b.String()))
	}
	var classes []string
	if v.Tabbar {
		classes = append(classes, "has-tabbar")
	}
	if v.Cartbar != "" {
		classes = append(classes, "has-cart")
	}
	v.BodyClass = strings.Join(classes, " ")
	return v, nil
}

// place sets the header and the tabs of a customer page for whoever views it.
func place(page customerPage, v view) view {
	v.Tab, v.Header = page.tab, page.header
	if page.tabbar && (v.Member != nil || v.Guest) {
		v.Tabbar = true
	}
	return v
}

func formatMoney(m Money) string {
	switch m.Currency {
	case "JPY":
		return fmt.Sprintf("¥%s", thousands(m.Value))
	default:
		return fmt.Sprintf("$%s.%02d", thousands(m.Value/100), m.Value%100)
	}
}

func formatSignedMoney(m Money) string {
	switch {
	case m.Value > 0:
		return "+" + formatMoney(m)
	case m.Value < 0:
		return "−" + formatMoney(Money{Value: -m.Value, Currency: m.Currency})
	default:
		return "±" + formatMoney(m)
	}
}

func thousands(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h == 0 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d min", h, m)
}

func stateLabel(s string) string {
	switch s {
	case "active":
		return "Active"
	case "checking_out":
		return "Checking out"
	case "at_register":
		return "Waiting at the register"
	case "payment_failed":
		return "Payment failed"
	case "closed":
		return "Closed"
	case "closed_unpaid":
		return "Closed unpaid"
	}
	return s
}

// chart is the small stepped line of the rate over a visit on the bill, drawn as SVG coordinates.
type chart struct {
	Path          string
	Width, Height int
	// From is the rate at check-in and To the latest one; the line is read out with them.
	From, To string
}

// rateChart draws the rate from the session's check-in to end: each rate holds until the next.
func rateChart(points []RatePoint, start, end time.Time) chart {
	const w, h, pad = 120, 32, 4
	c := chart{Width: w, Height: h}
	if len(points) == 0 || !end.After(start) {
		return c
	}
	lo, hi := points[0].RatePerHour.Value, points[0].RatePerHour.Value
	for _, p := range points {
		lo, hi = min(lo, p.RatePerHour.Value), max(hi, p.RatePerHour.Value)
	}
	c.From, c.To = formatMoney(points[0].RatePerHour), formatMoney(points[len(points)-1].RatePerHour)
	span := float64(end.Sub(start))
	x := func(t time.Time) float64 {
		if t.Before(start) {
			t = start
		}
		return float64(t.Sub(start)) / span * w
	}
	y := func(v int64) float64 {
		if hi == lo {
			return h / 2
		}
		return pad + float64(hi-v)/float64(hi-lo)*(h-2*pad)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "M%.1f,%.1f", x(start), y(points[0].RatePerHour.Value))
	for _, p := range points[1:] {
		fmt.Fprintf(&b, " H%.1f V%.1f", x(p.PublishedAt), y(p.RatePerHour.Value))
	}
	fmt.Fprintf(&b, " H%.1f", x(end))
	c.Path = b.String()
	return c
}
