package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// settingsFormValues is the form of the Settings tab as staff send it, with values BarPay takes.
func settingsFormValues() url.Values {
	return url.Values{"currency": {"USD"}, "floor": {"9.50"}, "ceiling": {"20.00"},
		"cap_per_visit": {"25.00"}, "block_minutes": {"10"}, "pause_minutes": {"0"}, "goal": {"60"}}
}

type settingsLanded struct {
	status     int
	path, body string
}

// settingsPost sends a form from the Settings tab, with its CSRF token, and returns where the
// browser lands, whatever its status.
func (s *staff) settingsPost(form url.Values) settingsLanded {
	s.t.Helper()
	m := csrfPattern.FindStringSubmatch(s.open("/admin/settings"))
	if m == nil {
		s.t.Fatal("the Settings tab has no form")
	}
	form.Set("csrf", m[1])
	resp, err := s.client.PostForm(s.app.URL+"/admin/settings", form)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return settingsLanded{resp.StatusCode, resp.Request.URL.Path, string(b)}
}

func TestTheSettingsTabShowsTheSettingsUnderThreeHeadings(t *testing.T) {
	s, _ := newPricingStaff(t)
	s.logIn()
	page := s.open("/admin/settings")
	for _, want := range []string{
		`<a href="/admin/settings" aria-current="page">Settings</a>`,
		`<a href="/admin/pricing" aria-current="page"><i data-icon="trending-up"></i><span>Pricing</span>`,
		`<button class="textbutton" type="button" data-sheet="spec"><i data-icon="info"></i>&nbsp;How the cover charge works</button>`,
		`<h2 style="margin-top: 0">Cover charge</h2>`, `<label for="floor">Rate per hour</label>`,
		`id="floor" name="floor" type="text" inputmode="decimal" value="8.00" aria-label="Floor"`,
		`id="ceiling" name="ceiling" type="text" inputmode="decimal" value="20.00" aria-label="Ceiling"`,
		`<label for="cap">Cap per visit</label>`, `name="cap_per_visit" type="text" inputmode="decimal" value="25.00"`, `0 for no cap`,
		`<h2 style="margin-top: 0">Charging</h2>`, `<label for="block">Every</label>`, `name="block_minutes" type="number" min="1" max="240" value="10"`, `>min<`,
		`<label for="pause">Free after an order</label>`, `name="pause_minutes" type="number" min="0" max="240" value="0"`, `0 for none`,
		`<h2>AI</h2>`, `<label for="goal">Pricing goal</label>`, `<span class="muted">Bring people in</span><input id="goal" name="goal" type="range" min="0" max="100" value="50"><span class="muted">Grow revenue</span>`,
		`When it's quiet`, `When it's busy`, `<a class="textbutton" href="/admin/pricing" style="padding-left: 0">See the AI's decisions`,
		`<button class="button" type="submit">Save</button>`,
		`<dialog class="sheet spec" id="spec">`, `<h2>How the cover charge works</h2>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Settings tab has no %s", want)
		}
	}
}

func TestSavingTheSettingsSendsThemAndSaysSaved(t *testing.T) {
	s, f := newPricingStaff(t)
	s.logIn()
	got := s.settingsPost(settingsFormValues())
	if got.status != http.StatusOK || got.path != "/admin/settings" {
		t.Fatalf("saving landed on %s with %d, want /admin/settings", got.path, got.status)
	}
	if !strings.Contains(got.body, `<p class="notice good" role="status"><i data-icon="circle-check"></i><span>Saved</span></p>`) {
		t.Errorf("the Settings tab does not say Saved:\n%s", got.body)
	}
	want := Settings{Floor: usd(950), Ceiling: usd(2000), CapPerVisit: usd(2500), BlockMinutes: 10, Goal: 60}
	if len(f.saved) != 1 || f.saved[0] != want {
		t.Errorf("BarPay got %+v, want %+v", f.saved, want)
	}
	if again := s.open("/admin/settings"); strings.Contains(again, "Saved") {
		t.Error("Saved is still shown on the next visit to the tab")
	}
}

func TestSettingsThatCannotBeSavedAreMarkedWithHowToFixThem(t *testing.T) {
	for _, c := range []struct {
		name, field, value string
		bad                []string
		hint               string
	}{
		{"a ceiling below the floor", "ceiling", "9.00", []string{"ceiling"}, "Make the right amount larger than the left, in steps of $0.05"},
		{"a ceiling equal to the floor", "ceiling", "9.50", []string{"ceiling"}, "Make the right amount larger than the left, in steps of $0.05"},
		{"a ceiling off the step", "ceiling", "20.02", []string{"ceiling"}, "Make the right amount larger than the left, in steps of $0.05"},
		{"a floor of 0", "floor", "0", []string{"floor"}, "Make the left amount more than $0.00, in steps of $0.05"},
		{"a floor off the step", "floor", "9.99", []string{"floor"}, "Make the left amount more than $0.00, in steps of $0.05"},
		{"a cap that is not an amount", "cap_per_visit", "lots", []string{"cap"}, "Enter an amount such as 25.00, or 0 for no cap"},
		{"a block of 0 minutes", "block_minutes", "0", []string{"block"}, "Enter whole minutes from 1 to 240"},
		{"a pause over 240 minutes", "pause_minutes", "241", []string{"pause"}, "Enter whole minutes from 0 to 240, or 0 for none"},
		{"a goal over 100", "goal", "101", []string{"goal"}, "Choose a goal from 0 to 100"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, f := newPricingStaff(t)
			s.logIn()
			form := settingsFormValues()
			form.Set(c.field, c.value)
			got := s.settingsPost(form)
			if got.status != http.StatusOK || got.path != "/admin/settings" {
				t.Errorf("landed on %s with %d, want the Settings tab again", got.path, got.status)
			}
			if len(f.saved) != 0 {
				t.Errorf("BarPay got %+v", f.saved)
			}
			if !strings.Contains(got.body, `<span>Couldn't save. Fix the fields in red.</span>`) {
				t.Error("the page does not say it couldn't save")
			}
			if !strings.Contains(got.body, `<p class="hint up">`+c.hint+`</p>`) {
				t.Errorf("the page does not say %q", c.hint)
			}
			if n := strings.Count(got.body, `aria-invalid="true"`); n != len(c.bad) {
				t.Errorf("%d fields are marked, want %v", n, c.bad)
			}
			if !strings.Contains(got.body, `value="`+c.value+`"`) {
				t.Errorf("the page does not keep %s as typed", c.value)
			}
		})
	}
}

func TestTheSettingsTabMarksAnAmountFieldOnItsBox(t *testing.T) {
	s, _ := newPricingStaff(t)
	s.logIn()
	form := settingsFormValues()
	form.Set("ceiling", "9.00")
	got := s.settingsPost(form)
	want := `<span class="money invalid"><span>$</span><input id="ceiling" name="ceiling" type="text" inputmode="decimal" value="9.00" aria-label="Ceiling" aria-invalid="true"></span>`
	if !strings.Contains(got.body, want) {
		t.Errorf("the ceiling is not marked red:\n%s", got.body)
	}
}

func TestReadingTheSettingsFormTakesTheEndsBarPayTakes(t *testing.T) {
	// bad is the field marked, "" when the form is readable. Each case changes the form of
	// settingsFormValues (floor 9.50, ceiling 20.00) by set.
	for _, c := range []struct {
		set map[string]string
		bad string
	}{
		{map[string]string{"floor": "0.05"}, ""},
		{map[string]string{"ceiling": "9.55"}, ""},
		{map[string]string{"floor": "999999.95", "ceiling": "1000000.00"}, ""},
		{map[string]string{"floor": "1000000.05", "ceiling": "1000000.10"}, "floor"},
		{map[string]string{"ceiling": "1000000.05"}, "ceiling"},
		{map[string]string{"cap_per_visit": "1000000.00"}, ""},
		{map[string]string{"cap_per_visit": "1000000.01"}, "cap"},
		{map[string]string{"cap_per_visit": "0"}, ""},
		{map[string]string{"block_minutes": "1"}, ""},
		{map[string]string{"block_minutes": "240"}, ""},
		{map[string]string{"block_minutes": "241"}, "block"},
		{map[string]string{"block_minutes": "+5"}, "block"},
		{map[string]string{"pause_minutes": "0"}, ""},
		{map[string]string{"pause_minutes": "-1"}, "pause"},
		{map[string]string{"goal": "0"}, ""},
		{map[string]string{"goal": "100"}, ""},
		{map[string]string{"goal": "-1"}, "goal"},
	} {
		v := settingsFormValues()
		for k, x := range c.set {
			v.Set(k, x)
		}
		_, f, ok := readSettingsForm(v)
		if ok != (c.bad == "") || (c.bad != "" && !f.Bad[c.bad]) {
			t.Errorf("%v: readable %v, marked %v, want %q marked", c.set, ok, f.Bad, c.bad)
		}
	}
}

func TestACeilingOf0IsMarkedAlsoWhenTheFloorIsWrong(t *testing.T) {
	v := settingsFormValues()
	v.Set("floor", "0")
	v.Set("ceiling", "0")
	if _, f, _ := readSettingsForm(v); !f.Bad["floor"] || !f.Bad["ceiling"] {
		t.Errorf("marked %v, want the floor and the ceiling", f.Bad)
	}
}

// An amount has digits only on each side of its point, as admin.js reads it, so the card and the
// save agree on what can be saved.
func TestAnAmountStaffTypeHasOnlyDigitsAroundItsPoint(t *testing.T) {
	for _, c := range []struct {
		in    string
		cents int64
		ok    bool
	}{
		{"12.00", 1200, true},
		{"$12.5", 1250, true},
		{" 12 ", 1200, true},
		{"12.", 1200, true},
		{"+12.00", 0, false},
		{"12.+5", 0, false},
		{"-1", 0, false},
		{"1_000", 0, false},
		{".50", 0, false},
	} {
		got, ok := parseAmount(c.in)
		if ok != c.ok || (ok && got != c.cents) {
			t.Errorf("parseAmount(%q) = %d, %v; want %d, %v", c.in, got, ok, c.cents, c.ok)
		}
	}
}

func TestSettingsBarPayDoesNotReturnCannotBeChanged(t *testing.T) {
	s, _ := newPricingStaff(t)
	s.logIn()
	s.barpay.mu.Lock()
	s.barpay.down = true
	s.barpay.mu.Unlock()
	page, _, _ := strings.Cut(s.open("/admin/settings"), gateDialogStart)
	want := `<span>BarPay didn't return the current settings, so they can't be changed yet. Wait a moment, then <a href="/admin/settings">reload</a>.</span>`
	if !strings.Contains(page, want) {
		t.Errorf("the page does not say the settings could not be read:\n%s", page)
	}
	if strings.Contains(page, `action="/admin/settings"`) {
		t.Error("the form is drawn without the settings")
	}
	if strings.Contains(page, "BarPay didn't answer.") {
		t.Error("the page also draws the notice of the other admin pages")
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

// settingsBarPay is a pricingBarPay whose published rate can be refused on its own.
type settingsBarPay struct {
	*pricingBarPay
	rateDown bool // under pricingBarPay.mu
}

func (f *settingsBarPay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.rateDown
	f.mu.Unlock()
	if down && r.URL.Path == "/v1/rate" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"unauthorized","message":"down for the test"}}`))
		return
	}
	f.pricingBarPay.ServeHTTP(w, r)
}

// newSettingsStaff is newPricingStaff with the app talking to a settingsBarPay.
func newSettingsStaff(t *testing.T) (*staff, *settingsBarPay) {
	t.Helper()
	s := newStaff(t)
	f := &settingsBarPay{pricingBarPay: &pricingBarPay{adminBarPay: s.barpay}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s.a.cfg.BarPayURL = srv.URL
	s.a.barpay = newBarPay(s.a.cfg)
	return s, f
}

// The amounts of the card are worked out in the browser (static/admin.js); the server gives it
// the rate, the drink and the saved settings.
func TestThePreviewIsGivenTheRateTheDrinkAndTheSavedSettings(t *testing.T) {
	s, _ := newSettingsStaff(t)
	s.logIn()
	page := s.open("/admin/settings")
	for _, want := range []string{
		`<section class="card preview" id="preview" data-drink="700" data-rate="1200" data-floor="800" data-ceiling="2000" data-cap="2500" data-block="10" data-pause="0" aria-label="What a customer pays" aria-live="polite">`,
		`<h2 style="margin: 0">What a customer pays</h2>`,
		`Per customer (cover charge + drinks at <span data-p="drink">$7.00</span> each)`,
		`<span>Total</span>`, `<span>Cover charge only</span>`,
		`<span>Floor <span data-p="floor"></span></span>`,
		`<input type="radio" name="basis" value="now" checked><span>Now <span data-p="now"></span></span>`,
		`<span>Ceiling <span data-p="ceiling"></span></span>`,
		`<div class="label">2 h, 2 drinks</div>`, `About <span data-p="headline"></span>`,
		`Cover charge <span data-p="headcharge"></span> + drinks <span data-p="headdrink"></span>`,
		`1 drink</span>`, `2 drinks</span>`, `3 drinks</span>`, `<i class="sw free"></i>Free after an order</span>`,
		`The drinks are the menu's Draft beer (Glass) at $7.00: the first on arrival, the second 30 minutes later and the third 60 minutes later.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Settings tab has no %s", want)
		}
	}
}

func TestThePreviewDrinkIsTheFirstDrinkOfTheMenu(t *testing.T) {
	name, price := previewDrink()
	if name != "Draft beer (Glass)" || price != usd(700) {
		t.Errorf("the drink is %s at %+v, want Draft beer (Glass) at $7.00", name, price)
	}
}

func TestWithoutTheRateThePreviewOffersTheFloorAndTheCeiling(t *testing.T) {
	s, f := newSettingsStaff(t)
	s.logIn()
	f.mu.Lock()
	f.rateDown = true
	f.mu.Unlock()
	page := s.open("/admin/settings")
	if strings.Contains(page, "data-rate=") || strings.Contains(page, `value="now"`) {
		t.Error("the card offers Now without the published rate")
	}
	if !strings.Contains(page, `<input type="radio" name="basis" value="floor" checked>`) {
		t.Error("the floor is not chosen in place of Now")
	}
}

func TestAFormThatCannotBeSavedKeepsThePreviewOfTheSavedSettings(t *testing.T) {
	s, _ := newSettingsStaff(t)
	s.logIn()
	form := settingsFormValues()
	form.Set("ceiling", "9.00")
	got := s.settingsPost(form)
	if !strings.Contains(got.body, `data-rate="1200" data-floor="800" data-ceiling="2000" data-cap="2500" data-block="10" data-pause="0"`) {
		t.Errorf("the card is not given the saved settings:\n%s", got.body)
	}
}
