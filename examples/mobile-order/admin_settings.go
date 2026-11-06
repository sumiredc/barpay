package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Pricing: the "Settings" tab, which changes BarPay's settings.

// maxAmount is the largest amount BarPay takes (docs/api/openapi.yaml, Money), in cents.
const maxAmount = 100_000_000

// usdRateStep is the step of USD rates (5 cents), which BarPay takes the floor and the ceiling
// on. The app's amounts are in USD, like its menu.
const usdRateStep = 5

// settingsForm is the form of the "Settings" tab as staff typed it. Bad marks the fields that
// cannot be saved, and Hint says under each group of fields how to fix it: "range" (the floor
// and the ceiling), "cap", "block", "pause" and "goal".
type settingsForm struct {
	Currency                                string
	Floor, Ceiling, Cap, Block, Pause, Goal string
	Bad                                     map[string]bool
	Hint                                    map[string]string
}

func formOf(s Settings) settingsForm {
	return settingsForm{
		Currency: s.Floor.Currency,
		Floor:    plainAmount(s.Floor.Value), Ceiling: plainAmount(s.Ceiling.Value), Cap: plainAmount(s.CapPerVisit.Value),
		Block: strconv.Itoa(s.BlockMinutes), Pause: strconv.Itoa(s.PauseMinutes), Goal: strconv.Itoa(s.Goal),
	}
}

// plainAmount writes cents as staff type them, such as 12.50.
func plainAmount(cents int64) string {
	return strconv.FormatInt(cents/100, 10) + "." + strconv.FormatInt(cents%100/10, 10) + strconv.FormatInt(cents%10, 10)
}

// readSettingsForm reads the posted form into the settings BarPay takes, or returns false with
// the form marked where it cannot be saved. The rules are BarPay's for the amounts and the
// numbers (docs/api/openapi.yaml, Settings and Money). The currency is not checked: it comes from
// the hidden field as BarPay sent it, and BarPay refuses one that is not the store's.
func readSettingsForm(v url.Values) (Settings, settingsForm, bool) {
	f := settingsForm{
		Currency: v.Get("currency"),
		Floor:    v.Get("floor"), Ceiling: v.Get("ceiling"), Cap: v.Get("cap_per_visit"),
		Block: v.Get("block_minutes"), Pause: v.Get("pause_minutes"), Goal: v.Get("goal"),
		Bad: map[string]bool{}, Hint: map[string]string{},
	}
	whole := func(s string, lo, hi int) (int, bool) {
		s = strings.TrimSpace(s)
		n, err := strconv.Atoi(s)
		return n, err == nil && onlyDigits(s) && n >= lo && n <= hi
	}
	amount := func(s string) (int64, bool) {
		c, ok := parseAmount(s)
		return c, ok && c <= maxAmount
	}
	floor, okFloor := amount(f.Floor)
	okFloor = okFloor && floor > 0 && floor%usdRateStep == 0
	ceiling, okCeiling := amount(f.Ceiling)
	okCeiling = okCeiling && ceiling > 0 && ceiling%usdRateStep == 0 && (!okFloor || ceiling > floor)
	capPerVisit, okCap := amount(f.Cap)
	block, okBlock := whole(f.Block, 1, 240)
	pause, okPause := whole(f.Pause, 0, 240)
	goal, okGoal := whole(f.Goal, 0, 100)
	f.Bad["floor"], f.Bad["ceiling"] = !okFloor, !okCeiling
	switch {
	case !okFloor:
		f.Hint["range"] = "Make the left amount more than $0.00, in steps of $0.05"
	case !okCeiling:
		f.Hint["range"] = "Make the right amount larger than the left, in steps of $0.05"
	}
	if !okCap {
		f.Bad["cap"], f.Hint["cap"] = true, "Enter an amount such as 25.00, or 0 for no cap"
	}
	if !okBlock {
		f.Bad["block"], f.Hint["block"] = true, "Enter whole minutes from 1 to 240"
	}
	if !okPause {
		f.Bad["pause"], f.Hint["pause"] = true, "Enter whole minutes from 0 to 240, or 0 for none"
	}
	if !okGoal {
		f.Bad["goal"], f.Hint["goal"] = true, "Choose a goal from 0 to 100"
	}
	if !(okFloor && okCeiling && okCap && okBlock && okPause && okGoal) {
		return Settings{}, f, false
	}
	money := func(c int64) Money { return Money{Value: c, Currency: f.Currency} }
	return Settings{Floor: money(floor), Ceiling: money(ceiling), BlockMinutes: block, PauseMinutes: pause,
		CapPerVisit: money(capPerVisit), Goal: goal}, f, true
}

type settingsPage struct {
	// Form is nil when BarPay did not return the settings: they are not changed without knowing
	// them.
	Form    *settingsForm
	Invalid bool
	Preview settingsPreview
}

// settingsPreview is what the card "What a customer pays" is drawn from. admin.js works the
// amounts out in the browser from these and the form, as staff type, without asking the server.
type settingsPreview struct {
	// Rate is the published rate, which "Now" starts from; nil when BarPay did not return it, and
	// the card then offers the floor and the ceiling only.
	Rate *Money
	// Saved are the settings BarPay has. The card is drawn from them while the form cannot be
	// saved; nil when they could not be read.
	Saved *Settings
	// Drink is the drink the customer orders on arrival, 30 and 60 minutes later.
	Drink      string
	DrinkPrice Money
}

// previewDrink is the first item of the menu's Drinks with its first choices, such as
// "Draft beer (Glass)" at $7.00.
func previewDrink() (string, Money) {
	for _, item := range menuItems {
		if item.Kind != "Drinks" {
			continue
		}
		name, price := item.Name, item.Price
		var labels []string
		for _, o := range item.Options {
			labels = append(labels, o.Choices[0].Label)
			price += o.Choices[0].Extra
		}
		if len(labels) > 0 {
			name += " (" + strings.Join(labels, ", ") + ")"
		}
		return name, Money{Value: price, Currency: menuCurrency}
	}
	return "", Money{Currency: menuCurrency}
}

func (a *App) settingsPreview(ctx context.Context, saved *Settings) settingsPreview {
	p := settingsPreview{Saved: saved}
	p.Drink, p.DrinkPrice = previewDrink()
	if r, err := a.barpay.getRate(ctx); err == nil {
		p.Rate = &r.RatePerHour
	}
	return p
}

// adminSettings is the "Settings" tab, with the settings BarPay has.
func (a *App) adminSettings(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	var data settingsPage
	if s, err := a.barpay.getSettings(ctx, b.StaffID); err == nil {
		f := formOf(*s)
		data.Form = &f
		data.Preview = a.settingsPreview(ctx, s)
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_settings", v)
}

// adminUpdateSettings saves the form of the "Settings" tab and goes back to it. A form that
// cannot be saved is drawn again as it was typed, with what to fix.
func (a *App) adminUpdateSettings(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	s, f, ok := readSettingsForm(r.PostForm)
	if !ok {
		v := a.adminView(r, id, b, "Pricing", "pricing")
		saved, err := a.barpay.getSettings(ctx, b.StaffID)
		if err != nil {
			saved = nil
		}
		v.Data = settingsPage{Form: &f, Invalid: true, Preview: a.settingsPreview(ctx, saved)}
		a.render(w, http.StatusOK, "admin_settings", v)
		return
	}
	if _, err := a.barpay.updateSettings(ctx, b.StaffID, s); err != nil {
		a.flash(id, "The settings were not changed: "+err.Error())
	} else {
		a.toast(id, "Saved")
	}
	redirect(w, r, "/admin/settings")
}
