package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/store"
)

func TestSampleConfigDoesNotStart(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", "../../barpay.example.toml"}, &stderr, nil); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "keys.store_api_key: credential is missing") {
		t.Errorf("stderr does not say why:\n%s", stderr.String())
	}
}

func TestMissingConfigFileDoesNotStart(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"-config", filepath.Join(t.TempDir(), "none.toml")}, &stderr, nil); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// writeConfig writes a complete config file into dir, from the sample, with notificationURL.
func writeConfig(t *testing.T, dir, notificationURL string) string {
	t.Helper()
	sample, err := os.ReadFile("../../barpay.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(sample)
	for _, r := range [][2]string{
		{`listen_address = ":8080"`, `listen_address = "127.0.0.1:0"`},
		{`database_path = "barpay.db"`, `database_path = "` + filepath.Join(dir, "barpay.db") + `"`},
		{`store_app_origin = ""`, `store_app_origin = "https://store.example"`},
		{`public_url = ""`, `public_url = "http://127.0.0.1:8080"`},
		{`store_api_key = ""`, `store_api_key = "k-store"`},
		{`admin_api_key = ""`, `admin_api_key = "k-admin"`},
		{`edge_shared_secret = ""`, `edge_shared_secret = "k-edge"`},
		{`client_id = ""`, `client_id = "id"`},
		{`client_secret = ""`, `client_secret = "secret"`},
		{`url = ""`, `url = "` + notificationURL + `"`},
		{`signing_secret = ""`, `signing_secret = "sig"`},
		{`address = ""`, `address = "http://llm.example"`},
	} {
		if !strings.Contains(cfg, r[0]) {
			t.Fatalf("the sample has no line %s", r[0])
		}
		cfg = strings.Replace(cfg, r[0], r[1], 1)
	}
	path := filepath.Join(dir, "barpay.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlainHTTPNotificationURLDoesNotStart(t *testing.T) {
	var stderr bytes.Buffer
	path := writeConfig(t, t.TempDir(), "http://localhost:3000/notify")
	if code := run(context.Background(), []string{"-config", path}, &stderr, nil); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "notifications.url: must use https") {
		t.Errorf("stderr does not say why:\n%s", stderr.String())
	}
}

// With a complete config file BarPay serves, refuses a request without a key, and stops cleanly.
func TestStartsServesAndStops(t *testing.T) {
	path := writeConfig(t, t.TempDir(), "https://store.example/notify")
	// A local LLM that proposes $13.00 per hour, at the config file's address.
	var asked struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"rate_per_hour\":1300,\"reason\":\"Busy\",\"reasoning\":\"r\"}"}}]}`)
	}))
	defer model.Close()
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `address = "http://llm.example"`, `address = "`+model.URL+`/v1"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan net.Addr, 1)
	var stderr syncBuffer
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"-config", path}, &stderr, func(a net.Addr) { addr <- a }) }()

	var a net.Addr
	select {
	case a = <-addr:
	case c := <-code:
		t.Fatalf("exited with %d before serving:\n%s", c, stderr.String())
	}
	resp, err := http.Get("http://" + a.String() + "/v1/admin/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("request without a key: status = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+a.String()+"/v1/admin/notifications", nil)
	req.Header.Set("Authorization", "Bearer k-admin")
	req.Header.Set("BarPay-Staff-Id", "staff_1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("listNotifications: status = %d, want 200", resp.StatusCode)
	}

	// The starting rate is published at start and read with the store API key.
	req, _ = http.NewRequest(http.MethodGet, "http://"+a.String()+"/v1/rate", nil)
	req.Header.Set("Authorization", "Bearer k-store")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rate struct {
		RatePerHour struct{ Value int64 } `json:"rate_per_hour"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rate)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || rate.RatePerHour.Value != 1200 {
		t.Errorf("GET /v1/rate: status %d, rate %+v, %v; want 200 with the sample's starting rate 1200", resp.StatusCode, rate, err)
	}

	// An evaluation asks the model at the config file's address and publishes its proposal.
	req, _ = http.NewRequest(http.MethodPost, "http://"+a.String()+"/v1/rate-evaluations", nil)
	req.Header.Set("Authorization", "Bearer k-store")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Rate struct {
			RatePerHour struct{ Value int64 } `json:"rate_per_hour"`
		} `json:"rate"`
		Evaluated bool    `json:"evaluated"`
		Fallback  *string `json:"fallback"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ev)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || !ev.Evaluated || ev.Fallback != nil || ev.Rate.RatePerHour.Value != 1300 {
		t.Errorf("requestRateEvaluation: status %d, %+v, %v; want 1300 evaluated", resp.StatusCode, ev, err)
	}
	if asked.Model != "qwen3:8b" || asked.ReasoningEffort != "none" {
		t.Errorf("the model was asked with %+v, want the sample's model qwen3:8b and reasoning effort none", asked)
	}

	// The member operations are served, and a link URL is on the config file's public URL. Asking
	// for one calls nothing in PayPal.
	req, _ = http.NewRequest(http.MethodPost, "http://"+a.String()+"/v1/members/m_1/paypal-link", strings.NewReader(
		`{"state":"state-0123456789abcdef","consent_version":"v1","return_url":"https://store.example/settings"}`))
	req.Header.Set("Authorization", "Bearer k-store")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var link struct {
		LinkURL string `json:"link_url"`
	}
	err = json.NewDecoder(resp.Body).Decode(&link)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || err != nil || !strings.HasPrefix(link.LinkURL, "http://127.0.0.1:8080/v1/paypal-links/pll_") {
		t.Errorf("createPaypalLink: status %d, %+v, %v; want a link URL on the public URL", resp.StatusCode, link, err)
	}

	cancel()
	if c := <-code; c != 0 {
		t.Errorf("exit code after stop = %d, want 0:\n%s", c, stderr.String())
	}
	for _, s := range []string{"k-store", "k-admin", "k-edge", "secret", "sig"} {
		if strings.Contains(stderr.String(), `"`+s+`"`) || strings.Contains(stderr.String(), "="+s) {
			t.Errorf("the log contains the secret %q:\n%s", s, stderr.String())
		}
	}
}

// Stopping BarPay while the AI is answering waits for the evaluation, which stores its decision
// before the store closes.
func TestStopWaitsForAnEvaluation(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "https://store.example/notify")
	entered := make(chan struct{})
	var once sync.Once
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"rate_per_hour\":1300,\"reason\":\"Busy\",\"reasoning\":\"r\"}"}}]}`)
	}))
	defer model.Close()
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `address = "http://llm.example"`, `address = "`+model.URL+`/v1"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan net.Addr, 1)
	var stderr syncBuffer
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"-config", path}, &stderr, func(a net.Addr) { addr <- a }) }()
	var a net.Addr
	select {
	case a = <-addr:
	case c := <-code:
		t.Fatalf("exited with %d before serving:\n%s", c, stderr.String())
	}
	// The store app gives up on its call, so the server has no request left to wait for.
	reqCtx, giveUp := context.WithCancel(context.Background())
	go func() {
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, "http://"+a.String()+"/v1/rate-evaluations", nil)
		req.Header.Set("Authorization", "Bearer k-store")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	giveUp()
	cancel()
	if c := <-code; c != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", c, stderr.String())
	}

	st, err := store.Open(context.Background(), filepath.Join(dir, "barpay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.Read().QueryRowContext(context.Background(), `SELECT count(*) FROM decisions WHERE proposal = 1300`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d decisions with the AI's proposal, %v; want 1", n, err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
