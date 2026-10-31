package paypal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/secret"
)

// SandboxURL is the base URL of PayPal's sandbox REST API.
const SandboxURL = "https://api-m.sandbox.paypal.com"

// CallTimeout is how long one call waits for PayPal's answer; no answer within it is Unknown.
const CallTimeout = 10 * time.Second

// maxResponseBytes bounds what is read from PayPal; its answers here are far smaller.
const maxResponseBytes = 1 << 20

// tokenMargin is how long before PayPal says an access token expires a new one is asked for.
const tokenMargin = time.Minute

// Options configure HTTP.
type Options struct {
	BaseURL      string // such as SandboxURL
	ClientID     secret.Value
	ClientSecret secret.Value
	Clock        clock.Clock
	Log          *slog.Logger
	// HTTP sends the requests; nil uses one that follows no redirect.
	HTTP *http.Client
}

// HTTP is the Client that calls PayPal. It is safe for concurrent use.
type HTTP struct {
	opts Options

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New returns the Client that calls PayPal at opts.BaseURL.
func New(opts Options) *HTTP {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	opts.BaseURL = strings.TrimSuffix(opts.BaseURL, "/")
	return &HTTP{opts: opts}
}

// answer is PayPal's reply to one call, or no reply (err).
type answer struct {
	status int
	body   []byte
	err    error
}

// noAnswer is an answer that did not come.
func (a answer) noAnswer() bool { return a.err != nil }

// call sends one request and reads the reply. requestID, if not empty, is the PayPal-Request-Id.
func (c *HTTP) call(ctx context.Context, method, path, requestID string, body any) answer {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	token, err := c.accessToken(ctx)
	if err != nil {
		return answer{err: err}
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return answer{err: err}
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opts.BaseURL+path, r)
	if err != nil {
		return answer{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		req.Header.Set("PayPal-Request-Id", requestID)
	}
	resp, err := c.opts.HTTP.Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return answer{err: err}
	}
	if len(b) > maxResponseBytes {
		return answer{err: errors.New("the answer is too large")}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.forgetToken(token)
	}
	return answer{status: resp.StatusCode, body: b}
}

// accessToken returns an OAuth access token for the store's credentials, asking PayPal for a new
// one when there is none or it is about to expire.
func (c *HTTP) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.opts.Clock.Now()
	if c.token != "" && now.Before(c.expires) {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/v1/oauth2/token",
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.opts.ClientID.Reveal(), c.opts.ClientSecret.Reveal())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.opts.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("paypal: access token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"` // seconds
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paypal: access token: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", errors.New("paypal: access token: the answer has no token")
	}
	c.token = out.AccessToken
	c.expires = now.Add(time.Duration(out.ExpiresIn)*time.Second - tokenMargin)
	return c.token, nil
}

// forgetToken drops token after PayPal refused it, so the next call asks for a new one.
func (c *HTTP) forgetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// outcome reads a reply to a call that changes something: a 2xx whose body read gives ok is
// Succeeded, a 4xx other than 408 and 429 is Failed, and everything else, including a 5xx, which
// does not say whether PayPal did it, is Unknown.
func (c *HTTP) outcome(op string, a answer, ok bool) Outcome {
	switch {
	case a.noAnswer():
		c.opts.Log.Warn("PayPal gave no answer", "call", op, "error", cause(a.err))
		return Unknown
	case a.status >= 200 && a.status < 300 && ok:
		return Succeeded
	case a.status >= 400 && a.status < 500 && a.status != http.StatusRequestTimeout && a.status != http.StatusTooManyRequests:
		c.opts.Log.Warn("PayPal refused", "call", op, "status", a.status, "issue", issue(a.body))
		return Failed
	}
	c.opts.Log.Warn("PayPal's answer does not say", "call", op, "status", a.status, "issue", issue(a.body))
	return Unknown
}

// cause is err without the request URL a *url.Error quotes, which holds a vault or setup token
// ID, for the log.
func cause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// issue is the name PayPal gives an error, such as TOKEN_NOT_FOUND, for the log. It is never a
// token or an amount.
func issue(body []byte) string {
	var e struct {
		Name    string `json:"name"`
		Details []struct {
			Issue string `json:"issue"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if len(e.Details) > 0 && e.Details[0].Issue != "" {
		return e.Details[0].Issue
	}
	return e.Name
}

type setupTokenRequest struct {
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

type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

// CreateSetupToken is POST /v3/vault/setup-tokens with usage_type MERCHANT (the PRD's System
// overview).
func (c *HTTP) CreateSetupToken(ctx context.Context, requestID string, urls URLs) (SetupToken, Outcome) {
	var in setupTokenRequest
	in.PaymentSource.PayPal.UsageType = "MERCHANT"
	in.PaymentSource.PayPal.ExperienceContext.ReturnURL = urls.Return
	in.PaymentSource.PayPal.ExperienceContext.CancelURL = urls.Cancel
	a := c.call(ctx, http.MethodPost, "/v3/vault/setup-tokens", requestID, in)
	var out struct {
		ID    string `json:"id"`
		Links []link `json:"links"`
	}
	var st SetupToken
	if json.Unmarshal(a.body, &out) == nil {
		st.ID = out.ID
		for _, l := range out.Links {
			if l.Rel == "approve" {
				st.ApproveURL = l.Href
			}
		}
	}
	o := c.outcome("create setup token", a, st.ID != "" && strings.HasPrefix(st.ApproveURL, "https://"))
	if o != Succeeded {
		return SetupToken{}, o
	}
	return st, o
}

// SetupTokenApproved is GET /v3/vault/setup-tokens/{id}: approved is status APPROVED. A 404 is a
// readable answer that it is not approved.
func (c *HTTP) SetupTokenApproved(ctx context.Context, id string) (approved, ok bool) {
	a := c.call(ctx, http.MethodGet, "/v3/vault/setup-tokens/"+url.PathEscape(id), "", nil)
	var out struct {
		Status string `json:"status"`
	}
	switch {
	case a.noAnswer():
		c.opts.Log.Warn("PayPal gave no answer", "call", "read setup token", "error", cause(a.err))
		return false, false
	case a.status == http.StatusOK && json.Unmarshal(a.body, &out) == nil && out.Status != "":
		return out.Status == "APPROVED", true
	case a.status == http.StatusNotFound:
		return false, true
	}
	c.opts.Log.Warn("PayPal's answer does not say", "call", "read setup token", "status", a.status, "issue", issue(a.body))
	return false, false
}

// CreatePaymentToken is POST /v3/vault/payment-tokens from the setup token.
func (c *HTTP) CreatePaymentToken(ctx context.Context, requestID, setupTokenID string) (string, Outcome) {
	type token struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	in := map[string]map[string]token{"payment_source": {"token": {ID: setupTokenID, Type: "SETUP_TOKEN"}}}
	a := c.call(ctx, http.MethodPost, "/v3/vault/payment-tokens", requestID, in)
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(a.body, &out)
	o := c.outcome("create payment token", a, out.ID != "")
	if o != Succeeded {
		return "", o
	}
	return out.ID, o
}

// CheckPaymentToken is GET /v3/vault/payment-tokens/{id}: 200 is Present and 404 is Gone (the
// PRD's Fixed constraints, verified in the sandbox); anything else says nothing.
func (c *HTTP) CheckPaymentToken(ctx context.Context, vaultID string) Presence {
	a := c.call(ctx, http.MethodGet, "/v3/vault/payment-tokens/"+url.PathEscape(vaultID), "", nil)
	var out struct {
		ID string `json:"id"`
	}
	switch {
	case a.noAnswer():
		c.opts.Log.Warn("PayPal gave no answer", "call", "check payment token", "error", cause(a.err))
		return PresenceUnknown
	case a.status == http.StatusOK && json.Unmarshal(a.body, &out) == nil && out.ID == vaultID:
		return Present
	case a.status == http.StatusNotFound:
		return Gone
	}
	c.opts.Log.Warn("PayPal's answer does not say", "call", "check payment token", "status", a.status, "issue", issue(a.body))
	return PresenceUnknown
}

// DeletePaymentToken is DELETE /v3/vault/payment-tokens/{id}. PayPal answers 204 also for one it
// no longer has (the PRD's Fixed constraints); a 404 says the same.
func (c *HTTP) DeletePaymentToken(ctx context.Context, requestID, vaultID string) Outcome {
	a := c.call(ctx, http.MethodDelete, "/v3/vault/payment-tokens/"+url.PathEscape(vaultID), requestID, nil)
	if !a.noAnswer() && a.status == http.StatusNotFound {
		return Succeeded
	}
	return c.outcome("delete payment token", a, true)
}
