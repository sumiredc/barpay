package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var adminKey = "Bearer " + keys.Admin.Reveal()

const validBody = `{"floor":{"value":1500,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"},
	"block_minutes":15,"pause_minutes":0,"cap_per_visit":{"value":0,"currency":"USD"},"goal":90}`

func send(r http.Handler, method, path, auth, staffID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if staffID != "" {
		req.Header.Set(staff.Header, staffID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func putSettings(r http.Handler, body string) *httptest.ResponseRecorder {
	return send(r, http.MethodPut, "/v1/admin/settings", adminKey, "staff_1", body)
}

func savedSettings(t *testing.T, st *store.Store) settings.Settings {
	t.Helper()
	s, _, err := settings.Load(context.Background(), st.Read())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGetSettings(t *testing.T) {
	w := send(pricingRouter(started(t)), http.MethodGet, "/v1/admin/settings", adminKey, "staff_1", "")
	want := settingsJSON{Floor: Money{600, "USD"}, Ceiling: Money{2400, "USD"}, BlockMinutes: 10,
		PauseMinutes: 30, CapPerVisit: Money{3000, "USD"}, Goal: 50}
	if w.Code != http.StatusOK || decode[settingsJSON](t, w) != want {
		t.Errorf("GET settings: %d %s", w.Code, w.Body)
	}
}

// A valid change takes effect with 200, is saved with the staff member and survives a restart;
// a floor above the rate moves it.
func TestUpdateSettings(t *testing.T) {
	st := started(t)
	w := putSettings(pricingRouter(st), validBody)
	want := settingsJSON{Floor: Money{1500, "USD"}, Ceiling: Money{2400, "USD"}, BlockMinutes: 15,
		PauseMinutes: 0, CapPerVisit: Money{0, "USD"}, Goal: 90}
	if w.Code != http.StatusOK || decode[settingsJSON](t, w) != want {
		t.Fatalf("PUT settings: %d %s", w.Code, w.Body)
	}

	st = storetest.Reopen(t, st)
	r := pricingRouter(st)
	if w := send(r, http.MethodGet, "/v1/admin/settings", adminKey, "staff_2", ""); decode[settingsJSON](t, w) != want {
		t.Errorf("after restart GET settings = %s", w.Body)
	}
	actions, err := staff.Actions(context.Background(), st.Read(), "settings")
	if err != nil || len(actions) != 1 || actions[0].Staff != "staff_1" {
		t.Errorf("staff actions = %+v, %v; want one by staff_1", actions, err)
	}
	if rate := decode[rateWithCap](t, do(r, "/v1/rate", storeAuth)); rate.RatePerHour.Value != 1500 || rate.Change.Value != 300 {
		t.Errorf("rate after raising the floor to 1500 = %+v; want 1500 (+300)", rate)
	}
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code, field string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Field string `json:"field"`
			} `json:"details"`
		} `json:"error"`
	}
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != code ||
		body.Error.Details.Field != field || body.Error.Message == "" {
		t.Errorf("got %d %s; want %d %s on %q", w.Code, w.Body, status, code, field)
	}
}

func with(field, value string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(validBody), &m); err != nil {
		panic(err)
	}
	if value == "" {
		delete(m, field)
	} else {
		m[field] = json.RawMessage(value)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// A body that does not fit the spec's Settings schema is 400 invalid_request, and nothing changes.
func TestUpdateSettingsRefusesBodiesOutsideTheSchema(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"not JSON", "floor=1500", ""},
		{"an array", "[]", ""},
		{"two values", validBody + validBody, ""},
		{"floor missing", with("floor", ""), "floor"},
		{"goal missing", with("goal", ""), "goal"},
		{"floor null", with("floor", "null"), "floor"},
		{"value missing", with("floor", `{"currency":"USD"}`), "floor.value"},
		{"currency missing", with("ceiling", `{"value":2400}`), "ceiling.currency"},
		{"unknown field", with("max_step", "300"), "max_step"},
		{"unknown money field", with("floor", `{"value":1500,"currency":"USD","x":1}`), "floor.x"},
		{"a key in another case", strings.Replace(validBody, `"floor"`, `"Floor"`, 1), "Floor"},
		{"a money key in another case", with("ceiling", `{"Value":2400,"currency":"USD"}`), "ceiling.Value"},
		{"a duplicate key", strings.Replace(validBody, `"goal":90`, `"goal":90,"goal":10`, 1), "goal"},
		{"a duplicate key, the first invalid", strings.Replace(validBody, `"goal":90`, `"goal":"x","goal":10`, 1), "goal"},
		{"a duplicate key after nested values", strings.Replace(validBody, `"goal":90`, `"goal":90,"floor":{"value":600,"currency":"USD"}`, 1), "floor"},
		{"a duplicate money key", with("floor", `{"value":1500,"value":1000,"currency":"USD"}`), "floor.value"},
		{"an object where a number goes", with("goal", `{"goal":1}`), "goal"},
		{"a string", with("block_minutes", `"15"`), "block_minutes"},
		{"not an integer", with("goal", "1.5"), "goal"},
		{"value not an integer", with("floor", `{"value":1500.5,"currency":"USD"}`), "floor.value"},
		{"negative value", with("cap_per_visit", `{"value":-1,"currency":"USD"}`), "cap_per_visit.value"},
		{"value above the maximum", with("ceiling", `{"value":100000001,"currency":"USD"}`), "ceiling.value"},
		{"currency not in the enum", with("floor", `{"value":1500,"currency":"EUR"}`), "floor.currency"},
		{"block 0", with("block_minutes", "0"), "block_minutes"},
		{"block 241", with("block_minutes", "241"), "block_minutes"},
		{"pause -1", with("pause_minutes", "-1"), "pause_minutes"},
		{"pause 241", with("pause_minutes", "241"), "pause_minutes"},
		{"goal -1", with("goal", "-1"), "goal"},
		{"goal 101", with("goal", "101"), "goal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := started(t)
			assertError(t, putSettings(pricingRouter(st), c.body), http.StatusBadRequest, "invalid_request", c.field)
			if s := savedSettings(t, st); s != startingSettings() {
				t.Errorf("settings changed to %+v", s)
			}
		})
	}
	// The schema's bounds themselves pass.
	for _, body := range []string{with("block_minutes", "240"), with("pause_minutes", "240"), with("goal", "0"),
		with("ceiling", `{"value":100000000,"currency":"USD"}`), with("block_minutes", "1")} {
		if w := putSettings(pricingRouter(started(t)), body); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s; want 200", body, w.Code, w.Body)
		}
	}
}

// A body that fits the schema but breaks UC7 or the rate step is 422 invalid_settings, and
// nothing changes: not the settings, the rate or the staff record.
func TestUpdateSettingsRefusesInvalidSettings(t *testing.T) {
	cases := []struct{ name, body, field string }{
		{"floor 0", with("floor", `{"value":0,"currency":"USD"}`), "floor"},
		{"floor equal to ceiling", with("floor", `{"value":2400,"currency":"USD"}`), "floor"},
		{"floor above ceiling", with("floor", `{"value":2405,"currency":"USD"}`), "floor"},
		{"floor off the rate step", with("floor", `{"value":1501,"currency":"USD"}`), "floor"},
		{"ceiling off the rate step", with("ceiling", `{"value":2404,"currency":"USD"}`), "ceiling"},
		{"floor in another currency", with("floor", `{"value":1500,"currency":"JPY"}`), "floor"},
		{"cap in another currency", with("cap_per_visit", `{"value":0,"currency":"JPY"}`), "cap_per_visit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := started(t)
			r := pricingRouter(st)
			assertError(t, putSettings(r, c.body), http.StatusUnprocessableEntity, "invalid_settings", c.field)
			actions, _ := staff.Actions(context.Background(), st.Read(), "settings")
			rate := decode[rateWithCap](t, do(r, "/v1/rate", storeAuth))
			if s := savedSettings(t, st); s != startingSettings() || len(actions) != 0 || rate.RatePerHour.Value != 1200 {
				t.Errorf("something changed: settings %+v, %d staff actions, rate %+v", s, len(actions), rate)
			}
		})
	}
	// On the step, just above and below: 1495 and 1505 pass.
	for _, v := range []string{"1495", "1505"} {
		if w := putSettings(pricingRouter(started(t)), with("floor", `{"value":`+v+`,"currency":"USD"}`)); w.Code != http.StatusOK {
			t.Errorf("floor %s: %d %s; want 200", v, w.Code, w.Body)
		}
	}
}

// Only the admin API key reads and changes the settings, and a refused request changes nothing.
func TestSettingsOperationsNeedTheAdminKey(t *testing.T) {
	st := started(t)
	r := pricingRouter(st)
	for name, auth := range map[string]string{
		"no key": "", "wrong key": "Bearer nope", "store API key": storeAuth,
		"edge shared secret": "Bearer " + keys.Edge.Reveal(),
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(name+" "+method, func(t *testing.T) {
				assertUnauthorized(t, send(r, method, "/v1/admin/settings", auth, "staff_1", validBody))
			})
		}
	}
	if s := savedSettings(t, st); s != startingSettings() {
		t.Errorf("settings changed to %+v", s)
	}
}

func TestSettingsOperationsNeedAStaffID(t *testing.T) {
	st := started(t)
	r := pricingRouter(st)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		assertError(t, send(r, method, "/v1/admin/settings", adminKey, "", validBody),
			http.StatusBadRequest, "invalid_request", "BarPay-Staff-Id")
	}
	if s := savedSettings(t, st); s != startingSettings() {
		t.Errorf("settings changed to %+v", s)
	}
}
