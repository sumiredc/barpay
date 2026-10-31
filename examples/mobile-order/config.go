package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Config is the sample app's config file: "key = value" lines, "#" comments, and a "staff" key
// that may repeat. The file holds the keys BarPay gave the store, so git ignores it; only the
// example file with no values is in the repository.
type Config struct {
	ListenAddress string
	// PublicURL is this app as the customer's browser opens it. Its origin is the one set as
	// store_app_origin in BarPay's config file, and PayPal links return to it.
	PublicURL     *url.URL
	DataPath      string
	StoreName     string
	BarPayURL     string
	StoreAPIKey   string
	AdminAPIKey   string
	SigningSecret string
	// Staff maps a staff ID, which is sent to BarPay as BarPay-Staff-Id, to its password hash.
	Staff map[string]string
}

var staffIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	cfg := &Config{Staff: map[string]string{}}
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want key = value", path, n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "staff" {
			id, hash, ok := strings.Cut(value, " ")
			if !ok || !staffIDPattern.MatchString(id) || !isPasswordHash(strings.TrimSpace(hash)) {
				return nil, fmt.Errorf("%s:%d: staff wants a staff ID (letters, digits, _ or -) and the hash printed by hash-password", path, n)
			}
			cfg.Staff[id] = strings.TrimSpace(hash)
			continue
		}
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("%s:%d: %s is set twice", path, n, key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var problems []string
	// Every key is required.
	take := func(key string) string {
		v := values[key]
		delete(values, key)
		if v == "" {
			problems = append(problems, key+" is missing or empty")
		}
		return v
	}
	cfg.ListenAddress = take("listen_address")
	public := take("public_url")
	cfg.DataPath = take("data_path")
	cfg.StoreName = take("store_name")
	cfg.BarPayURL = strings.TrimSuffix(take("barpay_url"), "/")
	cfg.StoreAPIKey = take("store_api_key")
	cfg.AdminAPIKey = take("admin_api_key")
	cfg.SigningSecret = take("notification_signing_secret")
	for key := range values {
		problems = append(problems, "unknown key "+key)
	}
	if public != "" {
		u, err := url.Parse(public)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			problems = append(problems, "public_url must be an origin such as http://127.0.0.1:3000")
		} else {
			u.Path = ""
			cfg.PublicURL = u
		}
	}
	if len(cfg.Staff) == 0 {
		problems = append(problems, "no staff account: add a staff line")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	if cfg.StoreAPIKey == cfg.AdminAPIKey {
		return nil, errors.New(path + ": store_api_key and admin_api_key must differ")
	}
	return cfg, nil
}

// secureCookies says whether cookies are sent over https only.
func (c *Config) secureCookies() bool { return c.PublicURL.Scheme == "https" }
