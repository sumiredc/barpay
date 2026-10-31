// Package edge calls BarPay's edge device API (docs/api/edge.yaml) as the edge device does, with
// its shared secret and nothing else. The development tools use it, so a call that would not work
// from the real edge device does not work from them either.
package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UnlockWait is how long waitForUnlock waits at most before it answers (edge.yaml).
const UnlockWait = 25 * time.Second

// maxResponseSize bounds what is read from BarPay; every answer of edge.yaml is far smaller.
const maxResponseSize = 1 << 20

// Client is safe for concurrent use.
type Client struct {
	base   string
	secret string
	http   *http.Client
}

// New returns a Client for the BarPay deployment at baseURL, such as "http://127.0.0.1:8080",
// sending secret as the edge device's shared secret. It follows no redirect, so the secret goes to
// baseURL only.
func New(baseURL, secret string) *Client {
	h := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{base: strings.TrimSuffix(baseURL, "/") + "/v1/edge", secret: secret, http: h}
}

// Error is an answer that is not the operation's success: an error code from edge.yaml, or a
// status the spec does not name.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("BarPay answered %d", e.Status)
	}
	return fmt.Sprintf("BarPay answered %d %s: %s", e.Status, e.Code, e.Message)
}

// ScanResult is edge.yaml's ScanResult.
type ScanResult struct {
	Unlock        bool   `json:"unlock"`
	UnlockID      string `json:"unlock_id"`
	ReaderMessage string `json:"reader_message"`
}

// Scan reports token, as read from the exit QR (scanExitToken).
func (c *Client) Scan(ctx context.Context, token string) (ScanResult, error) {
	var out ScanResult
	status, err := c.do(ctx, http.MethodPost, "/exit-scans", map[string]string{"token": token}, &out)
	if err == nil && status != http.StatusOK {
		err = &Error{Status: status}
	}
	return out, err
}

// WaitForUnlock waits for an unlock staff make by hand (waitForUnlock), and returns its ID, or ""
// when none came while BarPay waited.
func (c *Client) WaitForUnlock(ctx context.Context) (string, error) {
	var out struct {
		UnlockID string `json:"unlock_id"`
	}
	status, err := c.do(ctx, http.MethodGet, "/unlocks/next", nil, &out)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNoContent:
		return "", nil
	case status != http.StatusOK:
		return "", &Error{Status: status}
	}
	return out.UnlockID, nil
}

// ReportUnlock says whether the unlock with id worked (reportUnlockResult). A repeat is answered
// as the first was, so it is safe to send again when no answer came.
func (c *Client) ReportUnlock(ctx context.Context, id string, unlocked bool) error {
	status, err := c.do(ctx, http.MethodPost, "/unlocks/"+url.PathEscape(id)+"/result", map[string]bool{"unlocked": unlocked}, nil)
	if err == nil && status != http.StatusNoContent {
		err = &Error{Status: status}
	}
	return err
}

// SendLiveliness sends the room's liveliness, from 0 to 1 (sendSignals).
func (c *Client) SendLiveliness(ctx context.Context, liveliness float64) error {
	status, err := c.do(ctx, http.MethodPost, "/signals", map[string]float64{"liveliness": liveliness}, nil)
	if err == nil && status != http.StatusNoContent {
		err = &Error{Status: status}
	}
	return err
}

// do sends the request and decodes a 200 into out. An error status with edge.yaml's Error body
// is returned as *Error; other statuses are returned for the caller to judge.
func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL in the error is BarPay's; the secret is in a header, which the error leaves out.
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, &Error{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, errors.New("BarPay's answer is not the spec's")
		}
	}
	return resp.StatusCode, nil
}
