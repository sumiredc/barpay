// Package config reads and checks the config file the owner writes (the PRD's UC8).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/settings"
)

// DefaultPath is read when BarPay starts without -config. It is ignored by git.
const DefaultPath = "barpay.toml"

// MaxStayLimit is the PRD's UC8 exception: a maximum stay this long or longer does not start.
const MaxStayLimit = 72 * time.Hour

// P0Currencies are the currencies BarPay starts with. JPY is planned in P1 (the PRD's Priorities).
var P0Currencies = []money.Currency{money.USD}

// Options change what Parse accepts. The zero value accepts P0Currencies.
type Options struct {
	// Currencies BarPay can start with; nil means P0Currencies.
	Currencies []money.Currency
}

// Config is the config file after every check has passed. Secrets print as redacted.
type Config struct {
	ListenAddress string
	DatabasePath  string

	PayPalClientID     secret.Value
	PayPalClientSecret secret.Value

	StoreAPIKey      secret.Value
	AdminAPIKey      secret.Value
	EdgeSharedSecret secret.Value

	NotificationURL           string
	NotificationSigningSecret secret.Value

	// StoreAppOrigin is the only place members are returned to after linking.
	StoreAppOrigin string

	// Currency is the one the file names. While a currency change waits for a start with no open
	// session, BarPay runs in another one: use startup.State, not this, for the running currency.
	// StartingRate and StartingSettings are in this currency too.
	Currency     money.Currency
	StartingRate money.Amount // rate per hour
	// MaxStep is the most the rate per hour moves in one evaluation (the PRD's Pricing model, Guardrails).
	MaxStep money.Amount

	MinEvaluationInterval time.Duration
	LLMAddress            string
	LLMModel              string

	Capacity         int
	MaxStay          time.Duration
	ReunlockWindow   time.Duration
	ExitTokenRefresh time.Duration

	// StartingSettings are used on the first start and on a start that applies a currency change.
	StartingSettings settings.Settings
}

// file is the TOML layout. Amounts are integers in the currency's smallest unit.
type file struct {
	ListenAddress         string        `toml:"listen_address"`
	DatabasePath          string        `toml:"database_path"`
	Currency              string        `toml:"currency"`
	StartingRate          int64         `toml:"starting_rate"`
	MaxStep               int64         `toml:"max_step"`
	MinEvaluationInterval time.Duration `toml:"min_evaluation_interval"`
	Capacity              int           `toml:"capacity"`
	MaxStay               time.Duration `toml:"max_stay"`
	ReunlockWindow        time.Duration `toml:"reunlock_window"`
	ExitTokenRefresh      time.Duration `toml:"exit_token_refresh"`
	StoreAppOrigin        string        `toml:"store_app_origin"`

	Keys struct {
		StoreAPIKey      secret.Value `toml:"store_api_key"`
		AdminAPIKey      secret.Value `toml:"admin_api_key"`
		EdgeSharedSecret secret.Value `toml:"edge_shared_secret"`
	} `toml:"keys"`
	PayPal struct {
		ClientID     secret.Value `toml:"client_id"`
		ClientSecret secret.Value `toml:"client_secret"`
	} `toml:"paypal"`
	Notifications struct {
		URL           string       `toml:"url"`
		SigningSecret secret.Value `toml:"signing_secret"`
	} `toml:"notifications"`
	LLM struct {
		Address string `toml:"address"`
		Model   string `toml:"model"`
	} `toml:"llm"`
	Settings struct {
		Floor        int64 `toml:"floor"`
		Ceiling      int64 `toml:"ceiling"`
		BlockMinutes int   `toml:"block_minutes"`
		PauseMinutes int   `toml:"pause_minutes"`
		CapPerVisit  int64 `toml:"cap_per_visit"`
		Goal         int   `toml:"goal"`
	} `toml:"settings"`
}

// Defaults stated in the PRD (Pricing model, UC3, UC5). A key without one is required.
var defaults = map[string]any{
	"max_stay":               12 * time.Hour,
	"reunlock_window":        2 * time.Minute,
	"exit_token_refresh":     30 * time.Second,
	"settings.block_minutes": 10,
	"settings.pause_minutes": 30,
	// ¥3,000 or $30: 3000 in the smallest unit of either currency.
	"settings.cap_per_visit": int64(3000),
}

// Problem is one reason the config file does not start BarPay. Key is the TOML key.
// Reason never contains the value of a secret.
type Problem struct {
	Key    string
	Reason string
}

// Error lists every problem found, so the owner can fix them in one pass.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("config file:")
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		if p.Key != "" {
			b.WriteString(p.Key + ": ")
		}
		b.WriteString(p.Reason)
	}
	return b.String()
}

// Load reads and checks the file at path.
func Load(path string, opts Options) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	return Parse(data, opts)
}

// Parse checks a config file. It returns *Error when the file is readable TOML but breaks a rule.
func Parse(data []byte, opts Options) (*Config, error) {
	var f file
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&f)
	if err != nil {
		// The parser's message can quote the text it failed on, which may be a secret.
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, &Error{Problems: []Problem{{Key: pe.LastKey, Reason: fmt.Sprintf("cannot be read at line %d", pe.Position.Line)}}}
		}
		return nil, &Error{Problems: []Problem{{Reason: "cannot be decoded: " + err.Error()}}}
	}
	c := &checker{md: md}
	for _, k := range md.Undecoded() {
		c.add(k.String(), "unknown key")
	}
	c.applyDefaults(&f)
	cfg := c.build(&f, opts)
	if len(c.problems) > 0 {
		return nil, &Error{Problems: c.problems}
	}
	return cfg, nil
}

type checker struct {
	md       toml.MetaData
	problems []Problem
}

func (c *checker) add(key, format string, args ...any) {
	c.problems = append(c.problems, Problem{Key: key, Reason: fmt.Sprintf(format, args...)})
}

func (c *checker) defined(key string) bool {
	return c.md.IsDefined(strings.Split(key, ".")...)
}

func (c *checker) applyDefaults(f *file) {
	set := map[string]func(any){
		"max_stay":               func(v any) { f.MaxStay = v.(time.Duration) },
		"reunlock_window":        func(v any) { f.ReunlockWindow = v.(time.Duration) },
		"exit_token_refresh":     func(v any) { f.ExitTokenRefresh = v.(time.Duration) },
		"settings.block_minutes": func(v any) { f.Settings.BlockMinutes = v.(int) },
		"settings.pause_minutes": func(v any) { f.Settings.PauseMinutes = v.(int) },
		"settings.cap_per_visit": func(v any) { f.Settings.CapPerVisit = v.(int64) },
	}
	for key, v := range defaults {
		if !c.defined(key) {
			set[key](v)
		}
	}
}

func (c *checker) required(key string) bool {
	if c.defined(key) {
		return true
	}
	c.add(key, "required")
	return false
}

func (c *checker) text(key, v string) string {
	if c.required(key) && v == "" {
		c.add(key, "must not be empty")
	}
	return v
}

func (c *checker) credential(key string, v secret.Value) secret.Value {
	if !c.defined(key) || v.IsZero() {
		c.add(key, "credential is missing")
	}
	return v
}

func (c *checker) positive(key string, d time.Duration) time.Duration {
	if _, hasDefault := defaults[key]; !hasDefault && !c.required(key) {
		return d
	}
	// The decoder also takes a bare integer as nanoseconds, so "max_stay = 60" would be 60ns.
	if c.defined(key) && c.md.Type(strings.Split(key, ".")...) != "String" {
		c.add(key, `must be a duration such as "30s"`)
		return d
	}
	if d <= 0 {
		c.add(key, "must be longer than 0")
	}
	return d
}

// databasePath refuses "?" and "#", which the store's SQLite URI would read as the start of its
// options or a fragment, opening another file than the one written here.
func (c *checker) databasePath(p string) string {
	if c.text("database_path", p) != "" && strings.ContainsAny(p, "?#") {
		c.add("database_path", `must not contain "?" or "#"`)
	}
	return p
}

// notificationURL enforces the PRD's UC8 exception for the notification URL: https, or http to
// a loopback IP address written as one. A name such as localhost is refused even over http,
// because what it resolves to is not known when the file is read.
func (c *checker) notificationURL(v string) string {
	const key = "notifications.url"
	if c.text(key, v) == "" {
		return v
	}
	// The reasons do not quote the URL: it can carry credentials in its user info or query.
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		c.add(key, "must be an absolute URL such as https://store.example/barpay")
		return v
	}
	switch u.Scheme {
	case "https":
		return v
	case "http":
		// An IPv4-mapped IPv6 address such as ::ffff:127.0.0.1 is refused: the PRD names only
		// 127.0.0.0/8 written as IPv4, and ::1.
		host := u.Hostname()
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && (ip.Equal(net.IPv6loopback) || !strings.Contains(host, ":")) {
			return v
		}
		c.add(key, "must use https; http is accepted only to a loopback IP address such as 127.0.0.1 or [::1], not to a name such as localhost")
	default:
		c.add(key, "must use https")
	}
	return v
}

func (c *checker) build(f *file, opts Options) *Config {
	cfg := &Config{
		ListenAddress: c.text("listen_address", f.ListenAddress),
		DatabasePath:  c.databasePath(f.DatabasePath),

		PayPalClientID:     c.credential("paypal.client_id", f.PayPal.ClientID),
		PayPalClientSecret: c.credential("paypal.client_secret", f.PayPal.ClientSecret),
		StoreAPIKey:        c.credential("keys.store_api_key", f.Keys.StoreAPIKey),
		AdminAPIKey:        c.credential("keys.admin_api_key", f.Keys.AdminAPIKey),
		EdgeSharedSecret:   c.credential("keys.edge_shared_secret", f.Keys.EdgeSharedSecret),

		NotificationURL:           c.notificationURL(f.Notifications.URL),
		NotificationSigningSecret: c.credential("notifications.signing_secret", f.Notifications.SigningSecret),
		StoreAppOrigin:            c.text("store_app_origin", f.StoreAppOrigin),

		MinEvaluationInterval: c.positive("min_evaluation_interval", f.MinEvaluationInterval),
		LLMAddress:            c.text("llm.address", f.LLM.Address),
		LLMModel:              c.text("llm.model", f.LLM.Model),

		MaxStay:          c.positive("max_stay", f.MaxStay),
		ReunlockWindow:   c.positive("reunlock_window", f.ReunlockWindow),
		ExitTokenRefresh: c.positive("exit_token_refresh", f.ExitTokenRefresh),
	}
	c.distinctKeys(cfg)

	if cfg.MaxStay >= MaxStayLimit {
		c.add("max_stay", "must be shorter than 3 days")
	}
	if c.required("capacity") && f.Capacity < 1 {
		c.add("capacity", "must be at least 1")
	}
	cfg.Capacity = f.Capacity

	cfg.Currency = c.currency(f.Currency, opts)
	cur := cfg.Currency
	cfg.StartingSettings = settings.Settings{
		Currency:     cur,
		Floor:        money.New(f.Settings.Floor, cur),
		Ceiling:      money.New(f.Settings.Ceiling, cur),
		BlockMinutes: f.Settings.BlockMinutes,
		PauseMinutes: f.Settings.PauseMinutes,
		CapPerVisit:  money.New(f.Settings.CapPerVisit, cur),
		Goal:         f.Settings.Goal,
	}
	cfg.StartingRate = money.New(f.StartingRate, cur)

	floorSet := c.required("settings.floor")
	ceilingSet := c.required("settings.ceiling")
	goalSet := c.required("settings.goal")
	settingsOK := true
	if floorSet && ceilingSet && goalSet && cur != "" {
		var es settings.Errors
		if errors.As(cfg.StartingSettings.Validate(), &es) {
			settingsOK = false
			for _, e := range es {
				c.add("settings."+e.Field, "%s", e.Reason)
			}
		}
	} else {
		settingsOK = false
	}
	if c.required("starting_rate") && settingsOK {
		step, _ := money.RateStep(cur)
		switch {
		case f.StartingRate < f.Settings.Floor || f.StartingRate > f.Settings.Ceiling:
			c.add("starting_rate", "must be within settings.floor (%d) and settings.ceiling (%d)", f.Settings.Floor, f.Settings.Ceiling)
		case !cfg.StartingRate.IsMultipleOf(step):
			c.add("starting_rate", "must be a multiple of the rate step (%d)", step.Value())
		}
	}
	cfg.MaxStep = c.maxStep(f.MaxStep, cur)
	return cfg
}

const maxAmount = money.MaxValue

// maxStep enforces the PRD's UC8 exception for the max step: at least one rate step, so the rate
// can move, and a multiple of it, so a rate rounded to the step never moves further than this.
func (c *checker) maxStep(v int64, cur money.Currency) money.Amount {
	a := money.New(v, cur)
	if !c.required("max_step") || cur == "" {
		return a
	}
	step, _ := money.RateStep(cur)
	switch {
	case v < step.Value():
		c.add("max_step", "must be at least the rate step (%d)", step.Value())
	case v > maxAmount:
		c.add("max_step", "must be at most %d", maxAmount)
	case !a.IsMultipleOf(step):
		c.add("max_step", "must be a multiple of the rate step (%d)", step.Value())
	}
	return a
}

// distinctKeys enforces the PRD's UC8 exception: with two equal keys, one would pass where the
// other is required, and Access's separation of the keys would mean nothing.
func (c *checker) distinctKeys(cfg *Config) {
	keys := []struct {
		name string
		v    secret.Value
	}{
		{"keys.store_api_key", cfg.StoreAPIKey},
		{"keys.admin_api_key", cfg.AdminAPIKey},
		{"keys.edge_shared_secret", cfg.EdgeSharedSecret},
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			a, b := keys[i], keys[j]
			if !a.v.IsZero() && a.v.Equal(b.v) {
				c.add(b.name, "must differ from %s", a.name)
			}
		}
	}
}

func (c *checker) currency(v string, opts Options) money.Currency {
	if !c.required("currency") {
		return ""
	}
	cur := money.Currency(v)
	if !slices.Contains(money.Known, cur) {
		c.add("currency", "must be one of %s", join(money.Known))
		return ""
	}
	supported := opts.Currencies
	if supported == nil {
		supported = P0Currencies
	}
	if !slices.Contains(supported, cur) {
		c.add("currency", "%s is not supported yet; use %s", cur, join(supported))
		return ""
	}
	return cur
}

func join(cs []money.Currency) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ", ")
}
