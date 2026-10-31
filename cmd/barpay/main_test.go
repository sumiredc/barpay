package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// With a complete config file BarPay serves, refuses a request without a key, and stops cleanly.
func TestStartsServesAndStops(t *testing.T) {
	dir := t.TempDir()
	sample, err := os.ReadFile("../../barpay.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(sample)
	for _, r := range [][2]string{
		{`listen_address = ":8080"`, `listen_address = "127.0.0.1:0"`},
		{`database_path = "barpay.db"`, `database_path = "` + filepath.Join(dir, "barpay.db") + `"`},
		{`store_app_origin = ""`, `store_app_origin = "https://store.example"`},
		{`store_api_key = ""`, `store_api_key = "k-store"`},
		{`admin_api_key = ""`, `admin_api_key = "k-admin"`},
		{`edge_shared_secret = ""`, `edge_shared_secret = "k-edge"`},
		{`client_id = ""`, `client_id = "id"`},
		{`client_secret = ""`, `client_secret = "secret"`},
		{`url = ""`, `url = "https://store.example/notify"`},
		{`signing_secret = ""`, `signing_secret = "sig"`},
		{`address = ""`, `address = "http://llm.example"`},
		{`model = ""`, `model = "m"`},
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
