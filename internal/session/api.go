package session

import (
	"net/http"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/idempotency"
	"github.com/sumiredc/barpay/internal/store"
)

// Waker is told when a notification was stored, so it is sent at once (notify.Dispatcher).
type Waker interface{ Wake() }

// API serves the session operations of the store API: createCheckin.
type API struct {
	Store  *store.Store
	Clock  clock.Clock
	Keys   *idempotency.Keeper
	Notify Waker
}

// Register adds the operations to r.
func (a API) Register(r *httpapi.Router) {
	r.Store("POST /checkins", a.Keys.Require(http.HandlerFunc(a.createCheckin)))
}

// Error codes of the spec's Error that these operations return.
const codeMemberHasOpenSession httpapi.Code = "member_has_open_session"
