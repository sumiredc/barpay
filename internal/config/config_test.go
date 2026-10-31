package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
)

// Distinguishable secret values, so a leak of any of them is found by a substring search.
var secrets = map[string]string{
	"paypal.client_id":             "SECRET-paypal-id-41f2",
	"paypal.client_secret":         "SECRET-paypal-secret-77ab",
	"keys.store_api_key":           "SECRET-store-key-0c9d",
	"keys.admin_api_key":           "SECRET-admin-key-5e13",
	"keys.edge_shared_secret":      "SECRET-edge-secret-a6b0",
	"notifications.signing_secret": "SECRET-signing-3d88",
}

const valid = `
listen_address = ":8080"
database_path = "barpay.db"
currency = "USD"
starting_rate = 1200
max_step = 300
min_evaluation_interval = "1m"
capacity = 40
max_stay = "10h"
reunlock_window = "90s"
exit_token_refresh = "20s"
store_app_origin = "https://store.example"

[keys]
store_api_key = "SECRET-store-key-0c9d"
admin_api_key = "SECRET-admin-key-5e13"
edge_shared_secret = "SECRET-edge-secret-a6b0"

[paypal]
client_id = "SECRET-paypal-id-41f2"
client_secret = "SECRET-paypal-secret-77ab"

[notifications]
url = "https://store.example/barpay/notify"
signing_secret = "SECRET-signing-3d88"

[llm]
address = "http://llm.example:11434"
model = "pricing-model"

[settings]
floor = 600
ceiling = 2400
block_minutes = 15
pause_minutes = 20
cap_per_visit = 2500
goal = 70
`

// with returns valid with the line that sets key replaced by line, or removed when line is "".
// key is written as in the file: "currency", or "settings.floor" for floor under [settings].
func with(t *testing.T, key, line string) string {
	t.Helper()
	return edit(t, valid, key, line)
}

// edit is with on data instead of valid.
func edit(t *testing.T, data, key, line string) string {
	t.Helper()
	name := key[strings.LastIndex(key, ".")+1:]
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` = .*$`)
	locs := re.FindAllStringIndex(data, -1)
	if len(locs) != 1 {
		t.Fatalf("edit(%q): %d lines match", key, len(locs))
	}
	return data[:locs[0][0]] + line + data[locs[0][1]:]
}

func parse(t *testing.T, data string) (*Config, *Error) {
	t.Helper()
	cfg, err := Parse([]byte(data), Options{})
	if err == nil {
		return cfg, nil
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("Parse error is %T (%v), want *Error", err, err)
	}
	return nil, ce
}

func mustParse(t *testing.T, data string) *Config {
	t.Helper()
	cfg, ce := parse(t, data)
	if ce != nil {
		t.Fatalf("Parse: %v", ce)
	}
	return cfg
}

func mustFail(t *testing.T, data, key string) *Error {
	t.Helper()
	_, ce := parse(t, data)
	if ce == nil {
		t.Fatalf("Parse succeeded, want a problem with %s", key)
	}
	if !slices.ContainsFunc(ce.Problems, func(p Problem) bool { return p.Key == key }) {
		t.Fatalf("Parse: %v; want a problem with %s", ce, key)
	}
	return ce
}

func TestEveryItemIsLoaded(t *testing.T) {
	cfg := mustParse(t, valid)
	usd := func(v int64) money.Amount { return money.New(v, money.USD) }
	checks := []struct {
		name      string
		got, want any
	}{
		{"listen address", cfg.ListenAddress, ":8080"},
		{"database path", cfg.DatabasePath, "barpay.db"},
		{"PayPal client ID", cfg.PayPalClientID.Reveal(), secrets["paypal.client_id"]},
		{"PayPal client secret", cfg.PayPalClientSecret.Reveal(), secrets["paypal.client_secret"]},
		{"store API key", cfg.StoreAPIKey.Reveal(), secrets["keys.store_api_key"]},
		{"admin API key", cfg.AdminAPIKey.Reveal(), secrets["keys.admin_api_key"]},
		{"edge shared secret", cfg.EdgeSharedSecret.Reveal(), secrets["keys.edge_shared_secret"]},
		{"notification URL", cfg.NotificationURL, "https://store.example/barpay/notify"},
		{"signing secret", cfg.NotificationSigningSecret.Reveal(), secrets["notifications.signing_secret"]},
		{"store app origin", cfg.StoreAppOrigin, "https://store.example"},
		{"currency", cfg.Currency, money.USD},
		{"starting rate", cfg.StartingRate, usd(1200)},
		{"max step", cfg.MaxStep, usd(300)},
		{"min evaluation interval", cfg.MinEvaluationInterval, time.Minute},
		{"LLM address", cfg.LLMAddress, "http://llm.example:11434"},
		{"LLM model", cfg.LLMModel, "pricing-model"},
		{"capacity", cfg.Capacity, 40},
		{"max stay", cfg.MaxStay, 10 * time.Hour},
		{"re-unlock window", cfg.ReunlockWindow, 90 * time.Second},
		{"exit token refresh", cfg.ExitTokenRefresh, 20 * time.Second},
		{"starting settings", cfg.StartingSettings, settings.Settings{
			Currency: money.USD, Floor: usd(600), Ceiling: usd(2400),
			BlockMinutes: 15, PauseMinutes: 20, CapPerVisit: usd(2500), Goal: 70,
		}},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The PRD's defaults apply only to keys left out.
func TestDefaults(t *testing.T) {
	data := valid
	for _, k := range []string{"max_stay", "reunlock_window", "exit_token_refresh",
		"settings.block_minutes", "settings.pause_minutes", "settings.cap_per_visit"} {
		name := k[strings.LastIndex(k, ".")+1:]
		data = regexp.MustCompile(`(?m)^`+name+` = .*$`).ReplaceAllString(data, "")
	}
	cfg := mustParse(t, data)
	s := cfg.StartingSettings
	if cfg.MaxStay != 12*time.Hour || cfg.ReunlockWindow != 2*time.Minute || cfg.ExitTokenRefresh != 30*time.Second {
		t.Errorf("durations = %v, %v, %v; want 12h, 2m, 30s", cfg.MaxStay, cfg.ReunlockWindow, cfg.ExitTokenRefresh)
	}
	if s.BlockMinutes != 10 || s.PauseMinutes != 30 || s.CapPerVisit != money.New(3000, money.USD) {
		t.Errorf("settings = %+v; want block 10, pause 30, cap 3000", s)
	}

	// 0 written in the file is kept, not replaced by the default.
	cfg = mustParse(t, with(t, "settings.pause_minutes", "pause_minutes = 0"))
	if cfg.StartingSettings.PauseMinutes != 0 {
		t.Errorf("pause_minutes = 0 in the file became %d", cfg.StartingSettings.PauseMinutes)
	}
}

func TestSampleDoesNotStartForMissingCredentials(t *testing.T) {
	data, err := os.ReadFile("../../barpay.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	_, ce := parse(t, string(data))
	if ce == nil {
		t.Fatal("the sample config file starts BarPay")
	}
	for key := range secrets {
		i := slices.IndexFunc(ce.Problems, func(p Problem) bool { return p.Key == key })
		if i < 0 || !strings.Contains(ce.Problems[i].Reason, "credential") {
			t.Errorf("no missing-credential problem for %s in:\n%v", key, ce)
		}
		if !strings.Contains(ce.Error(), key) {
			t.Errorf("the error text does not name %s", key)
		}
	}
	// Apart from the credentials, only the store's own addresses are left for the owner.
	for _, p := range ce.Problems {
		if _, isSecret := secrets[p.Key]; !isSecret && !slices.Contains(
			[]string{"store_app_origin", "notifications.url", "llm.address", "llm.model"}, p.Key) {
			t.Errorf("unexpected problem in the sample: %s: %s", p.Key, p.Reason)
		}
	}
}

func TestMissingCredential(t *testing.T) {
	for key := range secrets {
		name := key[strings.LastIndex(key, ".")+1:]
		t.Run(key+" empty", func(t *testing.T) { mustFail(t, with(t, key, name+` = ""`), key) })
		t.Run(key+" absent", func(t *testing.T) { mustFail(t, with(t, key, ""), key) })
	}
}

func TestKeysMustDiffer(t *testing.T) {
	store, admin := secrets["keys.store_api_key"], secrets["keys.admin_api_key"]
	cases := map[string]string{
		"store = admin": with(t, "keys.admin_api_key", `admin_api_key = "`+store+`"`),
		"store = edge":  with(t, "keys.edge_shared_secret", `edge_shared_secret = "`+store+`"`),
		"admin = edge":  with(t, "keys.edge_shared_secret", `edge_shared_secret = "`+admin+`"`),
	}
	want := map[string]string{"store = admin": "keys.admin_api_key", "store = edge": "keys.edge_shared_secret", "admin = edge": "keys.edge_shared_secret"}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) { mustFail(t, data, want[name]) })
	}
}

func TestFloorMustBeBelowCeiling(t *testing.T) {
	mustFail(t, with(t, "settings.floor", "floor = 2400"), "settings.floor")
	mustFail(t, with(t, "settings.floor", "floor = 2401"), "settings.floor")
	mustParse(t, edit(t, with(t, "settings.floor", "floor = 2395"), "starting_rate", "starting_rate = 2400"))
}

func TestStartingRateWithinFloorAndCeiling(t *testing.T) {
	for rate, ok := range map[string]bool{"599": false, "600": true, "2400": true, "2401": false} {
		data := with(t, "starting_rate", "starting_rate = "+rate)
		if ok {
			mustParse(t, data)
		} else {
			mustFail(t, data, "starting_rate")
		}
	}
}

// The starting rate is on the rate step (5 cents for USD), as every published rate is.
func TestStartingRateOnTheRateStep(t *testing.T) {
	for rate, ok := range map[string]bool{"1199": false, "1200": true, "1201": false, "1204": false, "1205": true} {
		data := with(t, "starting_rate", "starting_rate = "+rate)
		if ok {
			mustParse(t, data)
		} else {
			mustFail(t, data, "starting_rate")
		}
	}
}

// The max step is in cents per hour, at least one rate step (5) and a multiple of it.
func TestMaxStep(t *testing.T) {
	for v, ok := range map[string]bool{
		"-5": false, "0": false, "4": false, "5": true, "6": false, "9": false, "10": true,
		"99999995": true, "100000000": true, "100000005": false,
	} {
		data := with(t, "max_step", "max_step = "+v)
		if ok {
			if got := mustParse(t, data).MaxStep; got.Value() == 0 || got.Currency() != money.USD {
				t.Errorf("max_step = %s read as %v", v, got)
			}
		} else {
			mustFail(t, data, "max_step")
		}
	}
	for _, line := range []string{`max_step = "300"`, `max_step = 1.5`} {
		if _, ce := parse(t, with(t, "max_step", line)); ce == nil || !strings.Contains(ce.Error(), "max_step") {
			t.Errorf("%s: Parse error = %v; want one naming max_step", line, ce)
		}
	}
}

func TestMaxStayShorterThanThreeDays(t *testing.T) {
	mustParse(t, with(t, "max_stay", `max_stay = "71h59m59s"`))
	mustFail(t, with(t, "max_stay", `max_stay = "72h"`), "max_stay")
	mustFail(t, with(t, "max_stay", `max_stay = "72h0m1s"`), "max_stay")
	mustFail(t, with(t, "max_stay", `max_stay = "0s"`), "max_stay")
}

// The starting values pass the same conditions as a change through the admin API (UC7).
func TestStartingSettingsFollowUC7(t *testing.T) {
	cases := []struct{ key, line string }{
		{"settings.floor", "floor = 0"},
		{"settings.floor", "floor = 601"},
		{"settings.ceiling", "ceiling = 2399"},
		{"settings.ceiling", "ceiling = 100000001"},
		{"settings.block_minutes", "block_minutes = 0"},
		{"settings.block_minutes", "block_minutes = 241"},
		{"settings.pause_minutes", "pause_minutes = -1"},
		{"settings.cap_per_visit", "cap_per_visit = -1"},
		{"settings.goal", "goal = 101"},
		{"settings.goal", ""},
	}
	for _, c := range cases {
		t.Run(c.key+" "+c.line, func(t *testing.T) { mustFail(t, with(t, c.key, c.line), c.key) })
	}
}

func TestCurrency(t *testing.T) {
	ce := mustFail(t, with(t, "currency", `currency = "JPY"`), "currency")
	if !strings.Contains(ce.Error(), "JPY is not supported") {
		t.Errorf("JPY error does not say why: %v", ce)
	}
	mustFail(t, with(t, "currency", `currency = "EUR"`), "currency")
	mustFail(t, with(t, "currency", ""), "currency")

	// With JPY in the supported list, the same file starts, and amounts are in JPY.
	cfg, err := Parse([]byte(with(t, "currency", `currency = "JPY"`)), Options{Currencies: []money.Currency{money.USD, money.JPY}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Currency != money.JPY || cfg.StartingSettings.Floor.Currency() != money.JPY || cfg.StartingRate.Currency() != money.JPY {
		t.Errorf("currency = %s, floor %v, starting rate %v; want everything in JPY", cfg.Currency, cfg.StartingSettings.Floor, cfg.StartingRate)
	}
}

// A bare integer would decode as nanoseconds; every duration key must be written as a string.
func TestDurationsMustBeStrings(t *testing.T) {
	for _, key := range []string{"min_evaluation_interval", "max_stay", "reunlock_window", "exit_token_refresh"} {
		t.Run(key, func(t *testing.T) {
			ce := mustFail(t, with(t, key, key+" = 60"), key)
			if !strings.Contains(ce.Error(), `must be a duration such as "30s"`) {
				t.Errorf("error does not say how to write it: %v", ce)
			}
		})
	}
}

// The PRD's UC8 exception for the notification URL.
func TestNotificationURL(t *testing.T) {
	accepted := []string{
		"https://store.example/barpay/notify",
		"https://203.0.113.7:8443/notify",
		"http://127.0.0.1:9000/notify",
		"http://127.255.0.9/notify",
		"http://[::1]:9000/notify",
	}
	for _, u := range accepted {
		t.Run(u, func(t *testing.T) {
			cfg := mustParse(t, with(t, "notifications.url", `url = "`+u+`"`))
			if cfg.NotificationURL != u {
				t.Errorf("NotificationURL = %q, want %q", cfg.NotificationURL, u)
			}
		})
	}
	refused := map[string]string{
		"http://localhost:9000/notify": "localhost",
		"http://LOCALHOST/notify":      "localhost",
		"http://store.example/notify":  "https",
		"http://192.0.2.10/notify":     "https",
		"http://128.0.0.1/notify":      "https",
		"http://[::2]/notify":          "https",
		"http://[::ffff:127.0.0.1]/n":  "https",
		"http://127.0.0.1.example/n":   "https",
		"ftp://store.example/notify":   "https",
		"ws://127.0.0.1/notify":        "https",
		"store.example/notify":         "absolute URL",
		"https:///notify":              "absolute URL",
		"https:store.example":          "absolute URL",
	}
	for u, why := range refused {
		t.Run(u, func(t *testing.T) {
			ce := mustFail(t, with(t, "notifications.url", `url = "`+u+`"`), "notifications.url")
			if !strings.Contains(ce.Error(), why) {
				t.Errorf("error does not say why (%q): %v", why, ce)
			}
		})
	}
}

func TestDatabasePathWithoutURISeparators(t *testing.T) {
	for _, p := range []string{"barpay.db?mode=memory", "data#1.db"} {
		mustFail(t, with(t, "database_path", `database_path = "`+p+`"`), "database_path")
	}
}

func TestOtherRequiredValues(t *testing.T) {
	for _, key := range []string{"listen_address", "database_path", "store_app_origin", "notifications.url",
		"llm.address", "llm.model", "min_evaluation_interval", "capacity", "starting_rate", "max_step", "settings.floor", "settings.ceiling"} {
		t.Run(key, func(t *testing.T) { mustFail(t, with(t, key, ""), key) })
	}
	mustFail(t, with(t, "capacity", "capacity = 0"), "capacity")
}

func TestUnknownKey(t *testing.T) {
	mustFail(t, valid+"\n[extra]\nstore_api_kye = \"x\"\n", "extra.store_api_kye")
}

func TestSecretsDoNotLeak(t *testing.T) {
	cfg := mustParse(t, valid)
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		outs = append(outs, fmt.Sprintf(verb, cfg), fmt.Sprintf(verb, *cfg))
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(js))
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "config", cfg)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", cfg)
	outs = append(outs, buf.String())

	// Errors from checks that look at secrets, and from a file the parser cannot read.
	store := secrets["keys.store_api_key"]
	// The TOML lexer quotes a bare word up to the first character it cannot take, such as "-" or a digit.
	const bare = "SECRETbareword"
	broken := []string{
		with(t, "keys.admin_api_key", `admin_api_key = "`+store+`"`),
		with(t, "keys.store_api_key", `store_api_key = `+bare),
		with(t, "keys.store_api_key", `store_api_key = "`+store),
		with(t, "keys.store_api_key", `store_api_key = 12`) + "\n" + store,
		with(t, "settings.floor", "floor = 0"),
	}
	for _, data := range broken {
		if _, err := Parse([]byte(data), Options{}); err != nil {
			outs = append(outs, err.Error(), fmt.Sprintf("%+v", err))
		} else {
			t.Errorf("a broken file parsed")
		}
	}

	for _, out := range outs {
		for _, v := range append(slices.Collect(maps.Values(secrets)), bare) {
			if strings.Contains(out, v) {
				t.Errorf("output contains a secret value:\n%s", out)
			}
		}
	}
}
