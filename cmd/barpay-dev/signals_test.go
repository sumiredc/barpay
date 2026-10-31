package main

import (
	"net/http"
	"testing"
	"time"
)

// latestLiveliness is the liveliness in the inputs of the newest decision in the decision log.
func (b *barpay) latestLiveliness(t *testing.T) *float64 {
	t.Helper()
	var log struct {
		Decisions []struct {
			Inputs *struct {
				Liveliness *float64 `json:"liveliness"`
			} `json:"inputs"`
		} `json:"decisions"`
	}
	b.must(t, http.StatusOK, http.MethodGet, "/v1/admin/decisions", adminKey, "", &log)
	if len(log.Decisions) == 0 || log.Decisions[0].Inputs == nil {
		t.Fatalf("no decision with inputs: %+v", log.Decisions)
	}
	return log.Decisions[0].Inputs.Liveliness
}

func (b *barpay) evaluate(t *testing.T) {
	t.Helper()
	var ev struct {
		Evaluated bool `json:"evaluated"`
	}
	b.must(t, http.StatusOK, http.MethodPost, "/v1/rate-evaluations", storeKey, "", &ev)
	if !ev.Evaluated {
		t.Fatal("the rate was not evaluated")
	}
}

// Pricing model, Signals: the liveliness the simulator sends is in the inputs of the next
// evaluation, and a value typed in replaces it.
func TestSimulatedLivelinessIsInTheNextEvaluation(t *testing.T) {
	b := newBarPay(t)
	s := start(t, b, "signals", "-liveliness", "0.73")
	s.expect(t, "liveliness 0.73 sent")
	b.evaluate(t)
	if l := b.latestLiveliness(t); l == nil || *l != 0.73 {
		t.Errorf("liveliness in the decision log = %v, want 0.73", l)
	}

	b.clk.Advance(time.Minute) // the minimum evaluation interval
	s.typeLine(t, "0.2")
	s.expect(t, "liveliness 0.2 sent")
	b.evaluate(t)
	if l := b.latestLiveliness(t); l == nil || *l != 0.2 {
		t.Errorf("liveliness in the decision log = %v, want 0.2", l)
	}
}

// A value outside 0 to 1 is not sent; the simulator keeps sending the last one.
func TestSimulatorRefusesLivelinessOutsideZeroToOne(t *testing.T) {
	b := newBarPay(t)
	s := start(t, b, "signals", "-liveliness", "0.5")
	s.expect(t, "liveliness 0.5 sent")
	n := len(b.devRequests())
	s.typeLine(t, "1.5")
	s.expect(t, `"1.5" is not a number from 0 to 1; still sending 0.5`)
	if got := len(b.devRequests()); got != n {
		t.Errorf("%d requests sent for 1.5, want none", got-n)
	}
}
