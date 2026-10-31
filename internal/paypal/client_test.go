package paypal

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/secret"
)

// server stands in for PayPal: it hands out an access token and answers every other request with
// handle, recording what it received.
type server struct {
	t       *testing.T
	tokens  atomic.Int64
	handle  func(w http.ResponseWriter, req *http.Request, body []byte)
	clk     *clock.Fake
	client  *HTTP
	srv     *httptest.Server
	lastReq *http.Request
	last    []byte
}

func newServer(t *testing.T, handle func(w http.ResponseWriter, req *http.Request, body []byte)) *server {
	s := &server{t: t, handle: handle, clk: clock.NewFake(time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC))}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path == "/v1/oauth2/token" {
			id, pw, ok := req.BasicAuth()
			if !ok || id != "client-id" || pw != "client-secret" || string(body) != "grant_type=client_credentials" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			s.tokens.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":32400}`)
			return
		}
		if req.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
		}
		s.lastReq, s.last = req, body
		s.handle(w, req, body)
	}))
	t.Cleanup(s.srv.Close)
	s.client = New(Options{
		BaseURL: s.srv.URL + "/", ClientID: secret.New("client-id"), ClientSecret: secret.New("client-secret"),
		Clock: s.clk, Log: slog.New(slog.DiscardHandler),
	})
	return s
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// Setup tokens are for merchant-initiated charges, return to BarPay, and carry the stored
// PayPal-Request-Id; the approve link is what the member is sent to.
func TestCreateSetupToken(t *testing.T) {
	s := newServer(t, reply(http.StatusCreated, `{"id":"ST-1","status":"PAYER_ACTION_REQUIRED",
		"links":[{"href":"https://api.sandbox.paypal.com/x","rel":"self"},{"href":"https://www.sandbox.paypal.com/agreements/approve?approval_session_id=1","rel":"approve"}]}`))
	st, o := s.client.CreateSetupToken(context.Background(), "req-1", URLs{Return: "https://b.example/r", Cancel: "https://b.example/c"})
	if o != Succeeded || st.ID != "ST-1" || st.ApproveURL != "https://www.sandbox.paypal.com/agreements/approve?approval_session_id=1" {
		t.Fatalf("got %+v, %v", st, o)
	}
	if s.lastReq.Method != http.MethodPost || s.lastReq.URL.Path != "/v3/vault/setup-tokens" || s.lastReq.Header.Get("PayPal-Request-Id") != "req-1" {
		t.Errorf("request %s %s, PayPal-Request-Id %q", s.lastReq.Method, s.lastReq.URL.Path, s.lastReq.Header.Get("PayPal-Request-Id"))
	}
	var in struct {
		PaymentSource struct {
			PayPal struct {
				UsageType         string `json:"usage_type"`
				ExperienceContext struct {
					ReturnURL string `json:"return_url"`
					CancelURL string `json:"cancel_url"`
				} `json:"experience_context"`
			} `json:"paypal"`
		} `json:"payment_source"`
	}
	if err := json.Unmarshal(s.last, &in); err != nil {
		t.Fatal(err)
	}
	p := in.PaymentSource.PayPal
	if p.UsageType != "MERCHANT" || p.ExperienceContext.ReturnURL != "https://b.example/r" || p.ExperienceContext.CancelURL != "https://b.example/c" {
		t.Errorf("body %s", s.last)
	}
}

// What each answer to a call that changes something comes to: only a readable success is
// Succeeded, a refusal is Failed, and anything that does not say is Unknown.
func TestOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   Outcome
	}{
		{"saved", http.StatusCreated, `{"id":"VT-1"}`, Succeeded},
		{"saved again", http.StatusOK, `{"id":"VT-1"}`, Succeeded},
		{"2xx without an ID", http.StatusOK, `{}`, Unknown},
		{"2xx unreadable", http.StatusCreated, `<html>`, Unknown},
		{"refused", http.StatusUnprocessableEntity, `{"name":"UNPROCESSABLE_ENTITY","details":[{"issue":"INTERNAL_ERROR"}]}`, Failed},
		{"server error", http.StatusInternalServerError, `{}`, Unknown},
		{"too many requests", http.StatusTooManyRequests, `{}`, Unknown},
		{"redirect", http.StatusFound, ``, Unknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, reply(c.status, c.body))
			vault, o := s.client.CreatePaymentToken(context.Background(), "req-2", "ST-1")
			if o != c.want || (o == Succeeded) != (vault == "VT-1") {
				t.Errorf("got %q, %v; want %v", vault, o, c.want)
			}
			if s.lastReq.URL.Path != "/v3/vault/payment-tokens" || s.lastReq.Header.Get("PayPal-Request-Id") != "req-2" ||
				!strings.Contains(string(s.last), `"token":{"id":"ST-1","type":"SETUP_TOKEN"}`) {
				t.Errorf("request %s %s", s.lastReq.URL.Path, s.last)
			}
		})
	}
}

// A call PayPal does not answer within CallTimeout is Unknown.
func TestNoAnswerIsUnknown(t *testing.T) {
	block := make(chan struct{})
	s := newServer(t, func(http.ResponseWriter, *http.Request, []byte) { <-block })
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if o := s.client.DeletePaymentToken(ctx, "req-3", "VT-1"); o != Unknown {
		t.Errorf("delete = %v, want unknown", o)
	}
}

// The check tells only whether PayPal still has it: 404 is gone, and nothing else is.
func TestCheckPaymentToken(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   Presence
	}{
		{http.StatusOK, `{"id":"VT-1"}`, Present},
		{http.StatusNotFound, `{"name":"RESOURCE_NOT_FOUND","details":[{"issue":"TOKEN_NOT_FOUND"}]}`, Gone},
		{http.StatusOK, `garbage`, PresenceUnknown},
		{http.StatusInternalServerError, `{}`, PresenceUnknown},
		{http.StatusForbidden, `{}`, PresenceUnknown},
	} {
		s := newServer(t, reply(c.status, c.body))
		if got := s.client.CheckPaymentToken(context.Background(), "VT-1"); got != c.want {
			t.Errorf("%d %s: %v, want %v", c.status, c.body, got, c.want)
		}
		if s.lastReq.Method != http.MethodGet || s.lastReq.URL.Path != "/v3/vault/payment-tokens/VT-1" {
			t.Errorf("request %s %s", s.lastReq.Method, s.lastReq.URL.Path)
		}
	}
}

// A deletion is confirmed by 204, and by 404, which also says PayPal does not have it.
func TestDeletePaymentToken(t *testing.T) {
	for status, want := range map[int]Outcome{
		http.StatusNoContent: Succeeded, http.StatusNotFound: Succeeded,
		http.StatusBadRequest: Failed, http.StatusServiceUnavailable: Unknown,
	} {
		s := newServer(t, reply(status, ``))
		if got := s.client.DeletePaymentToken(context.Background(), "req-4", "VT-1"); got != want {
			t.Errorf("%d: %v, want %v", status, got, want)
		}
		if s.lastReq.Method != http.MethodDelete || s.lastReq.Header.Get("PayPal-Request-Id") != "req-4" {
			t.Errorf("request %s, PayPal-Request-Id %q", s.lastReq.Method, s.lastReq.Header.Get("PayPal-Request-Id"))
		}
	}
}

func TestSetupTokenApproved(t *testing.T) {
	for _, c := range []struct {
		status       int
		body         string
		approved, ok bool
	}{
		{http.StatusOK, `{"id":"ST-1","status":"APPROVED"}`, true, true},
		{http.StatusOK, `{"id":"ST-1","status":"PAYER_ACTION_REQUIRED"}`, false, true},
		{http.StatusNotFound, `{}`, false, true},
		{http.StatusOK, `{}`, false, false},
		{http.StatusBadGateway, ``, false, false},
	} {
		s := newServer(t, reply(c.status, c.body))
		approved, ok := s.client.SetupTokenApproved(context.Background(), "ST-1")
		if approved != c.approved || ok != c.ok {
			t.Errorf("%d %s: approved %v ok %v", c.status, c.body, approved, ok)
		}
	}
}

// One access token serves calls until shortly before it expires.
func TestAccessTokenIsReused(t *testing.T) {
	s := newServer(t, reply(http.StatusNoContent, ``))
	for range 3 {
		s.client.DeletePaymentToken(context.Background(), "r", "VT-1")
	}
	if n := s.tokens.Load(); n != 1 {
		t.Errorf("%d access tokens asked for, want 1", n)
	}
	s.clk.Advance(9*time.Hour - time.Minute)
	s.client.DeletePaymentToken(context.Background(), "r", "VT-1")
	if n := s.tokens.Load(); n != 2 {
		t.Errorf("%d access tokens asked for after it expired, want 2", n)
	}
}

// The log of a call PayPal did not answer leaves out the URL, which holds the vault ID.
func TestCauseLeavesOutTheURL(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://api-m.sandbox.paypal.com/v3/vault/payment-tokens/VT-SECRET", Err: context.DeadlineExceeded}
	if got := cause(err); strings.Contains(got.Error(), "VT-SECRET") || got != context.DeadlineExceeded {
		t.Errorf("cause = %v", got)
	}
}
