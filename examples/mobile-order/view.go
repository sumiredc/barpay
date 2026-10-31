package main

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

type pages struct {
	byName map[string]*template.Template
}

var funcs = template.FuncMap{
	"money":       formatMoney,
	"signedMoney": formatSignedMoney,
	"time":        func(t time.Time) string { return t.Local().Format("Jan 2 15:04:05") },
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
		if n.Name() == "layout.html" {
			continue
		}
		t := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+n.Name()))
		p.byName[strings.TrimSuffix(n.Name(), ".html")] = t
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
}

func (a *App) render(w http.ResponseWriter, status int, name string, v view) {
	t := a.pages.byName[name]
	if t == nil {
		panic("no page " + name)
	}
	v.StoreName = a.cfg.StoreName
	var b strings.Builder
	if err := t.Execute(&b, v); err != nil {
		log.Printf("page %s: %v", name, err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, b.String())
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

// chart is a stepped ticker of the rate over a visit, drawn as SVG coordinates.
type chart struct {
	Path        string
	Width       int
	Height      int
	Low, High   string
	Start, End  string
	HasMultiple bool
}

// rateChart draws the rate from the session's check-in to end: each rate holds until the next.
func rateChart(points []RatePoint, start, end time.Time) chart {
	const w, h, pad = 320, 120, 8
	c := chart{Width: w, Height: h, Start: start.Local().Format("15:04"), End: end.Local().Format("15:04")}
	if len(points) == 0 || !end.After(start) {
		return c
	}
	lo, hi := points[0].RatePerHour.Value, points[0].RatePerHour.Value
	for _, p := range points {
		lo, hi = min(lo, p.RatePerHour.Value), max(hi, p.RatePerHour.Value)
	}
	c.Low, c.High = formatMoney(Money{lo, points[0].RatePerHour.Currency}), formatMoney(Money{hi, points[0].RatePerHour.Currency})
	c.HasMultiple = hi != lo
	span := float64(end.Sub(start))
	x := func(t time.Time) float64 {
		if t.Before(start) {
			t = start
		}
		return pad + float64(t.Sub(start))/span*(w-2*pad)
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
