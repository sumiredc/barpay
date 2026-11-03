package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const edgeSecret = "test-edge-secret-0123456789"

// syncBuffer is an output safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

// server is one run of the test BarPay.
type server struct {
	ready          ready
	stdin          *io.PipeWriter
	stdout, stderr *syncBuffer
	code           chan int
}

// startServer runs the test BarPay with getenv and waits for its ready line. It is stopped when
// the test ends if the test did not stop it.
func startServer(t *testing.T, getenv func(string) string) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &server{stdin: inW, stdout: &syncBuffer{}, stderr: &syncBuffer{}, code: make(chan int, 1)}
	go func() {
		s.code <- run(ctx, nil, getenv, inR, io.MultiWriter(s.stdout, outW), s.stderr)
		outW.Close()
	}()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		<-s.code
	})
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil {
		t.Fatalf("no ready line: %v; stderr: %s", err, s.stderr)
	}
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	if err := json.Unmarshal([]byte(line), &s.ready); err != nil {
		t.Fatalf("the ready line is not JSON: %v", err)
	}
	return s
}

func onlyEdgeSecret(k string) string {
	if k == edgeSecretEnv {
		return edgeSecret
	}
	return ""
}

// stop closes stdin and returns the exit code.
func (s *server) stop(t *testing.T) int {
	t.Helper()
	s.stdin.Close()
	select {
	case c := <-s.code:
		s.code <- c
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("the test BarPay did not stop after stdin closed")
		return 0
	}
}

// call sends a request with key ("" for none) and decodes a JSON answer into out if not nil.
func (s *server) call(t *testing.T, method, path, key, body string, want int, out any) {
	t.Helper()
	req, err := http.NewRequest(method, s.ready.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Idempotency-Key", fmt.Sprintf("key-%d", time.Now().UnixNano()))
	req.Header.Set("Content-Type", "application/json")
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		// Only an error body is shown: another answer could hold an exit token.
		shown := ""
		if resp.StatusCode >= 400 {
			shown = string(data)
		}
		t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, shown, want)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// browse is the member's browser opening u, with no key; it returns where it was sent.
func (s *server) browse(t *testing.T, u string) string {
	t.Helper()
	resp, err := noRedirect.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// linkedSession checks memberID in, links PayPal as the store app and the member's browser do
// and orders $13.00, and returns the session's exit token.
func (s *server) linkedSession(t *testing.T, memberID string) string {
	t.Helper()
	var sess struct {
		SessionID string `json:"session_id"`
	}
	s.call(t, http.MethodPost, "/v1/checkins", storeKey, `{"member_id":"`+memberID+`"}`, http.StatusCreated, &sess)
	var link struct {
		LinkURL string `json:"link_url"`
	}
	s.call(t, http.MethodPost, "/v1/members/"+memberID+"/paypal-link", storeKey,
		`{"state":"state-0123456789abcdef","consent_version":"v1","return_url":"`+s.ready.StoreAppOrigin+`/settings"}`,
		http.StatusCreated, &link)
	var approved struct {
		ReturnURL string `json:"return_url"`
	}
	s.call(t, http.MethodPost, "/test/paypal/approve", "",
		`{"approve_url":"`+s.browse(t, link.LinkURL)+`","payer":"payer-`+memberID+`"}`, http.StatusOK, &approved)
	back, err := url.Parse(s.browse(t, approved.ReturnURL))
	if err != nil {
		t.Fatal(err)
	}
	s.call(t, http.MethodPost, "/v1/members/"+memberID+"/paypal-link/confirm", storeKey,
		`{"code":"`+back.Query().Get("code")+`"}`, http.StatusCreated, nil)
	s.call(t, http.MethodPost, "/v1/sessions/"+sess.SessionID+"/orders", storeKey,
		`{"order_id":"o-1","lines":[{"name":"Beer","quantity":2,"unit_price":{"value":650,"currency":"USD"}}]}`,
		http.StatusCreated, nil)
	var tok struct {
		Token string `json:"token"`
	}
	s.call(t, http.MethodGet, "/v1/sessions/"+sess.SessionID+"/exit-token", storeKey, "", http.StatusOK, &tok)
	return tok.Token
}

type scanResult struct {
	Unlock        bool   `json:"unlock"`
	ReaderMessage string `json:"reader_message"`
}

func (s *server) scan(t *testing.T, token string) scanResult {
	t.Helper()
	var r scanResult
	s.call(t, http.MethodPost, "/v1/edge/exit-scans", edgeSecret, `{"token":"`+token+`"}`, http.StatusOK, &r)
	return r
}

func (s *server) charges(t *testing.T) int {
	t.Helper()
	var c struct {
		Count int `json:"count"`
	}
	s.call(t, http.MethodGet, "/test/paypal/charges", "", "", http.StatusOK, &c)
	return c.Count
}

func TestListensOnTheLoopbackAddressOnly(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)

	u, err := url.Parse(s.ready.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		t.Fatalf("listens on %s, want a loopback address", u.Host)
	}
	// Where this machine has another address, the port is not reachable through it.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), u.Port()), time.Second)
		if err == nil {
			c.Close()
			t.Errorf("reachable through a non-loopback address of this machine")
		}
	}
}

func TestReadsOnlyTheEdgeSecretAndTakesNoArgument(t *testing.T) {
	var mu sync.Mutex
	var read []string
	s := startServer(t, func(k string) string {
		mu.Lock()
		defer mu.Unlock()
		read = append(read, k)
		return onlyEdgeSecret(k)
	})
	s.stop(t)
	mu.Lock()
	defer mu.Unlock()
	if len(read) != 1 || read[0] != edgeSecretEnv {
		t.Errorf("read the environment variables %q, want only %s", read, edgeSecretEnv)
	}

	for _, args := range [][]string{{"-config", "barpay.toml"}, {"-store-api-key", "k"}, {"-admin-api-key", "k"}, {"barpay.toml"}} {
		var stderr strings.Builder
		if c := run(context.Background(), args, onlyEdgeSecret, strings.NewReader(""), io.Discard, &stderr); c != 2 {
			t.Errorf("%q: exit code %d, want 2 (refused)", args, c)
		}
	}
}

func TestRefusesToStartWithoutAnEdgeSecretOfItsOwn(t *testing.T) {
	for i, v := range []string{"", storeKey, adminKey} {
		var out, stderr strings.Builder
		getenv := func(string) string { return v }
		if c := run(context.Background(), nil, getenv, strings.NewReader(""), &out, &stderr); c != 2 {
			t.Errorf("edge secret %d: exit code %d, want 2", i, c)
		}
		if out.Len() != 0 {
			t.Errorf("edge secret %d: wrote %d bytes to stdout, want nothing", i, out.Len())
		}
	}
}

// What it writes is the ready line only, however it is used: BarPay's logs are discarded, so no
// key or exit token is written anywhere.
func TestWritesNothingButTheReadyLine(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)
	token := s.linkedSession(t, "m_1")
	s.scan(t, token)
	s.call(t, http.MethodPost, "/v1/edge/exit-scans", "wrong-secret", `{"token":"`+token+`"}`, http.StatusUnauthorized, nil)
	if c := s.stop(t); c != 0 {
		t.Errorf("exit code %d, want 0", c)
	}

	if e := s.stderr.String(); e != "" {
		t.Errorf("wrote %d bytes to stderr, want nothing", len(e))
	}
	out := s.stdout.String()
	if strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "{") {
		t.Errorf("wrote %d lines to stdout, want the ready line only", strings.Count(out, "\n"))
	}
	for _, v := range []string{edgeSecret, storeKey, adminKey, token} {
		if strings.Contains(out, v) {
			t.Errorf("stdout holds a key or the exit token")
		}
	}
}

func TestStopsWhenStdinIsClosed(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)
	// A request waiting for an unlock does not hold the stop.
	waiting := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, s.ready.URL+"/v1/edge/unlocks/next", nil)
		req.Header.Set("Authorization", "Bearer "+edgeSecret)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		waiting <- err
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if c := s.stop(t); c != 0 {
		t.Errorf("exit code %d, want 0", c)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v to stop", d)
	}
	if _, err := net.DialTimeout("tcp", strings.TrimPrefix(s.ready.URL, "http://"), time.Second); err == nil {
		t.Errorf("still accepts connections after stopping")
	}
	<-waiting
}

func TestClockAdvanceMovesTheExitTokenOn(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)
	old := s.linkedSession(t, "m_1")

	s.call(t, http.MethodPost, "/test/clock/advance", "", fmt.Sprintf(`{"seconds":%d}`, s.ready.ExitTokenRefreshSeconds+1), http.StatusNoContent, nil)

	if r := s.scan(t, old); r.Unlock || r.ReaderMessage != "Please show a new QR code" {
		t.Errorf("scanning the old token: %+v, want a new QR code asked for", r)
	}
	if n := s.charges(t); n != 0 {
		t.Errorf("%d charges, want 0", n)
	}
}

func TestChargesCountsTheChargesMade(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)
	if n := s.charges(t); n != 0 {
		t.Fatalf("%d charges before any scan, want 0", n)
	}
	token := s.linkedSession(t, "m_1")
	if r := s.scan(t, token); !r.Unlock {
		t.Fatalf("scan: %+v, want unlock", r)
	}
	if n := s.charges(t); n != 1 {
		t.Errorf("%d charges, want 1", n)
	}
}

func TestOperationsForTheTestsRefuseWhatTheyCannotDo(t *testing.T) {
	s := startServer(t, onlyEdgeSecret)
	for _, body := range []string{`{"seconds":0}`, `{"seconds":-1}`, `{"seconds":1e300}`, `{"second":1}`, `{}`, `x`} {
		s.call(t, http.MethodPost, "/test/clock/advance", "", body, http.StatusBadRequest, nil)
	}
	for _, body := range []string{`{"approve_url":"https://paypal.test/approve?token=ST-9999","payer":"p"}`, `{"approve_url":"x","payer":"p"}`, `{"approve_url":"","payer":""}`} {
		s.call(t, http.MethodPost, "/test/paypal/approve", "", body, http.StatusBadRequest, nil)
	}
}
