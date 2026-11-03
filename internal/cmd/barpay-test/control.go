package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/barpaytest"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
)

// control serves the operations for the tests. They need no key: only this machine reaches them.
type control struct{ b *barpaytest.BarPay }

func (c control) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /test/clock/advance", c.advance)
	mux.HandleFunc("POST /test/paypal/approve", c.approve)
	mux.HandleFunc("GET /test/paypal/charges", c.charges)
}

const maxAdvance = 365 * 24 * time.Hour

// advance moves the fake clock forward by {"seconds": n} and answers 204.
func (c control) advance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Seconds float64 `json:"seconds"`
	}
	if !decode(w, r, &in) {
		return
	}
	// Bounded so the conversion to a time.Duration cannot overflow.
	if !(in.Seconds > 0 && in.Seconds <= maxAdvance.Seconds()) {
		http.Error(w, "seconds must be a positive number of at most a year's", http.StatusBadRequest)
		return
	}
	c.b.Clock.Advance(time.Duration(in.Seconds * float64(time.Second)))
	w.WriteHeader(http.StatusNoContent)
}

// approve is the buyer with PayPal account "payer" approving at "approve_url", where BarPay sent
// the member's browser. It answers {"return_url": ...}, where PayPal then sends the browser.
func (c control) approve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ApproveURL string `json:"approve_url"`
		Payer      string `json:"payer"`
	}
	if !decode(w, r, &in) {
		return
	}
	token := paypaltest.SetupTokenOf(in.ApproveURL)
	if in.Payer == "" || !c.b.PayPal.HasSetupToken(token) {
		http.Error(w, "approve_url must be one the fake PayPal made, and payer must be set", http.StatusBadRequest)
		return
	}
	c.b.PayPal.Approve(token, in.Payer)
	writeJSON(w, map[string]string{"return_url": c.b.PayPal.URLs(token).Return})
}

// charges answers {"count": n}, the charges the fake PayPal made.
func (c control) charges(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]int{"count": len(c.b.PayPal.Captures())})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "the body must be the operation's JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
