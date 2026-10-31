package session

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/gate"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/paypal"
	"github.com/sumiredc/barpay/internal/store"
)

// Waker is told when a notification was stored, so it is sent at once (notify.Dispatcher).
type Waker interface{ Wake() }

// API serves the session operations of the store API (createCheckin, getSession, reportOrder,
// requestRegister) and of the admin API (listSessions and the operations on a session).
type API struct {
	Store  *store.Store
	Clock  clock.Clock
	Keys   *idempotency.Keeper
	Notify Waker
	// PayPal charges a linked session's saved PayPal at checkout; Saved reads the saved PayPals.
	PayPal paypal.Client
	Saved  SavedPaypals
	// Gate is the edge device at the exit gate.
	Gate *gate.Hub
	// ExitTokenRefresh, ReunlockWindow and MaxStay are the config file's (the PRD's UC8).
	ExitTokenRefresh time.Duration
	ReunlockWindow   time.Duration
	MaxStay          time.Duration
	Log              *slog.Logger
}

// Register adds the operations to r.
func (a API) Register(r *httpapi.Router) {
	r.Store("POST /checkins", a.Keys.Require(http.HandlerFunc(a.createCheckin)))
	r.Store("GET /sessions/{session_id}", http.HandlerFunc(a.getSession))
	r.Store("POST /sessions/{session_id}/orders", http.HandlerFunc(a.reportOrder))
	r.Store("POST /sessions/{session_id}/register-request", http.HandlerFunc(a.requestRegister))
	r.Store("GET /sessions/{session_id}/exit-token", http.HandlerFunc(a.getExitToken))
	r.Admin("GET /sessions", http.HandlerFunc(a.listSessions))
	r.Admin("POST /sessions/{session_id}/register-payment", http.HandlerFunc(a.recordRegisterPayment))
	r.Admin("POST /sessions/{session_id}/mark-unpaid", http.HandlerFunc(a.markUnpaid))
	r.Admin("POST /sessions/{session_id}/close", a.Keys.Require(http.HandlerFunc(a.closeSession)))
	r.Admin("POST /sessions/{session_id}/retry", a.Keys.Require(http.HandlerFunc(a.retryCharge)))
	r.Admin("POST /sessions/{session_id}/settle-outside", http.HandlerFunc(a.settleOutside))
	r.Admin("POST /gate/unlock", http.HandlerFunc(a.unlockGate))
	r.Edge("POST /exit-scans", http.HandlerFunc(a.scanExitToken))
	r.Edge("GET /unlocks/next", http.HandlerFunc(a.waitForUnlock))
	r.Edge("POST /unlocks/{unlock_id}/result", http.HandlerFunc(a.reportUnlockResult))
}

// Error codes of the spec's Error that these operations return.
const (
	codeSessionNotFound      httpapi.Code = "session_not_found"
	codeSessionNotActive     httpapi.Code = "session_not_active"
	codeOrderConflict        httpapi.Code = "order_conflict"
	codeCurrencyMismatch     httpapi.Code = "currency_mismatch"
	codeAmountTooLarge       httpapi.Code = "amount_too_large"
	codeMemberHasOpenSession httpapi.Code = "member_has_open_session"
	codeSessionNotAtRegister httpapi.Code = "session_not_at_register"
	codeExitTokenUnavailable httpapi.Code = "exit_token_unavailable"
	codeSessionNotLinked     httpapi.Code = "session_not_linked"
	codeNotPaymentFailed     httpapi.Code = "session_not_payment_failed"
	codeRetryWindowOver      httpapi.Code = "retry_window_over"
	codeSavedPaypalNotFound  httpapi.Code = "saved_paypal_not_found"
	codeGateUnavailable      httpapi.Code = "gate_unavailable"
	codeUnlockNotFound       httpapi.Code = "unlock_not_found"
)
