package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// pricingBarPay is an adminBarPay that also answers the settings save.
type pricingBarPay struct {
	*adminBarPay
	mu    sync.Mutex
	saved []Settings
}

func (f *pricingBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "PUT" && r.URL.Path == "/v1/admin/settings":
		var s Settings
		json.NewDecoder(r.Body).Decode(&s)
		f.saved = append(f.saved, s)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s)
	default:
		f.adminBarPay.ServeHTTP(w, r)
	}
}

func usd(v int64) Money { return Money{Value: v, Currency: "USD"} }

// newPricingStaff is newStaff with the app talking to a pricingBarPay.
func newPricingStaff(t *testing.T) (*staff, *pricingBarPay) {
	t.Helper()
	s := newStaff(t)
	f := &pricingBarPay{adminBarPay: s.barpay}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.a.cfg.BarPayURL = srv.URL
	s.a.barpay = newBarPay(s.a.cfg)
	return s, f
}

func TestTheSettingsTabSavesTheFormAndComesBack(t *testing.T) {
	s, f := newPricingStaff(t)
	s.logIn()
	page := s.open("/admin/settings")
	for _, want := range []string{`<a href="/admin/settings" aria-current="page">Settings</a>`, `name="floor" type="text" value="8.00"`,
		`<a href="/admin/pricing" aria-current="page"><i data-icon="trending-up"></i><span>Pricing</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings tab has no %s", want)
		}
	}
	resp := s.postTo("/admin/settings", url.Values{"currency": {"USD"}, "floor": {"9.50"}, "ceiling": {"20.00"},
		"cap_per_visit": {"25.00"}, "block_minutes": {"10"}, "pause_minutes": {"0"}, "goal": {"60"}})
	if resp.path != "/admin/settings" || !strings.Contains(resp.body, "The settings were saved.") {
		t.Errorf("saving went to %s, want /admin/settings telling it was saved", resp.path)
	}
	want := Settings{Floor: usd(950), Ceiling: usd(2000), CapPerVisit: usd(2500), BlockMinutes: 10, Goal: 60}
	if len(f.saved) != 1 || f.saved[0] != want {
		t.Errorf("BarPay got %+v, want %+v", f.saved, want)
	}
	if resp := s.postTo("/admin/settings", url.Values{"floor": {"x"}}); resp.path != "/admin/settings" {
		t.Errorf("a form that cannot be read went to %s, want /admin/settings", resp.path)
	}
}

func TestTheGateCanBeUnlockedFromTheSettingsTab(t *testing.T) {
	s, _ := newPricingStaff(t)
	s.logIn()
	path, body := s.unlock("/admin/settings", "none")
	if path != "/admin/settings" || !strings.Contains(body, "The exit gate was told to unlock once.") {
		t.Errorf("an unlock from the settings tab went to %s", path)
	}
}

type landed struct{ path, body string }

// postTo sends a form from the settings tab and returns where the browser lands.
func (s *staff) postTo(path string, form url.Values) landed {
	s.t.Helper()
	m := csrfPattern.FindStringSubmatch(s.open("/admin/settings"))
	form.Set("csrf", m[1])
	resp, err := s.client.PostForm(s.app.URL+path, form)
	if err != nil {
		s.t.Fatal(err)
	}
	return landed{resp.Request.URL.Path, s.read(resp)}
}
