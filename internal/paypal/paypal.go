// Package paypal calls PayPal's REST API with the store's credentials (the PRD's Fixed
// constraints, The store's own PayPal): the setup tokens and saved PayPals (vault payment tokens)
// of linking, checking and deleting a member's saved PayPal, and the checkout charge with it.
//
// Every call that changes something in PayPal takes the PayPal-Request-Id its caller stored
// before calling, and says whether PayPal did it, refused it, or gave no readable answer
// (Outcome), so that the caller can send the same call again (the PRD's Fixed constraints,
// Idempotency).
package paypal

import (
	"context"

	"github.com/sumiredc/barpay/internal/money"
)

// Outcome is what a call that changes something in PayPal came to.
type Outcome int

const (
	// Unknown: no readable answer (a timeout, a dropped connection, a body that does not parse,
	// or an answer that does not say). PayPal may have done it; send the same call again with the
	// same PayPal-Request-Id.
	Unknown Outcome = iota
	// Succeeded: PayPal answered that it did it.
	Succeeded
	// Failed: PayPal answered that it did not.
	Failed
)

func (o Outcome) String() string {
	switch o {
	case Succeeded:
		return "succeeded"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Presence is what a check of a saved PayPal found.
type Presence int

const (
	// PresenceUnknown: no readable answer. It says nothing about whether PayPal still has it.
	PresenceUnknown Presence = iota
	// Present: PayPal still has it. It does not tell whether a charge with it will go through.
	Present
	// Gone: PayPal answered that it does not have it.
	Gone
)

// URLs are where PayPal sends the member's browser after approving or cancelling.
type URLs struct {
	Return string
	Cancel string
}

// SetupToken is a setup token PayPal made, and where the member approves it.
type SetupToken struct {
	ID         string
	ApproveURL string
}

// Charge is one send of the checkout charge: the total, charged with a saved PayPal (the PRD's
// Payment model). RequestID and InvoiceID are stored before it is sent.
type Charge struct {
	RequestID string
	InvoiceID string
	VaultID   string
	Amount    money.Amount
}

// Refusal is why PayPal answered that it did not charge.
type Refusal int

const (
	// Refused: PayPal refused or failed the charge (such as TRANSACTION_REFUSED or
	// INSTRUMENT_DECLINED). It does not use up the invoice_id, so a staff retry can still succeed.
	Refused Refusal = iota + 1
	// NotSaved: PayPal no longer has the saved PayPal (403 PERMISSION_DENIED).
	NotSaved
	// DuplicateInvoice: a charge with this invoice_id already succeeded (422 DUPLICATE_INVOICE_ID).
	DuplicateInvoice
)

// Charged is what a charge came to: the capture ID when Succeeded, why when Failed.
type Charged struct {
	Outcome   Outcome
	CaptureID string
	Refusal   Refusal
}

// Client is PayPal as BarPay uses it. Tests use a fake that can be told to misbehave.
type Client interface {
	// CreateSetupToken makes a setup token for saving PayPal for merchant-initiated charges.
	CreateSetupToken(ctx context.Context, requestID string, urls URLs) (SetupToken, Outcome)
	// SetupTokenApproved reads whether the member approved the setup token with id. ok is false
	// when there was no readable answer.
	SetupTokenApproved(ctx context.Context, id string) (approved, ok bool)
	// CreatePaymentToken saves PayPal from an approved setup token and returns the saved PayPal's
	// ID (the vault ID). It may be one PayPal already held for the same PayPal account.
	CreatePaymentToken(ctx context.Context, requestID, setupTokenID string) (string, Outcome)
	// CheckPaymentToken reads whether PayPal still has the saved PayPal with vaultID.
	CheckPaymentToken(ctx context.Context, vaultID string) Presence
	// DeletePaymentToken deletes the saved PayPal with vaultID. Succeeded also when PayPal no
	// longer had it.
	DeletePaymentToken(ctx context.Context, requestID, vaultID string) Outcome
	// Charge charges c.Amount with the saved PayPal c.VaultID as an order with intent CAPTURE.
	// Sent again with the same RequestID, it returns the first charge if that one succeeded.
	Charge(ctx context.Context, c Charge) Charged
}
