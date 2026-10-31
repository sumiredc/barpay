package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
	"github.com/sumiredc/barpay/internal/spectest"
)

// payer is the PayPal account of member m in the fake PayPal.
func payer(m string) string { return "payer-" + m }

// linkedIn checks member m in and links the session to a saved PayPal that PayPal holds.
func (f *fixture) linkedIn(t *testing.T, m string) View {
	t.Helper()
	v := f.checkedIn(t, m)
	f.saved.mu.Lock()
	f.saved.vaults[m] = f.pp.Vault(payer(m))
	f.saved.mu.Unlock()
	if !f.link(t, v.SessionID) {
		t.Fatal("not linked")
	}
	return f.looked(t, v.SessionID)
}

// ordered is linkedIn at t0 with an order of $13.00 at t0+5m, the clock then at t0+25m: two
// blocks of $2.00 have ended, so the total is $17.00, and the next block ends at t0+30m.
func (f *fixture) ordered(t *testing.T, m string) View {
	t.Helper()
	v := f.linkedIn(t, m)
	f.clk.Set(t0.Add(5 * time.Minute))
	decodeOrder201(t, f.order(v.SessionID, orderBody("o-1", line("Beer", 2, 650))), http.StatusCreated)
	f.clk.Set(t0.Add(25 * time.Minute))
	return v
}

func (f *fixture) exitToken(id string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodGet, path: "/v1/sessions/" + id + "/exit-token"})
}

// token gets the session's exit token, failing the test unless it is issued.
func (f *fixture) token(t *testing.T, id string) exitTokenJSON {
	t.Helper()
	w := f.exitToken(id)
	if w.Code != http.StatusOK {
		t.Fatalf("getExitToken: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "ExitToken", w.Body.Bytes())
	var out exitTokenJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) scanRaw(token string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"token": token})
	return f.do(call{method: http.MethodPost, path: "/v1/edge/exit-scans", auth: edgeAuth, body: string(body)})
}

// scan reads token at the gate, failing the test unless BarPay answered.
func (f *fixture) scan(t *testing.T, token string) scanResultJSON {
	t.Helper()
	w := f.scanRaw(token)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: %d %s\nlog: %s", w.Code, w.Body, f.log)
	}
	spectest.ValidEdge(t, "ScanResult", w.Body.Bytes())
	var out scanResultJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) closeSession(id, key string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + id + "/close", auth: adminAuth, key: key})
}

func (f *fixture) retry(id, key string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + id + "/retry", auth: adminAuth, key: key})
}

func (f *fixture) settleOutside(id, body string) *httptest.ResponseRecorder {
	return f.do(call{method: http.MethodPost, path: "/v1/admin/sessions/" + id + "/settle-outside", auth: adminAuth, body: body})
}

// closed closes the session by staff, failing the test unless it answers 200; it returns the
// session as answered.
func (f *fixture) closed(t *testing.T, id string) View {
	t.Helper()
	w := f.closeSession(id, "close-"+newID())
	if w.Code != http.StatusOK {
		t.Fatalf("closeSession: %d %s", w.Code, w.Body)
	}
	spectest.Valid(t, "Session", w.Body.Bytes())
	return decodeView(t, w.Body.Bytes())
}

// failed is ordered with the member's PayPal refusing the charge at the gate: the session is
// PaymentFailed, checkout having begun at t0+25m.
func (f *fixture) failed(t *testing.T, m string) View {
	t.Helper()
	v := f.ordered(t, m)
	f.pp.Broke(payer(m), true)
	if r := f.scan(t, f.token(t, v.SessionID).Token); r.Unlock {
		t.Fatal("unlocked on a refused charge")
	}
	if s := f.state(t, v.SessionID); s != PaymentFailed {
		t.Fatalf("state %s, want payment_failed", s)
	}
	return f.looked(t, v.SessionID)
}

func (f *fixture) charges() []paypaltest.Call { return f.pp.Calls(paypaltest.Charge) }

func (f *fixture) chargeDue(t *testing.T) int {
	t.Helper()
	n, err := f.api.ChargeDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) maxStayDue(t *testing.T) int {
	t.Helper()
	n, err := f.api.MaxStayDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// paidReceipt decodes the PaypalReceipt of a session's JSON.
type paidReceipt struct {
	Receipt *struct {
		Settlement      string                `json:"settlement"`
		Total           struct{ Value int64 } `json:"total"`
		PaypalCaptureID *string               `json:"paypal_capture_id"`
	} `json:"receipt"`
	UnpaidAmount *struct{ Value int64 } `json:"unpaid_amount"`
}

func decodePaid(t *testing.T, b []byte) paidReceipt {
	t.Helper()
	var r paidReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// eventData decodes an event's data.session state and data.reason.
func eventData(t *testing.T, b []byte) (state State, reason string) {
	t.Helper()
	var e struct {
		Data struct {
			Session struct {
				State State `json:"state"`
			} `json:"session"`
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e.Data.Session.State, e.Data.Reason
}
