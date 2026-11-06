package main

import "net/http"

// adminSettings is the "Settings" tab: the settings form, which is sent to adminUpdateSettings.
func (a *App) adminSettings(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	ctx, cancel := a.ctx(r)
	defer cancel()
	v := a.adminView(r, id, b, "Pricing", "pricing")
	s, err := a.barpay.getSettings(ctx, b.StaffID)
	data := struct {
		Settings *Settings
		Failed   bool
	}{Failed: err != nil}
	if err == nil {
		data.Settings = s
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_settings", v)
}
