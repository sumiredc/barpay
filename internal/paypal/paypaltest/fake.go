// Package paypaltest is a fake PayPal for tests: it behaves as the PRD's Fixed constraints say the
// sandbox did, and can be told to give no answer, with or without having done the work.
package paypaltest

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/paypal"
)

// Op names a call.
type Op string

const (
	CreateSetupToken   Op = "create_setup_token"
	SetupTokenApproved Op = "setup_token_approved"
	CreatePaymentToken Op = "create_payment_token"
	CheckPaymentToken  Op = "check_payment_token"
	DeletePaymentToken Op = "delete_payment_token"
	Charge             Op = "charge"
)

// Fault is how the fake misbehaves on one call.
type Fault int

const (
	// NoFault answers as PayPal does.
	NoFault Fault = iota
	// NoAnswer does nothing and gives no answer, as a call that never reached PayPal.
	NoAnswer
	// LostAnswer does the work and gives no answer, as a timeout after PayPal did it.
	LostAnswer
	// Refuse does nothing and answers with a refusal; for a charge, one that does not use up the
	// invoice_id, as the sandbox's mock INSTRUMENT_DECLINED.
	Refuse
)

// ApproveBase is where the fake's setup tokens are approved; the token ID is its "token" query.
const ApproveBase = "https://paypal.test/approve"

// Call is one call the fake received.
type Call struct {
	Op        Op
	RequestID string // the PayPal-Request-Id; empty for reads
	ID        string // the setup token or vault ID the call was about
	// InvoiceID and Amount are a charge's.
	InvoiceID string
	Amount    money.Amount
}

// Capture is a charge PayPal made.
type Capture struct {
	ID        string
	InvoiceID string
	VaultID   string
	Amount    money.Amount
}

type setupToken struct {
	urls     paypal.URLs
	approved bool
	payer    string
	// existing is the vault ID the payer already had with the store when approving: saving then
	// returns it, and fails once it is gone (the PRD's Fixed constraints).
	existing string
	vault    string
}

// Fake is a PayPal. It is safe for concurrent use.
type Fake struct {
	// During, if set, is called with each call when it arrives, before the fake acts on it, so a
	// test can look at what BarPay stored before calling, or change something meanwhile.
	During func(Call)

	mu      sync.Mutex
	faults  map[Op][]Fault
	calls   []Call
	setups  map[string]*setupToken
	vaults  map[string]string // vault ID → payer
	results map[string]any    // PayPal-Request-Id → first successful result
	broke   map[string]bool   // payers with nothing to pay with
	capts   []Capture
	n       int
}

// New returns a Fake that holds nothing.
func New() *Fake {
	return &Fake{faults: map[Op][]Fault{}, setups: map[string]*setupToken{}, vaults: map[string]string{}, results: map[string]any{}, broke: map[string]bool{}}
}

var _ paypal.Client = (*Fake)(nil)

// Next makes the next calls of op misbehave as faults say, one fault per call, in order.
func (f *Fake) Next(op Op, faults ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[op] = append(f.faults[op], faults...)
}

// Clear drops the faults still waiting for op, so its next calls behave.
func (f *Fake) Clear(op Op) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.faults, op)
}

// Calls returns the calls received so far of op, or of every op when op is empty.
func (f *Fake) Calls(op Op) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Call
	for _, c := range f.calls {
		if op == "" || c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Approve is the buyer with PayPal account payer approving the setup token with id.
func (f *Fake) Approve(id, payer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.setups[id]
	if st == nil {
		panic("paypaltest: no setup token " + id)
	}
	st.approved, st.payer = true, payer
	for v, p := range f.vaults {
		if p == payer {
			st.existing = v
		}
	}
}

// HasSetupToken reports whether the fake made the setup token with id.
func (f *Fake) HasSetupToken(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setups[id] != nil
}

// URLs returns the return and cancel URLs the setup token with id was made with.
func (f *Fake) URLs(id string) paypal.URLs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setups[id].urls
}

// Vaults returns the saved PayPals PayPal holds, by vault ID, with their payer.
func (f *Fake) Vaults() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.vaults))
	for k, v := range f.vaults {
		out[k] = v
	}
	return out
}

// Vault is PayPal holding a new saved PayPal of the buyer with PayPal account payer, as linking
// leaves it, and returns its vault ID.
func (f *Fake) Vault(payer string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.newID("VT-")
	f.vaults[v] = payer
	return v
}

// Revoke is the buyer cancelling the saved PayPal with vaultID in their PayPal account.
func (f *Fake) Revoke(vaultID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vaults, vaultID)
}

// Broke makes the buyer with PayPal account payer have nothing to pay with (true) or have it again
// (false): while broke, a charge with their saved PayPal is refused (TRANSACTION_REFUSED), and the
// refusal is neither remembered under its PayPal-Request-Id nor uses up the invoice_id.
func (f *Fake) Broke(payer string, broke bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broke[payer] = broke
}

// Captures returns the charges PayPal made, in order.
func (f *Fake) Captures() []Capture {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Capture(nil), f.capts...)
}

// SetupTokenOf returns the setup token ID in an approve URL the fake made.
func SetupTokenOf(approveURL string) string {
	u, err := url.Parse(approveURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("token")
}

// begin records a call, calls During, and returns the call's fault with f.mu held; the caller
// unlocks it.
func (f *Fake) begin(c Call) Fault {
	if f.During != nil {
		f.During(c)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	q := f.faults[c.Op]
	if len(q) == 0 {
		return NoFault
	}
	f.faults[c.Op] = q[1:]
	return q[0]
}

func (f *Fake) newID(prefix string) string {
	f.n++
	return fmt.Sprintf("%s%04d", prefix, f.n)
}

func (f *Fake) CreateSetupToken(_ context.Context, requestID string, urls paypal.URLs) (paypal.SetupToken, paypal.Outcome) {
	fault := f.begin(Call{Op: CreateSetupToken, RequestID: requestID})
	defer f.mu.Unlock()
	if fault == NoAnswer {
		return paypal.SetupToken{}, paypal.Unknown
	}
	if fault == Refuse {
		return paypal.SetupToken{}, paypal.Failed
	}
	st, ok := f.results[requestID].(paypal.SetupToken)
	if !ok {
		st = paypal.SetupToken{ID: f.newID("ST-")}
		st.ApproveURL = ApproveBase + "?token=" + st.ID
		f.setups[st.ID] = &setupToken{urls: urls}
		f.results[requestID] = st
	}
	if fault == LostAnswer {
		return paypal.SetupToken{}, paypal.Unknown
	}
	return st, paypal.Succeeded
}

func (f *Fake) SetupTokenApproved(_ context.Context, id string) (approved, ok bool) {
	fault := f.begin(Call{Op: SetupTokenApproved, ID: id})
	defer f.mu.Unlock()
	if fault == NoAnswer || fault == LostAnswer {
		return false, false
	}
	st := f.setups[id]
	return st != nil && st.approved, true
}

func (f *Fake) CreatePaymentToken(_ context.Context, requestID, setupTokenID string) (string, paypal.Outcome) {
	fault := f.begin(Call{Op: CreatePaymentToken, RequestID: requestID, ID: setupTokenID})
	defer f.mu.Unlock()
	if fault == NoAnswer {
		return "", paypal.Unknown
	}
	if fault == Refuse {
		return "", paypal.Failed
	}
	vault, ok := f.results[requestID].(string)
	if !ok {
		st := f.setups[setupTokenID]
		switch {
		case st == nil || !st.approved:
			return "", paypal.Failed
		case st.vault != "":
			vault = st.vault
		case st.existing != "":
			if _, held := f.vaults[st.existing]; !held {
				return "", paypal.Failed // 422 INTERNAL_ERROR in the sandbox
			}
			vault = st.existing
		default:
			vault = f.newID("VT-")
			f.vaults[vault] = st.payer
		}
		st.vault = vault
		f.results[requestID] = vault
	}
	if fault == LostAnswer {
		return "", paypal.Unknown
	}
	return vault, paypal.Succeeded
}

func (f *Fake) CheckPaymentToken(_ context.Context, vaultID string) paypal.Presence {
	fault := f.begin(Call{Op: CheckPaymentToken, ID: vaultID})
	defer f.mu.Unlock()
	if fault != NoFault {
		return paypal.PresenceUnknown
	}
	if _, ok := f.vaults[vaultID]; ok {
		return paypal.Present
	}
	return paypal.Gone
}

func (f *Fake) DeletePaymentToken(_ context.Context, requestID, vaultID string) paypal.Outcome {
	fault := f.begin(Call{Op: DeletePaymentToken, RequestID: requestID, ID: vaultID})
	defer f.mu.Unlock()
	switch fault {
	case NoAnswer:
		return paypal.Unknown
	case Refuse:
		return paypal.Failed
	case LostAnswer:
		delete(f.vaults, vaultID)
		return paypal.Unknown
	}
	delete(f.vaults, vaultID)
	return paypal.Succeeded
}

func (f *Fake) Charge(_ context.Context, c paypal.Charge) paypal.Charged {
	fault := f.begin(Call{Op: Charge, RequestID: c.RequestID, ID: c.VaultID, InvoiceID: c.InvoiceID, Amount: c.Amount})
	defer f.mu.Unlock()
	switch fault {
	case NoAnswer:
		return paypal.Charged{Outcome: paypal.Unknown}
	case Refuse:
		return paypal.Charged{Outcome: paypal.Failed, Refusal: paypal.Refused}
	}
	got, ok := f.results[c.RequestID].(paypal.Charged)
	if !ok {
		payer, held := f.vaults[c.VaultID]
		switch {
		case !held:
			return paypal.Charged{Outcome: paypal.Failed, Refusal: paypal.NotSaved}
		case f.invoiced(c.InvoiceID):
			return paypal.Charged{Outcome: paypal.Failed, Refusal: paypal.DuplicateInvoice}
		case f.broke[payer]:
			return paypal.Charged{Outcome: paypal.Failed, Refusal: paypal.Refused}
		}
		got = paypal.Charged{Outcome: paypal.Succeeded, CaptureID: f.newID("CAP-")}
		f.capts = append(f.capts, Capture{ID: got.CaptureID, InvoiceID: c.InvoiceID, VaultID: c.VaultID, Amount: c.Amount})
		f.results[c.RequestID] = got
	}
	if fault == LostAnswer {
		return paypal.Charged{Outcome: paypal.Unknown}
	}
	return got
}

// invoiced tells whether a charge with invoiceID succeeded; f.mu is held.
func (f *Fake) invoiced(invoiceID string) bool {
	for _, c := range f.capts {
		if c.InvoiceID == invoiceID {
			return true
		}
	}
	return false
}
