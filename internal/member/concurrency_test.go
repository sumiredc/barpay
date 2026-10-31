package member

import (
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
)

// The same code confirmed twice at once saves once: one request gets 201, the other
// link_code_invalid, and PayPal saw one save.
func TestConcurrentConfirmsSaveOnce(t *testing.T) {
	for range 20 {
		f := newFixture(t)
		code := f.approvedCode(t, "m_1", "payer-1")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Go(func() { codes[i] = f.confirm("m_1", code).Code })
		}
		wg.Wait()
		slices.Sort(codes)
		if !slices.Equal(codes, []int{http.StatusCreated, http.StatusUnprocessableEntity}) {
			t.Fatalf("codes %v; want one 201 and one 422", codes)
		}
		if n := len(f.pp.Calls(paypaltest.CreatePaymentToken)); n != 1 {
			t.Fatalf("PayPal saw %d saves, want 1", n)
		}
	}
}

// Two deletions asked for at once delete once: PayPal saw one deletion and the store app is
// notified once.
func TestConcurrentDeletesDeleteOnce(t *testing.T) {
	for range 20 {
		f := newFixture(t)
		f.linked(t, "m_1", "payer-1")
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { f.deleteSaved("m_1") })
		}
		wg.Wait()
		if n := len(f.pp.Calls(paypaltest.DeletePaymentToken)); n != 1 {
			t.Fatalf("PayPal saw %d deletions, want 1", n)
		}
		if n := len(f.events(t, "m_1", "member.saved_paypal_deleted")); n != 1 {
			t.Fatalf("%d member.saved_paypal_deleted events, want 1", n)
		}
	}
}
